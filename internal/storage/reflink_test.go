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
