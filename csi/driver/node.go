package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Runner runs a command and returns what it wrote. A variable on Node, so tests can see what would run.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// Node connects to volumes over NVMe/TCP, and formats and mounts them. It
// leaves the connection to a subsystem up when a volume is unstaged, as other
// volumes on the node use the same one; the controller takes the volume out.
type Node struct {
	csi.UnimplementedNodeServer
	cfg       Config
	hostNQN   string
	run       Runner
	sys       string // sysfs, /sys
	mountinfo string // /proc/self/mountinfo
	findWait  time.Duration
	locks     locks
}

// NewNode makes the node plugin, with the identity it gives hosts: its NVMe host NQN.
func NewNode(cfg Config, hostNQNFile string) (*Node, error) {
	b, err := os.ReadFile(hostNQNFile)
	if err != nil {
		return nil, fmt.Errorf("the node's NVMe host NQN, which is its identity to nvweaver: %w", err)
	}
	nqn := strings.TrimSpace(string(b))
	if !strings.HasPrefix(nqn, "nqn.") {
		return nil, fmt.Errorf("%s holds %q, which isn't an NQN", hostNQNFile, nqn)
	}
	return &Node{cfg: cfg, hostNQN: nqn, run: execRunner, sys: "/sys", mountinfo: "/proc/self/mountinfo", findWait: time.Minute}, nil
}

func (n *Node) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: n.hostNQN}, nil
}

func (n *Node) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	var caps []*csi.NodeServiceCapability
	for _, t := range []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	} {
		caps = append(caps, &csi.NodeServiceCapability{Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{Type: t}}})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

func (n *Node) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "a volume ID and a staging path are both needed")
	}
	if err := checkCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}
	pc := req.GetPublishContext()
	defer n.locks.lock("volume/" + req.GetVolumeId())()
	if err := n.connect(ctx, pc); err != nil {
		return nil, err
	}
	dev, err := n.waitDevice(ctx, pc[ctxNGUID], pc[ctxUUID], pc[ctxNQN])
	if err != nil {
		return nil, err
	}
	mnt := req.GetVolumeCapability().GetMount()
	if mnt == nil {
		return &csi.NodeStageVolumeResponse{}, nil // A block volume is bound to its target when published.
	}
	staging := req.GetStagingTargetPath()
	if src, ok, err := n.mountedAt(staging); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	} else if ok {
		if src != dev {
			return nil, status.Errorf(codes.AlreadyExists, "%s has %s mounted, not %s", staging, src, dev)
		}
		return &csi.NodeStageVolumeResponse{}, nil
	}
	fsType := mnt.GetFsType()
	if fsType == "" {
		fsType = "ext4"
	}
	if fsType != "ext4" && fsType != "xfs" {
		return nil, status.Errorf(codes.InvalidArgument, "filesystem %q: this driver makes ext4 or xfs", fsType)
	}
	have, err := n.fsOn(ctx, dev)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	switch {
	case have == "":
		if _, err := n.run(ctx, "mkfs."+fsType, mkfsArgs(fsType, dev)...); err != nil {
			return nil, status.Errorf(codes.Internal, "formatting %s: %v", dev, err)
		}
	case have != fsType:
		return nil, status.Errorf(codes.FailedPrecondition, "%s already has %s on it, not %s", dev, have, fsType)
	}
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	args := []string{"-t", fsType}
	if flags := mnt.GetMountFlags(); len(flags) > 0 {
		args = append(args, "-o", strings.Join(flags, ","))
	}
	if _, err := n.run(ctx, "mount", append(args, dev, staging)...); err != nil {
		return nil, status.Errorf(codes.Internal, "mounting %s: %v", dev, err)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func mkfsArgs(fsType, dev string) []string {
	if fsType == "xfs" {
		return []string{"-q", dev}
	}
	return []string{"-q", "-F", dev}
}

func (n *Node) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "a volume ID and a staging path are both needed")
	}
	defer n.locks.lock("volume/" + req.GetVolumeId())()
	if err := n.unmount(ctx, req.GetStagingTargetPath()); err != nil {
		return nil, err
	}
	// A volume shared by several nodes has a subsystem of its own, which goes when the last node leaves: disconnect
	// from it, or the kernel keeps trying to reconnect. A node's own subsystem stays, for its other volumes.
	shared := n.cfg.sharedSubsystem(req.GetVolumeId())
	defer n.locks.lock("subsystem/" + shared)()
	present, err := n.hasSubsystem(shared)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if present {
		if _, err := n.run(ctx, "nvme", "disconnect", "-n", shared); err != nil {
			return nil, status.Errorf(codes.Internal, "disconnecting from %s: %v", shared, err)
		}
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (n *Node) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if req.GetVolumeId() == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "a volume ID and a target path are both needed")
	}
	if err := checkCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}
	defer n.locks.lock("volume/" + req.GetVolumeId())()
	if _, ok, err := n.mountedAt(target); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	} else if ok {
		return &csi.NodePublishVolumeResponse{}, nil
	}
	var src string
	if req.GetVolumeCapability().GetBlock() != nil {
		pc := req.GetPublishContext()
		dev, err := n.findDevice(pc[ctxNGUID], pc[ctxUUID], pc[ctxNQN])
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if dev == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s isn't connected: stage it first", req.GetVolumeId())
		}
		src = dev
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		f, err := os.OpenFile(target, os.O_CREATE, 0o640)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		f.Close()
	} else {
		src = req.GetStagingTargetPath()
		if src == "" {
			return nil, status.Error(codes.InvalidArgument, "no staging path")
		}
		if err := os.MkdirAll(target, 0o750); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if _, err := n.run(ctx, "mount", "--bind", src, target); err != nil {
		return nil, status.Errorf(codes.Internal, "binding %s to %s: %v", src, target, err)
	}
	if req.GetReadonly() {
		if _, err := n.run(ctx, "mount", "-o", "remount,bind,ro", target); err != nil {
			n.run(ctx, "umount", target)
			return nil, status.Errorf(codes.Internal, "making %s read-only: %v", target, err)
		}
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (n *Node) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "a volume ID and a target path are both needed")
	}
	defer n.locks.lock("volume/" + req.GetVolumeId())()
	if err := n.unmount(ctx, req.GetTargetPath()); err != nil {
		return nil, err
	}
	if err := os.Remove(req.GetTargetPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (n *Node) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	path := req.GetVolumePath()
	if req.GetVolumeId() == "" || path == "" {
		return nil, status.Error(codes.InvalidArgument, "a volume ID and a path are both needed")
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "%s isn't there", path)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !info.IsDir() {
		out, err := n.run(ctx, "blockdev", "--getsize64", path)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		var total int64
		fmt.Sscan(string(out), &total)
		return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{{Unit: csi.VolumeUsage_BYTES, Total: total}}}, nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	bs := int64(st.Bsize)
	return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
		{Unit: csi.VolumeUsage_BYTES, Total: int64(st.Blocks) * bs, Available: int64(st.Bavail) * bs, Used: int64(st.Blocks-st.Bfree) * bs},
		{Unit: csi.VolumeUsage_INODES, Total: int64(st.Files), Available: int64(st.Ffree), Used: int64(st.Files - st.Ffree)},
	}}, nil
}

