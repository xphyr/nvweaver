package driver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"nvweaver/internal/client"
)

// groupAPI is nvweaver as the group controller sees it: volumes a, b and c, with a exported to node A, b to node B, and
// c not exported.
type groupAPI struct {
	vols    map[string]*client.Volume
	subs    map[string]*client.Subsystem
	snaps   map[string]*client.Snapshot
	groups  map[string]*client.SnapshotGroup
	created int
}

func newGroupAPI() *groupAPI {
	a := &groupAPI{vols: map[string]*client.Volume{}, subs: map[string]*client.Subsystem{},
		snaps: map[string]*client.Snapshot{}, groups: map[string]*client.SnapshotGroup{}}
	for v, host := range map[string]string{"a": nodeA, "b": nodeB, "c": ""} {
		a.vols[v] = &client.Volume{Name: v, SizeBytes: 4096}
		if host != "" {
			sub := testCfg.nodeSubsystem(host)
			a.vols[v].MappedTo = &client.VolumeMapping{Subsystem: sub, NSID: 1}
			a.subs[sub] = &client.Subsystem{NQN: sub, AllowedHosts: []string{host}}
		}
	}
	return a
}

func (a *groupAPI) GetVolume(_ context.Context, n string) (*client.Volume, error) {
	if v := a.vols[n]; v != nil {
		return v, nil
	}
	return nil, &client.Error{Status: http.StatusNotFound}
}
func (a *groupAPI) GetSubsystem(_ context.Context, n string) (*client.Subsystem, error) {
	if s := a.subs[n]; s != nil {
		return s, nil
	}
	return nil, &client.Error{Status: http.StatusNotFound}
}
func (a *groupAPI) GetSnapshot(_ context.Context, n string) (*client.Snapshot, error) {
	if s := a.snaps[n]; s != nil {
		return s, nil
	}
	return nil, &client.Error{Status: http.StatusNotFound}
}
func (a *groupAPI) GetSnapshotGroup(_ context.Context, n string) (*client.SnapshotGroup, error) {
	if g := a.groups[n]; g != nil {
		return g, nil
	}
	return nil, &client.Error{Status: http.StatusNotFound}
}
func (a *groupAPI) CreateSnapshotGroup(_ context.Context, in client.SnapshotGroupCreate) (*client.SnapshotGroup, error) {
	a.created++
	g := &client.SnapshotGroup{Name: in.Name, Volumes: in.Volumes, CreatedAt: "2026-11-10T22:00:00Z"}
	for i, v := range in.Volumes {
		name := in.Name + "-" + string(rune('1'+i))
		a.snaps[name] = &client.Snapshot{Name: name, Volume: v, SizeBytes: 4096, CreatedAt: g.CreatedAt, Group: in.Name}
		g.Snapshots = append(g.Snapshots, name)
	}
	a.groups[in.Name] = g
	return g, nil
}
func (a *groupAPI) DeleteSnapshotGroup(_ context.Context, n string) error {
	g := a.groups[n]
	if g == nil {
		return &client.Error{Status: http.StatusNotFound}
	}
	for _, s := range g.Snapshots {
		delete(a.snaps, s)
	}
	delete(a.groups, n)
	return nil
}

// groupController has nodes A and B in the cluster, and C, which uses none of the volumes. canPause says which can be
// paused; asked records which were asked.
func groupController(api *groupAPI, canPause map[string]bool, asked *[]string) *GroupController {
	nodes := func(context.Context) (map[string]string, error) {
		return map[string]string{nodeA: "worker-a", nodeB: "worker-b", "nqn.2014-08.org.nvmexpress:uuid:cccc": "worker-c"}, nil
	}
	pausable := func(_ context.Context, node string) (bool, string, error) {
		*asked = append(*asked, node)
		if canPause[node] {
			return true, "", nil
		}
		return false, "native NVMe multipath is off", nil
	}
	return NewGroupController(testCfg, api, nodes, pausable)
}

func createGroup(g *GroupController, name string, vols ...string) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	return g.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{Name: name, SourceVolumeIds: vols})
}

