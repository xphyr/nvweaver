package driver

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"nvweaver/internal/client"
)

// fakeAPI keeps what nvweaver would, in memory, with its rules: a volume is
// exported once, names are unique, and a create again with other settings is a conflict.
type fakeAPI struct {
	mu         sync.Mutex
	hosts      map[string]bool
	subsystems map[string]*client.Subsystem
	volumes    map[string]*client.Volume
	snapshots  map[string]*client.Snapshot
	ports      map[int]*client.Port
	n          int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		hosts: map[string]bool{}, subsystems: map[string]*client.Subsystem{}, volumes: map[string]*client.Volume{},
		snapshots: map[string]*client.Snapshot{}, ports: map[int]*client.Port{1: {ID: 1, Transport: "tcp", Address: "10.98.0.1", Service: 4420}},
	}
}

func apiErr(code int, msg string) error { return &client.Error{Status: code, Message: msg} }

func (f *fakeAPI) Health(context.Context) (*client.Health, error) { return &client.Health{}, nil }
func (f *fakeAPI) GetPort(_ context.Context, id int) (*client.Port, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.ports[id]; ok {
		return p, nil
	}
	return nil, apiErr(http.StatusNotFound, "no port")
}
func (f *fakeAPI) GetHost(_ context.Context, nqn string) (*client.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hosts[nqn] {
		return &client.Host{NQN: nqn}, nil
	}
	return nil, apiErr(http.StatusNotFound, "no host")
}
func (f *fakeAPI) CreateHost(_ context.Context, in client.HostCreate) (*client.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hosts[in.NQN] {
		return nil, apiErr(http.StatusConflict, "host exists")
	}
	f.hosts[in.NQN] = true
	return &client.Host{NQN: in.NQN}, nil
}
func (f *fakeAPI) GetSubsystem(_ context.Context, nqn string) (*client.Subsystem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.subsystems[nqn]; ok {
		c := *s
		c.AllowedHosts = slices.Clone(s.AllowedHosts)
		return &c, nil
	}
	return nil, apiErr(http.StatusNotFound, "no subsystem")
}
func (f *fakeAPI) CreateSubsystem(_ context.Context, in client.SubsystemCreate) (*client.Subsystem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.subsystems[in.NQN]; ok {
		return nil, apiErr(http.StatusConflict, "subsystem exists")
	}
	for _, h := range in.AllowedHosts {
		if !f.hosts[h] {
			return nil, apiErr(http.StatusUnprocessableEntity, "no host "+h)
		}
	}
	f.subsystems[in.NQN] = &client.Subsystem{NQN: in.NQN, AllowedHosts: slices.Clone(in.AllowedHosts), Ports: slices.Clone(in.Ports)}
	return f.subsystems[in.NQN], nil
}
func (f *fakeAPI) DeleteSubsystem(_ context.Context, nqn string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subsystems[nqn]
	if !ok {
		return apiErr(http.StatusNotFound, "no subsystem")
	}
	if len(s.Namespaces) > 0 {
		return apiErr(http.StatusConflict, "has namespaces")
	}
	delete(f.subsystems, nqn)
	return nil
}
func (f *fakeAPI) AttachPort(_ context.Context, nqn string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subsystems[nqn].Ports = append(f.subsystems[nqn].Ports, port)
	return nil
}
func (f *fakeAPI) AllowHost(_ context.Context, nqn, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subsystems[nqn]
	if slices.Contains(s.AllowedHosts, host) {
		return apiErr(http.StatusConflict, "already allowed")
	}
	s.AllowedHosts = append(s.AllowedHosts, host)
	return nil
}
func (f *fakeAPI) DenyHost(_ context.Context, nqn, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subsystems[nqn]
	i := slices.Index(s.AllowedHosts, host)
	if i < 0 {
		return apiErr(http.StatusNotFound, "not allowed")
	}
	s.AllowedHosts = slices.Delete(s.AllowedHosts, i, i+1)
	return nil
}
func (f *fakeAPI) CreateNamespace(_ context.Context, nqn string, in client.NamespaceCreate) (*client.Namespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subsystems[nqn]
	if !ok {
		return nil, apiErr(http.StatusNotFound, "no subsystem")
	}
	v, ok := f.volumes[in.Volume]
	if !ok {
		return nil, apiErr(http.StatusNotFound, "no volume")
	}
	if v.MappedTo != nil {
		return nil, apiErr(http.StatusConflict, "already exported")
	}
	nsid := len(s.Namespaces) + 1
	ns := client.Namespace{NSID: nsid, Volume: in.Volume, NGUID: v.NGUID, UUID: v.UUID}
	s.Namespaces = append(s.Namespaces, ns)
	v.MappedTo = &client.VolumeMapping{Subsystem: nqn, NSID: nsid}
	return &ns, nil
}
func (f *fakeAPI) DeleteNamespace(_ context.Context, nqn string, nsid int, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subsystems[nqn]
	if !ok {
		return apiErr(http.StatusNotFound, "no subsystem")
	}
	i := slices.IndexFunc(s.Namespaces, func(n client.Namespace) bool { return n.NSID == nsid })
	if i < 0 {
		return apiErr(http.StatusNotFound, "no namespace")
	}
	f.volumes[s.Namespaces[i].Volume].MappedTo = nil
	s.Namespaces = slices.Delete(s.Namespaces, i, i+1)
	return nil
}
func (f *fakeAPI) GetVolume(_ context.Context, name string) (*client.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.volumes[name]; ok {
		c := *v
		return &c, nil
	}
	return nil, apiErr(http.StatusNotFound, "no volume")
}
func (f *fakeAPI) CreateVolume(_ context.Context, in client.VolumeCreate) (*client.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.volumes[in.Name]; ok {
		if v.SizeBytes != in.SizeBytes {
			return nil, apiErr(http.StatusConflict, "different settings")
		}
		c := *v
		return &c, nil
	}
	f.n++
	v := &client.Volume{Name: in.Name, Pool: in.Pool, SizeBytes: in.SizeBytes, Labels: in.Labels, Source: in.Source,
		NGUID: "0123456789abcdef0000000000" + string(rune('a'+f.n)) + "00001", UUID: "uuid"}
	f.volumes[in.Name] = v
	c := *v
	return &c, nil
}
func (f *fakeAPI) UpdateVolume(_ context.Context, name string, in client.VolumeUpdate) (*client.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.volumes[name]
	if in.SizeBytes != nil {
		v.SizeBytes = *in.SizeBytes
	}
	c := *v
	return &c, nil
}
func (f *fakeAPI) DeleteVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[name]; !ok {
		return apiErr(http.StatusNotFound, "no volume")
	}
	delete(f.volumes, name)
	return nil
}
func (f *fakeAPI) GetSnapshot(_ context.Context, name string) (*client.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.snapshots[name]; ok {
		c := *s
		return &c, nil
	}
	return nil, apiErr(http.StatusNotFound, "no snapshot")
}
func (f *fakeAPI) CreateSnapshot(_ context.Context, in client.SnapshotCreate) (*client.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.snapshots[in.Name]; ok {
		return nil, apiErr(http.StatusConflict, "exists")
	}
	v, ok := f.volumes[in.Volume]
	if !ok {
		return nil, apiErr(http.StatusNotFound, "no volume")
	}
	f.snapshots[in.Name] = &client.Snapshot{Name: in.Name, Volume: in.Volume, SizeBytes: v.SizeBytes, CreatedAt: "2026-11-09T12:00:00Z"}
	c := *f.snapshots[in.Name]
	return &c, nil
}
func (f *fakeAPI) DeleteSnapshot(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.snapshots[name]; !ok {
		return apiErr(http.StatusNotFound, "no snapshot")
	}
	delete(f.snapshots, name)
	return nil
}

