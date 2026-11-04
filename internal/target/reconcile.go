package target

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// ConfigFS is the small part of the filesystem the reconciler needs. Tests
// substitute a fake that enforces the kernel's ordering rules.
type ConfigFS interface {
	Mkdir(path string) error
	Rmdir(path string) error
	Read(path string) (string, error)
	Write(path, value string) error
	Symlink(target, link string) error
	Unlink(path string) error
	List(path string) ([]string, error)
}

// OSConfigFS is the real configfs.
type OSConfigFS struct{}

func (OSConfigFS) Mkdir(p string) error { return os.Mkdir(p, 0o755) }
func (OSConfigFS) Rmdir(p string) error { return syscall.Rmdir(p) }
func (OSConfigFS) Read(p string) (string, error) {
	b, err := os.ReadFile(p)
	return strings.TrimSpace(string(b)), err
}

// Write sends the value in a single write, as configfs attributes require.
func (OSConfigFS) Write(p, v string) error {
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(v)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
func (OSConfigFS) Symlink(t, l string) error { return os.Symlink(t, l) }
func (OSConfigFS) Unlink(p string) error     { return syscall.Unlink(p) }
func (OSConfigFS) List(p string) ([]string, error) {
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

// Target applies states to one nvmet configfs tree.
type Target struct {
	FS   ConfigFS
	Root string // for example /sys/kernel/config/nvmet
}

// KernelError is a failure reported by the kernel while applying configuration.
type KernelError struct {
	Op   string
	Path string
	Err  error
}

func (e *KernelError) Error() string {
	return fmt.Sprintf("%s %s: %v", e.Op, e.Path, e.Err)
}
func (e *KernelError) Unwrap() error { return e.Err }

func (t *Target) subsysDir(nqn string) string { return filepath.Join(t.Root, "subsystems", nqn) }
func (t *Target) hostDir(nqn string) string   { return filepath.Join(t.Root, "hosts", nqn) }
func (t *Target) portDir(id uint16) string {
	return filepath.Join(t.Root, "ports", strconv.Itoa(int(id)))
}
func (t *Target) nsDir(nqn string, nsid uint32) string {
	return filepath.Join(t.subsysDir(nqn), "namespaces", strconv.Itoa(int(nsid)))
}
func (t *Target) portLink(id uint16, nqn string) string {
	return filepath.Join(t.portDir(id), "subsystems", nqn)
}
func (t *Target) hostLink(sub, host string) string {
	return filepath.Join(t.subsysDir(sub), "allowed_hosts", host)
}

func (t *Target) mkdir(p string) error {
	if err := t.FS.Mkdir(p); err != nil && !errors.Is(err, fs.ErrExist) {
		return &KernelError{"mkdir", p, err}
	}
	return nil
}

func (t *Target) rmdir(p string) error {
	if err := t.FS.Rmdir(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &KernelError{"rmdir", p, err}
	}
	return nil
}

func (t *Target) link(target, link string) error {
	if err := t.FS.Symlink(target, link); err != nil && !errors.Is(err, fs.ErrExist) {
		return &KernelError{"link", link, err}
	}
	return nil
}

func (t *Target) unlink(p string) error {
	if err := t.FS.Unlink(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &KernelError{"unlink", p, err}
	}
	return nil
}

func (t *Target) write(p, v string) error {
	if err := t.FS.Write(p, v); err != nil {
		return &KernelError{"write " + strconv.Quote(v) + " to", p, err}
	}
	return nil
}

// differs reports whether an attribute needs writing. Skipping writes that
// change nothing is what lets a restarted service adopt a running target
// without disturbing connected hosts.
func (t *Target) differs(p, want string, norm func(string) string) bool {
	got, err := t.FS.Read(p)
	if err != nil {
		return true
	}
	if norm != nil {
		return norm(got) != norm(want)
	}
	return got != want
}

func boolAttr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func normID(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "-", ""))
}

