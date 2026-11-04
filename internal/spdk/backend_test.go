package spdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"nvweaver/internal/target"
)

// fake is an SPDK that enforces the rules the backend must respect: a namespace
// needs its bdev, and a bdev can't be deleted while a namespace uses it.
type fake struct {
	subs  map[string]*liveSubsystem
	bdevs map[string]bool
	keys  map[string]bool
	calls []string
	fail  map[string]error
}

func newFake() *fake {
	return &fake{
		subs:  map[string]*liveSubsystem{discoveryNQN: {NQN: discoveryNQN, AllowAnyHost: true}},
		bdevs: map[string]bool{},
		keys:  map[string]bool{},
		fail:  map[string]error{},
	}
}

func (f *fake) Call(method string, params, result any) error {
	if !strings.Contains(method, "_get_") {
		f.calls = append(f.calls, method)
	}
	if err := f.fail[method]; err != nil {
		return err
	}
	raw, _ := json.Marshal(params)
	var p struct {
		NQN           string       `json:"nqn"`
		Name          string       `json:"name"`
		Host          string       `json:"host"`
		NSID          uint32       `json:"nsid"`
		AllowAny      bool         `json:"allow_any_host"`
		Listen        liveListener `json:"listen_address"`
		Key           string       `json:"dhchap_key"`
		CtrlKey       string       `json:"dhchap_ctrlr_key"`
		Namespace     liveNamespace
		RawNamespaces json.RawMessage `json:"namespace"`
	}
	if params != nil {
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if len(p.RawNamespaces) > 0 {
			_ = json.Unmarshal(p.RawNamespaces, &p.Namespace)
		}
	}
	sub := f.subs[p.NQN]
	switch method {
	case "nvmf_get_subsystems":
		list := []*liveSubsystem{}
		for _, s := range f.subs {
			list = append(list, s)
		}
		b, _ := json.Marshal(list)
		return json.Unmarshal(b, result)
	case "bdev_get_bdevs":
		list := []liveBdev{}
		for n := range f.bdevs {
			list = append(list, liveBdev{n})
		}
		b, _ := json.Marshal(list)
		return json.Unmarshal(b, result)
	case "keyring_get_keys":
		list := []liveKey{}
		for n := range f.keys {
			list = append(list, liveKey{n})
		}
		b, _ := json.Marshal(list)
		return json.Unmarshal(b, result)
	case "keyring_file_add_key":
		f.keys[p.Name] = true
	case "keyring_file_remove_key":
		delete(f.keys, p.Name)
	case "bdev_aio_create":
		f.bdevs[p.Name] = true
	case "bdev_aio_delete":
		for _, s := range f.subs {
			for _, ns := range s.Namespaces {
				if ns.BdevName == p.Name {
					return &RPCError{Code: -1, Message: "bdev is claimed"}
				}
			}
		}
		delete(f.bdevs, p.Name)
	case "nvmf_create_subsystem":
		f.subs[p.NQN] = &liveSubsystem{NQN: p.NQN, AllowAnyHost: p.AllowAny}
	case "nvmf_delete_subsystem":
		delete(f.subs, p.NQN)
	case "nvmf_subsystem_allow_any_host":
		sub.AllowAnyHost = p.AllowAny
	case "nvmf_subsystem_add_host":
		sub.Hosts = append(sub.Hosts, liveHost{p.Host, p.Key, p.CtrlKey})
	case "nvmf_subsystem_set_keys":
		for i := range sub.Hosts {
			if sub.Hosts[i].NQN == p.Host {
				sub.Hosts[i].Key, sub.Hosts[i].CtrlKey = p.Key, p.CtrlKey
			}
		}
	case "nvmf_subsystem_remove_host":
		sub.Hosts = slices.DeleteFunc(sub.Hosts, func(h liveHost) bool { return h.NQN == p.Host })
	case "nvmf_subsystem_add_ns":
		if !f.bdevs[p.Namespace.BdevName] {
			return &RPCError{Code: invalidParams, Message: "no such bdev"}
		}
		sub.Namespaces = append(sub.Namespaces, p.Namespace)
	case "nvmf_subsystem_remove_ns":
		sub.Namespaces = slices.DeleteFunc(sub.Namespaces, func(n liveNamespace) bool { return n.NSID == p.NSID })
	case "nvmf_subsystem_add_listener":
		sub.Listeners = append(sub.Listeners, p.Listen)
	case "nvmf_subsystem_remove_listener":
		sub.Listeners = slices.DeleteFunc(sub.Listeners, func(l liveListener) bool { return l.address() == p.Listen.address() })
	default:
		return fmt.Errorf("fake has no %s", method)
	}
	return nil
}

const (
	subA  = "nqn.2026-11.net.example:a"
	hostA = "nqn.2026-11.net.example:h"
)

