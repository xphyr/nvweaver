package api

import (
	"sync"
	"time"

	"nvweaver/internal/storage"
	"nvweaver/internal/target"
)

// An LVM snapshot is kept inactive, with no device node until something needs it, so whether its device exists says
// nothing about whether it does. These are the names LVM has, per pool, read now and then, so that a listing of thousands
// of snapshots costs one lvs, not one each.
const (
	lvNamesFresh   = 30 * time.Second // how long a pool's list is used
	lvNamesRecheck = 2 * time.Second  // a name not in a list older than this reads it again, as it may be new
)

type lvNames struct {
	mu     sync.Mutex
	byPool map[string]lvList
}

type lvList struct {
	at    time.Time
	names map[string]bool
}

// lvExists reports whether a pool's LVM has a volume of this internal name. If LVM can't say, it says so.
func (m *Manager) lvExists(pool *target.Pool, internal string) (exists, known bool) {
	m.lvs.mu.Lock()
	defer m.lvs.mu.Unlock()
	if m.lvs.byPool == nil {
		m.lvs.byPool = map[string]lvList{}
	}
	l, ok := m.lvs.byPool[pool.Name]
	age := time.Since(l.at)
	if ok && age < lvNamesFresh && (l.names[internal] || age < lvNamesRecheck) {
		return l.names[internal], true
	}
	d, err := m.newDriver(pool)
	if err != nil {
		return false, false
	}
	names, err := d.List()
	if err != nil {
		return false, false
	}
	l = lvList{at: time.Now(), names: map[string]bool{}}
	for _, n := range names {
		l.names[n] = true
	}
	m.lvs.byPool[pool.Name] = l
	return l.names[internal], true
}

// snapshotState is whether a snapshot's storage is there: its device or file, or for LVM, which keeps snapshots
// inactive, the volume itself. A snapshot LVM can't say about isn't reported missing, as that would be a guess.
func (m *Manager) snapshotState(snap *target.Snapshot) string {
	if storageState(snap.DevicePath) == "online" {
		return "online"
	}
	pool := m.current().Pools[snap.Pool]
	if pool == nil || pool.Type != storage.TypeLVMThin {
		return "missing"
	}
	if exists, known := m.lvExists(pool, snap.InternalName); exists || !known {
		return "online"
	}
	return "missing"
}