// Reconcile moves the kernel from prev to next. Objects are only ever removed
// if they appear in prev, so anything configured outside this service is left
// alone. Subsystems are attached to ports last, so a host can never discover
// one that is only partly configured.
func (t *Target) Reconcile(prev, next *State) error {
	if err := t.removeStale(prev, next); err != nil {
		return err
	}
	for _, nqn := range sortedKeys(next.Hosts) {
		if err := t.applyHost(next.Hosts[nqn]); err != nil {
			return err
		}
	}
	for _, nqn := range sortedKeys(next.Subsystems) {
		if err := t.applySubsystem(next.Subsystems[nqn]); err != nil {
			return err
		}
	}
	for _, id := range sortedKeys(next.Ports) {
		if err := t.applyPort(next.Ports[id]); err != nil {
			return err
		}
	}
	for _, nqn := range sortedKeys(next.Subsystems) {
		for _, id := range next.Subsystems[nqn].Ports {
			// The first link on a port is what binds its socket.
			if err := t.link(t.subsysDir(nqn), t.portLink(id, nqn)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Target) removeStale(prev, next *State) error {
	for _, nqn := range sortedKeys(prev.Subsystems) {
		old := prev.Subsystems[nqn]
		cur := next.Subsystems[nqn]
		for _, id := range old.Ports {
			if cur == nil || !contains(cur.Ports, id) {
				if err := t.unlink(t.portLink(id, nqn)); err != nil {
					return err
				}
			}
		}
		for _, h := range old.AllowedHosts {
			if cur == nil || !contains(cur.AllowedHosts, h) {
				if err := t.unlink(t.hostLink(nqn, h)); err != nil {
					return err
				}
			}
		}
		for _, nsid := range sortedKeys(old.Namespaces) {
			if cur == nil || cur.Namespaces[nsid] == nil {
				if err := t.rmdir(t.nsDir(nqn, nsid)); err != nil {
					return err
				}
			}
		}
		if old.Passthru != "" && (cur == nil || cur.Passthru != old.Passthru) {
			if err := t.write(filepath.Join(t.subsysDir(nqn), "passthru", "enable"), "0"); err != nil {
				return err
			}
		}
		if cur == nil {
			if err := t.rmdir(t.subsysDir(nqn)); err != nil {
				return err
			}
		}
	}
	for _, nqn := range sortedKeys(prev.Hosts) {
		if next.Hosts[nqn] == nil {
			if err := t.rmdir(t.hostDir(nqn)); err != nil {
				return err
			}
		}
	}
	for _, id := range sortedKeys(prev.Ports) {
		if next.Ports[id] == nil {
			if err := t.rmdir(t.portDir(id)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Target) applyHost(h *Host) error {
	dir := t.hostDir(h.NQN)
	if err := t.mkdir(dir); err != nil {
		return err
	}
	attrs := []struct{ name, value string }{
		{"dhchap_hash", h.DHCHAPHash},
		{"dhchap_dhgroup", h.DHCHAPDHGroup},
		{"dhchap_key", h.DHCHAPKey},
		{"dhchap_ctrl_key", h.DHCHAPCtrlKey},
	}
	for _, a := range attrs {
		if a.value == "" {
			continue
		}
		if p := filepath.Join(dir, a.name); t.differs(p, a.value, nil) {
			if err := t.write(p, a.value); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Target) applySubsystem(s *Subsystem) error {
	dir := t.subsysDir(s.NQN)
	if err := t.mkdir(dir); err != nil {
		return err
	}
	// The kernel freezes serial and model once a host has identified the
	// controller, which is why the API has no way to change them.
	for _, a := range []struct{ name, value string }{
		{"attr_serial", s.Serial},
		{"attr_model", s.Model},
		{"attr_allow_any_host", boolAttr(s.AllowAnyHost)},
	} {
		if p := filepath.Join(dir, a.name); t.differs(p, a.value, nil) {
			if err := t.write(p, a.value); err != nil {
				return err
			}
		}
	}
	for _, h := range s.AllowedHosts {
		if err := t.link(t.hostDir(h), t.hostLink(s.NQN, h)); err != nil {
			return err
		}
	}
	for _, nsid := range sortedKeys(s.Namespaces) {
		if err := t.applyNamespace(s.NQN, s.Namespaces[nsid]); err != nil {
			return err
		}
	}
	if s.Passthru != "" {
		return t.applyPassthru(dir, s.Passthru)
	}
	return nil
}

func (t *Target) applyPassthru(subsysDir, device string) error {
	dir := filepath.Join(subsysDir, "passthru")
	enable := filepath.Join(dir, "enable")
	path := filepath.Join(dir, "device_path")
	if !t.differs(path, device, nil) && !t.differs(enable, "1", nil) {
		return nil
	}
	if !t.differs(enable, "1", nil) {
		if err := t.write(enable, "0"); err != nil {
			return err
		}
	}
	if err := t.write(path, device); err != nil {
		return err
	}
	return t.write(enable, "1")
}

func (t *Target) applyNamespace(nqn string, n *Namespace) error {
	dir := t.nsDir(nqn, n.NSID)
	if err := t.mkdir(dir); err != nil {
		return err
	}
	enable := filepath.Join(dir, "enable")
	// These attributes can only be written while the namespace is disabled.
	attrs := []struct {
		name, value string
		norm        func(string) string
	}{
		{"device_path", n.DevicePath, nil},
		{"device_uuid", n.UUID, normID},
		{"device_nguid", n.NGUID, normID},
		{"buffered_io", boolAttr(n.BufferedIO), nil},
	}
	var pending []int
	for i, a := range attrs {
		if t.differs(filepath.Join(dir, a.name), a.value, a.norm) {
			pending = append(pending, i)
		}
	}
	running := !t.differs(enable, "1", nil)
	if running && (!n.Enabled || len(pending) > 0) {
		if err := t.write(enable, "0"); err != nil {
			return err
		}
		running = false
	}
	for _, i := range pending {
		if err := t.write(filepath.Join(dir, attrs[i].name), attrs[i].value); err != nil {
			return err
		}
	}
	if n.Enabled && !running {
		return t.write(enable, "1")
	}
	return nil
}

func (t *Target) applyPort(p *Port) error {
	dir := t.portDir(p.ID)
	if err := t.mkdir(dir); err != nil {
		return err
	}
	type attr struct {
		name, value string
	}
	attrs := []attr{
		{"addr_trtype", "tcp"},
		{"addr_adrfam", p.Family()},
		{"addr_traddr", p.Address},
		{"addr_trsvcid", strconv.Itoa(p.Service)},
	}
	if p.InlineDataSize != nil {
		attrs = append(attrs, attr{"param_inline_data_size", strconv.Itoa(*p.InlineDataSize)})
	}
	var pending []attr
	for _, a := range attrs {
		if t.differs(filepath.Join(dir, a.name), a.value, nil) {
			pending = append(pending, a)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	// A port's attributes are read-only while it has subsystems attached.
	// Detaching drops its connections; hosts reconnect once Reconcile relinks.
	linked, err := t.FS.List(filepath.Join(dir, "subsystems"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &KernelError{"list", dir, err}
	}
	for _, nqn := range linked {
		if err := t.unlink(t.portLink(p.ID, nqn)); err != nil {
			return err
		}
	}
	for _, a := range pending {
		if err := t.write(filepath.Join(dir, a.name), a.value); err != nil {
			return err
		}
	}
	return nil
}

// Available reports whether the nvmet configfs tree exists.
func (t *Target) Available() bool {
	_, err := t.FS.List(filepath.Join(t.Root, "subsystems"))
	return err == nil
}

// Revalidate tells the kernel a namespace's backing size changed, which also
// sends connected hosts an asynchronous event.
func (t *Target) Revalidate(nqn string, nsid uint32) error {
	return t.write(filepath.Join(t.nsDir(nqn, nsid), "revalidate_size"), "1")
}

// LiveEnabled reports whether the kernel has the namespace enabled, which can
// differ from the desired state if the backing device has gone away.
func (t *Target) LiveEnabled(nqn string, nsid uint32) bool {
	v, err := t.FS.Read(filepath.Join(t.nsDir(nqn, nsid), "enable"))
	return err == nil && v == "1"
}

func sortedKeys[K interface{ ~string | ~uint16 | ~uint32 }, V any](m map[K]V) []K {
	return slices.Sorted(maps.Keys(m))
}