func TestAGroupSnapshotIsTaken(t *testing.T) {
	api := newGroupAPI()
	var asked []string
	g := groupController(api, map[string]bool{"worker-a": true, "worker-b": true}, &asked)
	resp, err := createGroup(g, "gs-1", "a", "b", "c")
	if err != nil {
		t.Fatal(err)
	}
	gs := resp.GetGroupSnapshot()
	if gs.GetGroupSnapshotId() != "lab-gs-1" || len(gs.GetSnapshots()) != 3 || !gs.GetReadyToUse() {
		t.Fatalf("group snapshot %v", gs)
	}
	for i, s := range gs.GetSnapshots() {
		if s.GetGroupSnapshotId() != "lab-gs-1" || s.GetSourceVolumeId() != []string{"a", "b", "c"}[i] || s.GetSizeBytes() != 4096 {
			t.Errorf("snapshot %d: %v", i, s)
		}
	}
	slices.Sort(asked)
	if !slices.Equal(asked, []string{"worker-a", "worker-b"}) {
		t.Errorf("asked %v whether they can be paused, want only the nodes the volumes are on", asked)
	}
	// A retry, with the volumes in another order, is the same group, and nothing more is taken.
	if again, err := createGroup(g, "gs-1", "c", "b", "a"); err != nil || again.GetGroupSnapshot().GetGroupSnapshotId() != "lab-gs-1" || api.created != 1 {
		t.Errorf("a retry: %v, %v, %d taken", again, err, api.created)
	}
	if _, err := createGroup(g, "gs-1", "a"); status.Code(err) != codes.AlreadyExists {
		t.Errorf("the same name for other volumes: %v", err)
	}
	got, err := g.GetVolumeGroupSnapshot(context.Background(), &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "lab-gs-1"})
	if err != nil || len(got.GetGroupSnapshot().GetSnapshots()) != 3 {
		t.Errorf("get: %v, %v", got, err)
	}
	if _, err := g.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: "lab-gs-1", SnapshotIds: []string{"lab-gs-1-1"}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleting with only some of its snapshots: %v", err)
	}
	for range 2 { // Again is fine: it's gone.
		if _, err := g.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: "lab-gs-1", SnapshotIds: []string{"lab-gs-1-3", "lab-gs-1-1", "lab-gs-1-2"}}); err != nil {
			t.Errorf("delete: %v", err)
		}
	}
	if len(api.snaps) != 0 {
		t.Errorf("left %v", api.snaps)
	}
}

// A group whose volumes are on a node that can't be paused is refused, naming the node, before nvweaver is asked. A
// node that uses none of them doesn't matter.
func TestAGroupSnapshotThatWouldPauseANodeThatCantBeIsRefused(t *testing.T) {
	api := newGroupAPI()
	var asked []string
	g := groupController(api, map[string]bool{"worker-a": true, "worker-b": false}, &asked)
	_, err := createGroup(g, "gs-1", "a", "b")
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "worker-b") || !strings.Contains(err.Error(), "multipath is off") {
		t.Errorf("got %v", err)
	}
	if api.created != 0 {
		t.Error("nvweaver was asked anyway")
	}
	// worker-c can't be paused either, but uses neither.
	g = groupController(api, map[string]bool{"worker-a": true}, &asked)
	if _, err := createGroup(g, "gs-2", "a", "c"); err != nil {
		t.Errorf("a node using none of the volumes stopped it: %v", err)
	}
}

// Volumes that aren't exported need no nodes asked, so it works even when the nodes can't be listed.
func TestAGroupSnapshotOfVolumesNotExported(t *testing.T) {
	api := newGroupAPI()
	g := NewGroupController(testCfg, api,
		func(context.Context) (map[string]string, error) { return nil, errors.New("not in a cluster") },
		func(context.Context, string) (bool, string, error) { return false, "", errors.New("not in a cluster") })
	if _, err := createGroup(g, "gs-1", "c"); err != nil {
		t.Errorf("got %v", err)
	}
	// But an exported one does.
	if _, err := createGroup(g, "gs-2", "a"); status.Code(err) != codes.Unavailable {
		t.Errorf("with the nodes unknown: %v", err)
	}
}

func TestGroupSnapshotRequestsAreChecked(t *testing.T) {
	g := groupController(newGroupAPI(), nil, new([]string))
	for name, req := range map[string]*csi.CreateVolumeGroupSnapshotRequest{
		"no name":    {SourceVolumeIds: []string{"a"}},
		"no volumes": {Name: "gs"},
	} {
		if _, err := g.CreateVolumeGroupSnapshot(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := g.GetVolumeGroupSnapshot(context.Background(), &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("a missing group: %v", err)
	}
	caps, _ := g.GroupControllerGetCapabilities(context.Background(), nil)
	if len(caps.GetCapabilities()) != 1 {
		t.Errorf("capabilities %v", caps)
	}
}
