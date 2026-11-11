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
	"slices"
	"strings"
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
	// lvs are the names each LVM pool has, read now and then, for snapshots' state: see lvnames.go.
	lvs lvNames
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
	// deviceIDs identifies a block device, however it's named. Tests substitute a fake.
	deviceIDs func(string) []string
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
		deviceIDs:       deviceIDs,
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

// checkPassthruFree refuses a controller that's already in use: passed through by another subsystem, with a namespace
// exported, or with a namespace that something on this machine holds, such as a mounted filesystem.
func checkPassthruFree(s *target.State, ctrl string) error {
	for _, sub := range s.Subsystems {
		if sub.Passthru != "" && resolve(sub.Passthru) == resolve(ctrl) {
			return target.NewConflict("%s is already passed through by %s", ctrl, sub.NQN)
		}
	}
	namespaces, err := controllerNamespaces(ctrl)
	if err != nil {
		return err
	}
	for _, ns := range namespaces {
		if err := checkUnshared(s, ns, "", 0, ""); err != nil {
			return target.NewConflict("%s can't be passed through: %v", ctrl, err)
		}
		if err := checkInUse(ns); err != nil {
			return target.NewConflict("%s can't be passed through: %v", ctrl, err)
		}
	}
	return nil
}

// checkInUse is checkBacking, a variable so that tests can make a namespace busy.
var checkInUse = checkBacking

// Where controllerNamespaces looks. Tests use a copy of the layouts below.
var (
	sysRoot = "/sys"
	devRoot = "/dev"
)

var (
	nsDeviceRE = regexp.MustCompile(`^nvme[0-9]+n[0-9]+$`)
	nsPathRE   = regexp.MustCompile(`^nvme[0-9]+c[0-9]+n[0-9]+$`)
)

// controllerNamespaces finds the block devices of an NVMe controller's namespaces. Without multipath they're in the
// controller's directory, as nvme1n1. With it, which fabrics controllers and shared namespaces have, the controller's
// directory has hidden path devices such as nvme2c2n1, with no device node, and the namespace is nvme2n1 in the NVMe
// subsystem's directory, beside the controller's name. Path devices with no namespace found is an error, rather than
// nothing to check.
func controllerNamespaces(ctrl string) ([]string, error) {
	name := filepath.Base(ctrl)
	entries, err := os.ReadDir(filepath.Join(sysRoot, "class/nvme", name))
	if err != nil {
		return nil, target.Invalid("can't list the namespaces of %s: %v (is /sys visible inside the container?)", ctrl, err)
	}
	found := map[string]bool{}
	paths := 0
	for _, e := range entries {
		switch {
		case nsDeviceRE.MatchString(e.Name()):
			found[e.Name()] = true
		case nsPathRE.MatchString(e.Name()):
			paths++
		}
	}
	if paths > 0 {
		subsystems, _ := filepath.Glob(filepath.Join(sysRoot, "class/nvme-subsystem/*", name))
		for _, link := range subsystems {
			entries, err := os.ReadDir(filepath.Dir(link))
			if err != nil {
				return nil, target.Invalid("can't list the namespaces of %s: %v (is /sys visible inside the container?)", ctrl, err)
			}
			for _, e := range entries {
				if nsDeviceRE.MatchString(e.Name()) {
					found[e.Name()] = true
				}
			}
		}
		if len(found) == 0 {
			return nil, target.Invalid("%s has %d namespace paths, but their namespaces weren't found in %s/class/nvme-subsystem", ctrl, paths, sysRoot)
		}
	}
	out := make([]string, 0, len(found))
	for n := range found {
		out = append(out, filepath.Join(devRoot, n))
	}
	slices.Sort(out)
	return out, nil
}

