package driver

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"nvweaver/internal/client"
)

// GroupController is CSI's group snapshots: VolumeGroupSnapshot. nvweaver pauses hosts' I/O to the volumes while it
// snapshots them, so before asking, this checks that every node the volumes are exported to can be paused, and refuses
// otherwise. See decision 56.
type GroupController struct {
	csi.UnimplementedGroupControllerServer
	cfg      Config
	api      GroupAPI
	nodes    NodeLister
	pausable PauseReader
	locks    locks
	// supported is set once nvweaver has said it has snapshot groups, which a version before them doesn't.
	supported atomic.Bool
}

// GroupAPI is what the group controller uses of nvweaver's API.
type GroupAPI interface {
	System(ctx context.Context) (*client.System, error)
	GetVolume(ctx context.Context, name string) (*client.Volume, error)
	GetSubsystem(ctx context.Context, nqn string) (*client.Subsystem, error)
	GetSnapshot(ctx context.Context, name string) (*client.Snapshot, error)
	GetSnapshotGroup(ctx context.Context, name string) (*client.SnapshotGroup, error)
	CreateSnapshotGroup(ctx context.Context, in client.SnapshotGroupCreate) (*client.SnapshotGroup, error)
	DeleteSnapshotGroup(ctx context.Context, name string) error
}

// NewGroupController returns the service. nodes maps host NQNs to nodes, and pausable says whether a node can be paused.
func NewGroupController(cfg Config, api GroupAPI, nodes NodeLister, pausable PauseReader) *GroupController {
	return &GroupController{cfg: cfg, api: api, nodes: nodes, pausable: pausable}
}

func (g *GroupController) GroupControllerGetCapabilities(context.Context, *csi.GroupControllerGetCapabilitiesRequest) (*csi.GroupControllerGetCapabilitiesResponse, error) {
	return &csi.GroupControllerGetCapabilitiesResponse{Capabilities: []*csi.GroupControllerServiceCapability{{
		Type: &csi.GroupControllerServiceCapability_Rpc{Rpc: &csi.GroupControllerServiceCapability_RPC{
			Type: csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT}},
	}}}, nil
}

// maxGroupName is nvweaver's limit on a group's name, as its snapshots are named from it.
const maxGroupName = 120

func sameVolumes(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

func (g *GroupController) CreateVolumeGroupSnapshot(ctx context.Context, req *csi.CreateVolumeGroupSnapshotRequest) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	if req.GetName() == "" || len(req.GetSourceVolumeIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "a name and source volumes are both needed")
	}
	name := scopedName(g.cfg.Cluster, req.GetName())
	if len(name) > maxGroupName {
		return nil, status.Errorf(codes.InvalidArgument, "group snapshot name %s is longer than %d", name, maxGroupName)
	}
	defer g.locks.lock("group/" + name)()
	if err := g.checkSupported(ctx); err != nil {
		return nil, err
	}
	// A retry, which may list the volumes in another order.
	if existing, err := g.api.GetSnapshotGroup(ctx, name); err == nil {
		if !sameVolumes(existing.Volumes, req.GetSourceVolumeIds()) {
			return nil, status.Errorf(codes.AlreadyExists, "group snapshot %s exists, of other volumes", name)
		}
		return g.response(ctx, existing)
	} else if !client.IsNotFound(err) {
		return nil, apiError(err, "group snapshot "+name)
	}
	if err := g.checkPausable(ctx, name, req.GetSourceVolumeIds()); err != nil {
		return nil, err
	}
	created, err := g.api.CreateSnapshotGroup(ctx, client.SnapshotGroupCreate{Name: name, Volumes: req.GetSourceVolumeIds()})
	if err != nil {
		return nil, apiError(err, "group snapshot "+name)
	}
	return g.response(ctx, created)
}

// checkSupported refuses a group snapshot, saying why, when nvweaver is a version without snapshot groups: it would answer
// 404, which reads as a group that isn't there.
func (g *GroupController) checkSupported(ctx context.Context) error {
	if g.supported.Load() {
		return nil
	}
	sys, err := g.api.System(ctx)
	if err != nil {
		return apiError(err, "nvweaver's system")
	}
	if !sys.Capabilities.SnapshotGroups {
		return status.Errorf(codes.FailedPrecondition,
			"nvweaver %s has no snapshot groups, which group snapshots need: upgrade it", sys.Version)
	}
	g.supported.Store(true)
	return nil
}

