package driver

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"nvweaver/internal/client"
)

const (
	defaultSize = 1 << 30
	sizeAlign   = 1 << 20

	// StorageClass parameters.
	paramPool = "pool"
	paramPort = "port"

	// Volume context and publish context keys.
	ctxPort      = "port"
	ctxNQN       = "nqn"
	ctxTransport = "transport"
	ctxAddress   = "address"
	ctxService   = "service"
	ctxNGUID     = "nguid"
	ctxUUID      = "uuid"

	// Labels the driver puts on what it creates.
	labelCluster = "csi-cluster"
	labelName    = "csi-name"
)

// Controller provisions volumes and snapshots, and exports volumes to nodes:
// single-node volumes in their node's subsystem, and volumes that several nodes
// use at once each in a subsystem of its own. See decision 24.
type Controller struct {
	csi.UnimplementedControllerServer
	cfg   Config
	api   API
	locks locks
}

func NewController(cfg Config, api API) *Controller { return &Controller{cfg: cfg, api: api} }

func (c *Controller) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	var caps []*csi.ControllerServiceCapability
	for _, t := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
	} {
		caps = append(caps, &csi.ControllerServiceCapability{Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{Type: t}}})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

// size picks a volume's size from what's asked: at least the required bytes, rounded up to a MiB, and no more than the limit.
func size(r *csi.CapacityRange) (int64, error) {
	want, limit := r.GetRequiredBytes(), r.GetLimitBytes()
	if want < 0 || limit < 0 {
		return 0, status.Error(codes.InvalidArgument, "a negative size")
	}
	if want == 0 {
		want = defaultSize
		if limit > 0 && limit < want {
			want = limit
		}
	}
	n := (want + sizeAlign - 1) / sizeAlign * sizeAlign
	if limit > 0 && n > limit {
		n = limit / 4096 * 4096
		if n < want || n == 0 {
			return 0, status.Errorf(codes.OutOfRange, "%d bytes, rounded to a whole MiB or 4 KiB, is more than the limit of %d", want, limit)
		}
	}
	return n, nil
}

func (c *Controller) port(params map[string]string) (int, error) {
	p := c.cfg.Port
	if s, ok := params[paramPort]; ok {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return 0, status.Errorf(codes.InvalidArgument, "port %q must be a port's ID", s)
		}
		p = n
	}
	return p, nil
}

func (c *Controller) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "no name")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no volume capabilities")
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if err := checkCapability(cap); err != nil {
			return nil, err
		}
	}
	n, err := size(req.GetCapacityRange())
	if err != nil {
		return nil, err
	}
	port, err := c.port(req.GetParameters())
	if err != nil {
		return nil, err
	}
	for k := range req.GetParameters() {
		if k != paramPool && k != paramPort && !isKubernetesParam(k) {
			return nil, status.Errorf(codes.InvalidArgument, "unknown parameter %q: this driver takes %q and %q", k, paramPool, paramPort)
		}
	}
	name := scopedName(c.cfg.Cluster, req.GetName())
	defer c.locks.lock("volume/" + name)()

	in := client.VolumeCreate{
		Name:      name,
		SizeBytes: n,
		Labels:    map[string]string{labelCluster: c.cfg.Cluster, labelName: req.GetName()},
	}
	src := req.GetVolumeContentSource()
	switch {
	case src.GetSnapshot() != nil:
		snap, err := c.api.GetSnapshot(ctx, src.GetSnapshot().GetSnapshotId())
		if err != nil {
			return nil, apiError(err, "the source snapshot")
		}
		in.Source = &client.VolumeSource{Snapshot: snap.Name}
		if snap.SizeBytes > n {
			return nil, status.Errorf(codes.OutOfRange, "snapshot %s is %d bytes, more than the %d asked for", snap.Name, snap.SizeBytes, n)
		}
	case src.GetVolume() != nil:
		vol, err := c.api.GetVolume(ctx, src.GetVolume().GetVolumeId())
		if err != nil {
			return nil, apiError(err, "the source volume")
		}
		in.Source = &client.VolumeSource{Volume: vol.Name}
		if vol.SizeBytes > n {
			return nil, status.Errorf(codes.OutOfRange, "volume %s is %d bytes, more than the %d asked for", vol.Name, vol.SizeBytes, n)
		}
	default:
		// A clone stays in its source's pool, so only a new volume names one.
		in.Pool = req.GetParameters()[paramPool]
	}
	vol, err := c.api.CreateVolume(ctx, in)
	if client.IsConflict(err) {
		return nil, status.Errorf(codes.AlreadyExists, "volume %s exists, with a different size, source or pool", name)
	}
	if err != nil {
		return nil, apiError(err, "creating volume "+name)
	}
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:      vol.Name,
		CapacityBytes: vol.SizeBytes,
		VolumeContext: map[string]string{ctxPort: strconv.Itoa(port)},
		ContentSource: src,
	}}, nil
}

