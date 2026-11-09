package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// fakeBlock builds a sysfs tree under a temporary sysRoot, as the kernel lays
// it out: devices under /sys/devices, a partition inside its disk's directory,
// /sys/dev/block/MAJ:MIN linking to each, and "slaves" linking to what a
// device-mapper device is built on. It returns a function that adds a device.
func fakeBlock(t *testing.T) (add func(dir, num string, partition bool, slaves ...string)) {
	t.Helper()
	root := t.TempDir()
	real := sysRoot
	sysRoot = root
	t.Cleanup(func() { sysRoot = real })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "dev/block"), 0o755))
	return func(dir, num string, partition bool, slaves ...string) {
		t.Helper()
		full := filepath.Join(root, "devices", dir)
		must(os.MkdirAll(full, 0o755))
		must(os.WriteFile(filepath.Join(full, "dev"), []byte(num+"\n"), 0o644))
		if partition {
			must(os.WriteFile(filepath.Join(full, "partition"), []byte("1\n"), 0o644))
		} else {
			must(os.MkdirAll(filepath.Join(full, "slaves"), 0o755))
		}
		for _, s := range slaves {
			must(os.Symlink(filepath.Join(root, "devices", s), filepath.Join(full, "slaves", filepath.Base(s))))
		}
		must(os.Symlink(full, filepath.Join(root, "dev/block", num)))
	}
}

// identifyBy gives paths the identities of fake devices, read from the fake sysfs.
func identifyBy(t *testing.T, devices map[string]string) {
	real := identify
	identify = func(path string) (storeID, bool) {
		if num, ok := devices[path]; ok {
			return blockID(num), true
		}
		return real(path)
	}
	t.Cleanup(func() { identify = real })
}

func TestASysfsThatCantAnswerRefusesTheExport(t *testing.T) {
	add := fakeBlock(t)
	add("sdb", "8:16", false)
	add("sdb/sdb1", "8:17", true)
	// A partition whose disk has no dev file to say what it is.
	add("orphan/sdc1", "8:33", true)
	identifyBy(t, map[string]string{"/dev/sdb": "8:16", "/dev/sdb1": "8:17", "/dev/sdc1": "8:33", "/dev/gone": "8:48"})
	st := target.NewState()
	for _, path := range []string{"/dev/sdc1", "/dev/gone"} {
		err := checkUnshared(st, path, "", 0, "")
		if err == nil || isConflict(err) || !strings.Contains(err.Error(), "tell a disk from its partitions") {
			t.Errorf("%s: %v", path, err)
		}
	}
	if err := checkUnshared(st, "/dev/sdb1", "", 0, ""); err != nil {
		t.Errorf("a partition that sysfs describes: %v", err)
	}
	// Complete sysfs: a disk and its partition still conflict.
	st.Subsystems[subNQN] = &target.Subsystem{NQN: subNQN, Namespaces: map[uint32]*target.Namespace{1: {NSID: 1, DevicePath: "/dev/sdb1"}}}
	if err := checkUnshared(st, "/dev/sdb", "", 0, ""); !isConflict(err) {
		t.Errorf("a disk whose partition is exported: %v", err)
	}
	// A namespace already exported that sysfs can't describe doesn't stop other exports.
	st.Subsystems[subNQN].Namespaces[2] = &target.Namespace{NSID: 2, DevicePath: "/dev/gone"}
	add("sdd", "8:64", false)
	identifyBy(t, map[string]string{"/dev/sdd": "8:64"})
	if err := checkUnshared(st, "/dev/sdd", "", 0, ""); err != nil {
		t.Errorf("another disk, with an undescribable one already exported: %v", err)
	}
}

