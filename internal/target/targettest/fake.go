// Package targettest provides an in-memory configfs that enforces the ordering
// rules of the kernel's nvmet configfs interface, so that reconciler mistakes
// show up without root or a kernel target.
package targettest

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// FS is a fake nvmet configfs.
type FS struct {
	mu    sync.Mutex
	dirs  map[string]bool
	attrs map[string]string
	links map[string]string
	// Writes counts every mutating call, for asserting no-op reconciles.
	Writes int
	// Fail makes creating or writing a path ending in the key return the error.
	// Removal is unaffected, so that rollback can run as it would in the kernel.
	Fail map[string]error
}

// New returns an empty fake.
func New() *FS {
	return &FS{
		dirs:  map[string]bool{},
		attrs: map[string]string{},
		links: map[string]string{},
		Fail:  map[string]error{},
	}
}

func (f *FS) injected(p string) error {
	for suffix, err := range f.Fail {
		if strings.HasSuffix(p, suffix) {
			return err
		}
	}
	return nil
}

func (f *FS) hasChildren(dir string) bool {
	prefix := dir + "/"
	for d := range f.dirs {
		if strings.HasPrefix(d, prefix) {
			return true
		}
	}
	for l := range f.links {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// Mkdir implements target.ConfigFS.
func (f *FS) Mkdir(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.injected(p); err != nil {
		return err
	}
	if f.dirs[p] {
		return syscall.EEXIST
	}
	f.Writes++
	f.dirs[p] = true
	return nil
}

// Rmdir implements target.ConfigFS.
func (f *FS) Rmdir(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirs[p] {
		return syscall.ENOENT
	}
	if f.hasChildren(p) {
		return syscall.ENOTEMPTY
	}
	for _, target := range f.links {
		if target == p {
			return syscall.EBUSY
		}
	}
	f.Writes++
	delete(f.dirs, p)
	for a := range f.attrs {
		if strings.HasPrefix(a, p+"/") {
			delete(f.attrs, a)
		}
	}
	return nil
}

// Read implements target.ConfigFS.
func (f *FS) Read(p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.attrs[p]
	if !ok {
		return "", syscall.ENOENT
	}
	return v, nil
}

var (
	portAttr = regexp.MustCompile(`/ports/[0-9]+/(addr_|param_)`)
	nsAttr   = regexp.MustCompile(`/namespaces/[0-9]+/(device_path|device_uuid|device_nguid|buffered_io)$`)
)

// Write implements target.ConfigFS.
func (f *FS) Write(p, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.injected(p); err != nil {
		return err
	}
	dir := filepath.Dir(p)
	switch {
	case !f.dirs[dir] && filepath.Base(dir) != "passthru":
		return syscall.ENOENT
	case portAttr.MatchString(p) && f.hasChildren(dir+"/subsystems"):
		return syscall.EACCES
	case nsAttr.MatchString(p) && f.attrs[dir+"/enable"] == "1":
		return syscall.EBUSY
	case filepath.Base(p) == "attr_allow_any_host" && v == "1" && f.hasChildren(dir+"/allowed_hosts"):
		return syscall.EINVAL
	case filepath.Base(p) == "enable" && v == "1" && f.attrs[dir+"/device_path"] == "":
		return syscall.EINVAL
	case filepath.Base(p) == "ana_grpid" && v != "1" && !f.anaGroupExists(v):
		// The kernel puts a namespace only in a group that some port has.
		return syscall.EINVAL
	}
	f.Writes++
	f.attrs[p] = v
	return nil
}

func (f *FS) anaGroupExists(grp string) bool {
	for d := range f.dirs {
		if strings.Contains(d, "/ports/") && strings.HasSuffix(d, "/ana_groups/"+grp) {
			return true
		}
	}
	return false
}

// Symlink implements target.ConfigFS.
func (f *FS) Symlink(target, link string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.injected(link); err != nil {
		return err
	}
	if !f.dirs[target] {
		return syscall.ENOENT
	}
	if _, ok := f.links[link]; ok {
		return syscall.EEXIST
	}
	if strings.Contains(link, "/allowed_hosts/") {
		sub := link[:strings.Index(link, "/allowed_hosts/")]
		if f.attrs[sub+"/attr_allow_any_host"] == "1" {
			return syscall.EINVAL
		}
	}
	f.Writes++
	f.links[link] = target
	return nil
}

// Unlink implements target.ConfigFS.
func (f *FS) Unlink(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.links[p]; !ok {
		return syscall.ENOENT
	}
	f.Writes++
	delete(f.links, p)
	return nil
}

// List implements target.ConfigFS.
func (f *FS) List(p string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	collect := func(path string) {
		if filepath.Dir(path) == p {
			names = append(names, filepath.Base(path))
		}
	}
	for d := range f.dirs {
		collect(d)
	}
	for l := range f.links {
		collect(l)
	}
	slices.Sort(names)
	return names, nil
}

// Paths returns every directory and link, sorted, for inspection by tests.
func (f *FS) Paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for d := range f.dirs {
		out = append(out, d)
	}
	for l := range f.links {
		out = append(out, l)
	}
	slices.Sort(out)
	return out
}

// Attr returns an attribute's value and whether it exists.
func (f *FS) Attr(p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.attrs[p]
	return v, ok
}
