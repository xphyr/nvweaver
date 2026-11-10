package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Changed block tracking, decision 50. What's reported comes from the storage's own mappings, so it's whole blocks of the
// pool's unit, and may include what didn't change, but test/vm/cbt-check.sh found it never leaves out what did.

// hostTool runs a program where LVM runs, as hostLVM runs lvm: the device-mapper and thin tools act on the host's devices.
func hostTool(prog string, args ...string) (string, error) {
	argv := hostCommand(append([]string{prog}, args...)...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	text := strings.TrimSpace(stderr.String())
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(text, "No such file or directory") && strings.Contains(text, prog) && len(text) < 120 {
		return "", fmt.Errorf("can't find %s on the host: install device-mapper-persistent-data (thin-provisioning-tools on Debian and Ubuntu) there", prog)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %s: %w", prog, text, err)
	}
	return stdout.String(), nil
}

// dmName escapes a name as device-mapper does for LVM: a hyphen is doubled, as one hyphen joins the parts.
func dmName(parts ...string) string {
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(p, "-", "--")
	}
	return strings.Join(parts, "-")
}

// A thin pool holds one metadata snapshot at a time, so one request at a time reads one, per pool.
var metadataSnapLocks sync.Map

// withMetadataSnap reserves a snapshot of the live pool's metadata, holding what was committed then, runs fn with the
// metadata device to read it from, and releases it. Thin snapshots are committed when they're taken, so they're all in it.
func (d *lvmThinDriver) withMetadataSnap(fn func(meta string) error) error {
	l, _ := metadataSnapLocks.LoadOrStore(d.vg+"/"+d.pool, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	tpool := dmName(d.vg, d.pool) + "-tpool"
	meta := "/dev/mapper/" + dmName(d.vg, d.pool+"_tmeta")
	reserve := func() error { _, err := d.tool("dmsetup", "message", tpool, "0", "reserve_metadata_snap"); return err }
	err := reserve()
	if err != nil && strings.Contains(err.Error(), "busy") {
		// Only this lock takes one, so it was left by a run that stopped before releasing it.
		slog.Warn("lvm: releasing a metadata snapshot left on the thin pool", "pool", d.lv(d.pool))
		if _, rerr := d.tool("dmsetup", "message", tpool, "0", "release_metadata_snap"); rerr == nil {
			err = reserve()
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "No such device") || strings.Contains(err.Error(), "not exist") {
			return fmt.Errorf("thin pool %s isn't active, so its mappings can't be read: %w", d.lv(d.pool), err)
		}
		return fmt.Errorf("thin pool %s: reserving a metadata snapshot: %w", d.lv(d.pool), err)
	}
	defer func() {
		if _, err := d.tool("dmsetup", "message", tpool, "0", "release_metadata_snap"); err != nil {
			slog.Error("lvm: releasing the metadata snapshot", "pool", d.lv(d.pool), "error", err)
		}
	}()
	return fn(meta)
}

func (d *lvmThinDriver) thinID(internal string) (string, error) {
	got, err := d.report(d.lv(internal), "thin_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(got["thin_id"])
	if _, err := strconv.ParseUint(id, 10, 32); err != nil {
		return "", fmt.Errorf("%s has no thin device ID (%q)", d.lv(internal), id)
	}
	return id, nil
}

// Allocated implements Driver, from the snapshot's mappings in thin_dump.
func (d *lvmThinDriver) Allocated(internal string) (Blocks, error) {
	id, err := d.thinID(internal)
	if err != nil {
		return Blocks{}, err
	}
	var out string
	if err := d.withMetadataSnap(func(meta string) error {
		out, err = d.tool("thin_dump", "-m", "--dev-id", id, meta)
		return err
	}); err != nil {
		return Blocks{}, err
	}
	return thinRanges(out, map[string][2]string{"range_mapping": {"origin_begin", "length"}, "single_mapping": {"origin_block", ""}})
}

// Changed implements Driver, from thin_delta: blocks mapped differently, or on one side only.
func (d *lvmThinDriver) Changed(base, internal string) (Blocks, error) {
	a, err := d.thinID(base)
	if err != nil {
		return Blocks{}, err
	}
	b, err := d.thinID(internal)
	if err != nil {
		return Blocks{}, err
	}
	var out string
	if err := d.withMetadataSnap(func(meta string) error {
		out, err = d.tool("thin_delta", "-m", "--snap1", a, "--snap2", b, meta)
		return err
	}); err != nil {
		return Blocks{}, err
	}
	return thinRanges(out, map[string][2]string{"different": {"begin", "length"}, "left_only": {"begin", "length"}, "right_only": {"begin", "length"}})
}

// thinRanges reads thin_dump's or thin_delta's XML: the superblock gives the block size in sectors, and the elements
// named in want give a first block and a count, or with no count attribute, one block. Anything else, such as thin_delta's
// "same", is skipped.
func thinRanges(out string, want map[string][2]string) (Blocks, error) {
	var b Blocks
	var ranges []Range
	dec := xml.NewDecoder(strings.NewReader(out))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Blocks{}, fmt.Errorf("parse the thin tools' output: %w", err)
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		attr := func(name string) (int64, error) {
			for _, a := range el.Attr {
				if a.Name.Local == name {
					return strconv.ParseInt(a.Value, 10, 64)
				}
			}
			return 0, fmt.Errorf("<%s> has no %s", el.Name.Local, name)
		}
		if el.Name.Local == "superblock" {
			sectors, err := attr("data_block_size")
			if err != nil || sectors <= 0 {
				return Blocks{}, fmt.Errorf("parse the thin tools' block size: %v", err)
			}
			b.BlockSize = sectors * 512
			continue
		}
		names, ok := want[el.Name.Local]
		if !ok {
			continue
		}
		if b.BlockSize == 0 {
			return Blocks{}, errors.New("the thin tools gave ranges before their block size")
		}
		first, err := attr(names[0])
		if err != nil {
			return Blocks{}, err
		}
		count := int64(1)
		if names[1] != "" {
			if count, err = attr(names[1]); err != nil {
				return Blocks{}, err
			}
		}
		ranges = append(ranges, Range{Offset: first * b.BlockSize, Length: count * b.BlockSize})
	}
	if b.BlockSize == 0 {
		return Blocks{}, errors.New("the thin tools gave no superblock")
	}
	b.Ranges = mergeRanges(ranges)
	return b, nil
}

// FIEMAP, from linux/fs.h and linux/fiemap.h: _IOWR('f', 11, struct fiemap), the same on x86-64 and arm64.
const (
	fsIocFiemap        = 0xC020660B
	fiemapFlagSync     = 0x1
	fiemapExtentLast   = 0x1
	fiemapExtentUnwrit = 0x800
	// Where the data is, or whether it's shared, isn't known: delayed allocation, encoded or inline data. Encoded includes
	// btrfs's compressed extents, whose physical address is the compressed extent's, not each byte's, so on a compressed
	// btrfs pool every compressed extent is reported as changed: correct, but backups read more.
	fiemapExtentUnsure = 0x2 | 0x4 | 0x8 | 0x200
	fiemapHeader       = 32
	fiemapExtentSize   = 56
	fiemapBatch        = 512
)

// extent is part of a file that holds data: where in the file, where on the device, and how long.
type extent struct {
	logical, physical, length int64
	unsure                    bool // Where it is isn't known, so it can't be said to be shared.
}

// fiemap returns a file's extents that hold data. Unwritten extents read as zeros, so they're left out, as holes are.
func fiemap(path string) ([]extent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []extent
	start := uint64(0)
	buf := make([]byte, fiemapHeader+fiemapExtentSize*fiemapBatch)
	for {
		clear(buf)
		ne := binary.NativeEndian
		ne.PutUint64(buf[0:], start)
		ne.PutUint64(buf[8:], math.MaxUint64-start)
		ne.PutUint32(buf[16:], fiemapFlagSync)
		ne.PutUint32(buf[24:], fiemapBatch)
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
			return nil, fmt.Errorf("FIEMAP on %s: %w", path, errno)
		}
		mapped := int(ne.Uint32(buf[20:]))
		if mapped == 0 {
			return out, nil
		}
		for i := range mapped {
			e := buf[fiemapHeader+fiemapExtentSize*i:]
			logical, physical, length := ne.Uint64(e[0:]), ne.Uint64(e[8:]), ne.Uint64(e[16:])
			flags := ne.Uint32(e[40:])
			if flags&fiemapExtentUnwrit == 0 {
				out = append(out, extent{int64(logical), int64(physical), int64(length), flags&fiemapExtentUnsure != 0})
			}
			start = logical + length
			if flags&fiemapExtentLast != 0 {
				return out, nil
			}
		}
	}
}

