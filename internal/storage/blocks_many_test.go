package storage

import (
	"crypto/rand"
	"os"
	"testing"
)

// A file in many extents, more than one FIEMAP call returns, with a region that's half written in the first snapshot and
// whole in the second, as a file being written when a snapshot is taken is: the second half is a hole in one and data in
// the other, which is a change. Found by vm-e2e.sh on SPDK, whose writes leave reflink volumes in thousands of extents.
func TestReflinkChangesAcrossManyExtents(t *testing.T) {
	d := &reflinkDriver{dir: t.TempDir()}
	if d.Check() != nil {
		t.Skip("this filesystem has no reflink")
	}
	const chunk = 128 << 10
	const chunks = 1600
	const size = chunks * chunk
	if _, err := d.Create("nvw-v", size); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(d.path("nvw-v"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Random, so that a compressing filesystem, such as btrfs with compress, stores it as it is.
	write := func(off int64, n int, _ byte) {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}
	}
	// Written whole, then every other chunk again after a snapshot, which copies it elsewhere: many extents, alternating.
	write(0, size/2, 1)
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Snapshot("nvw-v", "nvw-s0"); err != nil {
		t.Fatal(err)
	}
	for c := int64(0); c < chunks/2; c += 2 {
		write(c*chunk, chunk, 2)
	}
	// A file being written: its first quarter is there when the first snapshot is taken.
	region := int64(size * 3 / 4)
	write(region, 256<<10, 3)
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Snapshot("nvw-v", "nvw-s1"); err != nil {
		t.Fatal(err)
	}
	write(region+256<<10, 768<<10, 4)
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Snapshot("nvw-v", "nvw-s2"); err != nil {
		t.Fatal(err)
	}
	exts, err := fiemap(d.path("nvw-s2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) <= fiemapBatch {
		t.Fatalf("only %d extents, not more than one FIEMAP call's %d", len(exts), fiemapBatch)
	}
	ch, err := d.Changed("nvw-s1", "nvw-s2")
	if err != nil {
		t.Fatal(err)
	}
	if !coversRange(ch.Ranges, Range{region + 256<<10, 768 << 10}) {
		t.Errorf("the rest of the file, a hole in the first snapshot and data in the second, isn't reported: %d extents, changes %v",
			len(exts), ch.Ranges)
	}
	if total(ch.Ranges) > 4<<20 {
		t.Errorf("reported %d bytes for 768 KiB written", total(ch.Ranges))
	}
}
