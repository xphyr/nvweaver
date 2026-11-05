package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"nvweaver/internal/target"
)

const (
	overlayfsMagic = 0x794c7630
	// ficlone is FICLONE from linux/fs.h, the same on x86-64 and arm64.
	ficlone    = 0x40049409
	fileSuffix = ".img"
)

// reflinkDriver keeps each volume as a sparse file in one directory.
type reflinkDriver struct {
	dir string
}

func (d *reflinkDriver) path(internal string) string {
	return filepath.Join(d.dir, internal+fileSuffix)
}

// Check tries a real clone, because that's the only reliable way to know that reflink works.
func (d *reflinkDriver) Check() error {
	if !filepath.IsAbs(d.dir) {
		return target.Invalid("pool path %q must be absolute", d.dir)
	}
	info, err := os.Stat(d.dir)
	if err != nil {
		return target.Invalid("pool path %q: %v (is it visible to the service?)", d.dir, err)
	}
	if !info.IsDir() {
		return target.Invalid("pool path %q is not a directory", d.dir)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.dir, &st); err != nil {
		return target.Invalid("pool path %q: %v", d.dir, err)
	}
	if int64(st.Type) == overlayfsMagic {
		return target.Invalid("pool path %q is on overlayfs, such as a container's own filesystem; mount a real filesystem there", d.dir)
	}
	src, err := os.CreateTemp(d.dir, ".nvweaver-check-*")
	if err != nil {
		return target.Invalid("pool path %q isn't writable: %v", d.dir, err)
	}
	defer os.Remove(src.Name())
	defer src.Close()
	if _, err := src.Write(make([]byte, 4096)); err != nil {
		return target.Invalid("pool path %q isn't writable: %v", d.dir, err)
	}
	dst, err := os.CreateTemp(d.dir, ".nvweaver-check-*")
	if err != nil {
		return target.Invalid("pool path %q isn't writable: %v", d.dir, err)
	}
	defer os.Remove(dst.Name())
	defer dst.Close()
	if err := clone(src, dst); err != nil {
		return target.Invalid("pool path %q doesn't support reflink, which needs XFS or btrfs: %v", d.dir, err)
	}
	return nil
}

func clone(src, dst *os.File) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficlone, src.Fd())
	if errno != 0 {
		return errno
	}
	return nil
}

// AlignSize implements Driver.
func (d *reflinkDriver) AlignSize(sizeBytes int64) (int64, error) { return sizeBytes, nil }

// Activate implements Driver: a file needs nothing.
func (d *reflinkDriver) Activate() error { return nil }

// Capacity implements Driver.
func (d *reflinkDriver) Capacity() (Capacity, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.dir, &st); err != nil {
		return Capacity{}, fmt.Errorf("pool path %q: %w", d.dir, err)
	}
	size := int64(st.Bsize)
	return Capacity{
		TotalBytes: int64(st.Blocks) * size,
		UsedBytes:  (int64(st.Blocks) - int64(st.Bfree)) * size,
	}, nil
}

// Create implements Driver.
func (d *reflinkDriver) Create(internal string, sizeBytes int64) (string, error) {
	path := d.path(internal)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return "", fmt.Errorf("create volume file: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(sizeBytes); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("size volume file: %w", err)
	}
	return path, nil
}

// Delete implements Driver.
func (d *reflinkDriver) Delete(internal string) error {
	err := os.Remove(d.path(internal))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete volume file: %w", err)
	}
	return nil
}

// Grow implements Driver.
func (d *reflinkDriver) Grow(internal string, sizeBytes int64) error {
	if err := os.Truncate(d.path(internal), sizeBytes); err != nil {
		return fmt.Errorf("grow volume file: %w", err)
	}
	return nil
}

// Snapshot implements Driver. The mode only guards against mistakes, as root can still write to it.
func (d *reflinkDriver) Snapshot(source, internal string) (string, error) {
	return d.copy(source, internal, 0, 0o400)
}

// Clone implements Driver.
func (d *reflinkDriver) Clone(source, internal string, sizeBytes int64) (string, error) {
	return d.copy(source, internal, sizeBytes, 0o600)
}

// copy makes an instant copy that shares its data until either side changes. The kernel flushes
// the source first, so this holds whatever hosts have written, as if the power had gone at this moment.
func (d *reflinkDriver) copy(source, internal string, sizeBytes int64, mode os.FileMode) (string, error) {
	src, err := os.Open(d.path(source))
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	if sizeBytes != 0 && sizeBytes < info.Size() {
		return "", target.Invalid("a clone can't be smaller than its source (%d bytes)", info.Size())
	}
	path := d.path(internal)
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return "", fmt.Errorf("create copy: %w", err)
	}
	defer dst.Close()
	if err := finishCopy(src, dst, sizeBytes, mode); err != nil {
		os.Remove(path)
		return "", err
	}
	// A snapshot is a promise about the data, so it must survive a crash once we've reported it.
	dir, err := os.Open(d.dir)
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("sync pool directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("sync pool directory: %w", err)
	}
	return path, nil
}

func finishCopy(src, dst *os.File, sizeBytes int64, mode os.FileMode) error {
	if err := clone(src, dst); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	if sizeBytes != 0 {
		if err := dst.Truncate(sizeBytes); err != nil {
			return fmt.Errorf("size copy: %w", err)
		}
	}
	if err := dst.Chmod(mode); err != nil {
		return fmt.Errorf("set copy's mode: %w", err)
	}
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("sync copy: %w", err)
	}
	return nil
}

// List implements Driver.
func (d *reflinkDriver) List() ([]string, error) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return nil, fmt.Errorf("pool path %q: %w", d.dir, err)
	}
	var names []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), fileSuffix)
		if ok && strings.HasPrefix(name, target.InternalPrefix) {
			names = append(names, name)
		}
	}
	return names, nil
}