var testCfg = Config{Name: DefaultName, Cluster: "lab", NQNPrefix: "nqn.2026-11.io.github.xphyr.nvweaver:csi", Port: 1}

func capOf(mode csi.VolumeCapability_AccessMode_Mode, block bool) *csi.VolumeCapability {
	c := &csi.VolumeCapability{AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode}}
	if block {
		c.AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
	} else {
		c.AccessType = &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}
	}
	return c
}

var rwo = capOf(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false)

func code(err error) codes.Code { return status.Code(err) }

func create(t *testing.T, c *Controller, name string, size int64, caps ...*csi.VolumeCapability) *csi.Volume {
	t.Helper()
	if len(caps) == 0 {
		caps = []*csi.VolumeCapability{rwo}
	}
	r, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: name, CapacityRange: &csi.CapacityRange{RequiredBytes: size}, VolumeCapabilities: caps})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return r.GetVolume()
}

func TestCreateIsIdempotentAndScopedToTheCluster(t *testing.T) {
	api := newFakeAPI()
	c := NewController(testCfg, api)
	v := create(t, c, "pvc-1", 5<<20+1)
	if v.VolumeId != "lab-pvc-1" || v.CapacityBytes != 6<<20 {
		t.Errorf("%s, %d bytes", v.VolumeId, v.CapacityBytes)
	}
	if again := create(t, c, "pvc-1", 5<<20+1); again.VolumeId != v.VolumeId {
		t.Errorf("a retry made %s", again.VolumeId)
	}
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "pvc-1", CapacityRange: &csi.CapacityRange{RequiredBytes: 100 << 20}, VolumeCapabilities: []*csi.VolumeCapability{rwo}})
	if code(err) != codes.AlreadyExists {
		t.Errorf("the same name with another size: %v", err)
	}
	if got := api.volumes["lab-pvc-1"].Labels; got[labelCluster] != "lab" || got[labelName] != "pvc-1" {
		t.Errorf("labels %v", got)
	}
}

