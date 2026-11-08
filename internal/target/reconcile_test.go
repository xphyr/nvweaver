package target_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"nvweaver/internal/target"
	"nvweaver/internal/target/targettest"
)

// counting counts what reaches the kernel.
type counting struct {
	target.ConfigFS
	ops []string
}

func (c *counting) Mkdir(p string) error {
	c.ops = append(c.ops, "mkdir "+p)
	return c.ConfigFS.Mkdir(p)
}
func (c *counting) Rmdir(p string) error {
	c.ops = append(c.ops, "rmdir "+p)
	return c.ConfigFS.Rmdir(p)
}
func (c *counting) Read(p string) (string, error) {
	c.ops = append(c.ops, "read "+p)
	return c.ConfigFS.Read(p)
}
func (c *counting) Write(p, v string) error {
	c.ops = append(c.ops, "write "+p)
	return c.ConfigFS.Write(p, v)
}
func (c *counting) Symlink(t, l string) error {
	c.ops = append(c.ops, "link "+l)
	return c.ConfigFS.Symlink(t, l)
}
func (c *counting) Unlink(p string) error {
	c.ops = append(c.ops, "unlink "+p)
	return c.ConfigFS.Unlink(p)
}

func sized(subsystems, namespaces int) *target.State {
	st := target.NewState()
	st.Ports[1] = &target.Port{ID: 1, Transport: target.TransportTCP, Address: "127.0.0.1", Service: 4420}
	st.Hosts["nqn.2026-11.test:h"] = &target.Host{NQN: "nqn.2026-11.test:h"}
	for s := range subsystems {
		nqn := fmt.Sprintf("nqn.2026-11.test:s%d", s)
		sub := &target.Subsystem{NQN: nqn, Serial: "s", Model: "m", AllowedHosts: []string{"nqn.2026-11.test:h"}, Ports: []uint16{1}, Namespaces: map[uint32]*target.Namespace{}}
		for n := 1; n <= namespaces; n++ {
			sub.Namespaces[uint32(n)] = &target.Namespace{NSID: uint32(n), DevicePath: fmt.Sprintf("/d/%d-%d", s, n), UUID: target.NewUUID(), NGUID: target.NewNGUID(), Enabled: true}
		}
		st.Subsystems[nqn] = sub
	}
	return st
}

func newTarget(t *testing.T, st *target.State) (*target.Target, *counting) {
	t.Helper()
	c := &counting{ConfigFS: targettest.New()}
	tgt := &target.Target{FS: c, Root: "/nvmet"}
	if err := tgt.Reconcile(target.NewState(), st); err != nil {
		t.Fatal(err)
	}
	c.ops = nil
	return tgt, c
}

// The point of incremental reconcile: one change costs the same however much else there is.
func TestAChangeCostsTheSameAtAnyScale(t *testing.T) {
	cost := func(subsystems, namespaces int) int {
		st := sized(subsystems, namespaces)
		tgt, c := newTarget(t, st)
		next := st.Clone()
		next.Subsystems["nqn.2026-11.test:s0"].Namespaces[999] = &target.Namespace{NSID: 999, DevicePath: "/d/new", UUID: target.NewUUID(), NGUID: target.NewNGUID(), Enabled: true}
		if err := tgt.Reconcile(st, next); err != nil {
			t.Fatal(err)
		}
		for _, op := range c.ops {
			if !strings.Contains(op, "/namespaces/999") {
				t.Errorf("touched something unrelated: %s", op)
			}
		}
		return len(c.ops)
	}
	small, large := cost(2, 2), cost(100, 50)
	if small == 0 {
		t.Fatal("nothing happened, so this checks nothing")
	}
	if small != large {
		t.Errorf("adding a namespace took %d operations with 4 namespaces, and %d with 5,000", small, large)
	}
}

func TestNoChangeTouchesNothing(t *testing.T) {
	st := sized(3, 3)
	tgt, c := newTarget(t, st)
	if err := tgt.Reconcile(st, st.Clone()); err != nil {
		t.Fatal(err)
	}
	if len(c.ops) != 0 {
		t.Errorf("an unchanged state touched the kernel: %v", c.ops)
	}
}

func TestChangingAPortRelinksWhatsOnIt(t *testing.T) {
	st := sized(3, 1)
	tgt, c := newTarget(t, st)
	next := st.Clone()
	next.Ports[1].Service = 4421
	if err := tgt.Reconcile(st, next); err != nil {
		t.Fatal(err)
	}
	links := 0
	for _, op := range c.ops {
		if strings.HasPrefix(op, "link /nvmet/ports/1/subsystems/") {
			links++
		}
	}
	if links != 3 {
		t.Errorf("relinked %d of 3 subsystems: %v", links, c.ops)
	}
}

