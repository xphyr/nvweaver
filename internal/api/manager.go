// Package api exposes the target's configuration over HTTP and owns the
// desired state: every change is applied to the kernel, then saved.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"

	"nvweaver/internal/storage"
	"nvweaver/internal/target"
)

// Manager serialises changes to the target and persists its desired state.
type Manager struct {
	mu sync.Mutex // Held for each change, so one is applied at a time.
	// state is replaced whole, and never modified once it's here, so reads don't wait for a change.
	state atomic.Pointer[target.State]
	// waiting counts the changes queued for mu.
	waiting atomic.Int32
	busyMu  sync.Mutex
	// busy holds the objects reserved by requests in progress: the number sharing each, or -1 for one
	// that's exclusive.
	busy map[string]int
	// inFlight holds the internal names being worked on by those requests.
	inFlight  map[string]bool
	backend   target.Backend
	statePath string
	volumeDir string
	intentDir string
	log       *slog.Logger
	// newDriver returns the driver for a pool. Tests substitute a fake.
	newDriver func(*target.Pool) (storage.Driver, error)
	// visibleToLVM lists the devices this machine's LVM can read. Tests substitute a fake.
	visibleToLVM func() ([]string, error)
	// missingLVMTools says what LVM thin pools need that's missing. Tests substitute a fake.
	missingLVMTools func() []string
	// lvmChanged asks WatchLVM for another check.
	lvmChanged chan struct{}
	// warnings holds what WatchLVM last found.
	warnings atomic.Pointer[[]string]
	// crash stops a request at a named point. Only tests set it.
	crash func(point string)
}

