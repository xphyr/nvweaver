package target_test

import (
	"fmt"
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
