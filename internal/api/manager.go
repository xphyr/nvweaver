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
	"syscall"

	"nvweaver/internal/storage"
	"nvweaver/internal/target"
)

// Manager serialises changes to the target and persists its desired state.
type Manager struct {
	mu        sync.Mutex
	backend   target.Backend
	state     *target.State
	statePath string
	volumeDir string
	log       *slog.Logger
	// newDriver returns the driver for a pool. Tests substitute a fake.
	newDriver func(*target.Pool) (storage.Driver, error)
}

// NewManager loads any saved state and applies it to the kernel. Attributes
// that already match are not written, so adopting a target left running by a
// previous instance does not disturb its hosts.
func NewManager(b target.Backend, dataDir string, log *slog.Logger) (*Manager, error) {
	m := &Manager{
		backend:   b,
		state:     target.NewState(),
		statePath: filepath.Join(dataDir, "state.json"),
		volumeDir: filepath.Join(dataDir, "volumes"),
		log:       log,
		newDriver: storage.New,
	}
	if err := os.MkdirAll(m.volumeDir, 0o700); err != nil {
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
	if err := b.Reconcile(target.NewState(), loaded); err != nil {
		return nil, fmt.Errorf("restore saved configuration: %w", err)
	}
	m.state = loaded
	return m, nil
}

// view runs fn with the current state, which fn must not modify or retain.
func (m *Manager) view(fn func(*target.State) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fn(m.state)
}

// mutate applies fn to a copy of the state, moves the kernel to the result and
// saves it. On failure the kernel is moved back, so a request either happens
// completely or not at all.
func (m *Manager) mutate(fn func(*target.State) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.state.Clone()
	if err := fn(next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := m.backend.Check(next); err != nil {
		return err
	}
	err := m.backend.Reconcile(m.state, next)
	if err == nil {
		if err = m.save(next); err == nil {
			m.state = next
			return nil
		}
	}
	if rerr := m.backend.Reconcile(next, m.state); rerr != nil {
		m.log.Error("rollback failed; kernel may differ from saved state", "error", rerr, "cause", err)
	}
	return err
}

// save writes the state atomically. It can hold DH-CHAP secrets, hence 0600.
func (m *Manager) save(s *target.State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.statePath)
	tmp, err := os.CreateTemp(dir, ".state-*")
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
	if err := os.Rename(tmp.Name(), m.statePath); err != nil {
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
	return m.backend.Reconcile(m.state, target.NewState())
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
