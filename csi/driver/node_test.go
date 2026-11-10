package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
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
	f.Node = &Node{cfg: testNodeCfg(), hostNQN: nodeA, sys: f.sys, mountinfo: f.mnts, findWait: time.Second}
	f.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.ran = append(f.ran, name+" "+strings.Join(args, " "))
		if name == "blkid" {
			return nil, &exitErr{}
		}
		return nil, nil
	}
	return f
}

func testNodeCfg() Config {
	c := testCfg
	c.CtrlLossTmo, c.MaxVolumesPerNode, c.ReconnectDelay = -1, 1024, 10
	return c
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
		"nvme connect -t tcp -a 10.98.0.1 -s 4420 -n " + sub + " -q " + nodeA + " --ctrl-loss-tmo=-1 --reconnect-delay=10",
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

// Several nodes mount a ReadOnlyMany filesystem at once, so each must mount it read-only, or their journals corrupt it.
func TestReadOnlyManyIsStagedReadOnly(t *testing.T) {
	f := newFakeNode(t)
	sub := testCfg.sharedSubsystem("lab-pvc-r")
	pc := map[string]string{ctxNQN: sub, ctxTransport: "tcp", ctxAddress: "10.98.0.1", ctxService: "4420", ctxNGUID: nguid}
	f.subsystem(t, sub, "nvme1n1", nguid)
	stage := filepath.Join(t.TempDir(), "staging")
	rox := capOf(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, false)
	rox.GetMount().MountFlags = []string{"rw", "noatime"}
	req := &csi.NodeStageVolumeRequest{VolumeId: "lab-pvc-r", StagingTargetPath: stage, PublishContext: pc, VolumeCapability: rox}
	if _, err := f.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// An empty device is still formatted, by the first node, then mounted read-only: ro wins over the class's rw.
	want := []string{
		"blkid -p -o value -s TYPE /dev/nvme1n1",
		"mkfs.ext4 -q -F /dev/nvme1n1",
		"mount -t ext4 -o noatime,ro /dev/nvme1n1 " + stage,
	}
	if strings.Join(f.ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran:\n%s\nwant:\n%s", strings.Join(f.ran, "\n"), strings.Join(want, "\n"))
	}

	// Already mounted read-only: nothing to do.
	f.ran = nil
	write(t, f.mnts, "22 1 0:21 / / rw - xfs /dev/vda1 rw\n90 22 259:1 / "+stage+" ro,noatime - ext4 /dev/nvme1n1 ro")
	if _, err := f.NodeStageVolume(context.Background(), req); err != nil || len(f.ran) != 0 {
		t.Errorf("staging again ran %v: %v", f.ran, err)
	}
	// Mounted read-write, by something else: that's not this volume staged safely.
	write(t, f.mnts, "22 1 0:21 / / rw - xfs /dev/vda1 rw\n90 22 259:1 / "+stage+" rw,noatime - ext4 /dev/nvme1n1 rw")
	if _, err := f.NodeStageVolume(context.Background(), req); code(err) != codes.FailedPrecondition {
		t.Errorf("a read-write mount at the staging path: %v", err)
	}
	// ReadWriteOnce is unchanged: no ro, and a read-write mount is its own.
	if _, err := f.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: "lab-pvc-r", StagingTargetPath: stage,
		PublishContext: pc, VolumeCapability: rwo}); err != nil {
		t.Errorf("ReadWriteOnce, mounted read-write: %v", err)
	}
}

func TestAWildcardAddressIsntConnectedTo(t *testing.T) {
	f := newFakeNode(t)
	for _, addr := range []string{"0.0.0.0", "::"} {
		pc := map[string]string{ctxNQN: testCfg.nodeSubsystem(nodeA), ctxTransport: "tcp", ctxAddress: addr, ctxService: "4420", ctxNGUID: nguid}
		_, err := f.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: "lab-pvc-w", StagingTargetPath: t.TempDir(),
			PublishContext: pc, VolumeCapability: rwo})
		if code(err) != codes.FailedPrecondition || len(f.ran) != 0 {
			t.Errorf("address %q: ran %v: %v", addr, f.ran, err)
		}
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

// With no failover, an outage should pause I/O until nvweaver is back, not fail it for good after the kernel's default 10 minutes.
func TestTheNodeKeepsRetryingAndSaysHowManyVolumesFit(t *testing.T) {
	f := newFakeNode(t)
	info, err := f.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil || info.NodeId != nodeA || info.MaxVolumesPerNode != 1024 {
		t.Errorf("%+v, %v", info, err)
	}
	f.cfg.CtrlLossTmo = 600
	f.cfg.MaxVolumesPerNode = 100
	if info, _ := f.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{}); info.MaxVolumesPerNode != 100 {
		t.Errorf("configured: %d", info.MaxVolumesPerNode)
	}
	if err := f.connect(context.Background(), map[string]string{ctxNQN: "nqn.x", ctxAddress: "a", ctxService: "1"}); err != nil {
		t.Fatal(err)
	}
	f.cfg.ReconnectDelay = 2
	if err := f.connect(context.Background(), map[string]string{ctxNQN: "nqn.y", ctxAddress: "a", ctxService: "1"}); err != nil {
		t.Fatal(err)
	}
	if got := f.ran[len(f.ran)-1]; !strings.HasSuffix(got, " --ctrl-loss-tmo=600 --reconnect-delay=2") {
		t.Errorf("ran %s", got)
	}
}

// SINGLE_NODE_SINGLE_WRITER means one pod: a second target path for a volume already published must be refused.
func TestASingleWriterVolumeIsPublishedOnce(t *testing.T) {
	f := newFakeNode(t)
	sub := testCfg.nodeSubsystem(nodeA)
	f.subsystem(t, sub, "nvme1n1", nguid)
	pc := map[string]string{ctxNQN: sub, ctxNGUID: nguid}
	one := capOf(csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER, false)
	write(t, f.mnts, "22 1 0:21 / / rw - xfs /dev/vda1 rw\n"+
		"90 22 259:1 / /staging rw - ext4 /dev/nvme1n1 rw\n"+
		"91 22 259:1 / /pods/a rw - ext4 /dev/nvme1n1 rw")
	target := filepath.Join(t.TempDir(), "b")
	_, err := f.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: "v", StagingTargetPath: "/staging", TargetPath: target, VolumeCapability: one, PublishContext: pc})
	if code(err) != codes.FailedPrecondition || len(f.ran) != 0 {
		t.Errorf("a second target: %v, ran %v", err, f.ran)
	}
	// Several writers on one node may share it.
	many := capOf(csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER, false)
	if _, err := f.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: "v", StagingTargetPath: "/staging", TargetPath: target, VolumeCapability: many, PublishContext: pc}); err != nil {
		t.Errorf("multi-writer: %v", err)
	}
}
