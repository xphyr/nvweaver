package target_test

import (
	"slices"
	"testing"

	"nvweaver/internal/target"
	"nvweaver/internal/target/targettest"
)

func attr(t *testing.T, fs *targettest.FS, p string) string {
	t.Helper()
	v, _ := fs.Attr(p)
	return v
}

// Pausing a namespace moves it into an inaccessible ANA group on every port, without disabling it, and resuming moves
// it back and removes the group. Decision 56.
func TestPausingNamespacesWithANA(t *testing.T) {
	st := sized(1, 2)
	st.Ports[2] = &target.Port{ID: 2, Transport: target.TransportTCP, Address: "127.0.0.2", Service: 4420}
	fs := targettest.New()
	tgt := &target.Target{FS: fs, Root: "/nvmet"}
	if err := tgt.Reconcile(target.NewState(), st); err != nil {
		t.Fatal(err)
	}
	ns := "/nvmet/subsystems/nqn.2026-11.test:s0/namespaces/"
	if attr(t, fs, ns+"1/ana_grpid") != "1" || attr(t, fs, ns+"2/ana_grpid") != "1" {
		t.Fatal("namespaces don't start in group 1")
	}

	paused := st.Clone()
	paused.Subsystems["nqn.2026-11.test:s0"].Namespaces[1].Paused = true
	if err := tgt.Reconcile(st, paused); err != nil {
		t.Fatal(err)
	}
	for _, port := range []string{"1", "2"} {
		if got := attr(t, fs, "/nvmet/ports/"+port+"/ana_groups/2/ana_state"); got != "inaccessible" {
			t.Errorf("port %s: pause group is %q", port, got)
		}
	}
	if attr(t, fs, ns+"1/ana_grpid") != "2" || attr(t, fs, ns+"2/ana_grpid") != "1" {
		t.Error("only the paused namespace should move")
	}
	if attr(t, fs, ns+"1/enable") != "1" {
		t.Error("pausing disabled the namespace, which hosts would see as gone")
	}

	if err := tgt.Reconcile(paused, st); err != nil {
		t.Fatal(err)
	}
	if attr(t, fs, ns+"1/ana_grpid") != "1" {
		t.Error("not moved back")
	}
	if slices.ContainsFunc(fs.Paths(), func(p string) bool { return p == "/nvmet/ports/1/ana_groups/2" }) {
		t.Error("the pause group was left on a port")
	}
}

// A crash during a pause leaves namespaces in the inaccessible group, and hosts waiting. Paused is never saved, so the
// startup reconcile, from nothing, ends it.
func TestAPauseLeftByACrashEndsAtStartup(t *testing.T) {
	st := sized(1, 1)
	fs := targettest.New()
	tgt := &target.Target{FS: fs, Root: "/nvmet"}
	paused := st.Clone()
	paused.Subsystems["nqn.2026-11.test:s0"].Namespaces[1].Paused = true
	if err := tgt.Reconcile(target.NewState(), paused); err != nil {
		t.Fatal(err)
	}
	if attr(t, fs, "/nvmet/subsystems/nqn.2026-11.test:s0/namespaces/1/ana_grpid") != "2" {
		t.Fatal("not paused, so this checks nothing")
	}
	// A new process: the state as saved, which has no pause, applied from scratch.
	restarted := &target.Target{FS: fs, Root: "/nvmet"}
	if err := restarted.Reconcile(target.NewState(), st); err != nil {
		t.Fatal(err)
	}
	if attr(t, fs, "/nvmet/subsystems/nqn.2026-11.test:s0/namespaces/1/ana_grpid") != "1" {
		t.Error("still paused after a restart")
	}
	if slices.Contains(fs.Paths(), "/nvmet/ports/1/ana_groups/2") {
		t.Error("the pause group survived a restart")
	}
}

// A port can be removed while a pause group is on it, as the kernel needs that removed first.
func TestRemovingAPortWithAPauseGroup(t *testing.T) {
	st := sized(1, 1)
	st.Ports[2] = &target.Port{ID: 2, Transport: target.TransportTCP, Address: "127.0.0.2", Service: 4420}
	fs := targettest.New()
	tgt := &target.Target{FS: fs, Root: "/nvmet"}
	paused := st.Clone()
	paused.Subsystems["nqn.2026-11.test:s0"].Namespaces[1].Paused = true
	if err := tgt.Reconcile(target.NewState(), paused); err != nil {
		t.Fatal(err)
	}
	fewer := paused.Clone()
	delete(fewer.Ports, 2)
	if err := tgt.Reconcile(paused, fewer); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(fs.Paths(), func(p string) bool { return p == "/nvmet/ports/2" }) {
		t.Error("port 2 is still there")
	}
}
