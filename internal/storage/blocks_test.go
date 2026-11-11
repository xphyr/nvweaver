package storage

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestMergeRanges(t *testing.T) {
	got := mergeRanges([]Range{{100, 10}, {0, 10}, {10, 5}, {50, 0}, {105, 20}, {200, 1}})
	want := []Range{{0, 15}, {100, 25}, {200, 1}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := alignRanges([]Range{{4097, 1}, {8191, 2}}, 4096); !slices.Equal(got, []Range{{4096, 8192}}) {
		t.Errorf("aligned: %v", got)
	}
}

// As device-mapper-persistent-data 1.3.4 prints them.
const thinDeltaXML = `<superblock uuid="" time="3" transaction="9" flags="0" version="2" data_block_size="128" nr_data_blocks="0">
  <diff left="1" right="2">
    <same begin="0" length="4"/>
    <different begin="4" length="2"/>
    <left_only begin="10" length="1"/>
    <right_only begin="11" length="3"/>
    <same begin="14" length="100"/>
  </diff>
</superblock>
`

const thinDumpXML = `<superblock uuid="" time="3" transaction="9" flags="0" version="2" data_block_size="256" nr_data_blocks="4096">
  <device dev_id="7" mapped_blocks="5" transaction="0" creation_time="0" snap_time="2">
    <range_mapping origin_begin="0" data_begin="40" length="3" time="0"/>
    <single_mapping origin_block="9" data_block="99" time="1"/>
    <single_mapping origin_block="3" data_block="12" time="1"/>
  </device>
</superblock>
`

func TestThinToolsOutputIsRead(t *testing.T) {
	b, err := thinRanges(thinDeltaXML, thinDelta)
	if err != nil {
		t.Fatal(err)
	}
	const k = 64 * 1024 // 128 sectors
	if b.BlockSize != k || !slices.Equal(b.Ranges, []Range{{4 * k, 2 * k}, {10 * k, 4 * k}}) {
		t.Errorf("delta: %d, %v", b.BlockSize, b.Ranges)
	}
	b, err = thinRanges(thinDumpXML, thinDump("7"))
	if err != nil {
		t.Fatal(err)
	}
	const m = 128 * 1024
	if b.BlockSize != m || !slices.Equal(b.Ranges, []Range{{0, 4 * m}, {9 * m, m}}) {
		t.Errorf("dump: %d, %v", b.BlockSize, b.Ranges)
	}
	// Nothing changed, and nothing mapped: as 1.3.4 prints them, empty answers that are right.
	for out, f := range map[string]thinFormat{
		"<superblock data_block_size=\"128\">\n  <diff left=\"3\" right=\"4\">\n  </diff>\n</superblock>": thinDelta,
		"<superblock data_block_size=\"128\">\n  <device dev_id=\"3\">\n  </device>\n</superblock>":       thinDump("3"),
	} {
		if b, err := thinRanges(out, f); err != nil || len(b.Ranges) != 0 {
			t.Errorf("empty: %v, %v", b.Ranges, err)
		}
	}
}

// What the thin tools say that isn't understood is an error, never skipped: skipping could leave out a change. Each of
// these has a range the parser does know, so an answer with no error would have looked like a real one.
func TestThinToolsOutputNotUnderstoodIsAnError(t *testing.T) {
	const sb = `<superblock data_block_size="128">`
	for name, c := range map[string]struct {
		out string
		f   thinFormat
	}{
		"an unknown kind of difference": {sb + `<diff left="1" right="2"><different begin="0" length="1"/><moved begin="1" length="1"/></diff></superblock>`, thinDelta},
		"an unknown kind of mapping":    {sb + `<device dev_id="7"><range_mapping origin_begin="0" length="1"/><shared_mapping origin_block="3"/></device></superblock>`, thinDump("7")},
		"shared mappings, by reference": {sb + `<def name="a"><single_mapping origin_block="1"/></def><device dev_id="7"><ref name="a"/><single_mapping origin_block="0"/></device></superblock>`, thinDump("7")},
		"something inside a range":      {sb + `<diff><different begin="0" length="1"><different begin="5" length="1"/></different></diff></superblock>`, thinDelta},
		"a dump in place of a delta":    {sb + `<device dev_id="7"><range_mapping origin_begin="0" length="1"/></device></superblock>`, thinDelta},
		"two diffs":                     {sb + `<diff><different begin="0" length="1"/></diff><diff/></superblock>`, thinDelta},
		"another device":                {sb + `<device dev_id="8"><range_mapping origin_begin="0" length="1"/></device></superblock>`, thinDump("7")},
		// thin_dump --dev-id for a device that isn't there prints this, and succeeds.
		"no device":        {sb + "\n</superblock>", thinDump("9")},
		"no diff":          {sb + `</superblock>`, thinDelta},
		"a missing count":  {sb + `<diff><different begin="4"/></diff></superblock>`, thinDelta},
		"a bad number":     {sb + `<diff><different begin="x" length="1"/></diff></superblock>`, thinDelta},
		"no block size":    {`<superblock version="2"><diff/></superblock>`, thinDelta},
		"not a superblock": {`<metadata data_block_size="128"><diff/></metadata>`, thinDelta},
		"a negative count": {sb + `<diff><different begin="4" length="-1"/></diff></superblock>`, thinDelta},
	} {
		b, err := thinRanges(c.out, c.f)
		if err == nil || len(b.Ranges) != 0 {
			t.Errorf("%s: %v, %v", name, b.Ranges, err)
		}
	}
	_, err := thinRanges(`<superblock data_block_size="128"><diff><moved begin="1" length="1"/></diff></superblock>`, thinDelta)
	if err == nil || !strings.Contains(err.Error(), "<moved>") {
		t.Errorf("the error doesn't name the element: %v", err)
	}
}

func TestLVMChangesComeFromAMetadataSnapshot(t *testing.T) {
	d, f := lvmDriver("my-vg")
	f.replies["lvs --reportformat json --units b --nosuffix -o thin_id my-vg/nvw-a"] = lvsJSON(`"thin_id": "7"`)
	f.replies["lvs --reportformat json --units b --nosuffix -o thin_id my-vg/nvw-b"] = lvsJSON(`"thin_id": "8"`)
	f.replies["thin_delta"] = thinDeltaXML
	b, err := d.Changed("nvw-a", "nvw-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Ranges) != 2 {
		t.Errorf("ranges %v", b.Ranges)
	}
	// The IDs are read once the metadata is frozen, as an ID read before could be another device's by then.
	want := []string{
		"dmsetup message my--vg-pool0-tpool 0 reserve_metadata_snap",
		"lvs --reportformat json --units b --nosuffix -o thin_id my-vg/nvw-a",
		"lvs --reportformat json --units b --nosuffix -o thin_id my-vg/nvw-b",
		"thin_delta -m --snap1 7 --snap2 8 /dev/mapper/my--vg-pool0_tmeta",
		"dmsetup message my--vg-pool0-tpool 0 release_metadata_snap",
	}
	if got := f.calls; !slices.Equal(got, want) {
		t.Errorf("ran:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Released even when the tool fails.
	f.calls = nil
	f.fail["thin_delta"] = errors.New("thin_delta: bad")
	if _, err := d.Changed("nvw-a", "nvw-b"); err == nil {
		t.Error("a failing thin_delta wasn't an error")
	}
	if last := f.calls[len(f.calls)-1]; !strings.HasSuffix(last, "release_metadata_snap") {
		t.Errorf("not released after a failure: %v", f.calls)
	}
	delete(f.fail, "thin_delta")

	// One left behind by a run that stopped is released, then taken again.
	f.calls = nil
	busy := true
	tool := d.tool
	d.tool = func(prog string, args ...string) (string, error) {
		if busy && slices.Contains(args, "reserve_metadata_snap") {
			busy = false
			f.calls = append(f.calls, "dmsetup reserve, busy")
			return "", errors.New("dmsetup: device-mapper: message ioctl failed: Device or resource busy")
		}
		return tool(prog, args...)
	}
	if _, err := d.Changed("nvw-a", "nvw-b"); err != nil {
		t.Errorf("a stale metadata snapshot: %v", err)
	}
	if !slices.Contains(f.calls, "dmsetup message my--vg-pool0-tpool 0 release_metadata_snap") {
		t.Errorf("ran %v", f.calls)
	}
}

func TestLVMAllocatedComesFromThinDump(t *testing.T) {
	d, f := lvmDriver("vg")
	f.replies["lvs"] = lvsJSON(`"thin_id": "7"`)
	f.replies["thin_dump"] = thinDumpXML
	b, err := d.Allocated("nvw-a")
	if err != nil || len(b.Ranges) != 2 {
		t.Fatalf("%v, %v", b, err)
	}
	if !slices.Contains(f.calls, "thin_dump -m --dev-id 7 /dev/mapper/vg-pool0_tmeta") {
		t.Errorf("ran %v", f.calls)
	}
	f.replies["lvs"] = lvsJSON(`"thin_id": ""`)
	if _, err := d.Allocated("nvw-a"); err == nil {
		t.Error("an LV with no thin ID was accepted")
	}
}

// Device-mapper reuses a deleted thin device's ID. A snapshot deleted between reading its ID and reserving the metadata
// snapshot, with another taking its ID, must be an error, not the other's mappings: an empty or wrong answer makes every
// backup built on it wrong. LVs' names are never reused, so a name that's still there after reserving was there when the
// metadata was frozen.
func TestAThinIDReusedMeanwhileIsntRead(t *testing.T) {
	const otherDump = `<superblock data_block_size="128"><device dev_id="7"></device></superblock>`
	for _, call := range []string{"allocated", "changed, the base gone", "changed, the snapshot gone"} {
		d, f := lvmDriver("vg")
		f.replies["lvs --reportformat json --units b --nosuffix -o thin_id vg/nvw-a"] = lvsJSON(`"thin_id": "7"`)
		f.replies["lvs --reportformat json --units b --nosuffix -o thin_id vg/nvw-b"] = lvsJSON(`"thin_id": "8"`)
		f.replies["thin_dump"] = otherDump
		f.replies["thin_delta"] = `<superblock data_block_size="128"><diff left="7" right="8"></diff></superblock>`
		gone := map[string]string{"allocated": "vg/nvw-a", "changed, the base gone": "vg/nvw-a", "changed, the snapshot gone": "vg/nvw-b"}[call]
		reserved := false
		tool, run := d.tool, d.run
		d.tool = func(prog string, args ...string) (string, error) {
			if slices.Contains(args, "reserve_metadata_snap") {
				reserved = true
			}
			return tool(prog, args...)
		}
		d.run = func(args ...string) (string, error) {
			if reserved && slices.Contains(args, gone) {
				f.calls = append(f.calls, strings.Join(args, " ")+", gone")
				return "", errors.New(`Failed to find logical volume "` + gone + `"`)
			}
			return run(args...)
		}
		var err error
		var b Blocks
		if call == "allocated" {
			b, err = d.Allocated("nvw-a")
		} else {
			b, err = d.Changed("nvw-a", "nvw-b")
		}
		if err == nil {
			t.Errorf("%s: the reused ID was read, giving %v", call, b.Ranges)
		}
		if last := f.calls[len(f.calls)-1]; !strings.HasSuffix(last, "release_metadata_snap") {
			t.Errorf("%s: not released: %v", call, f.calls)
		}
	}
}

func TestExtentDelta(t *testing.T) {
	a := []extent{{0, 1000, 100, false}, {200, 5000, 100, false}, {400, 9000, 100, false}}
	b := []extent{
		{0, 1000, 50, false},  // Shared,
		{50, 7000, 50, false}, // then rewritten.
		{200, 5000, 100, false},
		// 400 to 500 is a hole now: TRIM.
		{600, 8000, 10, false}, // New.
		{700, 3000, 10, true},  // Where isn't known.
	}
	a = append(a, extent{700, 3000, 10, false})
	got := extentDelta(a, b)
	want := []Range{{50, 50}, {400, 100}, {600, 10}, {700, 10}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := extentDelta(a, a); len(got) != 0 {
		t.Errorf("the same file: %v", got)
	}
}

// On a filesystem with reflink, the real thing: run with TMPDIR on XFS or btrfs. test/vm/cbt-check.sh checks it byte by
// byte, and the end-to-end suites through the API.
func TestReflinkChangesAndAllocation(t *testing.T) {
	d := &reflinkDriver{dir: t.TempDir()}
	if d.Check() != nil {
		t.Skip("this filesystem has no reflink")
	}
	const size = 8 << 20
	if _, err := d.Create("nvw-v", size); err != nil {
		t.Fatal(err)
	}
	write := func(off int64, n int) {
		f, err := os.OpenFile(d.path("nvw-v"), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteAt(bytes.Repeat([]byte{0xa5}, n), off); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	write(0, 1<<20)
	if _, err := d.Snapshot("nvw-v", "nvw-s1"); err != nil {
		t.Fatal(err)
	}
	write(3<<20+100, 10)
	if _, err := d.Snapshot("nvw-v", "nvw-s2"); err != nil {
		t.Fatal(err)
	}
	alloc, err := d.Allocated("nvw-s1")
	if err != nil {
		t.Fatal(err)
	}
	if !coversRange(alloc.Ranges, Range{0, 1 << 20}) || total(alloc.Ranges) > 2<<20 {
		t.Errorf("allocated in s1: %v", alloc.Ranges)
	}
	ch, err := d.Changed("nvw-s1", "nvw-s2")
	if err != nil {
		t.Fatal(err)
	}
	// At most the 4 KiB block written, or on compressed btrfs, also every compressed extent, which can't be compared.
	if !coversRange(ch.Ranges, Range{3<<20 + 100, 10}) || total(ch.Ranges) > 2<<20 || ch.BlockSize <= 0 {
		t.Errorf("changed: block %d, %v", ch.BlockSize, ch.Ranges)
	}
	if same, err := d.Changed("nvw-s2", "nvw-s2"); err != nil || len(same.Ranges) != 0 {
		t.Errorf("a snapshot with itself: %v, %v", same.Ranges, err)
	}
	if _, err := d.Changed("nvw-s1", "nvw-none"); err == nil {
		t.Error("a missing snapshot wasn't an error")
	}
}

func coversRange(rs []Range, want Range) bool {
	for _, r := range rs {
		if r.Offset <= want.Offset && want.End() <= r.End() {
			return true
		}
	}
	return false
}

func total(rs []Range) (n int64) {
	for _, r := range rs {
		n += r.Length
	}
	return n
}
