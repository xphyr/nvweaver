// Package spdk implements the target using SPDK's userspace NVMe-oF target, for
// hosts whose kernel has no nvmet-tcp.
//
// This service starts and owns the nvmf_tgt process, so unlike the kernel
// backend there is nothing configured by anyone else to respect: Reconcile just
// makes SPDK's live configuration match the state it is given.
package spdk

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"nvweaver/internal/target"
)

const (
	discoveryNQN = "nqn.2014-08.org.nvmexpress.discovery"
	// namePrefix marks the bdevs and keys this service creates.
	namePrefix = "nvweaver-"
	busyRetry  = 100 * time.Millisecond
	busyLimit  = 100 // 10 seconds
)

// Backend is the SPDK target.Backend.
type Backend struct {
	// needsFull is set once a reconcile has failed, leaving what is live unknown.
	needsFull bool
	rpc       Caller
	keyDir    string
	// reactors is how many cores SPDK runs on. Zero skips the check on fs.aio-max-nr.
	reactors int
	// aioLimit reads fs.aio-max-nr.
	aioLimit func() (int, error)
	proc     *process
	// probe reports why an address could not be listened on, if it couldn't.
	probe func(address string) error
	done  chan struct{}
	// durability is target.DurabilitySyncAttribute, or target.DurabilityFUA for nvmf_tgt with nvweaver's patch. Empty
	// means the first, which is always safe. See decision 58.
	durability string
}

// fua says whether this nvmf_tgt makes FUA writes durable itself, so that files need no sync attribute.
func (b *Backend) fua() bool { return b.durability == target.DurabilityFUA }

// Name implements target.Backend.
func (b *Backend) Name() string { return "spdk" }

// Capabilities implements target.Backend.
func (b *Backend) Capabilities() target.Capabilities {
	durability := target.DurabilitySyncAttribute
	if b.fua() {
		durability = target.DurabilityFUA
	}
	return target.Capabilities{IdentifiesConnectedHosts: true, SnapshotGroups: true, Durability: durability,
		Transports: []string{target.TransportTCP}}
}

// aioPerNamespace is how much of fs.aio-max-nr each namespace uses, on each core.
const aioPerNamespace = 128