// NewManager loads any saved state and applies it to the kernel. Attributes
// that already match are not written, so adopting a target left running by a
// previous instance does not disturb its hosts.
func NewManager(b target.Backend, dataDir string, log *slog.Logger) (*Manager, error) {
	m := &Manager{
		busy:      map[string]int{},
		inFlight:  map[string]bool{},
		backend:   b,
		statePath: filepath.Join(dataDir, "state.json"),
		volumeDir: filepath.Join(dataDir, "volumes"),
		intentDir: filepath.Join(dataDir, "intents"),
		log:       log,
		newDriver: storage.New,

		visibleToLVM:    storage.VisibleToLVM,
		missingLVMTools: storage.MissingLVMTools,
		lvmChanged:      make(chan struct{}, 1),
	}
	m.state.Store(target.NewState())
	if err := os.MkdirAll(m.volumeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if err := os.MkdirAll(m.intentDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	saved, err := os.ReadFile(m.statePath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return m, nil
	case err != nil:
		return nil, fmt.Errorf("read state: %w", err)
	}
	loaded := target.NewState()
	if err := json.Unmarshal(saved, loaded); err != nil {
		return nil, fmt.Errorf("parse %s: %w", m.statePath, err)
	}
	loaded.Normalize()
	if err := b.Check(loaded); err != nil {
		return nil, fmt.Errorf("saved configuration cannot run on the %s backend: %w", b.Name(), err)
	}
	// Volumes may not be active yet this early in boot, and the backends are about to open them.
	for _, p := range loaded.Pools {
		d, err := m.newDriver(p)
		if err == nil {
			err = d.Activate()
		}
		if err != nil {
			log.Error("couldn't activate a pool", "pool", p.Name, "error", err)
		}
	}
	if err := b.Reconcile(target.NewState(), loaded); err != nil {
		return nil, fmt.Errorf("restore saved configuration: %w", err)
	}
	m.state.Store(loaded)
	return m, nil
}

// current returns the state as of now, which must not be modified. It stays valid, but goes out of date.
func (m *Manager) current() *target.State {
	return m.state.Load()
}

// view runs fn with the current state, which fn must not modify.
func (m *Manager) view(fn func(*target.State) error) error {
	return fn(m.current())
}

// mutate makes a change, if there's room for it in the queue. See commit.
func (m *Manager) mutate(fn func(*target.State) error) error {
	if err := m.admit(); err != nil {
		return err
	}
	return m.commit(fn)
}

// commit applies fn to a copy of the state, moves the target to the result and saves it. On failure
// the target is moved back, so a request either happens completely or not at all. fn runs with the lock
// held, so it must be quick. Work that's already been done uses this directly, so as not to be refused.
func (m *Manager) commit(fn func(*target.State) error) error {
	m.waiting.Add(1)
	m.mu.Lock()
	m.waiting.Add(-1)
	defer m.mu.Unlock()
	state := m.current()
	next := state.Clone()
	if err := fn(next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := m.backend.Check(next); err != nil {
		return err
	}
	err := m.backend.Reconcile(state, next)
	if err == nil {
		if err = m.save(next); err == nil {
			m.state.Store(next)
			m.noteExportsChanged()
			return nil
		}
	}
	if rerr := m.backend.Reconcile(next, state); rerr != nil {
		m.log.Error("rollback failed; kernel may differ from saved state", "error", rerr, "cause", err)
	}
	return err
}

// save writes the state atomically. It can hold DH-CHAP secrets, hence 0600.
func (m *Manager) save(s *target.State) error {
	// Compact, because this runs on every change, and indenting it cost more than half the time at scale.
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeDurably(m.statePath, b)
}

// writeDurably replaces a file atomically, so that it survives a crash once this returns.
func writeDurably(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Teardown removes everything this service configured from the target. The
// saved state is kept, so the next start brings it back.
func (m *Manager) Teardown() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.backend.Reconcile(m.current(), target.NewState())
}

// checkBacking refuses anything but a block device or regular file, and a
// block device that something else holds. nvmet opens devices non-exclusively
// and would otherwise export, say, a mounted root disk for hosts to overwrite.
func checkBacking(path string) error {
	if !filepath.IsAbs(path) {
		return target.Invalid("device_path %q must be absolute", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return target.Invalid("device_path %q: %v (is it visible inside the container?)", path, err)
	}
	switch mode := info.Mode(); {
	case mode.IsRegular():
		return nil
	case mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0:
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_EXCL, 0)
		if errors.Is(err, syscall.EBUSY) {
			return target.NewConflict("%s is in use (mounted, swap, or held by LVM/MD/another claimant)", path)
		}
		if err != nil {
			return target.Invalid("cannot open %s: %v", path, err)
		}
		return f.Close()
	default:
		return target.Invalid("%s is neither a block device nor a regular file", path)
	}
}

var nvmeCtrlRE = regexp.MustCompile(`^/dev/nvme[0-9]+$`)

func checkPassthru(path string) error {
	if !nvmeCtrlRE.MatchString(path) {
		return target.Invalid("passthru_device must be an NVMe controller such as /dev/nvme0, not %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return target.Invalid("passthru_device %q: %v (is it visible inside the container?)", path, err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return target.Invalid("%s is not a character device", path)
	}
	return nil
}

// checkUnshared stops the same backing store being exported twice, which
// would let two namespaces silently corrupt one another. skip names the
// namespace being updated, if any.
func checkUnshared(s *target.State, path, skipNQN string, skipNSID uint32) error {
	want := resolve(path)
	for _, vol := range s.Volumes {
		if resolve(vol.DevicePath) == want {
			return target.NewConflict("%s belongs to volume %s: map the volume instead", path, vol.Name)
		}
	}
	for _, snap := range s.Snapshots {
		if resolve(snap.DevicePath) == want {
			return target.NewConflict("%s belongs to snapshot %s: clone it to a volume instead", path, snap.Name)
		}
	}
	for _, sub := range s.Subsystems {
		for _, ns := range sub.Namespaces {
			if sub.NQN == skipNQN && ns.NSID == skipNSID {
				continue
			}
			if resolve(ns.DevicePath) == want {
				return target.NewConflict("%s is already exported by %s namespace %d", path, sub.NQN, ns.NSID)
			}
		}
	}
	return nil
}

func resolve(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return filepath.Clean(path)
}

var volumeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// createVolume makes a sparse file under the volume directory.
func (m *Manager) createVolume(name string, size int64) (string, error) {
	if !volumeNameRE.MatchString(name) {
		return "", target.Invalid("file name %q may only contain letters, digits, '.', '_' and '-'", name)
	}
	if size < 4096 || size%4096 != 0 {
		return "", target.Invalid("size_bytes must be a positive multiple of 4096")
	}
	path := filepath.Join(m.volumeDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return "", target.NewConflict("volume %q already exists", name)
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func backingSize(path string) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	size, err := f.Seek(0, 2)
	return size, err == nil
}