func (n *Node) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	path := req.GetVolumePath()
	if req.GetVolumeId() == "" || path == "" {
		return nil, status.Error(codes.InvalidArgument, "a volume ID and a path are both needed")
	}
	if req.GetVolumeCapability().GetBlock() != nil {
		return &csi.NodeExpandVolumeResponse{}, nil
	}
	dev, ok, err := n.mountedAt(path)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "nothing is mounted at %s", path)
	}
	// nvweaver tells connected hosts when a namespace grows, so the kernel already has the new size.
	fsType, err := n.fsOn(ctx, dev)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	switch fsType {
	case "xfs":
		_, err = n.run(ctx, "xfs_growfs", path)
	case "ext4", "ext3", "ext2":
		_, err = n.run(ctx, "resize2fs", dev)
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "can't grow %q on %s", fsType, dev)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "growing the filesystem on %s: %v", dev, err)
	}
	return &csi.NodeExpandVolumeResponse{}, nil
}

// connect connects to the subsystem in the publish context, unless a live controller already is.
func (n *Node) connect(ctx context.Context, pc map[string]string) error {
	nqn, transport, addr, svc := pc[ctxNQN], pc[ctxTransport], pc[ctxAddress], pc[ctxService]
	if nqn == "" || addr == "" || svc == "" {
		return status.Error(codes.InvalidArgument, "the publish context has no subsystem to connect to")
	}
	if transport == "" {
		transport = "tcp"
	}
	defer n.locks.lock("subsystem/" + nqn)()
	live, err := n.connected(nqn)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if live {
		return nil
	}
	out, err := n.run(ctx, "nvme", "connect", "-t", transport, "-a", addr, "-s", svc, "-n", nqn, "-q", n.hostNQN)
	if err != nil && !strings.Contains(string(out), "already connected") {
		return status.Errorf(codes.Unavailable, "connecting to %s at %s:%s: %v", nqn, addr, svc, err)
	}
	return nil
}

// hasSubsystem reports whether the node has the subsystem at all, connected or still trying to reconnect.
func (n *Node) hasSubsystem(nqn string) (bool, error) {
	subs, err := filepath.Glob(filepath.Join(n.sys, "class/nvme-subsystem/*"))
	if err != nil {
		return false, err
	}
	for _, s := range subs {
		if readTrim(filepath.Join(s, "subsysnqn")) == nqn {
			return true, nil
		}
	}
	return false, nil
}