func readAIOLimit() (int, error) {
	b, err := os.ReadFile("/proc/sys/fs/aio-max-nr")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// checkAIO refuses what the kernel would, as SPDK only reports "Invalid argument", and only partway through.
func (b *Backend) checkAIO(s *target.State) error {
	if b.reactors == 0 || b.aioLimit == nil {
		return nil
	}
	limit, err := b.aioLimit()
	if err != nil {
		return nil // It can't be read, so leave it to SPDK to find out.
	}
	namespaces := 0
	for _, sub := range s.Subsystems {
		for _, ns := range sub.Namespaces {
			if ns.Enabled {
				namespaces++
			}
		}
	}
	if namespaces*aioPerNamespace*b.reactors > limit {
		return target.Invalid("%d namespaces on %d SPDK cores need fs.aio-max-nr to be at least %d, and it's %d: raise it on the host, with sysctl -w fs.aio-max-nr=1048576",
			namespaces, b.reactors, namespaces*aioPerNamespace*b.reactors, limit)
	}
	return nil
}

// maxSubsystems is fixed when SPDK starts. Each one costs SPDK about 0.4 MB of memory, whether or not it's in use.
const maxSubsystems = 8192

// Check implements target.Backend, rejecting what SPDK has no equivalent for
// rather than ignoring a setting that someone may be relying on.
func (b *Backend) Check(s *target.State) error {
	for _, p := range s.Ports {
		if p.InlineDataSize != nil {
			return target.Invalid("inline_data_size on port %d is not supported by the spdk backend", p.ID)
		}
		// SPDK accepts a wildcard listener, then rejects every host because the
		// address they connected to doesn't match it, with an error that blames
		// the host. Binding each local address instead would go stale as they change.
		if net.ParseIP(p.Address).IsUnspecified() {
			return target.Invalid("port %d: the spdk backend cannot serve hosts on the wildcard address %s; use a specific IP address", p.ID, p.Address)
		}
	}
	if len(s.Subsystems) > maxSubsystems {
		return target.Invalid("the spdk backend is limited to %d subsystems: share a subsystem between volumes", maxSubsystems)
	}
	if err := b.checkAIO(s); err != nil {
		return err
	}
	for _, sub := range s.Subsystems {
		if sub.Passthru != "" {
			return target.Invalid("passthru_device on %s is not supported by the spdk backend", sub.NQN)
		}
		for _, ns := range sub.Namespaces {
			if ns.BufferedIO {
				return target.Invalid("buffered_io on %s namespace %d is not supported by the spdk backend", sub.NQN, ns.NSID)
			}
		}
	}
	return nil
}

// Done implements target.Backend.
func (b *Backend) Done() <-chan struct{} { return b.done }

// Available implements target.Backend.
func (b *Backend) Available() bool {
	return b.rpc.Call("spdk_get_version", nil, nil) == nil
}

type liveListener struct {
	TrType  string `json:"trtype,omitempty"`
	AdrFam  string `json:"adrfam,omitempty"`
	TrAddr  string `json:"traddr"`
	TrSvcID string `json:"trsvcid"`
}

func (l liveListener) address() string { return net.JoinHostPort(l.TrAddr, l.TrSvcID) }

type liveHost struct {
	NQN     string `json:"nqn"`
	Key     string `json:"dhchap_key"`
	CtrlKey string `json:"dhchap_ctrlr_key"`
}

type liveNamespace struct {
	NSID     uint32 `json:"nsid"`
	BdevName string `json:"bdev_name"`
	NGUID    string `json:"nguid"`
	UUID     string `json:"uuid"`
	// ANAGroup is listed when the subsystem has ANA reporting on. 0 when it isn't listed.
	ANAGroup uint32 `json:"anagrpid,omitempty"`
}

type liveSubsystem struct {
	NQN          string          `json:"nqn"`
	Listeners    []liveListener  `json:"listen_addresses"`
	AllowAnyHost bool            `json:"allow_any_host"`
	Hosts        []liveHost      `json:"hosts"`
	Namespaces   []liveNamespace `json:"namespaces"`
}

func (b *Backend) subsystems() (map[string]*liveSubsystem, error) {
	var list []*liveSubsystem
	if err := b.call("nvmf_get_subsystems", nil, &list); err != nil {
		return nil, err
	}
	out := make(map[string]*liveSubsystem, len(list))
	for _, s := range list {
		out[s.NQN] = s
	}
	return out, nil
}

// call retries while a subsystem is mid-transition, which SPDK reports as busy.
func (b *Backend) call(method string, params, result any) error {
	var err error
	for range busyLimit {
		if err = b.rpc.Call(method, params, result); !isBusy(err) {
			break
		}
		time.Sleep(busyRetry)
	}
	// A rollback follows, so what matters is telling the client why it was refused.
	var re *RPCError
	if errors.As(err, &re) {
		switch re.Code {
		case invalidParams, -int(syscall.EINVAL), -int(syscall.ENOENT), -int(syscall.ENODEV):
			return target.Invalid("%v", err)
		case -int(syscall.EEXIST), -int(syscall.EBUSY):
			return target.NewConflict("%v", err)
		}
	}
	return err
}

// fileBlockSize is the block size to give SPDK for a regular file, or 0 for
// anything else, for which SPDK detects the real sector size itself. It follows
// the kernel target, so a file looks the same to hosts on either backend.
func fileBlockSize(path string) uint32 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return 0
	}
	return clampBlockSize(int64(st.Blksize))
}

func clampBlockSize(size int64) uint32 {
	const minSize, maxSize = 512, 4096
	for b := int64(maxSize); b > minSize; b /= 2 {
		if size >= b {
			return uint32(b)
		}
	}
	return minSize
}

func hashName(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return namePrefix + hex.EncodeToString(h[:12])
}

// bdevName covers the path, so changing what a namespace is backed by yields a
// new bdev and the old one is cleaned up like any other leftover.
func bdevName(nqn string, nsid uint32, path string) string {
	return hashName("bdev", nqn, strconv.Itoa(int(nsid)), path)
}

func keyName(nqn, secret string) string {
	if secret == "" {
		return ""
	}
	return hashName("key", nqn, secret)
}

func normID(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "-", ""))
}

func listenerOf(p *target.Port) liveListener {
	return liveListener{TrType: "TCP", AdrFam: p.Family(), TrAddr: p.Address, TrSvcID: strconv.Itoa(p.Service)}
}