// checkPausable refuses a group whose volumes are exported to a node that can't be paused, or won't say, naming it. A
// node that uses none of them doesn't matter, nor does a host that's no longer a node.
func (g *GroupController) checkPausable(ctx context.Context, name string, volumes []string) error {
	var hosts []string
	for _, v := range volumes {
		vol, err := g.api.GetVolume(ctx, v)
		if err != nil {
			return apiError(err, "volume "+v)
		}
		if vol.MappedTo == nil {
			continue
		}
		sub, err := g.api.GetSubsystem(ctx, vol.MappedTo.Subsystem)
		if err != nil {
			return apiError(err, "where volume "+v+" is exported")
		}
		hosts = append(hosts, sub.AllowedHosts...)
	}
	if len(hosts) == 0 {
		return nil
	}
	nodes, err := g.nodes(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "can't tell which nodes use the volumes, so can't check that they can be paused: %v", err)
	}
	checked := map[string]bool{}
	for _, h := range hosts {
		node, ok := nodes[h]
		if !ok || checked[node] {
			continue
		}
		checked[node] = true
		ok, reason, err := g.pausable(ctx, node)
		if err != nil {
			return status.Errorf(codes.Unavailable, "can't tell whether node %s can be paused: %v", node, err)
		}
		if !ok {
			return status.Errorf(codes.FailedPrecondition,
				"group snapshot %s would pause I/O on node %s, which can't be paused: %s. See decision 56 and csi/README.md", name, node, reason)
		}
	}
	return nil
}

func (g *GroupController) response(ctx context.Context, grp *client.SnapshotGroup) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	out, err := g.groupSnapshot(ctx, grp)
	if err != nil {
		return nil, err
	}
	return &csi.CreateVolumeGroupSnapshotResponse{GroupSnapshot: out}, nil
}

// groupSnapshot describes a group as CSI does, with each of its snapshots, which are ordinary snapshots.
func (g *GroupController) groupSnapshot(ctx context.Context, grp *client.SnapshotGroup) (*csi.VolumeGroupSnapshot, error) {
	created := timestamppb.Now()
	if t, err := time.Parse(time.RFC3339Nano, grp.CreatedAt); err == nil {
		created = timestamppb.New(t)
	}
	out := &csi.VolumeGroupSnapshot{GroupSnapshotId: grp.Name, CreationTime: created, ReadyToUse: true}
	for _, name := range grp.Snapshots {
		snap, err := g.api.GetSnapshot(ctx, name)
		if err != nil {
			return nil, apiError(err, "group snapshot "+grp.Name+"'s snapshot "+name)
		}
		s := csiSnapshot(snap)
		s.GroupSnapshotId = grp.Name
		out.Snapshots = append(out.Snapshots, s)
	}
	return out, nil
}

func (g *GroupController) DeleteVolumeGroupSnapshot(ctx context.Context, req *csi.DeleteVolumeGroupSnapshotRequest) (*csi.DeleteVolumeGroupSnapshotResponse, error) {
	id := req.GetGroupSnapshotId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "no group snapshot ID")
	}
	defer g.locks.lock("group/" + id)()
	grp, err := g.api.GetSnapshotGroup(ctx, id)
	if client.IsNotFound(err) {
		return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
	}
	if err != nil {
		return nil, apiError(err, "group snapshot "+id)
	}
	// Its snapshots go with it, so the caller has to know which they are: CSI's rule.
	if ids := req.GetSnapshotIds(); len(ids) > 0 && !sameVolumes(ids, grp.Snapshots) {
		return nil, status.Errorf(codes.FailedPrecondition, "group snapshot %s has snapshots %v, not %v", id, grp.Snapshots, ids)
	}
	if err := g.api.DeleteSnapshotGroup(ctx, id); err != nil && !client.IsNotFound(err) {
		return nil, apiError(err, "deleting group snapshot "+id)
	}
	return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
}

func (g *GroupController) GetVolumeGroupSnapshot(ctx context.Context, req *csi.GetVolumeGroupSnapshotRequest) (*csi.GetVolumeGroupSnapshotResponse, error) {
	id := req.GetGroupSnapshotId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "no group snapshot ID")
	}
	grp, err := g.api.GetSnapshotGroup(ctx, id)
	if err != nil {
		return nil, apiError(err, "group snapshot "+id)
	}
	if ids := req.GetSnapshotIds(); len(ids) > 0 && !sameVolumes(ids, grp.Snapshots) {
		return nil, status.Errorf(codes.FailedPrecondition, "group snapshot %s has snapshots %v, not %v", id, grp.Snapshots, ids)
	}
	out, err := g.groupSnapshot(ctx, grp)
	if err != nil {
		return nil, err
	}
	return &csi.GetVolumeGroupSnapshotResponse{GroupSnapshot: out}, nil
}