func newBackend() (*Backend, *fake) {
	f := newFake()
	return &Backend{rpc: f, keyDir: "", probe: func(string) error { return nil }}, f
}

func state() *target.State {
	s := target.NewState()
	s.Ports[1] = &target.Port{ID: 1, Address: "127.0.0.1", Service: 4420}
	s.Hosts[hostA] = &target.Host{NQN: hostA}
	s.Subsystems[subA] = &target.Subsystem{
		NQN: subA, Serial: "s", Model: "m",
		AllowedHosts: []string{hostA},
		Ports:        []uint16{1},
		Namespaces: map[uint32]*target.Namespace{
			1: {NSID: 1, DevicePath: "/dev/x", UUID: "AAAA-BB", NGUID: "cc-dd", Enabled: true},
		},
	}
	return s
}

func apply(t *testing.T, b *Backend, s *target.State) {
	t.Helper()
	if err := b.Reconcile(nil, s); err != nil {
		t.Fatal(err)
	}
}

func TestListenersComeLast(t *testing.T) {
	b, f := newBackend()
	apply(t, b, state())
	first := slices.Index(f.calls, "nvmf_subsystem_add_listener")
	if first < 0 {
		t.Fatalf("no listener added: %v", f.calls)
	}
	for _, m := range []string{"bdev_aio_create", "nvmf_create_subsystem", "nvmf_subsystem_add_host", "nvmf_subsystem_add_ns"} {
		if i := slices.Index(f.calls, m); i < 0 || i > first {
			t.Errorf("%s must come before the first listener: %v", m, f.calls)
		}
	}
}

func TestRepeatReconcileChangesNothing(t *testing.T) {
	b, f := newBackend()
	apply(t, b, state())
	// SPDK reports IDs in its own format, which must still compare equal.
	f.subs[subA].Namespaces[0].UUID = "aaaa-bb"
	f.subs[subA].Namespaces[0].NGUID = "CCDD"
	f.calls = nil
	apply(t, b, state())
	if len(f.calls) != 0 {
		t.Errorf("second reconcile made changes: %v", f.calls)
	}
}

func TestDiscoveryFollowsAttachedPorts(t *testing.T) {
	b, f := newBackend()
	s := state()
	s.Ports[2] = &target.Port{ID: 2, Address: "127.0.0.1", Service: 4421}
	apply(t, b, s)
	addrs := func() (out []string) {
		for _, l := range f.subs[discoveryNQN].Listeners {
			out = append(out, l.TrSvcID)
		}
		return out
	}
	if got := addrs(); !slices.Equal(got, []string{"4420"}) {
		t.Errorf("discovery should be only on the attached port, got %v", got)
	}
	s.Subsystems[subA].Ports = []uint16{2}
	apply(t, b, s)
	if got := addrs(); !slices.Equal(got, []string{"4421"}) {
		t.Errorf("discovery should follow the attachment, got %v", got)
	}
}

func TestDeleteLeavesNothing(t *testing.T) {
	b, f := newBackend()
	s := state()
	s.Hosts[hostA].DHCHAPKey = "DHHC-1:00:abc:"
	b.keyDir = t.TempDir()
	apply(t, b, s)
	if len(f.keys) != 1 {
		t.Fatalf("key not created: %v", f.keys)
	}
	apply(t, b, target.NewState())
	if len(f.subs) != 1 || len(f.bdevs) != 0 || len(f.keys) != 0 {
		t.Errorf("left behind: subs=%v bdevs=%v keys=%v", f.subs, f.bdevs, f.keys)
	}
	if n := len(f.subs[discoveryNQN].Listeners); n != 0 {
		t.Errorf("%d discovery listeners left", n)
	}
}

func TestDisablingRemovesNamespaceButNotSubsystem(t *testing.T) {
	b, f := newBackend()
	s := state()
	apply(t, b, s)
	s.Subsystems[subA].Namespaces[1].Enabled = false
	apply(t, b, s)
	if f.subs[subA] == nil || len(f.subs[subA].Namespaces) != 0 || len(f.bdevs) != 0 {
		t.Errorf("subs=%+v bdevs=%v", f.subs[subA], f.bdevs)
	}
	s.Subsystems[subA].Namespaces[1].Enabled = true
	apply(t, b, s)
	if len(f.subs[subA].Namespaces) != 1 {
		t.Error("namespace not restored")
	}
}

func TestChangingPathReplacesBdev(t *testing.T) {
	b, f := newBackend()
	s := state()
	apply(t, b, s)
	old := f.subs[subA].Namespaces[0].BdevName
	s.Subsystems[subA].Namespaces[1].DevicePath = "/dev/y"
	apply(t, b, s)
	if len(f.bdevs) != 1 || f.bdevs[old] {
		t.Errorf("old bdev should be gone: %v", f.bdevs)
	}
}