// checkUnshared stops the same backing store being exported twice, which
// would let two namespaces silently corrupt one another. Paths are compared by
// what they reach: a hard link, a bind mount, or a disk and one of its
// partitions are the same store. skipNQN and skipNSID name the namespace being
// updated, if any, and skipVolume the volume being mapped, whose own entry and
// mappings don't count.
//
// Mapping a volume compares it only with namespaces exported by path. Volumes
// and snapshots are each a device or file their pool made for them alone, so
// one can't reach another, and a namespace of a volume is that volume's own
// device. Looking each of those up made every publish cost a few syscalls per
// volume, which at 5,000 LVM volumes was most of its time: see decision 49.
func checkUnshared(s *target.State, path, skipNQN string, skipNSID uint32, skipVolume string) error {
	want, err := newStore(path)
	if err != nil {
		return err
	}
	mapping := skipVolume != ""
	for _, vol := range s.Volumes {
		if !mapping && want.same(vol.DevicePath) {
			return target.NewConflict("%s belongs to volume %s: map the volume instead", path, vol.Name)
		}
	}
	for _, snap := range s.Snapshots {
		if !mapping && want.same(snap.DevicePath) {
			return target.NewConflict("%s belongs to snapshot %s: clone it to a volume instead", path, snap.Name)
		}
	}
	for _, sub := range s.Subsystems {
		if sub.Passthru != "" {
			// What can't be listed was checked when it was passed through, so isn't a reason to refuse here.
			namespaces, _ := controllerNamespaces(sub.Passthru)
			for _, ns := range namespaces {
				if want.same(ns) {
					return target.NewConflict("%s is a namespace of %s, which %s passes through", path, sub.Passthru, sub.NQN)
				}
			}
		}
		for _, ns := range sub.Namespaces {
			if sub.NQN == skipNQN && ns.NSID == skipNSID || mapping && ns.Volume != "" {
				continue
			}
			if want.same(ns.DevicePath) {
				return target.NewConflict("%s is already exported by %s namespace %d", path, sub.NQN, ns.NSID)
			}
		}
	}
	return nil
}

// store is a backing path, and what it reaches if it can be found.
type store struct {
	path  string
	id    storeID
	ok    bool
	under []storeID // the devices a block device is built on, such as a logical volume's partition
}

// newStore finds what a path reaches, and for a block device, everything it's
// built on. Opening with O_EXCL, in checkBacking, already refuses a device that
// something is built on, but not one built on a device already exported, as
// the target opens what it exports without claiming it. Only the new path is
// walked, so that each request reads sysfs for one device, not for every
// volume. A block device that sysfs can't describe is refused: the check would
// otherwise pass a disk and its partition.
func newStore(path string) (store, error) {
	id, ok := identify(path)
	s := store{path: resolve(path), id: id, ok: ok}
	if !ok || !id.block {
		return s, nil
	}
	if id.sysErr != nil {
		return s, id.sysErr
	}
	under, err := underneath(id.rdev)
	if err != nil {
		return s, err
	}
	s.under = under
	return s, nil
}

// same reports whether path reaches this store, or overlaps it.
func (s store) same(path string) bool {
	if resolve(path) == s.path {
		return true
	}
	if !s.ok {
		return false
	}
	id, ok := identify(path)
	if !ok {
		return false
	}
	if s.id.overlaps(id) {
		return true
	}
	for _, u := range s.under {
		if u.overlaps(id) {
			return true
		}
	}
	return false
}

// storeID identifies a backing store, whatever path reaches it: a file by its
// filesystem and inode, and a block device by its number, with the disk that
// it's a partition of. Numbers are "major:minor", as sysfs writes them.
type storeID struct {
	block    bool
	dev, ino uint64
	rdev     string
	whole    string
	sysErr   error // sysfs couldn't say whether a block device is a partition
}

// overlaps reports whether writing to one could change the other. Two
// partitions of one disk don't.
func (a storeID) overlaps(b storeID) bool {
	if a.block != b.block {
		return false
	}
	if !a.block {
		return a.dev == b.dev && a.ino == b.ino
	}
	return a.rdev == b.rdev || a.whole != "" && a.whole == b.rdev || b.whole != "" && b.whole == a.rdev
}