func TestAFilesystemCantBeWrittenFromSeveralNodes(t *testing.T) {
	c := NewController(testCfg, newFakeAPI())
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "pvc-x", VolumeCapabilities: []*csi.VolumeCapability{capOf(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, false)}})
	if code(err) != codes.InvalidArgument {
		t.Errorf("RWX filesystem: %v", err)
	}
	create(t, c, "pvc-rwx-block", 1<<20, capOf(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, true))
}

func TestSizes(t *testing.T) {
	for _, c := range []struct {
		want, limit, got int64
		code             codes.Code
	}{
		{0, 0, 1 << 30, codes.OK},
		{1, 0, 1 << 20, codes.OK},
		{1 << 20, 1 << 20, 1 << 20, codes.OK},
		{4097, 8192, 8192, codes.OK},
		{8193, 8192, 0, codes.OutOfRange},
		{0, 1 << 20, 1 << 20, codes.OK},
	} {
		n, err := size(&csi.CapacityRange{RequiredBytes: c.want, LimitBytes: c.limit})
		if n != c.got || code(err) != c.code {
			t.Errorf("want %d, limit %d: %d, %v", c.want, c.limit, n, err)
		}
	}
}

const nodeA, nodeB = "nqn.2014-08.org.nvmexpress:uuid:aaaa", "nqn.2014-08.org.nvmexpress:uuid:bbbb"

func publish(c *Controller, vol, node string, cap *csi.VolumeCapability) (*csi.ControllerPublishVolumeResponse, error) {
	return c.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{VolumeId: vol, NodeId: node, VolumeCapability: cap, VolumeContext: map[string]string{ctxPort: "1"}})
}

func unpublish(c *Controller, vol, node string) error {
	_, err := c.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{VolumeId: vol, NodeId: node})
	return err
}

func TestAVolumeIsPublishedInItsNodesSubsystem(t *testing.T) {
	api := newFakeAPI()
	c := NewController(testCfg, api)
	a := create(t, c, "pvc-a", 1<<20).VolumeId
	b := create(t, c, "pvc-b", 1<<20).VolumeId
	ra, err := publish(c, a, nodeA, rwo)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := publish(c, b, nodeA, rwo)
	if err != nil {
		t.Fatal(err)
	}
	pc := ra.PublishContext
	if pc[ctxNQN] != rb.PublishContext[ctxNQN] || pc[ctxNQN] != testCfg.nodeSubsystem(nodeA) {
		t.Errorf("two volumes on one node: %s and %s", pc[ctxNQN], rb.PublishContext[ctxNQN])
	}
	if pc[ctxAddress] != "10.98.0.1" || pc[ctxService] != "4420" || pc[ctxTransport] != "tcp" || pc[ctxNGUID] == "" {
		t.Errorf("publish context %v", pc)
	}
	if s := api.subsystems[pc[ctxNQN]]; len(s.Namespaces) != 2 || !slices.Equal(s.AllowedHosts, []string{nodeA}) || !slices.Equal(s.Ports, []int{1}) {
		t.Errorf("subsystem %+v", s)
	}
	if _, err := publish(c, a, nodeA, rwo); err != nil {
		t.Errorf("publishing again: %v", err)
	}
	if _, err := publish(c, a, nodeB, rwo); code(err) != codes.FailedPrecondition {
		t.Errorf("a single-node volume to a second node: %v", err)
	}
	if _, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: a}); code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a published volume: %v", err)
	}
	if err := unpublish(c, a, nodeB); err != nil || api.volumes[a].MappedTo == nil {
		t.Errorf("unpublishing from a node it isn't on: %v, %v", err, api.volumes[a].MappedTo)
	}
	for range 2 {
		if err := unpublish(c, a, nodeA); err != nil {
			t.Errorf("unpublish: %v", err)
		}
	}
	if api.volumes[a].MappedTo != nil || len(api.subsystems[pc[ctxNQN]].Namespaces) != 1 {
		t.Errorf("after unpublishing: %v", api.volumes[a].MappedTo)
	}
	if _, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: a}); err != nil {
		t.Errorf("delete: %v", err)
	}
	if _, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: a}); err != nil {
		t.Errorf("delete again: %v", err)
	}
}

