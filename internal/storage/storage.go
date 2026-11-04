// Package storage allocates volumes from pools. It knows nothing of NVMe: a volume is a
// path, which the target backends export like any other.
package storage

import (
	"nvweaver/internal/target"
)

// TypeReflink is a pool of files on a filesystem with reflink.
const TypeReflink = "reflink"

// Types lists the pool types this build can use.
func Types() []string { return []string{TypeReflink} }

// Capacity is what a pool's underlying storage reports.
type Capacity struct {
	TotalBytes int64
	UsedBytes  int64
}

// Driver manages the volumes of one pool. Volumes are identified by internal name, which is
// safe to use on disk, and all names it handles start with target.InternalPrefix.
type Driver interface {
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
	// List returns the internal names of volumes on disk, to find any that nothing refers to.
	List() ([]string, error)
}

// New returns the driver for a pool's type.
func New(p *target.Pool) (Driver, error) {
	switch p.Type {
	case TypeReflink:
		return &reflinkDriver{dir: p.Path}, nil
	default:
		return nil, target.Invalid("pool type %q isn't available: this build supports %v", p.Type, Types())
	}
}
