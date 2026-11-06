package spdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"nvweaver/internal/target"
	"os"
	"syscall"
	"unsafe"
)

// fake is an SPDK that enforces the rules the backend must respect: a namespace
// needs its bdev, and a bdev can't be deleted while a namespace uses it.
type fake struct {
	subs  map[string]*liveSubsystem
	bdevs map[string]bool
	keys  map[string]bool
	calls []string
	// rpcs counts every call, reads too: each takes about 8 ms on the real target, even one that changes nothing.
	rpcs int
	fail map[string]error
	// listed counts nvmf_get_subsystems calls, and listedAll those not naming a subsystem.
	listed, listedAll int
	listedBdevs       int
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
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
	f.rpcs++
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
		f.listed++
		if p.NQN == "" {
			f.listedAll++
		}
		list := []*liveSubsystem{}
		for _, s := range f.subs {
			if p.NQN == "" || s.NQN == p.NQN {
				list = append(list, s)
			}
		}
		b, _ := json.Marshal(list)
		return json.Unmarshal(b, result)
	case "bdev_get_bdevs":
		f.listedBdevs += btoi(p.Name == "")
		if p.Name != "" && !f.bdevs[p.Name] {
			return &RPCError{Code: -int(syscall.ENODEV), Message: "No such device"}
		}
		list := []liveBdev{}
		for n := range f.bdevs {
			if p.Name == "" || p.Name == n {
				list = append(list, liveBdev{n})
			}
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
	case "bdev_aio_rescan":
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
	s.Normalize()
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

func TestLiveChecksDontFetchEverything(t *testing.T) {
	b, f := newBackend()
	apply(t, b, state())

	f.listed, f.listedAll = 0, 0
	if !b.LiveEnabled(subA, 1) {
		t.Error("namespace 1 should be live")
	}
	if b.LiveEnabled(subA, 2) {
		t.Error("namespace 2 doesn't exist")
	}
	if b.LiveEnabled("nqn.2026-11.net.example:none", 1) {
		t.Error("that subsystem doesn't exist")
	}
	if f.listedAll != 0 {
		t.Errorf("a single check fetched every subsystem %d times", f.listedAll)
	}

	f.listed, f.listedAll = 0, 0
	live := b.LiveEnabledAll()
	for range 100 {
		if !live(subA, 1) {
			t.Error("namespace 1 should be live")
		}
	}
	if live(subA, 2) || live("nqn.2026-11.net.example:none", 1) {
		t.Error("reported something that doesn't exist as live")
	}
	if f.listed != 1 {
		t.Errorf("fetched the live state %d times for one snapshot, want 1", f.listed)
	}

	f.fail["nvmf_get_subsystems"] = &RPCError{Code: -1, Message: "down"}
	if b.LiveEnabled(subA, 1) || b.LiveEnabledAll()(subA, 1) {
		t.Error("nothing should be reported live when SPDK can't be asked")
	}
}

func TestMakeWritesDurable(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "volume")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var flags int64
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocGetFlags, uintptr(unsafe.Pointer(&flags))); errno != 0 {
		t.Skip("this filesystem has no attributes; the end-to-end suites cover it")
	}
	if err := makeWritesDurable(f.Name()); err != nil {
		t.Skipf("this filesystem won't set it, which is refused: %v", err)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocGetFlags, uintptr(unsafe.Pointer(&flags))); errno != 0 {
		t.Fatal(errno)
	}
	if flags&fsSyncFl == 0 {
		t.Errorf("flags = %#x", flags)
	}
	if err := makeWritesDurable(f.Name()); err != nil {
		t.Errorf("setting it twice: %v", err)
	}
	if err := makeWritesDurable("/nonexistent"); err != nil {
		t.Errorf("a missing path is for SPDK to report: %v", err)
	}
}