// isKubernetesParam reports parameters that Kubernetes adds, rather than the StorageClass's author.
func isKubernetesParam(k string) bool {
	return len(k) > len("csi.storage.k8s.io/") && k[:len("csi.storage.k8s.io/")] == "csi.storage.k8s.io/"
}

func (c *Controller) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	name := req.GetVolumeId()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "no volume ID")
	}
	defer c.locks.lock("volume/" + name)()
	vol, err := c.api.GetVolume(ctx, name)
	if client.IsNotFound(err) {
		return &csi.DeleteVolumeResponse{}, nil
	}
	if err != nil {
		return nil, apiError(err, "volume "+name)
	}
	if vol.MappedTo != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is still exported, by %s", name, vol.MappedTo.Subsystem)
	}
	if err := c.api.DeleteVolume(ctx, name); err != nil && !client.IsNotFound(err) {
		if client.IsConflict(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s: %v", name, err)
		}
		return nil, apiError(err, "deleting volume "+name)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (c *Controller) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "no volume ID")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no volume capabilities")
	}
	if _, err := c.api.GetVolume(ctx, req.GetVolumeId()); err != nil {
		return nil, apiError(err, "volume "+req.GetVolumeId())
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if err := checkCapability(cap); err != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: err.Error()}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeContext:      req.GetVolumeContext(),
		VolumeCapabilities: req.GetVolumeCapabilities(),
		Parameters:         req.GetParameters(),
	}}, nil
}

func (c *Controller) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	name, node := req.GetVolumeId(), req.GetNodeId()
	if name == "" || node == "" {
		return nil, status.Error(codes.InvalidArgument, "a volume ID and a node ID are both needed")
	}
	if err := checkCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(node, "nqn.") {
		return nil, status.Errorf(codes.NotFound, "node %q: a node's ID is its NVMe host NQN, and this isn't one", node)
	}
	port, err := c.port(req.GetVolumeContext())
	if err != nil {
		return nil, err
	}
	sub := c.cfg.nodeSubsystem(node)
	if shared(req.GetVolumeCapability()) {
		sub = c.cfg.sharedSubsystem(name)
	}
	// The subsystem first, then the volume, always in that order, so that two publishes can't wait on each other.
	defer c.locks.lock("subsystem/" + sub)()
	defer c.locks.lock("volume/" + name)()

	vol, err := c.api.GetVolume(ctx, name)
	if err != nil {
		return nil, apiError(err, "volume "+name)
	}
	if vol.MappedTo != nil && vol.MappedTo.Subsystem != sub {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is published elsewhere, in %s", name, vol.MappedTo.Subsystem)
	}
	p, err := c.api.GetPort(ctx, port)
	if err != nil {
		return nil, apiError(err, fmt.Sprintf("port %d", port))
	}
	if !routable(p.Address) {
		return nil, status.Errorf(codes.FailedPrecondition, "port %d listens on %q, which no node can connect to: "+
			"give the class a port with an address the nodes can route to", port, p.Address)
	}
	if err := c.ensureHost(ctx, node); err != nil {
		return nil, err
	}
	if err := c.ensureSubsystem(ctx, sub, node, port); err != nil {
		return nil, err
	}
	if vol.MappedTo == nil {
		if _, err := c.api.CreateNamespace(ctx, sub, client.NamespaceCreate{Volume: name}); err != nil {
			return nil, apiError(err, fmt.Sprintf("exporting volume %s in %s", name, sub))
		}
	}
	return &csi.ControllerPublishVolumeResponse{PublishContext: map[string]string{
		ctxNQN:       sub,
		ctxTransport: p.Transport,
		ctxAddress:   p.Address,
		ctxService:   strconv.Itoa(p.Service),
		ctxNGUID:     vol.NGUID,
		ctxUUID:      vol.UUID,
	}}, nil
}

func (c *Controller) ensureHost(ctx context.Context, node string) error {
	// Read first: a create that finds it there already still waits for nvweaver's lock, and every publish would make one.
	_, err := c.api.GetHost(ctx, node)
	if err == nil {
		return nil
	}
	if !client.IsNotFound(err) {
		return apiError(err, "host "+node)
	}
	// Labelled with the cluster; the node janitor adds the node's name, which only the cluster knows.
	_, err = c.api.CreateHost(ctx, client.HostCreate{NQN: node, Labels: map[string]string{labelCluster: c.cfg.Cluster}})
	if err == nil || client.IsConflict(err) {
		return nil
	}
	return apiError(err, "host "+node)
}

