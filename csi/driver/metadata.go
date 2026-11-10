package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"nvweaver/internal/client"
)

// SnapshotMetadata is CSI's changed block tracking service, for backups: where a snapshot holds data, and what changed
// between two snapshots of one volume. It's served only when asked for, as the external-snapshot-metadata sidecar that
// calls it needs Kubernetes 1.33 (decision 51). nvweaver does the work, from the pool's own mappings: see decision 50.
type SnapshotMetadata struct {
	csi.UnimplementedSnapshotMetadataServer
	api MetadataAPI
}

// MetadataAPI is what the service uses of nvweaver's API.
type MetadataAPI interface {
	SnapshotAllocated(ctx context.Context, snapshot string, offset int64, limit int) (*client.BlockRanges, error)
	SnapshotChanges(ctx context.Context, base, snapshot string, offset int64, limit int) (*client.BlockRanges, error)
}

// NewSnapshotMetadata returns the service.
func NewSnapshotMetadata(api MetadataAPI) *SnapshotMetadata { return &SnapshotMetadata{api: api} }

// maxRanges is the most ranges in one message: the API's own page limit.
const maxRanges = 10000

// streamRanges reads pages from nvweaver, from offset, and sends each as one message of at most limit ranges. Ranges are
// variable-length, as nvweaver merges blocks that touch.
func streamRanges(ctx context.Context, offset int64, maxResults int32, page func(offset int64, limit int) (*client.BlockRanges, error),
	send func(capacity int64, blocks []*csi.BlockMetadata) error) error {
	if offset < 0 {
		return status.Error(codes.InvalidArgument, "starting_offset can't be negative")
	}
	if maxResults < 0 {
		return status.Error(codes.InvalidArgument, "max_results can't be negative")
	}
	limit := int(maxResults)
	if limit == 0 || limit > maxRanges {
		limit = maxRanges
	}
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		p, err := page(offset, limit)
		if err != nil {
			return apiError(err, "snapshot metadata")
		}
		if first && offset >= p.SizeBytes && offset > 0 {
			return status.Errorf(codes.OutOfRange, "starting_offset %d is past the snapshot's %d bytes", offset, p.SizeBytes)
		}
		first = false
		if len(p.Ranges) > 0 {
			blocks := make([]*csi.BlockMetadata, len(p.Ranges))
			for i, r := range p.Ranges {
				blocks[i] = &csi.BlockMetadata{ByteOffset: r.Offset, SizeBytes: r.Length}
			}
			if err := send(p.SizeBytes, blocks); err != nil {
				return err
			}
		}
		if p.NextOffset == nil {
			return nil
		}
		offset = *p.NextOffset
	}
}

func (s *SnapshotMetadata) GetMetadataAllocated(req *csi.GetMetadataAllocatedRequest, stream csi.SnapshotMetadata_GetMetadataAllocatedServer) error {
	if req.GetSnapshotId() == "" {
		return status.Error(codes.InvalidArgument, "no snapshot ID")
	}
	ctx := stream.Context()
	return streamRanges(ctx, req.GetStartingOffset(), req.GetMaxResults(),
		func(offset int64, limit int) (*client.BlockRanges, error) {
			return s.api.SnapshotAllocated(ctx, req.GetSnapshotId(), offset, limit)
		},
		func(capacity int64, blocks []*csi.BlockMetadata) error {
			return stream.Send(&csi.GetMetadataAllocatedResponse{BlockMetadataType: csi.BlockMetadataType_VARIABLE_LENGTH,
				VolumeCapacityBytes: capacity, BlockMetadata: blocks})
		})
}

func (s *SnapshotMetadata) GetMetadataDelta(req *csi.GetMetadataDeltaRequest, stream csi.SnapshotMetadata_GetMetadataDeltaServer) error {
	if req.GetBaseSnapshotId() == "" || req.GetTargetSnapshotId() == "" {
		return status.Error(codes.InvalidArgument, "a base and a target snapshot ID are both needed")
	}
	ctx := stream.Context()
	return streamRanges(ctx, req.GetStartingOffset(), req.GetMaxResults(),
		func(offset int64, limit int) (*client.BlockRanges, error) {
			return s.api.SnapshotChanges(ctx, req.GetBaseSnapshotId(), req.GetTargetSnapshotId(), offset, limit)
		},
		func(capacity int64, blocks []*csi.BlockMetadata) error {
			return stream.Send(&csi.GetMetadataDeltaResponse{BlockMetadataType: csi.BlockMetadataType_VARIABLE_LENGTH,
				VolumeCapacityBytes: capacity, BlockMetadata: blocks})
		})
}
