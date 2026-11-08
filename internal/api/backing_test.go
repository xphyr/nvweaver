package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nvweaver/internal/target"
)

func TestAHardLinkIsTheSameStore(t *testing.T) {
	e := newEnv(t)
	first := e.file("first.img")
	link := filepath.Join(filepath.Dir(first), "link.img")
	if err := os.Link(first, link); err != nil {
		t.Fatal(err)
	}
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"nsid": 1, "device_path": first}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"nsid": 2, "device_path": link}, 409)
	other := e.file("other.img")
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"nsid": 2, "device_path": other}, 201)
	e.do("PATCH", "/v1/subsystems/"+subNQN+"/namespaces/2", obj{"device_path": link}, 409)
}

// A volume's file, reached by another name.
func TestAVolumesFileIsCheckedByIdentity(t *testing.T) {
	dir := t.TempDir()
	file, link := filepath.Join(dir, "vol"), filepath.Join(dir, "link")
	if err := os.WriteFile(file, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(file, link); err != nil {
		t.Fatal(err)
	}
	st := target.NewState()
	st.Volumes["v"] = &target.Volume{Name: "v", DevicePath: file}
	if err := checkUnshared(st, link, "", 0, ""); !isConflict(err) {
		t.Errorf("a hard link of a volume's file: %v", err)
	}

	// The other way round: the link exported first, then the volume mapped.
	st = target.NewState()
	st.Volumes["v"] = &target.Volume{Name: "v", DevicePath: file}
	st.Subsystems[subNQN] = &target.Subsystem{NQN: subNQN, Namespaces: map[uint32]*target.Namespace{
		1: {NSID: 1, DevicePath: link},
	}}
	if err := checkUnshared(st, file, "", 0, "v"); !isConflict(err) {
		t.Errorf("mapping a volume whose file is exported by another name: %v", err)
	}
	st.Subsystems[subNQN].Namespaces[1] = &target.Namespace{NSID: 1, Volume: "v", DevicePath: file}
	if err := checkUnshared(st, file, "", 0, "v"); err != nil {
		t.Errorf("the volume's own mapping isn't a conflict: %v", err)
	}
}

func TestBlockDevicesOverlap(t *testing.T) {
	disk := storeID{block: true, rdev: "8:16"}
	part1 := storeID{block: true, rdev: "8:17", whole: "8:16"}
	part2 := storeID{block: true, rdev: "8:18", whole: "8:16"}
	other := storeID{block: true, rdev: "8:32"}
	file := storeID{dev: 2049, ino: 12}
	cases := []struct {
		name string
		a, b storeID
		want bool
	}{
		{"the same device", disk, disk, true},
		{"a disk and its partition", disk, part1, true},
		{"a partition and its disk", part1, disk, true},
		{"two partitions of one disk", part1, part2, false},
		{"two disks", disk, other, false},
		{"a partition and another disk", part1, other, false},
		{"a file and a device", file, disk, false},
		{"the same file", file, file, true},
		{"another file", file, storeID{dev: 2049, ino: 13}, false},
		{"the same inode on another filesystem", file, storeID{dev: 2050, ino: 12}, false},
	}
	for _, c := range cases {
		if got := c.a.overlaps(c.b); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// Through the API, with the identities a disk and its partition would have.
func TestADiskAndItsPartitionAreTheSameStore(t *testing.T) {
	e := newEnv(t)
	disk, part := e.file("sdb"), e.file("sdb1")
	real := identify
	identify = func(path string) (storeID, bool) {
		switch path {
		case disk:
			return storeID{block: true, rdev: "8:16"}, true
		case part:
			return storeID{block: true, rdev: "8:17", whole: "8:16"}, true
		}
		return real(path)
	}
	t.Cleanup(func() { identify = real })
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"nsid": 1, "device_path": part}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"nsid": 2, "device_path": disk}, 409)
}

// The real lookup, on whatever this machine has. A partition must name its disk.
func TestIdentifyFindsAPartitionsDisk(t *testing.T) {
	parts, _ := filepath.Glob("/sys/class/block/*/partition")
	if len(parts) == 0 {
		t.Skip("no partitions here")
	}
	name := filepath.Base(filepath.Dir(parts[0]))
	if _, err := os.Stat("/dev/" + name); err != nil {
		t.Skipf("no /dev/%s", name)
	}
	id, ok := identify("/dev/" + name)
	if !ok || !id.block || id.whole == "" || id.whole == id.rdev {
		t.Errorf("/dev/%s: %+v, %v", name, id, ok)
	}
}

func isConflict(err error) bool {
	var c *target.ConflictError
	return errors.As(err, &c)
}