// ensureSubsystem makes sure the subsystem exists, lets the node in, and is offered on the port.
func (c *Controller) ensureSubsystem(ctx context.Context, sub, node string, port int) error {
	// Read first, as for hosts: a node's subsystem is made once, for its first volume.
	s, err := c.api.GetSubsystem(ctx, sub)
	if client.IsNotFound(err) {
		_, err = c.api.CreateSubsystem(ctx, client.SubsystemCreate{NQN: sub, AllowedHosts: []string{node}, Ports: []int{port}})
		if err == nil {
			return nil
		}
		if !client.IsConflict(err) {
			return apiError(err, "subsystem "+sub)
		}
		// Made since the read, by another publish: check it as if it had been there.
		s, err = c.api.GetSubsystem(ctx, sub)
	}
	if err != nil {
		return apiError(err, "subsystem "+sub)
	}
	if !slices.Contains(s.AllowedHosts, node) {
		if err := c.api.AllowHost(ctx, sub, node); err != nil && !client.IsConflict(err) {
			return apiError(err, "allowing "+node+" on "+sub)
		}
	}
	if !slices.Contains(s.Ports, port) {
		if err := c.api.AttachPort(ctx, sub, port); err != nil && !client.IsConflict(err) {
			return apiError(err, fmt.Sprintf("offering %s on port %d", sub, port))
		}
	}
	return nil
}

func (c *Controller) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	name, node := req.GetVolumeId(), req.GetNodeId()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "no volume ID")
	}
	vol, err := c.api.GetVolume(ctx, name)
	if client.IsNotFound(err) {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	if err != nil {
		return nil, apiError(err, "volume "+name)
	}
	if vol.MappedTo == nil {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	sub := vol.MappedTo.Subsystem
	defer c.locks.lock("subsystem/" + sub)()
	defer c.locks.lock("volume/" + name)()

	switch sub {
	case c.cfg.sharedSubsystem(name):
		// Others may still use it: take this node off, and the volume out only when nobody's left.
		if node == "" {
			return nil, status.Error(codes.InvalidArgument, "a shared volume needs the node to unpublish it from")
		}
		s, err := c.api.GetSubsystem(ctx, sub)
		if client.IsNotFound(err) {
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		if err != nil {
			return nil, apiError(err, "subsystem "+sub)
		}
		if slices.Contains(s.AllowedHosts, node) {
			if err := c.api.DenyHost(ctx, sub, node); err != nil && !client.IsNotFound(err) {
				return nil, apiError(err, "removing "+node+" from "+sub)
			}
		}
		if len(s.AllowedHosts) > 1 || len(s.AllowedHosts) == 1 && s.AllowedHosts[0] != node {
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		if err := c.api.DeleteNamespace(ctx, sub, vol.MappedTo.NSID, false); err != nil && !client.IsNotFound(err) {
			return nil, apiError(err, "un-exporting "+name)
		}
		if err := c.api.DeleteSubsystem(ctx, sub); err != nil && !client.IsNotFound(err) {
			return nil, apiError(err, "deleting "+sub)
		}
	default:
		if node != "" && sub != c.cfg.nodeSubsystem(node) {
			// It's published to another node: nothing to do for this one.
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		if err := c.api.DeleteNamespace(ctx, sub, vol.MappedTo.NSID, false); err != nil && !client.IsNotFound(err) {
			return nil, apiError(err, "un-exporting "+name)
		}
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (c *Controller) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	name := req.GetVolumeId()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "no volume ID")
	}
	n, err := size(req.GetCapacityRange())
	if err != nil {
		return nil, err
	}
	defer c.locks.lock("volume/" + name)()
	vol, err := c.api.GetVolume(ctx, name)
	if err != nil {
		return nil, apiError(err, "volume "+name)
	}
	if vol.SizeBytes < n {
		if vol, err = c.api.UpdateVolume(ctx, name, client.VolumeUpdate{SizeBytes: &n}); err != nil {
			return nil, apiError(err, "growing volume "+name)
		}
	}
	// A filesystem has to grow too, on the node. A raw block volume is done.
	fs := req.GetVolumeCapability() == nil || req.GetVolumeCapability().GetMount() != nil
	return &csi.ControllerExpandVolumeResponse{CapacityBytes: vol.SizeBytes, NodeExpansionRequired: fs}, nil
}

func (c *Controller) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	if req.GetName() == "" || req.GetSourceVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "a name and a source volume are both needed")
	}
	name := scopedName(c.cfg.Cluster, req.GetName())
	defer c.locks.lock("snapshot/" + name)()
	snap, err := c.api.CreateSnapshot(ctx, client.SnapshotCreate{Name: name, Volume: req.GetSourceVolumeId()})
	if client.IsConflict(err) {
		existing, gerr := c.api.GetSnapshot(ctx, name)
		if gerr != nil || existing.Volume != req.GetSourceVolumeId() {
			return nil, status.Errorf(codes.AlreadyExists, "snapshot %s exists, of another volume", name)
		}
		snap, err = existing, nil
	}
	if err != nil {
		return nil, apiError(err, "snapshotting "+req.GetSourceVolumeId())
	}
	return &csi.CreateSnapshotResponse{Snapshot: csiSnapshot(snap)}, nil
}

