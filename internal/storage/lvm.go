package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"nvweaver/internal/target"
)

// TypeLVMThin is a pool of thin volumes in an existing LVM thin pool.
const TypeLVMThin = "lvm-thin"

// noArchive stops LVM saving a copy of its metadata on every change, which reached 375 MB after 1,000 volumes.
const noArchive = "backup/archive=0"

var lvmNameRE = regexp.MustCompile(`^[A-Za-z0-9_+.][A-Za-z0-9_+.-]{0,126}$`)

// lvmThinDriver uses the host's own LVM, so that one version and one configuration act on the volume group.
type lvmThinDriver struct {
	vg, pool string
	run      func(args ...string) (string, error)
}

func newLVMThin(p *target.Pool) (Driver, error) {
	if p.Path != "" {
		return nil, target.Invalid("an %s pool takes volume_group and thin_pool, not path", TypeLVMThin)
	}
	if !lvmNameRE.MatchString(p.VolumeGroup) || !lvmNameRE.MatchString(p.ThinPool) {
		return nil, target.Invalid("an %s pool needs a valid volume_group and thin_pool", TypeLVMThin)
	}
	return &lvmThinDriver{vg: p.VolumeGroup, pool: p.ThinPool, run: hostLVM}, nil
}

// hostLVM runs lvm. A container started with --pid=host sees the host's init as PID 1, in other
// namespaces from its own, so it goes through nsenter. As a plain service, it's already there.
func hostLVM(args ...string) (string, error) {
	argv := append([]string{"lvm"}, args...)
	if inHostPIDContainer() {
		argv = append([]string{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p"}, argv...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "LVM_SUPPRESS_FD_WARNINGS=1"}
	// Apart, because a warning mixed into the output would stop it parsing.
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrNotFound) {
		return "", fmt.Errorf("can't find lvm: on a host, install lvm2; in a container, run it privileged with --pid=host, to use the host's")
	}
	if err != nil {
		text := strings.TrimSpace(stderr.String())
		if text == "" {
			// JSON reports put LVM's messages here.
			text = strings.Join(strings.Fields(stdout.String()), " ")
		}
		if strings.Contains(text, "executable file not found") || strings.Contains(text, "No such file") && strings.Contains(text, "lvm") && len(text) < 80 {
			return "", fmt.Errorf("can't find lvm on the host: install lvm2 there")
		}
		return "", fmt.Errorf("lvm %s: %s: %w", args[0], text, err)
	}
	logWarnings(args[0], stderr.String())
	return stdout.String(), nil
}

// logWarnings reports what LVM says when it succeeds anyway, since some of it matters, such as not being
// able to lock itself in memory. Thin pools are meant to overcommit, so saying so at every create is only noise.
func logWarnings(command, text string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.Contains(line, "exceeds the size of thin pool") && !strings.Contains(line, "thin_pool_autoextend") {
			slog.Warn("lvm: "+line, "command", command)
		}
	}
}

func inHostPIDContainer() bool {
	host, err := os.Readlink("/proc/1/ns/mnt")
	if err != nil {
		return false
	}
	self, err := os.Readlink("/proc/self/ns/mnt")
	return err == nil && host != self
}

// ErrNoLVM means this machine has no LVM at all, rather than that it couldn't be reached.
var ErrNoLVM = errors.New("this machine has no LVM")

// VisibleToLVM returns the devices that this machine's LVM can read, after its devices file and filters, whether they hold
// a PV or not. It uses lvmdiskscan because "pvs -a" leaves out what has no PV on it. In a container with no way to the
// host's LVM, it fails, as the host may well have LVM of its own.
func VisibleToLVM() ([]string, error) {
	out, err := hostLVM("lvmdiskscan")
	if err != nil {
		if strings.Contains(err.Error(), "can't find lvm") {
			if isContainer() && !inHostPIDContainer() {
				return nil, errors.New("this container can't reach the host's LVM: run it with --pid=host, or check the host by hand")
			}
			return nil, ErrNoLVM
		}
		return nil, err
	}
	return parseDiskscan(out), nil
}

// parseDiskscan takes the device from lines like "  /dev/sdb [ 8.00 GiB] LVM physical volume", leaving out its summary.
func parseDiskscan(out string) []string {
	var devices []string
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && strings.HasPrefix(fields[0], "/dev/") {
			devices = append(devices, fields[0])
		}
	}
	return devices
}