func addrOf(p *target.Port) string {
	return net.JoinHostPort(p.Address, strconv.Itoa(p.Service))
}

// Reconcile implements target.Backend. Live state is the truth here, and prev only says where to look,
// so that a change costs the same at any scale. Anything unusual gets a full reconcile, which ignores prev:
// startup and shutdown, a change to ports, and whatever follows a failure, which makes rollback just another
// reconcile.
func (b *Backend) Reconcile(prev, next *target.State) error {
	var err error
	if b.needsFull || prev == nil || emptyState(prev) || emptyState(next) || !reflect.DeepEqual(prev.Ports, next.Ports) {
		err = b.reconcileAll(next)
	} else {
		err = b.reconcileChanges(prev, next)
	}
	b.needsFull = err != nil
	return err
}

func emptyState(s *target.State) bool {
	return len(s.Subsystems) == 0 && len(s.Hosts) == 0 && len(s.Ports) == 0
}

func (b *Backend) reconcileAll(next *target.State) error {
	live, err := b.subsystems()
	if err != nil {
		return err
	}
	if _, err := b.removeStale(next, live); err != nil {
		return err
	}
	if err := b.createKeys(next); err != nil {
		return err
	}
	have, err := b.bdevs()
	if err != nil {
		return err
	}
	if err := b.createBdevs(next, func(name string) (bool, error) { return have[name], nil }); err != nil {
		return err
	}
	if live, err = b.subsystems(); err != nil {
		return err
	}
	if err := b.applySubsystems(next, next, live); err != nil {
		return err
	}
	if live, err = b.subsystems(); err != nil {
		return err
	}
	// Listeners go last, so that a host can't find a half-built subsystem.
	bound := boundAddresses(live)
	if err := b.applyListeners(next, next, live, bound); err != nil {
		return err
	}
	if err := b.applyPauses(next, next, live); err != nil {
		return err
	}
	if err := b.applyDiscovery(next, live[discoveryNQN], bound); err != nil {
		return err
	}
	return b.removeLeftovers(next)
}

// changedHosts names the hosts added, removed or changed.
func changedHosts(prev, next map[string]*target.Host) map[string]bool {
	out := map[string]bool{}
	for nqn, h := range prev {
		if !reflect.DeepEqual(h, next[nqn]) {
			out[nqn] = true
		}
	}
	for nqn := range next {
		if prev[nqn] == nil {
			out[nqn] = true
		}
	}
	return out
}

// listsAny reports whether a subsystem allows any of these hosts by name.
func listsAny(sub *target.Subsystem, hosts map[string]bool) bool {
	return slices.ContainsFunc(sub.AllowedHosts, func(h string) bool { return hosts[h] })
}

