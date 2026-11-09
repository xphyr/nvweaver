package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// fakeNode is a node plugin over a fake sysfs and mountinfo, recording the commands it would run.
type fakeNode struct {
	*Node
	ran  []string
	sys  string
	mnts string
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	dir := t.TempDir()
	f := &fakeNode{sys: filepath.Join(dir, "sys"), mnts: filepath.Join(dir, "mountinfo")}
	if err := os.WriteFile(f.mnts, []byte("22 1 0:21 / / rw - xfs /dev/vda1 rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.Node = &Node{cfg: testCfg, hostNQN: nodeA, sys: f.sys, mountinfo: f.mnts, findWait: time.Second}
	f.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.ran = append(f.ran, name+" "+strings.Join(args, " "))
		if name == "blkid" {
			return nil, &exitErr{}
		}
		return nil, nil
	}
	return f
}

type exitErr struct{}

func (*exitErr) Error() string { return "blkid: exit status 2" }

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// subsystem adds a connected subsystem with a live controller and a namespace, as sysfs shows them under native multipath.
func (f *fakeNode) subsystem(t *testing.T, nqn, ns, nguid string) {
	write(t, filepath.Join(f.sys, "class/nvme-subsystem/nvme-subsys1/subsysnqn"), nqn)
	write(t, filepath.Join(f.sys, "class/nvme-subsystem/nvme-subsys1/nvme1/state"), "live")
	write(t, filepath.Join(f.sys, "block", ns, "nguid"), nguid)
	write(t, filepath.Join(f.sys, "block", ns, "device/subsysnqn"), nqn)
	// The hidden path device, with the same identity, which has no device node.
	write(t, filepath.Join(f.sys, "block/nvme1c1n1/nguid"), nguid)
}

const nguid = "0123456789abcdef0123456789abcdef"

func TestStagingConnectsFormatsAndMounts(t *testing.T) {
	f := newFakeNode(t)
	sub := testCfg.nodeSubsystem(nodeA)
	pc := map[string]string{ctxNQN: sub, ctxTransport: "tcp", ctxAddress: "10.98.0.1", ctxService: "4420", ctxNGUID: "01234567-89ab-cdef-0123-456789ABCDEF"}
	stage := filepath.Join(t.TempDir(), "staging")
	// Connecting makes the subsystem appear, as the kernel would.
	run := f.run
	f.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "nvme" && args[0] == "connect" {
			f.subsystem(t, sub, "nvme1n1", nguid)
		}
		return run(ctx, name, args...)
	}
	_, err := f.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: "lab-pvc-a", StagingTargetPath: stage, PublishContext: pc, VolumeCapability: rwo})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"nvme connect -t tcp -a 10.98.0.1 -s 4420 -n " + sub + " -q " + nodeA,
		"blkid -p -o value -s TYPE /dev/nvme1n1",
		"mkfs.ext4 -q -F /dev/nvme1n1",
		"mount -t ext4 /dev/nvme1n1 " + stage,
	}
	if strings.Join(f.ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran:\n%s\nwant:\n%s", strings.Join(f.ran, "\n"), strings.Join(want, "\n"))
	}

	// Already connected and mounted: nothing to do.
	write(t, f.mnts, "22 1 0:21 / / rw - xfs /dev/vda1 rw\n90 22 259:1 / "+stage+" rw - ext4 /dev/nvme1n1 rw")
	f.ran = nil
	if _, err := f.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: "lab-pvc-a", StagingTargetPath: stage, PublishContext: pc, VolumeCapability: rwo}); err != nil || len(f.ran) != 0 {
		t.Errorf("staging again ran %v: %v", f.ran, err)
	}
}

func TestADeviceIsFoundOnlyInItsSubsystem(t *testing.T) {
	f := newFakeNode(t)
	f.subsystem(t, "nqn.other", "nvme1n1", nguid)
	if dev, err := f.findDevice(nguid, "", "nqn.mine"); err != nil || dev != "" {
		t.Errorf("the same identity in another subsystem: %q, %v", dev, err)
	}
	if dev, err := f.findDevice(nguid, "", "nqn.other"); err != nil || dev != "/dev/nvme1n1" {
		t.Errorf("found %q, %v", dev, err)
	}
	if _, err := f.findDevice("00000000000000000000000000000000", "", "nqn.other"); err == nil {
		t.Error("an all-zero NGUID is no identity")
	}
}

func TestPublishBindsAndUnpublishUndoes(t *testing.T) {
	f := newFakeNode(t)
	target := filepath.Join(t.TempDir(), "pod/mount")
	_, err := f.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: "v", StagingTargetPath: "/staging", TargetPath: target, VolumeCapability: rwo, Readonly: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "mount --bind /staging " + target + "\nmount -o remount,bind,ro " + target
	if strings.Join(f.ran, "\n") != want {
		t.Errorf("ran %v", f.ran)
	}
	write(t, f.mnts, "22 1 0:21 / / rw - xfs /dev/vda1 rw\n91 22 259:1 / "+strings.ReplaceAll(target, " ", `\040`)+" ro - ext4 /dev/nvme1n1 rw")
	f.ran = nil
	if _, err := f.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: "v", TargetPath: target}); err != nil || strings.Join(f.ran, "") != "umount "+target {
		t.Errorf("unpublish ran %v: %v", f.ran, err)
	}
}

func TestMountinfoEscapes(t *testing.T) {
	if got := unescapeMount(`/var/lib/a\040b`); got != "/var/lib/a b" {
		t.Errorf("%q", got)
	}
}

// A shared volume's subsystem is its own, so unstaging it disconnects, or the kernel would keep retrying a subsystem that's gone.
// A node's own subsystem stays connected for its other volumes.
func TestUnstagingASharedVolumeDisconnects(t *testing.T) {
	f := newFakeNode(t)
	shared := testCfg.sharedSubsystem("lab-pvc-s")
	f.subsystem(t, shared, "nvme1n1", nguid)
	if _, err := f.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "lab-pvc-s", StagingTargetPath: "/staging"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.ran, "\n") != "nvme disconnect -n "+shared {
		t.Errorf("ran %v", f.ran)
	}
	f.ran = nil
	if _, err := f.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "lab-pvc-other", StagingTargetPath: "/staging"}); err != nil || len(f.ran) != 0 {
		t.Errorf("a volume with no subsystem of its own ran %v: %v", f.ran, err)
	}
}
