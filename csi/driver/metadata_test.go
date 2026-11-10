package driver

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"nvweaver/internal/client"
)

// fakeMetadataAPI serves ranges a page at a time, as nvweaver does, and records what it was asked.
type fakeMetadataAPI struct {
	size   int64
	ranges []client.BlockRange
	err    error
	asked  []string
}

func (f *fakeMetadataAPI) page(offset int64, limit int) (*client.BlockRanges, error) {
	if f.err != nil {
		return nil, f.err
	}
	p := &client.BlockRanges{BlockSize: 65536, SizeBytes: f.size, Ranges: []client.BlockRange{}}
	for _, r := range f.ranges {
		if r.Offset+r.Length <= offset {
			continue
		}
		if len(p.Ranges) == limit {
			next := max(r.Offset, offset)
			p.NextOffset = &next
			break
		}
		p.Ranges = append(p.Ranges, r)
	}
	return p, nil
}

func (f *fakeMetadataAPI) SnapshotAllocated(_ context.Context, snapshot string, offset int64, limit int) (*client.BlockRanges, error) {
	f.asked = append(f.asked, snapshot)
	return f.page(offset, limit)
}

func (f *fakeMetadataAPI) SnapshotChanges(_ context.Context, base, snapshot string, offset int64, limit int) (*client.BlockRanges, error) {
	f.asked = append(f.asked, base+">"+snapshot)
	return f.page(offset, limit)
}

// deltaStream collects what the service sends.
type deltaStream struct {
	grpc.ServerStream
	sent []*csi.GetMetadataDeltaResponse
}

func (s *deltaStream) Context() context.Context { return context.Background() }
func (s *deltaStream) Send(r *csi.GetMetadataDeltaResponse) error {
	s.sent = append(s.sent, r)
	return nil
}

type allocatedStream struct {
	grpc.ServerStream
	sent []*csi.GetMetadataAllocatedResponse
}

func (s *allocatedStream) Context() context.Context { return context.Background() }
func (s *allocatedStream) Send(r *csi.GetMetadataAllocatedResponse) error {
	s.sent = append(s.sent, r)
	return nil
}

func TestChangesStreamAPageAtATime(t *testing.T) {
	api := &fakeMetadataAPI{size: 1 << 30, ranges: []client.BlockRange{{Offset: 0, Length: 65536}, {Offset: 1 << 20, Length: 131072}, {Offset: 1 << 29, Length: 65536}}}
	s := NewSnapshotMetadata(api)
	st := &deltaStream{}
	if err := s.GetMetadataDelta(&csi.GetMetadataDeltaRequest{BaseSnapshotId: "lab-a", TargetSnapshotId: "lab-b", MaxResults: 2}, st); err != nil {
		t.Fatal(err)
	}
	if len(st.sent) != 2 || len(st.sent[0].BlockMetadata) != 2 || len(st.sent[1].BlockMetadata) != 1 {
		t.Fatalf("sent %v", st.sent)
	}
	for _, r := range st.sent {
		if r.BlockMetadataType != csi.BlockMetadataType_VARIABLE_LENGTH || r.VolumeCapacityBytes != 1<<30 {
			t.Errorf("message %v", r)
		}
	}
	if b := st.sent[1].BlockMetadata[0]; b.ByteOffset != 1<<29 || b.SizeBytes != 65536 {
		t.Errorf("last range %v", b)
	}
	if !slices.Equal(api.asked, []string{"lab-a>lab-b", "lab-a>lab-b"}) {
		t.Errorf("asked %v", api.asked)
	}

	// From an offset, and with no limit given, as many as the API gives in one page.
	st = &deltaStream{}
	if err := s.GetMetadataDelta(&csi.GetMetadataDeltaRequest{BaseSnapshotId: "lab-a", TargetSnapshotId: "lab-b", StartingOffset: 1 << 20}, st); err != nil {
		t.Fatal(err)
	}
	if len(st.sent) != 1 || len(st.sent[0].BlockMetadata) != 2 {
		t.Errorf("from 1 MiB: %v", st.sent)
	}

	// Nothing changed: no message, and no error.
	api.ranges = nil
	st = &deltaStream{}
	if err := s.GetMetadataDelta(&csi.GetMetadataDeltaRequest{BaseSnapshotId: "lab-a", TargetSnapshotId: "lab-b"}, st); err != nil || len(st.sent) != 0 {
		t.Errorf("nothing changed: %v, %v", st.sent, err)
	}
}

