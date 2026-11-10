// Package storage allocates volumes from pools. It knows nothing of NVMe: a volume is a
// path, which the target backends export like any other.
package storage

import (
	"cmp"
	"slices"

	"nvweaver/internal/target"
)

// TypeReflink is a pool of files on a filesystem with reflink.
const TypeReflink = "reflink"

// Types lists the pool types this build can use.
func Types() []string { return []string{TypeLVMThin, TypeReflink} }

// Capacity is what a pool's underlying storage reports.
type Capacity struct {
	TotalBytes int64
	UsedBytes  int64
	// MetadataUsedPercent is how full the pool's own metadata is, where it has any.
	MetadataUsedPercent float64
	// Warnings are problems particular to this pool type.
	Warnings []string
}

// Driver manages the volumes of one pool. Volumes are identified by internal name, which is
// safe to use on disk, and all names it handles start with target.InternalPrefix.
type Driver interface {
	// AlignSize returns the size a volume of this size would really have.
	AlignSize(sizeBytes int64) (int64, error)
	// Activate makes the pool's volumes usable at startup, ahead of the backends opening them.
	Activate() error
	// Check reports why the pool can't be used, if it can't. It changes nothing lasting.
	Check() error
	// Capacity reports the pool's space.
	Capacity() (Capacity, error)
	// Create allocates a volume, returning the path to export. It fails if the name exists.
	Create(internal string, sizeBytes int64) (string, error)
	// Delete removes a volume. A volume that's already gone isn't an error.
	Delete(internal string) error
	// Grow makes a volume larger.
	Grow(internal string, sizeBytes int64) error
	// Snapshot makes a read-only copy of a volume, returning its path. It's kept apart from Clone
	// because pool types can differ: an LVM thin snapshot stays inactive until it's needed.
	Snapshot(source, internal string) (string, error)
	// Clone makes a new volume from a volume or snapshot, returning the path to export. It may
	// be larger than its source, and 0 means the same size. It fails if the name exists.
	Clone(source, internal string, sizeBytes int64) (string, error)
	// Used reports how many bytes of a volume are really allocated, including what it shares with snapshots and clones.
	Used(internal string) (int64, error)
	// List returns the internal names of volumes and snapshots on disk, to find any that nothing refers to.
	List() ([]string, error)
	// Allocated reports where a snapshot holds data: everything outside the ranges reads as zeros. It may report more,
	// never less, as a full backup built on it would miss data. See decision 50.
	Allocated(internal string) (Blocks, error)
	// Changed reports where two snapshots of one volume differ: everything outside the ranges is the same in both. It
	// may report more, never less, as an incremental backup built on it would be silently wrong.
	Changed(base, internal string) (Blocks, error)
}

// Blocks are byte ranges of a volume, in whole blocks of the pool's unit of tracking.
type Blocks struct {
	BlockSize int64
	Ranges    []Range // In order, apart and not touching.
}

// Range is a part of a volume, in bytes.
type Range struct{ Offset, Length int64 }

// End is where the range stops, exclusive.
func (r Range) End() int64 { return r.Offset + r.Length }

// mergeRanges sorts ranges and joins those that overlap or touch, dropping empty ones.
func mergeRanges(in []Range) []Range {
	rs := make([]Range, 0, len(in))
	for _, r := range in {
		if r.Length > 0 {
			rs = append(rs, r)
		}
	}
	slices.SortFunc(rs, func(a, b Range) int { return cmp.Compare(a.Offset, b.Offset) })
	out := rs[:0]
	for _, r := range rs {
		if n := len(out); n > 0 && r.Offset <= out[n-1].End() {
			if r.End() > out[n-1].End() {
				out[n-1].Length = r.End() - out[n-1].Offset
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// New returns the driver for a pool's type.
func New(p *target.Pool) (Driver, error) {
	switch p.Type {
	case TypeReflink:
		if p.Path == "" || p.VolumeGroup != "" || p.ThinPool != "" {
			return nil, target.Invalid("a %s pool takes path, not volume_group or thin_pool", TypeReflink)
		}
		return &reflinkDriver{dir: p.Path}, nil
	case TypeLVMThin:
		return newLVMThin(p)
	default:
		return nil, target.Invalid("pool type %q isn't available: this build supports %v", p.Type, Types())
	}
}