func isContainer() bool {
	for _, marker := range []string{"/run/.containerenv", "/.dockerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

func (d *lvmThinDriver) lv(name string) string   { return d.vg + "/" + name }
func (d *lvmThinDriver) path(name string) string { return "/dev/" + d.vg + "/" + name }

type lvReport struct {
	Report []struct {
		LV []map[string]string `json:"lv"`
	} `json:"report"`
}

// report returns one LV's fields, asked for by name.
func (d *lvmThinDriver) report(lv string, fields ...string) (map[string]string, error) {
	out, err := d.run("lvs", "--reportformat", "json", "--units", "b", "--nosuffix", "-o", strings.Join(fields, ","), lv)
	if err != nil {
		return nil, err
	}
	var r lvReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return nil, fmt.Errorf("parse lvs output: %w", err)
	}
	if len(r.Report) != 1 || len(r.Report[0].LV) != 1 {
		return nil, fmt.Errorf("lvs found no single %s", lv)
	}
	return r.Report[0].LV[0], nil
}

// Check implements Driver.
func (d *lvmThinDriver) Check() error {
	got, err := d.report(d.lv(d.pool), "segtype")
	if err != nil {
		return target.Invalid("thin pool %s: %v", d.lv(d.pool), err)
	}
	if got["segtype"] != "thin-pool" {
		return target.Invalid("%s is a %s, not a thin pool", d.lv(d.pool), got["segtype"])
	}
	return nil
}

// Capacity implements Driver.
func (d *lvmThinDriver) Capacity() (Capacity, error) {
	got, err := d.report(d.lv(d.pool), "lv_size", "data_percent", "metadata_percent", "vg_mda_free", "vg_mda_size", "lv_health_status")
	if err != nil {
		return Capacity{}, err
	}
	total, err := strconv.ParseInt(got["lv_size"], 10, 64)
	if err != nil {
		return Capacity{}, fmt.Errorf("parse lv_size %q: %w", got["lv_size"], err)
	}
	data, err := strconv.ParseFloat(got["data_percent"], 64)
	if err != nil {
		return Capacity{}, fmt.Errorf("parse data_percent %q: %w", got["data_percent"], err)
	}
	meta, err := strconv.ParseFloat(got["metadata_percent"], 64)
	if err != nil {
		return Capacity{}, fmt.Errorf("parse metadata_percent %q: %w", got["metadata_percent"], err)
	}
	c := Capacity{TotalBytes: total, UsedBytes: int64(float64(total) * data / 100), MetadataUsedPercent: meta}
	free, errFree := strconv.ParseInt(got["vg_mda_free"], 10, 64)
	size, errSize := strconv.ParseInt(got["vg_mda_size"], 10, 64)
	if errFree == nil && errSize == nil && size > 0 && free*10 < size {
		c.Warnings = append(c.Warnings, fmt.Sprintf("volume group %s has only %d bytes of its LVM metadata area left, which limits how many more volumes it can hold", d.vg, free))
	}
	if got["lv_health_status"] == "out_of_data" {
		c.Warnings = append(c.Warnings, fmt.Sprintf("thin pool %s is out of space: hosts' writes to new blocks stall for a while, then fail with I/O errors. Extend it with lvextend", d.lv(d.pool)))
	}
	return c, nil
}

var extentSizes sync.Map

// AlignSize implements Driver: LVM allocates whole extents. The size barely ever changes, and this
// runs for every request, so it's remembered.
func (d *lvmThinDriver) AlignSize(sizeBytes int64) (int64, error) {
	var extent int64
	if v, ok := extentSizes.Load(d.vg); ok {
		extent = v.(int64)
	} else {
		out, err := d.run("vgs", "--noheadings", "--units", "b", "--nosuffix", "-o", "vg_extent_size", d.vg)
		if err != nil {
			return 0, err
		}
		if extent, err = strconv.ParseInt(strings.TrimSpace(out), 10, 64); err != nil || extent <= 0 {
			return 0, fmt.Errorf("parse extent size %q: %w", out, err)
		}
		extentSizes.Store(d.vg, extent)
	}
	if rest := sizeBytes % extent; rest != 0 {
		sizeBytes += extent - rest
	}
	return sizeBytes, nil
}

// noSpace turns LVM's ways of saying it's out of room into what callers can recognise: "Insufficient free
// space" from a volume group, and "out of data space" from a thin pool that has filled up.
func noSpace(err error) error {
	if err == nil {
		return nil
	}
	for _, text := range []string{"nsufficient", "out of data space", "free space in thin pool"} {
		if strings.Contains(err.Error(), text) {
			return fmt.Errorf("%v: %w", err, syscall.ENOSPC)
		}
	}
	return err
}

// Create implements Driver.
func (d *lvmThinDriver) Create(internal string, sizeBytes int64) (string, error) {
	_, err := d.run("lvcreate", "--config", noArchive, "-y", "--thin", "-V", fmt.Sprintf("%db", sizeBytes), "-n", internal, d.lv(d.pool))
	if err != nil {
		return "", noSpace(err)
	}
	return d.path(internal), nil
}

// Delete implements Driver.
func (d *lvmThinDriver) Delete(internal string) error {
	_, err := d.run("lvremove", "--config", noArchive, "-y", d.lv(internal))
	if err != nil && strings.Contains(err.Error(), "Failed to find logical volume") {
		return nil
	}
	return err
}

// Grow implements Driver. Asking for the size it already has, as recovery can, isn't an error.
func (d *lvmThinDriver) Grow(internal string, sizeBytes int64) error {
	got, err := d.report(d.lv(internal), "lv_size")
	if err != nil {
		return err
	}
	if current, err := strconv.ParseInt(got["lv_size"], 10, 64); err == nil && current >= sizeBytes {
		return nil
	}
	_, err = d.run("lvextend", "--config", noArchive, "-L", fmt.Sprintf("%db", sizeBytes), d.lv(internal))
	return noSpace(err)
}

// Snapshot implements Driver. A thin snapshot is read-only here, and has LVM's activation-skip flag,
// so it has no device until something needs it. Keeping thousands inactive makes booting faster.
func (d *lvmThinDriver) Snapshot(source, internal string) (string, error) {
	_, err := d.run("lvcreate", "--config", noArchive, "-y", "-s", "-pr", "-ky", "-n", internal, d.lv(source))
	if err != nil {
		return "", noSpace(err)
	}
	return d.path(internal), nil
}

// Clone implements Driver. It's a writable thin snapshot, which has to be activated to be exported.
func (d *lvmThinDriver) Clone(source, internal string, sizeBytes int64) (string, error) {
	got, err := d.report(d.lv(source), "lv_size")
	if err != nil {
		return "", err
	}
	current, err := strconv.ParseInt(got["lv_size"], 10, 64)
	if err != nil {
		return "", fmt.Errorf("parse lv_size %q: %w", got["lv_size"], err)
	}
	if sizeBytes != 0 && sizeBytes < current {
		return "", target.Invalid("a clone can't be smaller than its source (%d bytes)", current)
	}
	if _, err := d.run("lvcreate", "--config", noArchive, "-y", "-s", "-prw", "-kn", "-ay", "-n", internal, d.lv(source)); err != nil {
		return "", noSpace(err)
	}
	if sizeBytes > current {
		if _, err := d.run("lvextend", "--config", noArchive, "-L", fmt.Sprintf("%db", sizeBytes), d.lv(internal)); err != nil {
			return "", noSpace(err)
		}
	}
	return d.path(internal), nil
}

// List implements Driver.
func (d *lvmThinDriver) List() ([]string, error) {
	out, err := d.run("lvs", "--noheadings", "-o", "lv_name", d.vg)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Fields(out) {
		if strings.HasPrefix(name, target.InternalPrefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// Activate implements Driver. Snapshots stay inactive, because of their activation-skip flag.
func (d *lvmThinDriver) Activate() error {
	_, err := d.run("vgchange", "-ay", d.vg)
	return err
}
