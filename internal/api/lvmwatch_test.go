package api

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"nvweaver/internal/storage"
	"nvweaver/internal/target"
)

// fakeIDs stands in for device numbers: the listed names are one device, and a partition also reports its disk.
func fakeIDs(path string) []string {
	return map[string][]string{
		"/dev/disk/by-id/a": {"8:16"},
		"/dev/sdb":          {"8:16"},
		"/dev/sdb1":         {"8:17", "8:16"},
		"/dev/sdc":          {"8:32"},
		"/dev/vg/nvw-1":     {"252:1"},
		"/dev/mapper/nvw-1": {"252:1"},
	}[path]
}

func lvmState(paths ...string) *target.State {
	st := target.NewState()
	sub := &target.Subsystem{NQN: "nqn.test:a", Namespaces: map[uint32]*target.Namespace{}}
	for i, p := range paths {
		sub.Namespaces[uint32(i+1)] = &target.Namespace{NSID: uint32(i + 1), DevicePath: p}
	}
	st.Subsystems[sub.NQN] = sub
	return st
}

func TestLVMExposure(t *testing.T) {
	cases := []struct {
		name     string
		exported []string
		visible  []string
		want     string
	}{
		{"protected: LVM reads only its own disk", []string{"/dev/sdb"}, []string{"/dev/sdc"}, ""},
		{"a disk LVM can read", []string{"/dev/sdb"}, []string{"/dev/sdb", "/dev/sdc"}, "/dev/sdb"},
		{"under another of its names", []string{"/dev/disk/by-id/a"}, []string{"/dev/sdb"}, "/dev/sdb"},
		{"only a partition that a client made", []string{"/dev/sdb"}, []string{"/dev/sdb1"}, "/dev/sdb1"},
		{"a thin volume, under another name", []string{"/dev/vg/nvw-1"}, []string{"/dev/mapper/nvw-1"}, "/dev/mapper/nvw-1"},
		{"files can't be read as PVs", []string{"/data/a.img"}, []string{"/dev/sdb"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := lvmWarnings(lvmState(c.exported...), func() ([]string, error) { return c.visible, nil }, fakeIDs)
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected warning: %v", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], c.want) || !strings.Contains(got[0], "docs/pools.md") {
				t.Fatalf("got %v, want one naming %s and the docs", got, c.want)
			}
		})
	}
}

func TestLVMExposureOfPassthrough(t *testing.T) {
	st := lvmState()
	st.Subsystems["nqn.test:a"].Passthru = "/dev/nvme0"
	got := lvmWarnings(st, func() ([]string, error) { return []string{"/dev/nvme0n1", "/dev/nvme10n1"}, nil }, fakeIDs)
	if len(got) != 1 || !strings.Contains(got[0], "/dev/nvme0n1") {
		t.Fatalf("got %v", got)
	}
	if strings.Contains(got[0], "nvme10") {
		t.Errorf("another controller was named: %v", got)
	}
}

func TestLVMExposureNamesOnlyAFew(t *testing.T) {
	var exported, visible []string
	ids := func(p string) []string { return []string{p} }
	for i := range 5000 {
		p := fmt.Sprintf("/dev/vg/nvw-%04d", i)
		exported, visible = append(exported, p), append(visible, p)
	}
	got := lvmWarnings(lvmState(exported...), func() ([]string, error) { return visible, nil }, ids)
	if len(got) != 1 || !strings.Contains(got[0], "and 4995 more") || strings.Contains(got[0], "nvw-0005") {
		t.Fatalf("got %v", got)
	}
}

func TestLVMExposureWhenItCantTell(t *testing.T) {
	asked := false
	none := lvmWarnings(lvmState("/data/a.img"), func() ([]string, error) { asked = true; return nil, nil }, fakeIDs)
	if asked || len(none) != 0 {
		t.Errorf("with nothing to ask about: asked=%v, got %v", asked, none)
	}
	if got := lvmWarnings(lvmState("/dev/sdb"), func() ([]string, error) { return nil, storage.ErrNoLVM }, fakeIDs); len(got) != 0 {
		t.Errorf("a machine with no LVM can't activate anything, but got %v", got)
	}
	got := lvmWarnings(lvmState("/dev/sdb"), func() ([]string, error) { return nil, errors.New("no access") }, fakeIDs)
	if len(got) != 1 || !strings.Contains(got[0], "couldn't check") || !strings.Contains(got[0], "no access") {
		t.Errorf("not being able to tell must be reported, but got %v", got)
	}
}

func TestMissingToolsOnlyMatterIfSomethingNeedsThem(t *testing.T) {
	missing := func() []string { return []string{"no thin_check"} }
	st := target.NewState()
	if got := toolWarnings(st, missing); len(got) != 0 {
		t.Errorf("with no pools, got %v", got)
	}
	st.Pools["r"] = &target.Pool{Name: "r", Type: storage.TypeReflink}
	if got := toolWarnings(st, missing); len(got) != 0 {
		t.Errorf("a reflink pool doesn't need LVM, but got %v", got)
	}
	st.Pools["l"] = &target.Pool{Name: "l", Type: storage.TypeLVMThin}
	if got := toolWarnings(st, missing); len(got) != 1 || got[0] != "no thin_check" {
		t.Errorf("with an LVM pool, got %v", got)
	}
}

func TestWarningsCollectEveryProblemAndClear(t *testing.T) {
	m := &Manager{log: slog.New(slog.DiscardHandler)}
	st := lvmState("/dev/sdb")
	st.Pools["l"] = &target.Pool{Name: "l", Type: storage.TypeLVMThin}
	m.state.Store(st)
	m.visibleToLVM = func() ([]string, error) { return nil, errors.New("no access") }
	m.missingLVMTools = func() []string { return []string{"no thin_check"} }
	if got := m.Warnings(); got == nil || len(got) != 0 {
		t.Errorf("before any check, want an empty list for the API, got %#v", got)
	}
	m.checkLVM()
	if got := m.Warnings(); len(got) != 2 {
		t.Errorf("want both problems, got %v", got)
	}
	m.visibleToLVM = func() ([]string, error) { return nil, storage.ErrNoLVM }
	m.missingLVMTools = func() []string { return nil }
	m.checkLVM()
	if got := m.Warnings(); got == nil || len(got) != 0 {
		t.Errorf("once they're fixed, want an empty list for the API, got %#v", got)
	}
}
