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
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	rpc    Caller
	keyDir string
	proc   *process
	// probe reports why an address could not be listened on, if it couldn't.
	probe func(address string) error
	done  chan struct{}
}

// Name implements target.Backend.
func (b *Backend) Name() string { return "spdk" }

// Capabilities implements target.Backend.
func (b *Backend) Capabilities() target.Capabilities {
	return target.Capabilities{}
}

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

// Reconcile implements target.Backend. It ignores prev: live state is the truth
// here, which also makes rollback just another reconcile.
func (b *Backend) Reconcile(_, next *target.State) error {
	if err := b.removeStale(next); err != nil {
		return err
	}
	if err := b.createKeys(next); err != nil {
		return err
	}
	if err := b.createBdevs(next); err != nil {
		return err
	}
	if err := b.applySubsystems(next); err != nil {
		return err
	}
	// Listeners go last, so that a host can't find a half-built subsystem.
	if err := b.applyListeners(next); err != nil {
		return err
	}
	return b.removeLeftovers(next)
}

func wantedNamespace(sub *target.Subsystem, ns liveNamespace) bool {
	want := sub.Namespaces[ns.NSID]
	return want != nil && want.Enabled &&
		ns.BdevName == bdevName(sub.NQN, want.NSID, want.DevicePath) &&
		normID(ns.UUID) == normID(want.UUID) && normID(ns.NGUID) == normID(want.NGUID)
}

func (b *Backend) removeStale(next *target.State) error {
	live, err := b.subsystems()
	if err != nil {
		return err
	}
	for _, nqn := range sortedKeys(live) {
		if nqn == discoveryNQN {
			continue
		}
		cur := live[nqn]
		want := next.Subsystems[nqn]
		if want == nil {
			if err := b.call("nvmf_delete_subsystem", map[string]any{"nqn": nqn}, nil); err != nil {
				return err
			}
			continue
		}
		wantAddrs := map[string]bool{}
		for _, id := range want.Ports {
			wantAddrs[addrOf(next.Ports[id])] = true
		}
		for _, l := range cur.Listeners {
			if !wantAddrs[l.address()] {
				if err := b.removeListener(nqn, l); err != nil {
					return err
				}
			}
		}
		for _, h := range cur.Hosts {
			if !slices.Contains(want.AllowedHosts, h.NQN) {
				params := map[string]any{"nqn": nqn, "host": h.NQN}
				if err := b.call("nvmf_subsystem_remove_host", params, nil); err != nil {
					return err
				}
			}
		}
		for _, ns := range cur.Namespaces {
			if !wantedNamespace(want, ns) {
				params := map[string]any{"nqn": nqn, "nsid": ns.NSID}
				if err := b.call("nvmf_subsystem_remove_ns", params, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
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
func (b *Backend) createBdevs(next *target.State) error {
	have, err := b.bdevs()
	if err != nil {
		return err
	}
	for _, nqn := range sortedKeys(next.Subsystems) {
		sub := next.Subsystems[nqn]
		for _, nsid := range sortedKeys(sub.Namespaces) {
			ns := sub.Namespaces[nsid]
			name := bdevName(nqn, nsid, ns.DevicePath)
			if !ns.Enabled || have[name] {
				continue
			}
			params := map[string]any{"name": name, "filename": ns.DevicePath}
			if size := fileBlockSize(ns.DevicePath); size != 0 {
				params["block_size"] = size
			}
			if err := b.call("bdev_aio_create", params, nil); err != nil {
				return err
			}
			have[name] = true
		}
	}
	return nil
}

func (b *Backend) applySubsystems(next *target.State) error {
	live, err := b.subsystems()
	if err != nil {
		return err
	}
	for _, nqn := range sortedKeys(next.Subsystems) {
		want := next.Subsystems[nqn]
		cur := live[nqn]
		if cur == nil {
			err := b.call("nvmf_create_subsystem", map[string]any{
				"nqn":            nqn,
				"serial_number":  want.Serial,
				"model_number":   want.Model,
				"allow_any_host": want.AllowAnyHost,
				"max_namespaces": target.MaxNSID,
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
		if err := b.applyHosts(next, want, cur); err != nil {
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
			if err := b.call("nvmf_subsystem_set_keys", hostParams(want.NQN, h), nil); err != nil {
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
			},
		}, nil)
		if err != nil {
			return err
		}
	}
	return nil
}

// applyListeners adds listeners, including on the discovery subsystem: the
// kernel serves discovery on any port with a subsystem attached, and hosts
// should see the same behaviour from either backend.
func (b *Backend) applyListeners(next *target.State) error {
	live, err := b.subsystems()
	if err != nil {
		return err
	}
	bound := map[string]bool{}
	for _, sub := range live {
		for _, l := range sub.Listeners {
			bound[l.address()] = true
		}
	}
	attached := map[string]bool{}
	add := func(nqn string, p *target.Port) error {
		addr := addrOf(p)
		cur := live[nqn]
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
	for _, nqn := range sortedKeys(next.Subsystems) {
		for _, id := range next.Subsystems[nqn].Ports {
			p := next.Ports[id]
			attached[addrOf(p)] = true
			if err := add(nqn, p); err != nil {
				return err
			}
		}
	}
	for _, id := range sortedKeys(next.Ports) {
		if p := next.Ports[id]; attached[addrOf(p)] {
			if err := add(discoveryNQN, p); err != nil {
				return err
			}
		}
	}
	if disc := live[discoveryNQN]; disc != nil {
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
	live, err := b.subsystems()
	if err != nil {
		return err
	}
	sub := live[nqn]
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
	live, err := b.subsystems()
	if err != nil || live[nqn] == nil {
		return false
	}
	return slices.ContainsFunc(live[nqn].Namespaces, func(l liveNamespace) bool { return l.NSID == nsid })
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
