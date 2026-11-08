package storage

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"nvweaver/internal/target"
)

// supportsReflink says whether the test directory's filesystem can clone. Machines running
// these tests vary, and the end-to-end suites cover reflink on a filesystem known to have it.
func supportsReflink(t *testing.T, dir string) bool {
	t.Helper()
	err := (&reflinkDriver{dir: dir}).Check()
	return err == nil
}

func driver(t *testing.T) (*reflinkDriver, string) {
	t.Helper()
	dir := t.TempDir()
	return &reflinkDriver{dir: dir}, dir
}

func TestNewRefusesUnknownTypes(t *testing.T) {
	var ve *target.ValidationError
	if _, err := New(&target.Pool{Type: "lvm-thin"}); !errors.As(err, &ve) {
		t.Errorf("want a validation error, got %v", err)
	}
	if _, err := New(&target.Pool{Type: TypeReflink, Path: "/x"}); err != nil {
		t.Errorf("reflink refused: %v", err)
	}
}

func TestVolumeLifecycle(t *testing.T) {
	d, dir := driver(t)
	path, err := d.Create(target.InternalPrefix+"aaaa", 8192)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, target.InternalPrefix+"aaaa.img") {
		t.Errorf("path = %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 8192 {
		t.Errorf("size = %d", info.Size())
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	if _, err := d.Create(target.InternalPrefix+"aaaa", 8192); err == nil {
		t.Error("creating over an existing volume must fail")
	}
	if err := d.Grow(target.InternalPrefix+"aaaa", 16384); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Size() != 16384 {
		t.Errorf("size after growing = %d", info.Size())
	}
	if err := d.Delete(target.InternalPrefix + "aaaa"); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(target.InternalPrefix + "aaaa"); err != nil {
		t.Errorf("deleting what's gone must not fail: %v", err)
	}
}

func TestListOnlyFindsOurOwnVolumes(t *testing.T) {
	d, dir := driver(t)
	for _, name := range []string{target.InternalPrefix + "one.img", "someone-elses.img", target.InternalPrefix + "not-a-volume.txt", ".nvweaver-check-1"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	names, err := d.List()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{target.InternalPrefix + "one"}) {
		t.Errorf("listed %v", names)
	}
}

func TestCheckRefusesWhatCantWork(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"relative":      "some/dir",
		"missing":       filepath.Join(t.TempDir(), "absent"),
		"not directory": file,
	} {
		var ve *target.ValidationError
		if err := (&reflinkDriver{dir: dir}).Check(); !errors.As(err, &ve) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}
}