// reconcileChanges does what a full reconcile does, for the subsystems that differ. Every RPC measured about 8 ms,
// even ones that do next to nothing, and at any scale. So what a change costs is how many it makes, and that mustn't
// depend on how many namespaces a subsystem has, let alone how many there are in total.
func (b *Backend) reconcileChanges(prev, next *target.State) error {
	changedHosts := changedHosts(prev.Hosts, next.Hosts)
	hostsChanged := len(changedHosts) > 0
	scope := &target.State{Hosts: next.Hosts, Ports: next.Ports, Subsystems: map[string]*target.Subsystem{}}
	live := map[string]*liveSubsystem{}
	gone := map[string]bool{}
	for nqn, old := range prev.Subsystems {
		want := next.Subsystems[nqn]
		// SPDK keeps a host's keys on each subsystem's entry for it, so a subsystem listing a changed host changes.
		if reflect.DeepEqual(old, want) && !listsAny(old, changedHosts) {
			continue
		}
		if want != nil {
			scope.Subsystems[nqn] = want
		}
		sub, err := b.subsystem(nqn)
		if err != nil {
			return err
		}
		if sub != nil {
			live[nqn] = sub
		}
		for nsid, ns := range old.Namespaces {
			gone[bdevName(nqn, nsid, ns.DevicePath)] = true
		}
	}
	for nqn, want := range next.Subsystems {
		if prev.Subsystems[nqn] == nil {
			scope.Subsystems[nqn] = want
		}
	}
	if len(live) == 0 && len(scope.Subsystems) == 0 && !hostsChanged {
		return nil
	}
	removed, err := b.removeStale(scope, live)
	if err != nil {
		return err
	}
	if hostsChanged {
		if err := b.createKeys(next); err != nil {
			return err
		}
	}
	// A namespace that stays is using its bdev, so there's no need to ask whether that exists.
	inUse := map[string]bool{}
	for nqn, sub := range live {
		if want := scope.Subsystems[nqn]; want != nil {
			for _, ns := range sub.Namespaces {
				if wantedNamespace(want, ns) {
					inUse[ns.BdevName] = true
				}
			}
		}
	}
	if err := b.createBdevs(scope, func(name string) (bool, error) {
		if inUse[name] {
			return true, nil
		}
		return b.bdevExists(name)
	}); err != nil {
		return err
	}
	for _, nqn := range sortedKeys(scope.Subsystems) {
		if prev.Subsystems[nqn] != nil && !removed {
			continue // What was fetched above is still right.
		}
		sub, err := b.subsystem(nqn)
		if err != nil {
			return err
		}
		if sub == nil {
			delete(live, nqn)
		} else {
			live[nqn] = sub
		}
	}
	if err := b.applySubsystems(scope, next, live); err != nil {
		return err
	}
	// Discovery's listeners are among the ports that were attached.
	bound := boundAddresses(live)
	for addr := range attachedAddresses(prev) {
		bound[addr] = true
	}
	if err := b.applyListeners(scope, next, live, bound); err != nil {
		return err
	}
	if err := b.applyPauses(scope, next, live); err != nil {
		return err
	}
	if !maps.Equal(attachedAddresses(prev), attachedAddresses(next)) {
		disc, err := b.subsystem(discoveryNQN)
		if err != nil {
			return err
		}
		if err := b.applyDiscovery(next, disc, bound); err != nil {
			return err
		}
	}
	// Only what these subsystems used, rather than listing every bdev.
	for _, sub := range next.Subsystems {
		for nsid, ns := range sub.Namespaces {
			if ns.Enabled {
				delete(gone, bdevName(sub.NQN, nsid, ns.DevicePath))
			}
		}
	}
	for _, name := range sortedKeys(gone) {
		exists, err := b.bdevExists(name)
		if err != nil {
			return err
		}
		if exists {
			if err := b.call("bdev_aio_delete", map[string]any{"name": name}, nil); err != nil {
				return err
			}
		}
	}
	if hostsChanged {
		return b.removeUnusedKeys(next)
	}
	return nil
}

// attachedAddresses finds the ports with a subsystem on them.
func attachedAddresses(s *target.State) map[string]bool {
	out := map[string]bool{}
	for _, sub := range s.Subsystems {
		for _, id := range sub.Ports {
			if p := s.Ports[id]; p != nil {
				out[addrOf(p)] = true
			}
		}
	}
	return out
}

// subsystem fetches one subsystem's live state, or nil if there's none.
func (b *Backend) subsystem(nqn string) (*liveSubsystem, error) {
	var list []*liveSubsystem
	err := b.call("nvmf_get_subsystems", map[string]any{"nqn": nqn}, &list)
	var ve *target.ValidationError
	if errors.As(err, &ve) {
		return nil, nil // SPDK says that it has no such subsystem.
	}
	if err != nil {
		return nil, err
	}
	for _, sub := range list {
		if sub.NQN == nqn {
			return sub, nil
		}
	}
	return nil, nil
}

// bdevExists asks SPDK about one bdev. Only "No such device" means it isn't there: another failure leaves it unknown,
// and creating or deleting it then isn't safe.
func (b *Backend) bdevExists(name string) (bool, error) {
	var list []liveBdev
	err := b.rpc.Call("bdev_get_bdevs", map[string]any{"name": name}, &list)
	var re *RPCError
	if errors.As(err, &re) && re.Code == -int(syscall.ENODEV) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(list) > 0, nil
}

func boundAddresses(live map[string]*liveSubsystem) map[string]bool {
	bound := map[string]bool{}
	for _, sub := range live {
		for _, l := range sub.Listeners {
			bound[l.address()] = true
		}
	}
	return bound
}

func wantedNamespace(sub *target.Subsystem, ns liveNamespace) bool {
	want := sub.Namespaces[ns.NSID]
	return want != nil && want.Enabled &&
		ns.BdevName == bdevName(sub.NQN, want.NSID, want.DevicePath) &&
		normID(ns.UUID) == normID(want.UUID) && normID(ns.NGUID) == normID(want.NGUID)
}