func TestKeyChangeDoesNotDropHost(t *testing.T) {
	b, f := newBackend()
	b.keyDir = t.TempDir()
	s := state()
	s.Hosts[hostA].DHCHAPKey = "DHHC-1:00:one:"
	apply(t, b, s)
	f.calls = nil
	s.Hosts[hostA].DHCHAPKey = "DHHC-1:00:two:"
	apply(t, b, s)
	if !slices.Contains(f.calls, "nvmf_subsystem_set_keys") || slices.Contains(f.calls, "nvmf_subsystem_remove_host") {
		t.Errorf("expected keys to change in place: %v", f.calls)
	}
	if got := f.subs[subA].Hosts[0].Key; got != keyName(hostA, "DHHC-1:00:two:") {
		t.Errorf("key = %q", got)
	}
}

func TestAddressInUseIsAConflictAndCanRollBack(t *testing.T) {
	b, f := newBackend()
	prev := state()
	apply(t, b, prev)
	next := prev.Clone()
	next.Ports[2] = &target.Port{ID: 2, Address: "127.0.0.1", Service: 4421}
	next.Subsystems[subA].Ports = []uint16{1, 2}
	b.probe = func(a string) error {
		if strings.HasSuffix(a, ":4421") {
			return target.NewConflict("%s is already in use", a)
		}
		return nil
	}
	var ce *target.ConflictError
	if err := b.Reconcile(prev, next); !errors.As(err, &ce) {
		t.Fatalf("want conflict, got %v", err)
	}
	apply(t, b, prev)
	if n := len(f.subs[subA].Listeners); n != 1 {
		t.Errorf("%d listeners after rollback", n)
	}
}

func TestSharedAddressIsNotProbed(t *testing.T) {
	b, _ := newBackend()
	s := state()
	s.Subsystems["nqn.2026-11.net.example:b"] = &target.Subsystem{
		NQN: "nqn.2026-11.net.example:b", Serial: "s", Model: "m", Ports: []uint16{1},
		Namespaces: map[uint32]*target.Namespace{},
	}
	probes := 0
	b.probe = func(string) error { probes++; return nil }
	apply(t, b, s)
	if probes != 1 {
		t.Errorf("probed %d times; SPDK itself would hold the address after the first", probes)
	}
}

func TestInvalidParamsBecomeValidationErrors(t *testing.T) {
	b, f := newBackend()
	f.fail["nvmf_create_subsystem"] = &RPCError{Code: invalidParams, Message: "Invalid parameters"}
	var ve *target.ValidationError
	if err := b.Reconcile(nil, state()); !errors.As(err, &ve) {
		t.Errorf("want validation error, got %v", err)
	}
}

func TestCheckRejectsWhatSPDKCannotDo(t *testing.T) {
	b, _ := newBackend()
	if err := b.Check(state()); err != nil {
		t.Errorf("plain state refused: %v", err)
	}
	size := 8192
	for name, mutate := range map[string]func(*target.State){
		"passthrough": func(s *target.State) { s.Subsystems[subA].Passthru = "/dev/nvme0" },
		"inline data": func(s *target.State) { s.Ports[1].InlineDataSize = &size },
		"buffered":    func(s *target.State) { s.Subsystems[subA].Namespaces[1].BufferedIO = true },
		"wildcard v4": func(s *target.State) { s.Ports[1].Address = "0.0.0.0" },
		"wildcard v6": func(s *target.State) { s.Ports[1].Address = "::" },
	} {
		s := state()
		mutate(s)
		var ve *target.ValidationError
		if err := b.Check(s); !errors.As(err, &ve) {
			t.Errorf("%s: want validation error, got %v", name, err)
		}
	}
}

func TestParseLastCPU(t *testing.T) {
	for in, want := range map[string]int{"0-7": 7, "\t0-3,8": 8, "5": 5, "0,2-4": 4} {
		if got, err := parseLastCPU(in); err != nil || got != want {
			t.Errorf("parseLastCPU(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}

func TestClampBlockSize(t *testing.T) {
	for in, want := range map[int64]uint32{0: 512, 512: 512, 1024: 1024, 4096: 4096, 65536: 4096, 3000: 2048} {
		if got := clampBlockSize(in); got != want {
			t.Errorf("clampBlockSize(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestErrnoCodesBecomeClientErrors(t *testing.T) {
	b, f := newBackend()
	f.fail["bdev_aio_create"] = &RPCError{Code: -22, Message: "Invalid argument"}
	var ve *target.ValidationError
	if err := b.Reconcile(nil, state()); !errors.As(err, &ve) {
		t.Errorf("want validation error, got %v", err)
	}
	f.fail["bdev_aio_create"] = &RPCError{Code: -17, Message: "File exists"}
	var ce *target.ConflictError
	if err := b.Reconcile(nil, state()); !errors.As(err, &ce) {
		t.Errorf("want conflict, got %v", err)
	}
}