func TestASharedVolumeHasItsOwnSubsystem(t *testing.T) {
	api := newFakeAPI()
	c := NewController(testCfg, api)
	rwx := capOf(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, true)
	v := create(t, c, "pvc-shared", 1<<20, rwx).VolumeId
	ra, err := publish(c, v, nodeA, rwx)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := publish(c, v, nodeB, rwx)
	if err != nil {
		t.Fatal(err)
	}
	sub := testCfg.sharedSubsystem(v)
	if ra.PublishContext[ctxNQN] != sub || rb.PublishContext[ctxNQN] != sub {
		t.Errorf("%s and %s, want %s", ra.PublishContext[ctxNQN], rb.PublishContext[ctxNQN], sub)
	}
	if got := api.subsystems[sub].AllowedHosts; !slices.Equal(got, []string{nodeA, nodeB}) {
		t.Errorf("allowed %v", got)
	}
	if err := unpublish(c, v, nodeA); err != nil {
		t.Fatal(err)
	}
	if api.volumes[v].MappedTo == nil || !slices.Equal(api.subsystems[sub].AllowedHosts, []string{nodeB}) {
		t.Errorf("after one node left: %v, %v", api.volumes[v].MappedTo, api.subsystems[sub].AllowedHosts)
	}
	if err := unpublish(c, v, nodeB); err != nil {
		t.Fatal(err)
	}
	if api.volumes[v].MappedTo != nil || api.subsystems[sub] != nil {
		t.Errorf("after the last node left: %v, %v", api.volumes[v].MappedTo, api.subsystems[sub])
	}
}

func TestSnapshotsAndClones(t *testing.T) {
	api := newFakeAPI()
	c := NewController(testCfg, api)
	v := create(t, c, "pvc-src", 1<<20).VolumeId
	s, err := c.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: v})
	if err != nil {
		t.Fatal(err)
	}
	if s.Snapshot.SnapshotId != "lab-snap-1" || !s.Snapshot.ReadyToUse || s.Snapshot.CreationTime == nil {
		t.Errorf("%+v", s.Snapshot)
	}
	if _, err := c.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: v}); err != nil {
		t.Errorf("again: %v", err)
	}
	other := create(t, c, "pvc-other", 1<<20).VolumeId
	if _, err := c.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: other}); code(err) != codes.AlreadyExists {
		t.Errorf("the same name of another volume: %v", err)
	}
	r, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "pvc-clone", VolumeCapabilities: []*csi.VolumeCapability{rwo},
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 20},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "lab-snap-1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if src := api.volumes[r.Volume.VolumeId].Source; src == nil || src.Snapshot != "lab-snap-1" || api.volumes[r.Volume.VolumeId].Pool != "" {
		t.Errorf("clone source %+v", src)
	}
	for range 2 {
		if _, err := c.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "lab-snap-1"}); err != nil {
			t.Errorf("delete snapshot: %v", err)
		}
	}
}

func TestExpand(t *testing.T) {
	api := newFakeAPI()
	c := NewController(testCfg, api)
	v := create(t, c, "pvc-grow", 1<<20).VolumeId
	r, err := c.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{VolumeId: v, CapacityRange: &csi.CapacityRange{RequiredBytes: 3 << 20}, VolumeCapability: rwo})
	if err != nil || r.CapacityBytes != 3<<20 || !r.NodeExpansionRequired {
		t.Errorf("%+v, %v", r, err)
	}
	r, err = c.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{VolumeId: v, CapacityRange: &csi.CapacityRange{RequiredBytes: 3 << 20}, VolumeCapability: capOf(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true)})
	if err != nil || r.NodeExpansionRequired {
		t.Errorf("a block volume needs nothing on the node: %+v, %v", r, err)
	}
}

func TestLongNamesAreHashedStably(t *testing.T) {
	long := "pvc-" + string(make([]byte, 0)) + "0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789"
	a, b := scopedName("lab", long), scopedName("lab", long)
	if a != b || !nameRE.MatchString(a) || a == "lab-"+long {
		t.Errorf("%s", a)
	}
	if scopedName("lab", "pvc 1") == scopedName("lab", "pvc 2") {
		t.Error("two names hashed the same")
	}
}