func blockSize(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bsize, nil
}

// alignRanges widens ranges to whole blocks.
func alignRanges(rs []Range, block int64) []Range {
	out := make([]Range, len(rs))
	for i, r := range rs {
		start := r.Offset / block * block
		end := (r.End() + block - 1) / block * block
		out[i] = Range{start, end - start}
	}
	return mergeRanges(out)
}

// Allocated implements Driver: the file's extents that hold data.
func (d *reflinkDriver) Allocated(internal string) (Blocks, error) {
	path := d.path(internal)
	block, err := blockSize(path)
	if err != nil {
		return Blocks{}, err
	}
	exts, err := fiemap(path)
	if err != nil {
		return Blocks{}, err
	}
	rs := make([]Range, len(exts))
	for i, e := range exts {
		rs[i] = Range{e.logical, e.length}
	}
	return Blocks{BlockSize: block, Ranges: alignRanges(rs, block)}, nil
}

// Changed implements Driver: where the two files' data is in different places on the device, or is on one side only.
// A reflinked snapshot shares its source's extents, and a write gives the written file new ones.
func (d *reflinkDriver) Changed(base, internal string) (Blocks, error) {
	pa, pb := d.path(base), d.path(internal)
	block, err := blockSize(pb)
	if err != nil {
		return Blocks{}, err
	}
	if base == internal {
		return Blocks{BlockSize: block}, nil
	}
	a, err := fiemap(pa)
	if err != nil {
		return Blocks{}, err
	}
	b, err := fiemap(pb)
	if err != nil {
		return Blocks{}, err
	}
	return Blocks{BlockSize: block, Ranges: alignRanges(extentDelta(a, b), block)}, nil
}