func TestALogicalVolumeOverlapsWhatItsBuiltOn(t *testing.T) {
	add := fakeBlock(t)
	add("sdb", "8:16", false)
	add("sdb/sdb1", "8:17", true)
	add("sdb/sdb2", "8:18", true)
	// A thin pool on sdb2, as LVM builds it, and two thin volumes in it.
	add("virtual/dm-0", "253:0", false, "sdb/sdb2")                     // tmeta
	add("virtual/dm-1", "253:1", false, "sdb/sdb2")                     // tdata
	add("virtual/dm-2", "253:2", false, "virtual/dm-0", "virtual/dm-1") // tpool
	add("virtual/dm-3", "253:3", false, "virtual/dm-2")                 // thin volume a
	add("virtual/dm-4", "253:4", false, "virtual/dm-2")                 // thin volume b
	identifyBy(t, map[string]string{
		"/dev/sdb": "8:16", "/dev/sdb1": "8:17", "/dev/sdb2": "8:18",
		"/dev/vg/a": "253:3", "/dev/vg/b": "253:4",
	})
	exported := func(path string) *target.State {
		st := target.NewState()
		st.Subsystems[subNQN] = &target.Subsystem{NQN: subNQN, Namespaces: map[uint32]*target.Namespace{1: {NSID: 1, DevicePath: path}}}
		return st
	}
	for _, c := range []struct {
		exported, adding string
		conflict         bool
	}{
		{"/dev/sdb2", "/dev/vg/a", true},  // the partition under the pool
		{"/dev/sdb", "/dev/vg/a", true},   // that partition's disk
		{"/dev/sdb1", "/dev/vg/a", false}, // another partition of the disk
		{"/dev/vg/b", "/dev/vg/a", false}, // another thin volume in the same pool
		{"/dev/sdb1", "/dev/sdb2", false}, // two partitions of one disk
		{"/dev/sdb1", "/dev/sdb", true},   // a disk and its partition, as before
	} {
		err := checkUnshared(exported(c.exported), c.adding, "", 0, "")
		if isConflict(err) != c.conflict || err != nil && !isConflict(err) {
			t.Errorf("%s exported, adding %s: %v", c.exported, c.adding, err)
		}
	}
}

func TestAStackTooDeepToWalkIsRefused(t *testing.T) {
	add := fakeBlock(t)
	add("virtual/dm-0", "253:0", false)
	for i := 1; i <= maxUnder+1; i++ {
		add(fmt.Sprintf("virtual/dm-%d", i), fmt.Sprintf("253:%d", i), false, fmt.Sprintf("virtual/dm-%d", i-1))
	}
	top := fmt.Sprintf("253:%d", maxUnder+1)
	identifyBy(t, map[string]string{"/dev/top": top})
	err := checkUnshared(target.NewState(), "/dev/top", "", 0, "")
	if err == nil || !strings.Contains(err.Error(), "too many") {
		t.Errorf("%d devices deep: %v", maxUnder+1, err)
	}
	// A loop in the links stops the walk, rather than failing the request.
	add("virtual/dm-90", "253:90", false, "virtual/dm-91")
	add("virtual/dm-91", "253:91", false)
	if err := os.Symlink(filepath.Join(sysRoot, "devices/virtual/dm-90"), filepath.Join(sysRoot, "devices/virtual/dm-91/slaves/dm-90")); err != nil {
		t.Fatal(err)
	}
	identifyBy(t, map[string]string{"/dev/loopy": "253:90"})
	if err := checkUnshared(target.NewState(), "/dev/loopy", "", 0, ""); err != nil {
		t.Errorf("a loop: %v", err)
	}
}

// The real walk, on whatever device-mapper devices this machine has.
func TestUnderneathFindsWhatARealDeviceIsBuiltOn(t *testing.T) {
	slaves, _ := filepath.Glob("/sys/class/block/dm-*/slaves/*")
	if len(slaves) == 0 {
		t.Skip("no device-mapper devices here")
	}
	dm := filepath.Dir(filepath.Dir(slaves[0]))
	b, err := os.ReadFile(filepath.Join(dm, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	under, err := underneath(strings.TrimSpace(string(b)))
	if err != nil || len(under) == 0 {
		t.Errorf("%s: %v, %v", dm, under, err)
	}
}