func TestNewSubsystemIsLinkedAndOthersAreLeftAlone(t *testing.T) {
	st := sized(3, 1)
	tgt, c := newTarget(t, st)
	next := st.Clone()
	next.Subsystems["nqn.2026-11.test:new"] = &target.Subsystem{NQN: "nqn.2026-11.test:new", Serial: "s", Model: "m", Ports: []uint16{1}, Namespaces: map[uint32]*target.Namespace{}}
	if err := tgt.Reconcile(st, next); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, op := range c.ops {
		if op == "link /nvmet/ports/1/subsystems/nqn.2026-11.test:new" {
			found = true
		}
		if strings.Contains(op, ":s0") || strings.Contains(op, ":s1") || strings.Contains(op, ":s2") {
			t.Errorf("touched an unrelated subsystem: %s", op)
		}
	}
	if !found {
		t.Errorf("the new subsystem wasn't attached to its port: %v", c.ops)
	}
}

func TestRollbackPutsAFailedChangeBack(t *testing.T) {
	st := sized(2, 1)
	tgt, c := newTarget(t, st)
	next := st.Clone()
	next.Subsystems["nqn.2026-11.test:s0"].Namespaces[7] = &target.Namespace{NSID: 7, DevicePath: "/d/7", UUID: target.NewUUID(), NGUID: target.NewNGUID(), Enabled: true}
	delete(next.Subsystems, "nqn.2026-11.test:s1")
	if err := tgt.Reconcile(st, next); err != nil {
		t.Fatal(err)
	}
	if err := tgt.Reconcile(next, st); err != nil {
		t.Fatal(err)
	}
	if !tgt.LiveEnabled("nqn.2026-11.test:s1", 1) {
		t.Error("rollback should bring the removed subsystem's namespace back")
	}
	if got, _ := c.ConfigFS.List("/nvmet/subsystems/nqn.2026-11.test:s0/namespaces"); len(got) != 1 {
		t.Errorf("rollback should remove the added namespace: %v", got)
	}
	if got, _ := c.ConfigFS.List("/nvmet/ports/1/subsystems"); len(got) != 2 {
		t.Errorf("rollback should reattach the subsystem: %v", got)
	}
}