func TestCheckLeavesNothingBehind(t *testing.T) {
	d, dir := driver(t)
	err := d.Check()
	if err != nil && !strings.Contains(err.Error(), "reflink") {
		t.Fatalf("unexpected failure: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestCapacityReportsTheFilesystem(t *testing.T) {
	d, _ := driver(t)
	c, err := d.Capacity()
	if err != nil {
		t.Fatal(err)
	}
	if c.TotalBytes <= 0 || c.UsedBytes < 0 || c.UsedBytes > c.TotalBytes {
		t.Errorf("capacity = %+v", c)
	}
	if _, err := (&reflinkDriver{dir: "/no/such/dir"}).Capacity(); err == nil {
		t.Error("a missing directory must be an error")
	}
}

func TestCloneCopiesData(t *testing.T) {
	d, dir := driver(t)
	if !supportsReflink(t, dir) {
		t.Skip("this filesystem has no reflink")
	}
	src, err := os.Create(filepath.Join(dir, "src"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.WriteString(strings.Repeat("x", 4096)); err != nil {
		t.Fatal(err)
	}
	dst, err := os.Create(filepath.Join(dir, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := clone(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := src.WriteAt([]byte("y"), 0); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 1)
	if _, err := dst.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if got[0] != 'x' {
		t.Errorf("clone changed when its source did: %q", got)
	}
	_ = d
}

func TestSnapshotsAndClonesAreIndependent(t *testing.T) {
	d, dir := driver(t)
	if !supportsReflink(t, dir) {
		// Skipped on tmpfs and ext4, so test/e2e.sh checks the snapshot's mode too.
		t.Skip("this filesystem has no reflink; test/e2e.sh checks that snapshots are read-only")
	}
	name := func(s string) string { return target.InternalPrefix + s }
	vol, err := d.Create(name("vol"), 8192)
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte("before"), make([]byte, 8186)...)
	if err := os.WriteFile(vol, before, 0o600); err != nil {
		t.Fatal(err)
	}

	snap, err := d.Snapshot(name("vol"), name("snap"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(snap); err != nil || info.Mode().Perm() != 0o400 || info.Size() != 8192 {
		t.Errorf("snapshot: %v, %v", info, err)
	}
	clone, err := d.Clone(name("snap"), name("clone"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(clone); err != nil || info.Mode().Perm() != 0o600 || info.Size() != 8192 {
		t.Errorf("clone: %v, %v", info, err)
	}

	// Changing any one of them leaves the others alone.
	if err := os.WriteFile(vol, append([]byte("volume"), make([]byte, 8186)...), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(clone, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("clone!"), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for path, want := range map[string]string{vol: "volume", snap: "before", clone: "clone!"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got[:6]) != want {
			t.Errorf("%s holds %q, want %q", filepath.Base(path), got[:6], want)
		}
	}

	// Each survives the others being deleted.
	if err := d.Delete(name("vol")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(snap); err != nil || string(got[:6]) != "before" {
		t.Errorf("snapshot after deleting its volume: %q, %v", got[:6], err)
	}
	if err := d.Delete(name("snap")); err != nil {
		t.Errorf("a read-only snapshot must still be deletable: %v", err)
	}
	if got, err := os.ReadFile(clone); err != nil || string(got[:6]) != "clone!" {
		t.Errorf("clone after deleting its snapshot: %q, %v", got[:6], err)
	}
}

func TestCloneSizes(t *testing.T) {
	d, dir := driver(t)
	if !supportsReflink(t, dir) {
		t.Skip("this filesystem has no reflink; the end-to-end suites cover it")
	}
	name := func(s string) string { return target.InternalPrefix + s }
	if _, err := d.Create(name("vol"), 8192); err != nil {
		t.Fatal(err)
	}
	big, err := d.Clone(name("vol"), name("big"), 16384)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(big); err != nil || info.Size() != 16384 {
		t.Errorf("larger clone: %v, %v", info, err)
	}
	var ve *target.ValidationError
	if _, err := d.Clone(name("vol"), name("small"), 4096); !errors.As(err, &ve) {
		t.Errorf("a smaller clone must be refused: %v", err)
	}
	if _, err := d.Clone(name("vol"), name("big"), 0); err == nil {
		t.Error("cloning over an existing name must fail")
	}
	if _, err := d.Clone(name("missing"), name("x"), 0); err == nil {
		t.Error("cloning something missing must fail")
	}
	names, err := d.List()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, name("big")) || slices.Contains(names, name("x")) || slices.Contains(names, name("small")) {
		t.Errorf("a failed clone must leave nothing behind: %v", names)
	}
}

// A sparse file allocates nothing until it's written, which is what thin storage is.
func TestReflinkUsedCountsWhatsAllocated(t *testing.T) {
	d := &reflinkDriver{dir: t.TempDir()}
	path, err := d.Create("nvw-a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := d.Used("nvw-a")
	if err != nil {
		t.Fatal(err)
	}
	if empty >= 1<<20 {
		t.Errorf("a new sparse file has %d allocated", empty)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 8<<20), 0); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		t.Fatal(err)
	}
	written, err := d.Used("nvw-a")
	if err != nil {
		t.Fatal(err)
	}
	if written < 8<<20 || written >= 1<<30 {
		t.Errorf("after writing 8 MiB of 1 GiB, %d is allocated", written)
	}
	if _, err := d.Used("nvw-missing"); err == nil {
		t.Error("a missing file has no usage")
	}
}
