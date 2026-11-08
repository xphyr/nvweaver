package api

import (
	"fmt"
	"testing"
	"time"

	"nvweaver/internal/target"
)

// TestWhereTimeGoes reports what each stage of a change costs at the scale target, on the fake
// backend, which leaves out the kernel and disk. Run with -v.
func TestWhereTimeGoes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 5,000 volumes")
	}
	st := target.NewState()
	st.Pools["p"] = &target.Pool{Name: "p", Type: "reflink", Path: "/p", WarningPercent: 80}
	st.Ports[1] = &target.Port{ID: 1, Transport: "tcp", Address: "127.0.0.1", Service: 4420}
	for n := range 100 {
		nqn := fmt.Sprintf("nqn.2026-11.net.example:node%d", n)
		st.Hosts[nqn+"-h"] = &target.Host{NQN: nqn + "-h"}
		st.Subsystems[nqn] = &target.Subsystem{NQN: nqn, Serial: "s", Model: "m", AllowedHosts: []string{nqn + "-h"}, Ports: []uint16{1}, Namespaces: map[uint32]*target.Namespace{}}
	}
	for v := range 5000 {
		name := fmt.Sprintf("pvc-%d", v)
		internal := target.NewInternalName()
		st.Volumes[name] = &target.Volume{Name: name, Pool: "p", InternalName: internal, DevicePath: "/p/" + internal + ".img", SizeBytes: 4096, UUID: target.NewUUID(), NGUID: target.NewNGUID(), CreatedAt: "2026-11-05T00:00:00Z"}
		nqn := fmt.Sprintf("nqn.2026-11.net.example:node%d", v%100)
		nsid := uint32(len(st.Subsystems[nqn].Namespaces) + 1)
		st.Subsystems[nqn].Namespaces[nsid] = &target.Namespace{NSID: nsid, DevicePath: "/p/" + internal + ".img", UUID: st.Volumes[name].UUID, NGUID: st.Volumes[name].NGUID, Enabled: true, Volume: name}
	}
	stage := func(name string, fn func()) {
		start := time.Now()
		for range 10 {
			fn()
		}
		t.Logf("%-28s %8.2f ms", name, float64(time.Since(start).Microseconds())/10000)
	}
	stage("Clone", func() { st.Clone() })
	stage("Validate", func() {
		if err := st.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	m := &Manager{statePath: t.TempDir() + "/state.json"}
	stage("save, with fsync", func() {
		if err := m.save(st); err != nil {
			t.Fatal(err)
		}
	})
	stage("checkUnshared", func() {
		if err := checkUnshared(st, "/p/none", "", 0, ""); err != nil {
			t.Fatal(err)
		}
	})
	stage("mappings", func() { mappings(st) })
}