// extentDelta walks two files' extents together, in order, and returns where they differ.
func extentDelta(a, b []extent) []Range {
	var out []Range
	var ia, ib int
	pos := int64(0)
	end := int64(0)
	for _, e := range a {
		end = max(end, e.logical+e.length)
	}
	for _, e := range b {
		end = max(end, e.logical+e.length)
	}
	// at says what's at pos in one file's extents, from index i on: whether it's mapped, where, whether that's sure, and
	// where that stops being true.
	at := func(exts []extent, i *int) (mapped bool, physical int64, unsure bool, next int64) {
		for *i < len(exts) && exts[*i].logical+exts[*i].length <= pos {
			*i++
		}
		if *i == len(exts) {
			return false, 0, false, math.MaxInt64
		}
		e := exts[*i]
		if e.logical > pos {
			return false, 0, false, e.logical
		}
		return true, e.physical + (pos - e.logical), e.unsure, e.logical + e.length
	}
	for pos < end {
		ma, phA, ua, na := at(a, &ia)
		mb, phB, ub, nb := at(b, &ib)
		next := min(na, nb, end)
		if ma != mb || ma && (phA != phB || ua || ub) {
			out = append(out, Range{pos, next - pos})
		}
		pos = next
	}
	return mergeRanges(out)
}
