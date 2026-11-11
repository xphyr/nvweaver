package api

import (
	"errors"
	"testing"

	"nvweaver/internal/storage"
	"nvweaver/internal/target"
)

// countingDriver is a pool whose List says what LVM has, and counts how often it's asked.
type countingDriver struct {
	fakeDriver
	names []string
	err   error
	lists *int
}

func (d countingDriver) List() ([]string, error) {
	*d.lists++
	return d.names, d.err
}

// LVM snapshots are inactive, with no device node, so that isn't how to tell whether one is there: found when every
// snapshot on the user's lab server showed as missing.
func TestInactiveLVMSnapshotsArentMissing(t *testing.T) {
	e := newEnv(t)
	st := target.NewState()
	st.Pools["lvm"] = &target.Pool{Name: "lvm", Type: storage.TypeLVMThin, VolumeGroup: "vg", ThinPool: "pool0", WarningPercent: 80}
	st.Snapshots["here"] = &target.Snapshot{Name: "here", Pool: "lvm", InternalName: "nvw-here", DevicePath: "/dev/vg/nvw-here"}
	st.Snapshots["gone"] = &target.Snapshot{Name: "gone", Pool: "lvm", InternalName: "nvw-gone", DevicePath: "/dev/vg/nvw-gone"}
	e.m.state.Store(st)
	lists := 0
	d := countingDriver{names: []string{"nvw-here", "nvw-other"}, lists: &lists}
	e.m.newDriver = func(*target.Pool) (storage.Driver, error) { return d, nil }

	if got := e.m.snapshotState(st.Snapshots["here"]); got != "online" {
		t.Errorf("an inactive snapshot LVM has: %s", got)
	}
	if got := e.m.snapshotState(st.Snapshots["gone"]); got != "missing" {
		t.Errorf("one LVM hasn't: %s", got)
	}
	for range 100 {
		e.m.snapshotState(st.Snapshots["here"])
	}
	if lists > 2 {
		t.Errorf("asked LVM %d times for 102 states", lists)
	}

	// When LVM can't be asked, it's no reason to say a snapshot is missing.
	e.m.lvs = lvNames{}
	e.m.newDriver = func(*target.Pool) (storage.Driver, error) {
		return countingDriver{err: errors.New("lvm: no such volume group"), lists: &lists}, nil
	}
	if got := e.m.snapshotState(st.Snapshots["gone"]); got != "online" {
		t.Errorf("with LVM unreachable: %s", got)
	}
}