// removeStale reports whether it removed anything, which makes what was fetched out of date.
func (b *Backend) removeStale(next *target.State, live map[string]*liveSubsystem) (bool, error) {
	removed := false
	for _, nqn := range sortedKeys(live) {
		if nqn == discoveryNQN {
			continue
		}
		cur := live[nqn]
		want := next.Subsystems[nqn]
		if want == nil {
			if err := b.call("nvmf_delete_subsystem", map[string]any{"nqn": nqn}, nil); err != nil {
				return removed, err
			}
			removed = true
			continue
		}
		wantAddrs := map[string]bool{}
		for _, id := range want.Ports {
			wantAddrs[addrOf(next.Ports[id])] = true
		}
		for _, l := range cur.Listeners {
			if !wantAddrs[l.address()] {
				if err := b.removeListener(nqn, l); err != nil {
					return removed, err
				}
				removed = true
			}
		}
		for _, h := range cur.Hosts {
			if !slices.Contains(want.AllowedHosts, h.NQN) {
				params := map[string]any{"nqn": nqn, "host": h.NQN}
				if err := b.call("nvmf_subsystem_remove_host", params, nil); err != nil {
					return removed, err
				}
				removed = true
			}
		}
		for _, ns := range cur.Namespaces {
			if !wantedNamespace(want, ns) {
				params := map[string]any{"nqn": nqn, "nsid": ns.NSID}
				if err := b.call("nvmf_subsystem_remove_ns", params, nil); err != nil {
					return removed, err
				}
				removed = true
			}
		}
	}
	return removed, nil
}

func (b *Backend) removeListener(nqn string, l liveListener) error {
	return b.call("nvmf_subsystem_remove_listener", map[string]any{
		"nqn": nqn,
		"listen_address": liveListener{
			TrType: "TCP", AdrFam: strings.ToLower(l.AdrFam), TrAddr: l.TrAddr, TrSvcID: l.TrSvcID,
		},
	}, nil)
}

type liveKey struct {
	Name string `json:"name"`
}

// createKeys hands secrets to SPDK's keyring, which reads them from files that
// must be private to this process.
func (b *Backend) createKeys(next *target.State) error {
	var live []liveKey
	if err := b.call("keyring_get_keys", nil, &live); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, k := range live {
		have[k.Name] = true
	}
	for _, nqn := range sortedKeys(next.Hosts) {
		h := next.Hosts[nqn]
		for _, secret := range []string{h.DHCHAPKey, h.DHCHAPCtrlKey} {
			name := keyName(nqn, secret)
			if name == "" || have[name] {
				continue
			}
			path := filepath.Join(b.keyDir, name)
			if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
				return err
			}
			params := map[string]any{"name": name, "path": path}
			if err := b.call("keyring_file_add_key", params, nil); err != nil {
				os.Remove(path)
				return err
			}
			have[name] = true
		}
	}
	return nil
}

type liveBdev struct {
	Name string `json:"name"`
}

func (b *Backend) bdevs() (map[string]bool, error) {
	var live []liveBdev
	if err := b.call("bdev_get_bdevs", nil, &live); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, d := range live {
		out[d.Name] = true
	}
	return out, nil
}