// connected reports whether a controller of the subsystem is live.
func (n *Node) connected(nqn string) (bool, error) {
	subs, err := filepath.Glob(filepath.Join(n.sys, "class/nvme-subsystem/*"))
	if err != nil {
		return false, err
	}
	for _, s := range subs {
		if readTrim(filepath.Join(s, "subsysnqn")) != nqn {
			continue
		}
		ctrls, _ := filepath.Glob(filepath.Join(s, "nvme[0-9]*"))
		for _, c := range ctrls {
			if regexp.MustCompile(`^nvme[0-9]+$`).MatchString(filepath.Base(c)) && readTrim(filepath.Join(c, "state")) == "live" {
				return true, nil
			}
		}
	}
	return false, nil
}

var namespaceRE = regexp.MustCompile(`^nvme[0-9]+n[0-9]+$`)

// findDevice finds the block device with a volume's identity, among those of
// the subsystem, by NGUID, or UUID if there's no NGUID. Path devices such as
// nvme0c0n1, which have no device node under native multipath, are skipped.
func (n *Node) findDevice(nguid, uuid, nqn string) (string, error) {
	want, key := normalID(nguid), "nguid"
	if want == "" {
		want, key = normalID(uuid), "uuid"
	}
	if want == "" {
		return "", errors.New("the publish context has no NGUID or UUID to find the volume by")
	}
	blocks, err := filepath.Glob(filepath.Join(n.sys, "block/nvme*"))
	if err != nil {
		return "", err
	}
	for _, b := range blocks {
		name := filepath.Base(b)
		if !namespaceRE.MatchString(name) || normalID(readTrim(filepath.Join(b, key))) != want {
			continue
		}
		// Belonging to another subsystem, the same identity would be another export of the same volume.
		if nqn != "" {
			if got := readTrim(filepath.Join(b, "device/subsysnqn")); got != "" && got != nqn {
				continue
			}
		}
		return "/dev/" + name, nil
	}
	return "", nil
}

// waitDevice waits for the volume's device to appear, as the namespace can take a moment after connecting.
func (n *Node) waitDevice(ctx context.Context, nguid, uuid, nqn string) (string, error) {
	deadline := time.Now().Add(n.findWait)
	for {
		dev, err := n.findDevice(nguid, uuid, nqn)
		if err != nil {
			return "", status.Error(codes.InvalidArgument, err.Error())
		}
		if dev != "" {
			return dev, nil
		}
		if time.Now().After(deadline) {
			return "", status.Errorf(codes.DeadlineExceeded, "no device with NGUID %q or UUID %q appeared from %s", nguid, uuid, nqn)
		}
		select {
		case <-ctx.Done():
			return "", status.FromContextError(ctx.Err()).Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func normalID(s string) string {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
	if strings.Trim(s, "0") == "" {
		return ""
	}
	return s
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// fsOn says which filesystem is on a device, or "" if none.
func (n *Node) fsOn(ctx context.Context, dev string) (string, error) {
	out, err := n.run(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", dev)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 2 {
			return "", nil // Nothing blkid recognizes.
		}
		if strings.Contains(err.Error(), "exit status 2") {
			return "", nil
		}
		return "", fmt.Errorf("reading what's on %s: %w", dev, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (n *Node) unmount(ctx context.Context, path string) error {
	if _, ok, err := n.mountedAt(path); err != nil {
		return status.Error(codes.Internal, err.Error())
	} else if !ok {
		return nil
	}
	if _, err := n.run(ctx, "umount", path); err != nil {
		return status.Errorf(codes.Internal, "unmounting %s: %v", path, err)
	}
	return nil
}

// mountedAt reports whether something is mounted at path, and what: the device,
// or for a bind mount of a device, the device it was bound from.
func (n *Node) mountedAt(path string) (string, bool, error) {
	b, err := os.ReadFile(n.mountinfo)
	if err != nil {
		return "", false, err
	}
	clean := filepath.Clean(path)
	found, src := false, ""
	for _, line := range strings.Split(string(b), "\n") {
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		f := strings.Fields(pre)
		if len(f) < 5 || unescapeMount(f[4]) != clean {
			continue
		}
		// The last mount at a path is the one in use.
		found, src = true, ""
		if g := strings.Fields(post); len(g) >= 2 {
			src = unescapeMount(g[1])
			if strings.HasPrefix(src, "devtmpfs") || g[0] == "devtmpfs" {
				src = "/dev" + unescapeMount(f[3])
			}
		}
	}
	return src, found, nil
}

// unescapeMount undoes mountinfo's octal escapes, such as \040 for a space.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