// A change looks only at what changed, which is what keeps it fast with thousands of namespaces.
func TestAChangeDoesntFetchEverything(t *testing.T) {
	b, f := newBackend()
	cur := state()
	apply(t, b, cur)
	f.listedAll, f.listedBdevs, f.calls = 0, 0, nil

	next := cur.Clone()
	next.Subsystems[subA].Namespaces[9] = &target.Namespace{NSID: 9, DevicePath: "/d/nine", UUID: target.NewUUID(), NGUID: target.NewNGUID(), Enabled: true}
	if err := b.Reconcile(cur, next); err != nil {
		t.Fatal(err)
	}
	if f.listedAll != 0 || f.listedBdevs != 0 {
		t.Errorf("fetched every subsystem %d times, and every bdev %d times", f.listedAll, f.listedBdevs)
	}
	if !slices.ContainsFunc(f.subs[subA].Namespaces, func(l liveNamespace) bool { return l.NSID == 9 }) {
		t.Errorf("the namespace wasn't added: %+v", f.subs[subA])
	}
	want := []string{"bdev_aio_create", "nvmf_subsystem_add_ns"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

// Every RPC takes about 8 ms, so this is what a change costs. Counting only writes missed a change costing 20
// reads, one for each namespace already in the subsystem.
func TestAChangeCostsTheSameHoweverBigItsSubsystem(t *testing.T) {
	cost := func(namespaces int) (add, remove, grow int) {
		b, f := newBackend()
		cur := state()
		apply(t, b, cur)
		for i := range namespaces {
			nsid := uint32(100 + i)
			cur.Subsystems[subA].Namespaces[nsid] = &target.Namespace{NSID: nsid, DevicePath: fmt.Sprintf("/d/n%d", i), UUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", nsid), NGUID: fmt.Sprintf("%032d", nsid), Enabled: true}
		}
		next := cur.Clone()
		apply(t, b, next)
		cur = next
		next = cur.Clone()
		next.Subsystems[subA].Namespaces[9] = &target.Namespace{NSID: 9, DevicePath: "/d/nine", UUID: target.NewUUID(), NGUID: target.NewNGUID(), Enabled: true}
		f.rpcs = 0
		if err := b.Reconcile(cur, next); err != nil {
			t.Fatal(err)
		}
		add = f.rpcs
		cur = next
		next = cur.Clone()
		delete(next.Subsystems[subA].Namespaces, 9)
		f.rpcs = 0
		if err := b.Reconcile(cur, next); err != nil {
			t.Fatal(err)
		}
		remove = f.rpcs
		f.rpcs = 0
		if err := b.Revalidate(subA, 100); err != nil {
			t.Fatal(err)
		}
		return add, remove, f.rpcs
	}
	add, remove, grow := cost(1)
	bigAdd, bigRemove, bigGrow := cost(200)
	if add != bigAdd || remove != bigRemove || grow != bigGrow {
		t.Errorf("RPCs for add, remove and grow: %d, %d, %d with 1 namespace, but %d, %d, %d with 200", add, remove, grow, bigAdd, bigRemove, bigGrow)
	}
	// Fetch the subsystem, check for a bdev left by a failure, create it, and add the namespace.
	if add != 4 {
		t.Errorf("adding a namespace took %d RPCs, want 4", add)
	}
}

// Whatever a series of changes does, it must match what a full reconcile would.
func TestChangesMatchAFullReconcile(t *testing.T) {
	type step func(*target.State)
	addNS := func(nsid uint32) step {
		return func(s *target.State) {
			s.Subsystems[subA].Namespaces[nsid] = &target.Namespace{NSID: nsid, DevicePath: fmt.Sprintf("/d/%d", nsid), UUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", nsid), NGUID: fmt.Sprintf("%032d", nsid), Enabled: true}
		}
	}
	steps := []step{
		addNS(5),
		addNS(6),
		func(s *target.State) { delete(s.Subsystems[subA].Namespaces, 5) },
		func(s *target.State) { s.Subsystems[subA].Namespaces[6].Enabled = false },
		func(s *target.State) { s.Subsystems[subA].Namespaces[6].Enabled = true },
		func(s *target.State) { s.Subsystems[subA].Namespaces[6].DevicePath = "/d/moved" },
		func(s *target.State) {
			s.Hosts["nqn.2026-11.test:new"] = &target.Host{NQN: "nqn.2026-11.test:new", DHCHAPKey: "DHHC-1:00:new"}
			s.Subsystems[subA].AllowedHosts = append(s.Subsystems[subA].AllowedHosts, "nqn.2026-11.test:new")
		},
		func(s *target.State) { s.Hosts["nqn.2026-11.test:new"].DHCHAPKey = "DHHC-1:00:changed" },
		func(s *target.State) {
			s.Subsystems["nqn.2026-11.test:other"] = &target.Subsystem{NQN: "nqn.2026-11.test:other", Serial: "s", Model: "m", Ports: []uint16{1}, Namespaces: map[uint32]*target.Namespace{}}
		},
		func(s *target.State) {
			s.Subsystems[subA].AllowedHosts = slices.DeleteFunc(s.Subsystems[subA].AllowedHosts, func(h string) bool { return h == "nqn.2026-11.test:new" })
			delete(s.Hosts, "nqn.2026-11.test:new")
		},
		func(s *target.State) { delete(s.Subsystems, "nqn.2026-11.test:other") },
		func(s *target.State) {
			s.Subsystems[subA].AllowAnyHost, s.Subsystems[subA].AllowedHosts = true, []string{}
		},
		func(s *target.State) { delete(s.Subsystems, subA) },
	}
	b, f := newBackend()
	cur := state()
	apply(t, b, cur)
	full, ff := newBackend()
	for i, change := range steps {
		next := cur.Clone()
		change(next)
		if err := b.Reconcile(cur, next); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		// The same state from scratch, by a full reconcile.
		full.needsFull = true
		if err := full.Reconcile(next, next); err != nil {
			t.Fatalf("step %d, full: %v", i, err)
		}
		a, _ := json.Marshal(sortedLive(f.subs))
		c, _ := json.Marshal(sortedLive(ff.subs))
		if string(a) != string(c) {
			t.Errorf("step %d: incremental and full differ:\n%s\n%s", i, a, c)
		}
		if fmt.Sprint(sortedKeys(f.bdevs)) != fmt.Sprint(sortedKeys(ff.bdevs)) {
			t.Errorf("step %d: bdevs differ: %v, %v", i, sortedKeys(f.bdevs), sortedKeys(ff.bdevs))
		}
		if fmt.Sprint(sortedKeys(f.keys)) != fmt.Sprint(sortedKeys(ff.keys)) {
			t.Errorf("step %d: keys differ: %v, %v", i, sortedKeys(f.keys), sortedKeys(ff.keys))
		}
		cur = next
	}
}

func sortedLive(subs map[string]*liveSubsystem) []liveSubsystem {
	var out []liveSubsystem
	for _, nqn := range sortedKeys(subs) {
		s := *subs[nqn]
		s.Namespaces = slices.Clone(s.Namespaces)
		slices.SortFunc(s.Namespaces, func(a, b liveNamespace) int { return int(a.NSID) - int(b.NSID) })
		s.Hosts = slices.Clone(s.Hosts)
		slices.SortFunc(s.Hosts, func(a, b liveHost) int { return strings.Compare(a.NQN, b.NQN) })
		out = append(out, s)
	}
	return out
}

// After a failure, what's live isn't known, so the next reconcile looks at everything.
func TestAFailedChangeIsFollowedByAFullReconcile(t *testing.T) {
	b, f := newBackend()
	cur := state()
	apply(t, b, cur)
	next := cur.Clone()
	next.Subsystems[subA].Namespaces[9] = &target.Namespace{NSID: 9, DevicePath: "/d/nine", UUID: target.NewUUID(), NGUID: target.NewNGUID(), Enabled: true}
	f.fail["nvmf_subsystem_add_ns"] = &RPCError{Code: -1, Message: "broken"}
	if err := b.Reconcile(cur, next); err == nil {
		t.Fatal("the failure should be reported")
	}
	if len(f.bdevs) == len(cur.Subsystems[subA].Namespaces) {
		t.Fatal("this test needs the bdev to have been created before the failure")
	}
	delete(f.fail, "nvmf_subsystem_add_ns")
	f.listedAll = 0
	// As the manager's rollback does.
	if err := b.Reconcile(next, cur); err != nil {
		t.Fatal(err)
	}
	if f.listedAll == 0 {
		t.Error("the rollback should look at everything")
	}
	for name := range f.bdevs {
		if name == bdevName(subA, 9, "/d/nine") {
			t.Error("the rollback should remove what the failed change left")
		}
	}
	f.listedAll = 0
	again := cur.Clone()
	again.Subsystems[subA].Namespaces[8] = &target.Namespace{NSID: 8, DevicePath: "/d/eight", UUID: target.NewUUID(), NGUID: target.NewNGUID(), Enabled: true}
	if err := b.Reconcile(cur, again); err != nil {
		t.Fatal(err)
	}
	if f.listedAll != 0 {
		t.Error("once a full reconcile has worked, changes should be incremental again")
	}
}

func TestTooManySubsystemsAreRefusedClearly(t *testing.T) {
	b, _ := newBackend()
	s := target.NewState()
	for i := range maxSubsystems {
		nqn := fmt.Sprintf("nqn.2026-11.test:s%d", i)
		s.Subsystems[nqn] = &target.Subsystem{NQN: nqn}
	}
	if err := b.Check(s); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	s.Subsystems["nqn.2026-11.test:one-more"] = &target.Subsystem{NQN: "nqn.2026-11.test:one-more"}
	err := b.Check(s)
	var ve *target.ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "8192") {
		t.Errorf("past the limit: %v", err)
	}
}

func TestFsAioMaxNrIsCheckedClearly(t *testing.T) {
	b, _ := newBackend()
	s := state()
	namespaces := 0
	for _, sub := range s.Subsystems {
		for _, ns := range sub.Namespaces {
			if ns.Enabled {
				namespaces++
			}
		}
	}
	if namespaces == 0 {
		t.Fatal("the test state needs a namespace")
	}
	const reactors = 2
	b.reactors = reactors
	need := namespaces * aioPerNamespace * reactors
	b.aioLimit = func() (int, error) { return need, nil }
	if err := b.Check(s); err != nil {
		t.Errorf("exactly enough: %v", err)
	}
	b.aioLimit = func() (int, error) { return need - 1, nil }
	err := b.Check(s)
	var ve *target.ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "sysctl -w fs.aio-max-nr") {
		t.Errorf("one short: %v", err)
	}
	b.aioLimit = func() (int, error) { return 0, errors.New("no /proc") }
	if err := b.Check(s); err != nil {
		t.Errorf("if it can't be read, that's not a reason to refuse: %v", err)
	}
}
