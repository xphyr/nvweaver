package storage

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"nvweaver/internal/target"
)

// fakeLVM records commands. The real thing is tested in test/vm/vm-e2e.sh.
type fakeLVM struct {
	calls   []string
	replies map[string]string // Command prefix to its output.
	fail    map[string]error
}

func (f *fakeLVM) run(args ...string) (string, error) {
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	for prefix, err := range f.fail {
		if strings.HasPrefix(line, prefix) {
			return "", err
		}
	}
	for prefix, out := range f.replies {
		if strings.HasPrefix(line, prefix) {
			return out, nil
		}
	}
	return "", nil
}

func lvmDriver(vg string) (*lvmThinDriver, *fakeLVM) {
	f := &fakeLVM{replies: map[string]string{}, fail: map[string]error{}}
	return &lvmThinDriver{vg: vg, pool: "pool0", run: f.run}, f
}

func lvsJSON(fields string) string {
	return fmt.Sprintf(`{"report": [{"lv": [{%s}]}]}`, fields)
}

func TestLVMPoolSettings(t *testing.T) {
	var ve *target.ValidationError
	for name, p := range map[string]*target.Pool{
		"no names":       {Type: TypeLVMThin},
		"no thin pool":   {Type: TypeLVMThin, VolumeGroup: "vg"},
		"path too":       {Type: TypeLVMThin, VolumeGroup: "vg", ThinPool: "p", Path: "/x"},
		"an option":      {Type: TypeLVMThin, VolumeGroup: "--force", ThinPool: "p"},
		"a path":         {Type: TypeLVMThin, VolumeGroup: "../vg", ThinPool: "p"},
		"reflink no dir": {Type: TypeReflink},
		"reflink + vg":   {Type: TypeReflink, Path: "/x", VolumeGroup: "vg"},
	} {
		if _, err := New(p); !errors.As(err, &ve) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}
	if _, err := New(&target.Pool{Type: TypeLVMThin, VolumeGroup: "vg", ThinPool: "pool0"}); err != nil {
		t.Errorf("a valid pool was refused: %v", err)
	}
}

func TestLVMCheckNeedsAThinPool(t *testing.T) {
	d, f := lvmDriver("vg")
	var ve *target.ValidationError
	f.fail["lvs"] = fmt.Errorf("Failed to find logical volume")
	if err := d.Check(); !errors.As(err, &ve) {
		t.Errorf("missing: %v", err)
	}
	delete(f.fail, "lvs")
	f.replies["lvs"] = lvsJSON(`"segtype": "linear"`)
	if err := d.Check(); !errors.As(err, &ve) || !strings.Contains(err.Error(), "linear") {
		t.Errorf("linear: %v", err)
	}
	f.replies["lvs"] = lvsJSON(`"segtype": "thin-pool"`)
	if err := d.Check(); err != nil {
		t.Errorf("thin pool: %v", err)
	}
}

func TestLVMCapacity(t *testing.T) {
	d, f := lvmDriver("vg")
	f.replies["lvs"] = lvsJSON(`"lv_size": "1000", "data_percent": "25.00", "metadata_percent": "3.50", "vg_mda_free": "500", "vg_mda_size": "1000"`)
	c, err := d.Capacity()
	if err != nil {
		t.Fatal(err)
	}
	if c.TotalBytes != 1000 || c.UsedBytes != 250 || c.MetadataUsedPercent != 3.5 || len(c.Warnings) != 0 {
		t.Errorf("capacity = %+v", c)
	}
	f.replies["lvs"] = lvsJSON(`"lv_size": "1000", "data_percent": "25.00", "metadata_percent": "3.50", "vg_mda_free": "99", "vg_mda_size": "1000"`)
	if c, _ = d.Capacity(); len(c.Warnings) != 1 {
		t.Errorf("a nearly full metadata area should warn: %+v", c)
	}
	f.replies["lvs"] = lvsJSON(`"lv_size": "1000", "data_percent": "100.00", "metadata_percent": "3.50", "lv_health_status": "out_of_data"`)
	if c, _ = d.Capacity(); len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "out of space") {
		t.Errorf("a pool that's out of space should say what that means: %+v", c)
	}
	f.replies["lvs"] = lvsJSON(`"lv_size": "1000", "data_percent": "", "metadata_percent": "1"`)
	if _, err := d.Capacity(); err == nil {
		t.Error("output it can't understand must be an error, not an empty pool")
	}
}