func csiSnapshot(s *client.Snapshot) *csi.Snapshot {
	out := &csi.Snapshot{SnapshotId: s.Name, SourceVolumeId: s.Volume, SizeBytes: s.SizeBytes, ReadyToUse: true}
	if t, err := time.Parse(time.RFC3339Nano, s.CreatedAt); err == nil {
		out.CreationTime = timestamppb.New(t)
	} else {
		out.CreationTime = timestamppb.Now()
	}
	return out
}

func (c *Controller) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "no snapshot ID")
	}
	defer c.locks.lock("snapshot/" + req.GetSnapshotId())()
	if err := c.api.DeleteSnapshot(ctx, req.GetSnapshotId()); err != nil && !client.IsNotFound(err) {
		if client.IsConflict(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "snapshot %s: %v", req.GetSnapshotId(), err)
		}
		return nil, apiError(err, "deleting snapshot "+req.GetSnapshotId())
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

// listPageSize is how many the API is asked for at a time, while looking for this cluster's own.
const listPageSize = 500

// pageStart checks a CSI starting token, which is the last name of the page before, and so one of this cluster's names.
func (c *Controller) pageStart(token string) (string, error) {
	if token != "" && !strings.HasPrefix(token, c.cfg.Cluster+"-") {
		return "", status.Errorf(codes.Aborted, "starting token %q isn't one this driver gave", token)
	}
	return token, nil
}

// ListVolumes lists this cluster's volumes, in name order, a page at a time.
func (c *Controller) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	if req.GetMaxEntries() < 0 {
		return nil, status.Error(codes.InvalidArgument, "max_entries is negative")
	}
	after, err := c.pageStart(req.GetStartingToken())
	if err != nil {
		return nil, err
	}
	max := int(req.GetMaxEntries())
	out := &csi.ListVolumesResponse{}
	for {
		page, err := c.api.VolumesPage(ctx, "", listPageSize, after)
		if err != nil {
			return nil, apiError(err, "listing volumes")
		}
		for _, v := range page.Items {
			if !strings.HasPrefix(v.Name, c.cfg.Cluster+"-") {
				continue
			}
			if max > 0 && len(out.Entries) == max {
				out.NextToken = out.Entries[max-1].Volume.VolumeId
				return out, nil
			}
			out.Entries = append(out.Entries, &csi.ListVolumesResponse_Entry{Volume: &csi.Volume{VolumeId: v.Name, CapacityBytes: v.SizeBytes}})
		}
		if page.NextAfter == "" {
			return out, nil
		}
		after = page.NextAfter
	}
}

// ListSnapshots lists this cluster's snapshots, in name order, a page at a time, or one snapshot, or one volume's.
func (c *Controller) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	if req.GetMaxEntries() < 0 {
		return nil, status.Error(codes.InvalidArgument, "max_entries is negative")
	}
	mine := func(name string) bool { return strings.HasPrefix(name, c.cfg.Cluster+"-") }
	if id := req.GetSnapshotId(); id != "" {
		s, err := c.api.GetSnapshot(ctx, id)
		if client.IsNotFound(err) || err == nil && !mine(s.Name) ||
			err == nil && req.GetSourceVolumeId() != "" && s.Volume != req.GetSourceVolumeId() {
			return &csi.ListSnapshotsResponse{}, nil
		}
		if err != nil {
			return nil, apiError(err, "snapshot "+id)
		}
		return &csi.ListSnapshotsResponse{Entries: []*csi.ListSnapshotsResponse_Entry{{Snapshot: csiSnapshot(s)}}}, nil
	}
	after, err := c.pageStart(req.GetStartingToken())
	if err != nil {
		return nil, err
	}
	max := int(req.GetMaxEntries())
	out := &csi.ListSnapshotsResponse{}
	for {
		page, err := c.api.SnapshotsPage(ctx, req.GetSourceVolumeId(), listPageSize, after)
		if client.IsNotFound(err) {
			return out, nil // A volume that isn't there has no snapshots.
		}
		if err != nil {
			return nil, apiError(err, "listing snapshots")
		}
		for i := range page.Items {
			s := &page.Items[i]
			if !mine(s.Name) {
				continue
			}
			if max > 0 && len(out.Entries) == max {
				out.NextToken = out.Entries[max-1].Snapshot.SnapshotId
				return out, nil
			}
			out.Entries = append(out.Entries, &csi.ListSnapshotsResponse_Entry{Snapshot: csiSnapshot(s)})
		}
		if page.NextAfter == "" {
			return out, nil
		}
		after = page.NextAfter
	}
}