// createBdevs uses AIO for everything, which is what keeps non-NVMe disks and
// files working: the storage stays on its normal kernel driver.
func (b *Backend) createBdevs(next *target.State, exists func(string) (bool, error)) error {
	for _, nqn := range sortedKeys(next.Subsystems) {
		sub := next.Subsystems[nqn]
		for _, nsid := range sortedKeys(sub.Namespaces) {
			ns := sub.Namespaces[nsid]
			if !ns.Enabled {
				continue
			}
			name := bdevName(nqn, nsid, ns.DevicePath)
			there, err := exists(name)
			if err != nil {
				return err
			}
			if there {
				continue
			}
			params := map[string]any{"name": name, "filename": ns.DevicePath}
			if size := fileBlockSize(ns.DevicePath); size != 0 {
				params["block_size"] = size
			}
			// Each start creates every bdev afresh, so changing the setting changes existing files too.
			if err := setSyncAttribute(ns.DevicePath, !b.fua()); err != nil {
				return fmt.Errorf("namespace %d of %s: %w", nsid, nqn, err)
			}
			if err := b.call("bdev_aio_create", params, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// From linux/fs.h, the same on x86-64 and arm64.
const (
	fsIocGetFlags = 0x80086601
	fsIocSetFlags = 0x40086602
	fsSyncFl      = 0x8
)

// setSyncAttribute sets or clears a file's sync attribute, as chattr +S and -S do. Upstream SPDK tells hosts that it has
// a write cache, but ignores FUA, and a filesystem such as XFS commits its log with a FUA write, then relies on it. On a
// block device those writes reach the device, but in a file, its new blocks stay in the page cache and the filesystem's
// journal until the next flush. So a power cut corrupted hosts' filesystems. With the attribute, the kernel treats each
// write as O_DSYNC (decision 28). nvmf_tgt with nvweaver's patch makes FUA writes durable itself, so it's cleared there,
// as it makes every other write slow too (decision 58). A block device has no such attribute, and is left as it is.
func setSyncAttribute(path string, on bool) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil // Anything wrong with the path is for SPDK to report.
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var flags int64
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocGetFlags, uintptr(unsafe.Pointer(&flags))); errno != 0 {
		if !on {
			return nil // No attribute to clear.
		}
		return fmt.Errorf("can't make %s safe from a power cut: this filesystem has no sync attribute: %w", path, errno)
	}
	if (flags&fsSyncFl != 0) == on {
		return nil
	}
	flags ^= fsSyncFl
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocSetFlags, uintptr(unsafe.Pointer(&flags))); errno != 0 {
		if on {
			return fmt.Errorf("can't make %s safe from a power cut: %w", path, errno)
		}
		return fmt.Errorf("can't clear the sync attribute of %s, which makes every write slow: %w", path, errno)
	}
	return nil
}

// applySubsystems applies the subsystems in scope. all is the whole state, for what they refer to.
func (b *Backend) applySubsystems(scope, all *target.State, live map[string]*liveSubsystem) error {
	for _, nqn := range sortedKeys(scope.Subsystems) {
		want := scope.Subsystems[nqn]
		cur := live[nqn]
		if cur == nil {
			err := b.call("nvmf_create_subsystem", map[string]any{
				"nqn":            nqn,
				"serial_number":  want.Serial,
				"model_number":   want.Model,
				"allow_any_host": want.AllowAnyHost,
				"max_namespaces": target.MaxNSID,
				// Needed to move namespaces between ANA groups, which is how a snapshot group pauses hosts: decision 56.
				"ana_reporting": true,
			}, nil)
			if err != nil {
				return err
			}
			cur = &liveSubsystem{NQN: nqn, AllowAnyHost: want.AllowAnyHost}
		}
		if cur.AllowAnyHost != want.AllowAnyHost {
			params := map[string]any{"nqn": nqn, "allow_any_host": want.AllowAnyHost}
			if err := b.call("nvmf_subsystem_allow_any_host", params, nil); err != nil {
				return err
			}
		}
		if err := b.applyHosts(all, want, cur); err != nil {
			return err
		}
		if err := b.applyNamespaces(want, cur); err != nil {
			return err
		}
	}
	return nil
}

func hostParams(nqn string, h *target.Host) map[string]any {
	params := map[string]any{"nqn": nqn, "host": h.NQN}
	if k := keyName(h.NQN, h.DHCHAPKey); k != "" {
		params["dhchap_key"] = k
	}
	if k := keyName(h.NQN, h.DHCHAPCtrlKey); k != "" {
		params["dhchap_ctrlr_key"] = k
	}
	return params
}

func (b *Backend) applyHosts(next *target.State, want *target.Subsystem, cur *liveSubsystem) error {
	for _, nqn := range want.AllowedHosts {
		h := next.Hosts[nqn]
		i := slices.IndexFunc(cur.Hosts, func(l liveHost) bool { return l.NQN == nqn })
		if i < 0 {
			if err := b.call("nvmf_subsystem_add_host", hostParams(want.NQN, h), nil); err != nil {
				return err
			}
			continue
		}
		l := cur.Hosts[i]
		if l.Key != keyName(nqn, h.DHCHAPKey) || l.CtrlKey != keyName(nqn, h.DHCHAPCtrlKey) {
			// nvmf_subsystem_set_keys leaves a connected host's session up, so it could carry on with the key it
			// logged in with. Removing the host drops it, and it has to log in again with its new keys. Others on the
			// subsystem aren't affected. See decision 30.
			params := map[string]any{"nqn": want.NQN, "host": nqn}
			if err := b.call("nvmf_subsystem_remove_host", params, nil); err != nil {
				return err
			}
			if err := b.call("nvmf_subsystem_add_host", hostParams(want.NQN, h), nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Backend) applyNamespaces(want *target.Subsystem, cur *liveSubsystem) error {
	for _, nsid := range sortedKeys(want.Namespaces) {
		ns := want.Namespaces[nsid]
		present := slices.ContainsFunc(cur.Namespaces, func(l liveNamespace) bool {
			return l.NSID == nsid && wantedNamespace(want, l)
		})
		if !ns.Enabled || present {
			continue
		}
		err := b.call("nvmf_subsystem_add_ns", map[string]any{
			"nqn": want.NQN,
			"namespace": map[string]any{
				"nsid":      nsid,
				"bdev_name": bdevName(want.NQN, nsid, ns.DevicePath),
				"uuid":      ns.UUID,
				"nguid":     normID(ns.NGUID),
				// SPDK's default is a group per NSID, which would put namespace 2 in the pause group.
				"anagrpid": 1,
			},
		}, nil)
		if err != nil {
			return err
		}
	}
	return nil
}

// applyPauses puts the namespaces of the subsystems in scope in the ANA group they belong in: the pause group, which is
// made inaccessible on the subsystem's listeners first, or group 1. live may predate namespaces just added, which are in
// group 1. SPDK pauses a namespace for a moment to move it. See decision 56.
func (b *Backend) applyPauses(scope, all *target.State, live map[string]*liveSubsystem) error {
	for _, nqn := range sortedKeys(scope.Subsystems) {
		want := scope.Subsystems[nqn]
		in := map[uint32]uint32{}
		if cur := live[nqn]; cur != nil {
			for _, ns := range cur.Namespaces {
				in[ns.NSID] = ns.ANAGroup
			}
		}
		group := func(nsid uint32) uint32 {
			if g := in[nsid]; g != 0 {
				return g
			}
			return 1
		}
		paused, wasPaused := false, false
		for _, ns := range want.Namespaces {
			paused = paused || ns.Enabled && ns.Paused
			wasPaused = wasPaused || group(ns.NSID) == target.PauseANAGroup
		}
		setState := func(state string) error {
			for _, id := range want.Ports {
				if err := b.call("nvmf_subsystem_listener_set_ana_state", map[string]any{
					"nqn": nqn, "listen_address": listenerOf(all.Ports[id]), "ana_state": state, "anagrpid": target.PauseANAGroup,
				}, nil); err != nil {
					return err
				}
			}
			return nil
		}
		if paused {
			if err := setState("inaccessible"); err != nil {
				return err
			}
		}
		for _, nsid := range sortedKeys(want.Namespaces) {
			ns := want.Namespaces[nsid]
			if g := uint32(target.ANAGroup(ns)); ns.Enabled && group(nsid) != g {
				if err := b.call("nvmf_subsystem_set_ns_ana_group", map[string]any{"nqn": nqn, "nsid": nsid, "anagrpid": g}, nil); err != nil {
					return err
				}
			}
		}
		if !paused && wasPaused {
			if err := setState("optimized"); err != nil {
				return err
			}
		}
	}
	return nil
}

// addListener adds a listener unless it's there already.
func (b *Backend) addListener(nqn string, p *target.Port, cur *liveSubsystem, bound map[string]bool) error {
	addr := addrOf(p)
	if cur != nil && slices.ContainsFunc(cur.Listeners, func(l liveListener) bool { return l.address() == addr }) {
		return nil
	}
	if !bound[addr] {
		if err := b.probe(addr); err != nil {
			return err
		}
		bound[addr] = true
	}
	return b.call("nvmf_subsystem_add_listener", map[string]any{"nqn": nqn, "listen_address": listenerOf(p)}, nil)
}

// applyListeners attaches the subsystems in scope to their ports. all is the whole state, for what they refer to.
func (b *Backend) applyListeners(scope, all *target.State, live map[string]*liveSubsystem, bound map[string]bool) error {
	for _, nqn := range sortedKeys(scope.Subsystems) {
		for _, id := range scope.Subsystems[nqn].Ports {
			if err := b.addListener(nqn, all.Ports[id], live[nqn], bound); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyDiscovery puts the discovery subsystem on every port with a subsystem attached. The kernel serves discovery
// on those ports, and hosts should see the same behaviour from either backend.
func (b *Backend) applyDiscovery(all *target.State, disc *liveSubsystem, bound map[string]bool) error {
	attached := attachedAddresses(all)
	for _, id := range sortedKeys(all.Ports) {
		if p := all.Ports[id]; attached[addrOf(p)] {
			if err := b.addListener(discoveryNQN, p, disc, bound); err != nil {
				return err
			}
		}
	}
	if disc != nil {
		for _, l := range disc.Listeners {
			if !attached[l.address()] {
				if err := b.removeListener(discoveryNQN, l); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// removeLeftovers deletes bdevs and keys nothing uses any more. A bdev stays
// claimed briefly after its namespace goes, which SPDK reports as busy.
func (b *Backend) removeLeftovers(next *target.State) error {
	wantBdevs := map[string]bool{}
	for nqn, sub := range next.Subsystems {
		for nsid, ns := range sub.Namespaces {
			if ns.Enabled {
				wantBdevs[bdevName(nqn, nsid, ns.DevicePath)] = true
			}
		}
	}
	have, err := b.bdevs()
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(have) {
		if strings.HasPrefix(name, namePrefix) && !wantBdevs[name] {
			if err := b.call("bdev_aio_delete", map[string]any{"name": name}, nil); err != nil {
				return err
			}
		}
	}
	return b.removeUnusedKeys(next)
}

func (b *Backend) removeUnusedKeys(next *target.State) error {
	wantKeys := map[string]bool{}
	for nqn, h := range next.Hosts {
		wantKeys[keyName(nqn, h.DHCHAPKey)] = true
		wantKeys[keyName(nqn, h.DHCHAPCtrlKey)] = true
	}
	var keys []liveKey
	if err := b.call("keyring_get_keys", nil, &keys); err != nil {
		return err
	}
	for _, k := range keys {
		if strings.HasPrefix(k.Name, namePrefix) && !wantKeys[k.Name] {
			if err := b.call("keyring_file_remove_key", map[string]any{"name": k.Name}, nil); err != nil {
				return err
			}
			os.Remove(filepath.Join(b.keyDir, k.Name))
		}
	}
	return nil
}

// Revalidate implements target.Backend. SPDK then notifies connected hosts.
func (b *Backend) Revalidate(nqn string, nsid uint32) error {
	sub, err := b.subsystem(nqn)
	if err != nil {
		return err
	}
	if sub == nil {
		return fmt.Errorf("subsystem %s is not live", nqn)
	}
	for _, ns := range sub.Namespaces {
		if ns.NSID == nsid {
			return b.call("bdev_aio_rescan", map[string]any{"name": ns.BdevName}, nil)
		}
	}
	return fmt.Errorf("namespace %d of %s is not live", nsid, nqn)
}

// LiveEnabled implements target.Backend.
func (b *Backend) LiveEnabled(nqn string, nsid uint32) bool {
	// Ask about this subsystem only: all of them is slow with thousands of namespaces.
	var list []*liveSubsystem
	if err := b.rpc.Call("nvmf_get_subsystems", map[string]any{"nqn": nqn}, &list); err != nil {
		return false
	}
	for _, sub := range list {
		if sub.NQN == nqn && slices.ContainsFunc(sub.Namespaces, func(l liveNamespace) bool { return l.NSID == nsid }) {
			return true
		}
	}
	return false
}

// LiveEnabledAll implements target.Backend, fetching the live state once for all the checks.
func (b *Backend) LiveEnabledAll() target.LiveChecker {
	live, err := b.subsystems()
	if err != nil {
		return func(string, uint32) bool { return false }
	}
	return func(nqn string, nsid uint32) bool {
		sub := live[nqn]
		return sub != nil && slices.ContainsFunc(sub.Namespaces, func(l liveNamespace) bool { return l.NSID == nsid })
	}
}

// probeListen finds problems SPDK would only report as "Invalid parameters".
func probeListen(address string) error {
	l, err := net.Listen("tcp", address)
	switch {
	case err == nil:
		return l.Close()
	case errors.Is(err, syscall.EADDRINUSE):
		return target.NewConflict("%s is already in use", address)
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return target.Invalid("%s is not an address on this host", address)
	}
	return err
}

func sortedKeys[K interface{ ~string | ~uint16 | ~uint32 }, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