func TestLosingAccessDropsConnections(t *testing.T) {
	const a, b = "nqn.2026-11.test:a", "nqn.2026-11.test:b"
	const nqn, other = "nqn.2026-11.test:s0", "nqn.2026-11.test:s1"
	start := func() *target.State {
		st := sized(2, 1)
		st.Ports[2] = &target.Port{ID: 2, Transport: target.TransportTCP, Address: "127.0.0.2", Service: 4420}
		st.Hosts[a] = &target.Host{NQN: a}
		st.Hosts[b] = &target.Host{NQN: b}
		st.Subsystems[nqn].AllowedHosts = []string{a, b}
		st.Subsystems[nqn].Ports = []uint16{1, 2}
		return st
	}
	bounce := func(id int) []string {
		link := fmt.Sprintf("/nvmet/ports/%d/subsystems/%s", id, nqn)
		return []string{"unlink " + link, "link " + link}
	}
	for _, tc := range []struct {
		name   string
		before func(*target.Subsystem)
		change func(*target.State)
		want   []string // Links that should be dropped and made again.
	}{
		{name: "a host is removed", change: func(s *target.State) { s.Subsystems[nqn].AllowedHosts = []string{b} },
			want: append(bounce(1), bounce(2)...)},
		{name: "any host is no longer allowed", before: func(s *target.Subsystem) { s.AllowAnyHost, s.AllowedHosts = true, nil },
			change: func(s *target.State) {
				s.Subsystems[nqn].AllowAnyHost = false
				s.Subsystems[nqn].AllowedHosts = []string{a}
			}, want: append(bounce(1), bounce(2)...)},
		{name: "a host is added", change: func(s *target.State) {
			s.Hosts["nqn.2026-11.test:c"] = &target.Host{NQN: "nqn.2026-11.test:c"}
			s.Subsystems[nqn].AllowedHosts = []string{a, b, "nqn.2026-11.test:c"}
		}},
		{name: "any host becomes allowed", change: func(s *target.State) {
			s.Subsystems[nqn].AllowAnyHost = true
			s.Subsystems[nqn].AllowedHosts = nil
		}},
		{name: "a namespace is removed", change: func(s *target.State) { delete(s.Subsystems[nqn].Namespaces, 1) }},
		{name: "another subsystem loses a host", change: func(s *target.State) { s.Subsystems[other].AllowedHosts = nil },
			want: []string{"unlink /nvmet/ports/1/subsystems/" + other, "link /nvmet/ports/1/subsystems/" + other}},
		// A host connected with a key it no longer has must log in again, as if it had been removed.
		{name: "a host's key is set", change: func(s *target.State) { s.Hosts[a].DHCHAPKey = "DHHC-1:00:new:" },
			want: append(bounce(1), bounce(2)...)},
		{name: "a host's key changes", before: func(s *target.Subsystem) {}, change: func(s *target.State) {
			s.Hosts[a].DHCHAPKey, s.Hosts[a].DHCHAPCtrlKey = "DHHC-1:00:one:", "DHHC-1:00:ctrl:"
		}, want: append(bounce(1), bounce(2)...)},
		{name: "a host's hash changes, and not its key", change: func(s *target.State) { s.Hosts[a].DHCHAPHash = "hmac(sha512)" }},
		{name: "a host that isn't on it changes its key", change: func(s *target.State) { s.Hosts[a].DHCHAPKey = "DHHC-1:00:new:" },
			before: func(s *target.Subsystem) { s.AllowedHosts = []string{b} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := start()
			if tc.before != nil {
				tc.before(st.Subsystems[nqn])
			}
			tgt, c := newTarget(t, st)
			next := st.Clone()
			tc.change(next)
			c.ops = nil
			if err := tgt.Reconcile(st, next); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, op := range c.ops {
				if strings.Contains(op, "/ports/") {
					got = append(got, op)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("port links: %v, want %v", got, tc.want)
			}
			// However it got there, it must end up attached.
			for _, sub := range next.Subsystems {
				for _, id := range sub.Ports {
					link := fmt.Sprintf("/nvmet/ports/%d/subsystems/%s", id, sub.NQN)
					if !slices.Contains(c.ConfigFS.(*targettest.FS).Paths(), link) {
						t.Errorf("%s isn't attached", link)
					}
				}
			}
		})
	}
}

// The host has to be unable to reconnect by the time the connections drop, or it could simply come back.
func TestAHostIsRemovedBeforeConnectionsDrop(t *testing.T) {
	const a, b = "nqn.2026-11.test:a", "nqn.2026-11.test:b"
	const nqn = "nqn.2026-11.test:s0"
	st := sized(1, 1)
	st.Hosts[a] = &target.Host{NQN: a}
	st.Hosts[b] = &target.Host{NQN: b}
	st.Subsystems[nqn].AllowedHosts = []string{a, b}
	tgt, c := newTarget(t, st)
	next := st.Clone()
	next.Subsystems[nqn].AllowedHosts = []string{b}
	c.ops = nil
	if err := tgt.Reconcile(st, next); err != nil {
		t.Fatal(err)
	}
	host := slices.Index(c.ops, "unlink /nvmet/subsystems/"+nqn+"/allowed_hosts/"+a)
	port := slices.Index(c.ops, "unlink /nvmet/ports/1/subsystems/"+nqn)
	if host < 0 || port < 0 || host > port {
		t.Errorf("the host must go first: %v", c.ops)
	}
}

// Changing a port detaches what's on it, and a subsystem this service doesn't manage must be put back too, even when
// writing the new attributes fails.
func TestAPortChangeKeepsForeignSubsystems(t *testing.T) {
	const foreign = "/nvmet/subsystems/nqn.2026-11.test:foreign"
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprintf("writing fails: %v", fails), func(t *testing.T) {
			st := sized(1, 1)
			tgt, c := newTarget(t, st)
			fake := c.ConfigFS.(*targettest.FS)
			if err := fake.Mkdir(foreign); err != nil {
				t.Fatal(err)
			}
			if err := fake.Symlink(foreign, "/nvmet/ports/1/subsystems/nqn.2026-11.test:foreign"); err != nil {
				t.Fatal(err)
			}
			if fails {
				fake.Fail["/addr_traddr"] = errors.New("refused")
			}
			next := st.Clone()
			next.Ports[1].Address = "127.0.0.9"
			err := tgt.Reconcile(st, next)
			if (err != nil) != fails {
				t.Fatalf("reconcile: %v", err)
			}
			linked, _ := fake.List("/nvmet/ports/1/subsystems")
			if !slices.Contains(linked, "nqn.2026-11.test:foreign") {
				t.Errorf("the foreign subsystem was left detached: %v", linked)
			}
			if !fails && !slices.Contains(linked, "nqn.2026-11.test:s0") {
				t.Errorf("the managed one: %v", linked)
			}
		})
	}
}