func TestAllocatedStreams(t *testing.T) {
	api := &fakeMetadataAPI{size: 1 << 20, ranges: []client.BlockRange{{Offset: 0, Length: 4096}}}
	st := &allocatedStream{}
	if err := NewSnapshotMetadata(api).GetMetadataAllocated(&csi.GetMetadataAllocatedRequest{SnapshotId: "lab-a"}, st); err != nil {
		t.Fatal(err)
	}
	if len(st.sent) != 1 || st.sent[0].VolumeCapacityBytes != 1<<20 || st.sent[0].BlockMetadata[0].SizeBytes != 4096 || api.asked[0] != "lab-a" {
		t.Errorf("sent %v, asked %v", st.sent, api.asked)
	}
}

func TestMetadataErrors(t *testing.T) {
	api := &fakeMetadataAPI{size: 1 << 20}
	s := NewSnapshotMetadata(api)
	delta := func(req *csi.GetMetadataDeltaRequest) codes.Code {
		return code(s.GetMetadataDelta(req, &deltaStream{}))
	}
	for name, c := range map[string]struct {
		req  *csi.GetMetadataDeltaRequest
		want codes.Code
	}{
		"no base":         {&csi.GetMetadataDeltaRequest{TargetSnapshotId: "b"}, codes.InvalidArgument},
		"no target":       {&csi.GetMetadataDeltaRequest{BaseSnapshotId: "a"}, codes.InvalidArgument},
		"negative offset": {&csi.GetMetadataDeltaRequest{BaseSnapshotId: "a", TargetSnapshotId: "b", StartingOffset: -1}, codes.InvalidArgument},
		"negative limit":  {&csi.GetMetadataDeltaRequest{BaseSnapshotId: "a", TargetSnapshotId: "b", MaxResults: -1}, codes.InvalidArgument},
		"past the end":    {&csi.GetMetadataDeltaRequest{BaseSnapshotId: "a", TargetSnapshotId: "b", StartingOffset: 1 << 20}, codes.OutOfRange},
	} {
		if got := delta(c.req); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	ok := &csi.GetMetadataDeltaRequest{BaseSnapshotId: "a", TargetSnapshotId: "b"}
	api.err = &client.Error{Status: http.StatusNotFound, Message: "no such snapshot b"}
	if got := delta(ok); got != codes.NotFound {
		t.Errorf("a missing snapshot: %v", got)
	}
	api.err = &client.Error{Status: http.StatusUnprocessableEntity, Message: "snapshots a and b aren't of the same volume"}
	if got := delta(ok); got != codes.InvalidArgument {
		t.Errorf("another volume's: %v", got)
	}
	if got := code(s.GetMetadataAllocated(&csi.GetMetadataAllocatedRequest{}, &allocatedStream{})); got != codes.InvalidArgument {
		t.Errorf("no snapshot: %v", got)
	}
}

func TestSnapshotMetadataIsAdvertisedOnlyWhenServed(t *testing.T) {
	has := func(cfg Config, api API) bool {
		r, err := NewIdentity(cfg, api).GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range r.Capabilities {
			if c.GetService().GetType() == csi.PluginCapability_Service_SNAPSHOT_METADATA_SERVICE {
				return true
			}
		}
		return false
	}
	cfg := testCfg
	if has(cfg, newFakeAPI()) {
		t.Error("advertised when not asked for")
	}
	cfg.SnapshotMetadata = true
	if !has(cfg, newFakeAPI()) {
		t.Error("not advertised when asked for")
	}
	if has(cfg, nil) {
		t.Error("advertised by a node, which doesn't serve it")
	}
}
