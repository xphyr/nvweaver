package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nvweaver/internal/target"
)

// fakeNVMe makes the sysfs layouts seen on a Fedora 44 VM: nvme1 is an emulated PCIe controller, with its namespace in
// its own directory, and nvme2 one over nvme-loop, with multipath, whose directory has only the hidden path device.
// Namespaces are regular files in a fake /dev.
func fakeNVMe(t *testing.T) (dev string) {
	t.Helper()
	sys, dev := t.TempDir(), t.TempDir()
	for _, d := range []string{
		"class/nvme/nvme1/nvme1n1",
		"class/nvme/nvme2/nvme2c2n1",
		"class/nvme-subsystem/nvme-subsys1/nvme1",
		"class/nvme-subsystem/nvme-subsys2/nvme2",
		"class/nvme-subsystem/nvme-subsys2/nvme2n1",
		"class/nvme/nvme3/nvme3c3n1", // with its subsystem missing
		"class/nvme/nvme4",           // with no namespaces
	} {
		if err := os.MkdirAll(filepath.Join(sys, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"nvme1n1", "nvme2n1"} {
		if err := os.WriteFile(filepath.Join(dev, f), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldSys, oldDev, oldInUse := sysRoot, devRoot, checkInUse
	sysRoot, devRoot = sys, dev
	t.Cleanup(func() { sysRoot, devRoot, checkInUse = oldSys, oldDev, oldInUse })
	return dev
}

func TestControllerNamespaces(t *testing.T) {
	dev := fakeNVMe(t)
	cases := []struct {
		ctrl, want string
		fails      bool
	}{
		{"/dev/nvme1", dev + "/nvme1n1", false},
		{"/dev/nvme2", dev + "/nvme2n1", false}, // reading only the controller's directory finds nothing here
		{"/dev/nvme3", "", true},                // path devices, but no namespace found: not "nothing to check"
		{"/dev/nvme4", "", false},
		{"/dev/nvme9", "", true},
	}
	for _, c := range cases {
		got, err := controllerNamespaces(c.ctrl)
		if (err != nil) != c.fails || strings.Join(got, " ") != c.want {
			t.Errorf("%s: %v, %v", c.ctrl, got, err)
		}
	}
}

func TestPassthroughNeedsAFreeController(t *testing.T) {
	dev := fakeNVMe(t)
	for _, ctrl := range []string{"/dev/nvme1", "/dev/nvme2"} {
		ns := dev + "/" + filepath.Base(ctrl) + "n1"
		t.Run(ctrl, func(t *testing.T) {
			st := target.NewState()
			if err := checkPassthruFree(st, ctrl); err != nil {
				t.Errorf("a free controller: %v", err)
			}

			st.Subsystems["a"] = &target.Subsystem{NQN: "a", Namespaces: map[uint32]*target.Namespace{1: {NSID: 1, DevicePath: ns}}}
			if err := checkPassthruFree(st, ctrl); !isConflict(err) {
				t.Errorf("with its namespace exported: %v", err)
			}
			delete(st.Subsystems, "a")

			st.Subsystems["b"] = &target.Subsystem{NQN: "b", Passthru: ctrl, Namespaces: map[uint32]*target.Namespace{}}
			if err := checkPassthruFree(st, ctrl); !isConflict(err) {
				t.Errorf("passed through already: %v", err)
			}
			// And the other way round: its namespace can't be exported while it's passed through.
			if err := checkUnshared(st, ns, "", 0, ""); !isConflict(err) {
				t.Errorf("exporting a namespace of a controller that's passed through: %v", err)
			}
			delete(st.Subsystems, "b")

			checkInUse = func(path string) error {
				if path == ns {
					return target.NewConflict("%s is in use", path)
				}
				return nil
			}
			defer func() { checkInUse = checkBacking }()
			if err := checkPassthruFree(st, ctrl); !isConflict(err) {
				t.Errorf("with its namespace mounted: %v", err)
			}
		})
	}
	if err := checkPassthruFree(target.NewState(), "/dev/nvme3"); err == nil {
		t.Error("a controller whose namespaces can't be found")
	}
}