func TestLVMAlignsToExtents(t *testing.T) {
	d, f := lvmDriver("align-vg")
	f.replies["vgs"] = "  4194304\n"
	for in, want := range map[int64]int64{4096: 4194304, 4194304: 4194304, 4194305: 8388608} {
		got, err := d.AlignSize(in)
		if err != nil || got != want {
			t.Errorf("AlignSize(%d) = %d, %v, want %d", in, got, err, want)
		}
	}
	if len(f.calls) != 1 {
		t.Errorf("the extent size should be remembered: %v", f.calls)
	}
}

func TestLVMCommands(t *testing.T) {
	d, f := lvmDriver("vg")
	want := func(label, line string) {
		t.Helper()
		if len(f.calls) == 0 || f.calls[len(f.calls)-1] != line {
			t.Errorf("%s: ran %v, want %q", label, f.calls, line)
		}
	}
	path, err := d.Create("nvw-a", 8388608)
	if err != nil || path != "/dev/vg/nvw-a" {
		t.Errorf("create: %s, %v", path, err)
	}
	want("create", "lvcreate --config backup/archive=0 -y --thin -V 8388608b -n nvw-a vg/pool0")
	if _, err := d.Snapshot("nvw-a", "nvw-s"); err != nil {
		t.Error(err)
	}
	want("snapshot", "lvcreate --config backup/archive=0 -y -s -pr -ky -n nvw-s vg/nvw-a")

	f.replies["lvs"] = lvsJSON(`"lv_size": "8388608"`)
	f.calls = nil
	if _, err := d.Clone("nvw-s", "nvw-c", 0); err != nil {
		t.Error(err)
	}
	want("same-size clone", "lvcreate --config backup/archive=0 -y -s -prw -kn -ay -n nvw-c vg/nvw-s")
	if _, err := d.Clone("nvw-s", "nvw-d", 16777216); err != nil {
		t.Error(err)
	}
	want("larger clone", "lvextend --config backup/archive=0 -L 16777216b vg/nvw-d")
	var ve *target.ValidationError
	if _, err := d.Clone("nvw-s", "nvw-e", 4194304); !errors.As(err, &ve) {
		t.Errorf("a smaller clone must be refused: %v", err)
	}

	f.calls = nil
	if err := d.Grow("nvw-a", 8388608); err != nil {
		t.Error(err)
	}
	if len(f.calls) != 1 {
		t.Errorf("growing to the size it already is must change nothing: %v", f.calls)
	}
	if err := d.Grow("nvw-a", 16777216); err != nil {
		t.Error(err)
	}
	want("grow", "lvextend --config backup/archive=0 -L 16777216b vg/nvw-a")
	if err := d.Delete("nvw-a"); err != nil {
		t.Error(err)
	}
	want("delete", "lvremove --config backup/archive=0 -y vg/nvw-a")
	if err := d.Activate(); err != nil {
		t.Error(err)
	}
	want("activate", "vgchange -ay vg")
}

func TestLVMDeleteOfWhatsGoneIsFine(t *testing.T) {
	d, f := lvmDriver("vg")
	f.fail["lvremove"] = fmt.Errorf("Failed to find logical volume \"vg/nvw-a\"")
	if err := d.Delete("nvw-a"); err != nil {
		t.Errorf("recovery relies on this: %v", err)
	}
	f.fail["lvremove"] = fmt.Errorf("Logical volume vg/nvw-a in use")
	if err := d.Delete("nvw-a"); err == nil {
		t.Error("a real failure must be reported")
	}
}

func TestLVMRunningOutOfRoom(t *testing.T) {
	d, f := lvmDriver("vg")
	f.fail["lvcreate"] = fmt.Errorf("Insufficient free space")
	if _, err := d.Create("nvw-a", 4194304); !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("create: %v", err)
	}
	// As a thin pool that has really filled up words it.
	f.fail["lvcreate"] = fmt.Errorf("WARNING: Thin pool vg/pool0 is out of data space.\nCannot create new thin volume, free space in thin pool vg/pool0 reached threshold")
	if _, err := d.Create("nvw-a", 4194304); !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("create in a full pool: %v", err)
	}
	if _, err := d.Snapshot("nvw-a", "nvw-b"); !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("snapshot in a full pool: %v", err)
	}
	f.fail["lvcreate"] = fmt.Errorf("Something else went wrong")
	if _, err := d.Create("nvw-a", 4194304); errors.Is(err, syscall.ENOSPC) {
		t.Errorf("not every failure is a full pool: %v", err)
	}
}

func TestLVMListIgnoresOthers(t *testing.T) {
	d, f := lvmDriver("vg")
	f.replies["lvs"] = "  pool0\n  root\n  nvw-aa\n  nvw-bb\n"
	names, err := d.List()
	if err != nil || fmt.Sprint(names) != "[nvw-aa nvw-bb]" {
		t.Errorf("%v, %v", names, err)
	}
}