// identify finds what a path reaches. It's a variable so that tests can give a
// disk and its partition, which they can't create.
var identify = func(path string) (storeID, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return storeID{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return storeID{}, false
	}
	if info.Mode().IsRegular() {
		return storeID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return storeID{}, false
	}
	return blockID(majorMinor(uint64(st.Rdev))), true
}

// blockID describes a block device from sysfs, by its number.
func blockID(rdev string) storeID {
	id := storeID{block: true, rdev: rdev}
	// Not filepath.Join, which would remove the "..": the kernel follows the symlink first.
	sys := filepath.Join(sysRoot, "dev/block", rdev)
	if _, err := os.Stat(sys); err != nil {
		id.sysErr = target.Invalid("cannot read %s, which is needed to tell a disk from its partitions: %v", sys, err)
		return id
	}
	if _, err := os.Stat(sys + "/partition"); err == nil {
		b, err := os.ReadFile(sys + "/../dev")
		if err != nil {
			id.sysErr = target.Invalid("%s is a partition, but its disk can't be read from sysfs, which is needed to tell a disk from its partitions: %v", rdev, err)
			return id
		}
		id.whole = strings.TrimSpace(string(b))
	} else if !errors.Is(err, fs.ErrNotExist) {
		id.sysErr = target.Invalid("cannot read %s/partition, which is needed to tell a disk from its partitions: %v", sys, err)
	}
	return id
}

// maxUnder bounds the walk down a stack of devices. Real stacks, such as a thin
// volume on a pool on an encrypted partition, are a handful deep.
const maxUnder = 32

// underneath walks sysfs from a block device down through what it's built on,
// its "slaves", to the disks at the bottom, each with the disk it's a partition
// of. A stack too deep to walk is refused rather than passed unchecked.
func underneath(rdev string) ([]storeID, error) {
	seen := map[string]bool{rdev: true}
	queue := []string{rdev}
	var out []storeID
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		dir := filepath.Join(sysRoot, "dev/block", cur, "slaves")
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue // Partitions have no slaves directory.
		}
		if err != nil {
			return nil, target.Invalid("cannot read %s, which is needed to find what a device is built on: %v", dir, err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(dir, e.Name(), "dev"))
			if err != nil {
				return nil, target.Invalid("cannot read the number of %s, under %s: %v", e.Name(), rdev, err)
			}
			num := strings.TrimSpace(string(b))
			if seen[num] {
				continue
			}
			if len(seen) > maxUnder {
				return nil, target.Invalid("more than %d devices are under %s, too many to check that none of them is exported", maxUnder, rdev)
			}
			seen[num] = true
			id := blockID(num)
			if id.sysErr != nil {
				return nil, id.sysErr
			}
			out = append(out, id)
			queue = append(queue, num)
		}
	}
	return out, nil
}

// majorMinor splits a Linux device number, as the kernel's new_encode_dev does.
func majorMinor(rdev uint64) string {
	major := (rdev>>8)&0xfff | (rdev>>32)&^0xfff
	minor := rdev&0xff | (rdev>>12)&0xffffff00
	return fmt.Sprintf("%d:%d", major, minor)
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
	if err := sizeFile(f, size); err != nil {
		if rerr := removeManagedFile(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %v, and %s couldn't be removed, which will be tried again at the next start: %v", errFileLeft, err, path, rerr)
		}
		return "", err
	}
	return path, nil
}

// errFileLeft is a failed create that left its file, which recovery removes.
var errFileLeft = errors.New("the new file was left")

// removeManagedFile and sizeFile are variables so that tests can make them fail.
var (
	removeManagedFile = os.Remove
	sizeFile          = (*os.File).Truncate
)

// checkNewFile refuses a managed file's name that's invalid or already taken, before anything is recorded about it.
func (m *Manager) checkNewFile(name string) error {
	if !volumeNameRE.MatchString(name) {
		return target.Invalid("file name %q may only contain letters, digits, '.', '_' and '-'", name)
	}
	if _, err := os.Lstat(filepath.Join(m.volumeDir, name)); err == nil {
		return target.NewConflict("volume %q already exists", name)
	}
	return nil
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
