// Package driver is nvweaver's CSI driver: a controller that provisions volumes
// and exports them through the API, and a node plugin that connects to them
// over NVMe/TCP and mounts them. It reaches nvweaver only through its public
// API, with internal/client, so it can do nothing the API can't. See decision 44.
package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"nvweaver/internal/client"
)

// DefaultName is the driver's name in Kubernetes, from the project's public GitHub namespace.
const DefaultName = "nvweaver.xphyr.github.io"

// API is what the controller uses of nvweaver's API: a subset of *client.Client, so tests can fake it.
type API interface {
	Health(ctx context.Context) (*client.Health, error)
	GetPort(ctx context.Context, id int) (*client.Port, error)
	GetHost(ctx context.Context, nqn string) (*client.Host, error)
	CreateHost(ctx context.Context, in client.HostCreate) (*client.Host, error)
	GetSubsystem(ctx context.Context, nqn string) (*client.Subsystem, error)
	CreateSubsystem(ctx context.Context, in client.SubsystemCreate) (*client.Subsystem, error)
	DeleteSubsystem(ctx context.Context, nqn string) error
	AttachPort(ctx context.Context, nqn string, port int) error
	AllowHost(ctx context.Context, nqn, host string) error
	DenyHost(ctx context.Context, nqn, host string) error
	CreateNamespace(ctx context.Context, nqn string, in client.NamespaceCreate) (*client.Namespace, error)
	DeleteNamespace(ctx context.Context, nqn string, nsid int, deleteFile bool) error
	GetVolume(ctx context.Context, name string) (*client.Volume, error)
	CreateVolume(ctx context.Context, in client.VolumeCreate) (*client.Volume, error)
	UpdateVolume(ctx context.Context, name string, in client.VolumeUpdate) (*client.Volume, error)
	DeleteVolume(ctx context.Context, name string) error
	VolumesPage(ctx context.Context, pool string, limit int, after string) (*client.VolumePage, error)
	SnapshotsPage(ctx context.Context, volume string, limit int, after string) (*client.SnapshotPage, error)
	GetSnapshot(ctx context.Context, name string) (*client.Snapshot, error)
	CreateSnapshot(ctx context.Context, in client.SnapshotCreate) (*client.Snapshot, error)
	DeleteSnapshot(ctx context.Context, name string) error
}

// Config is what the driver is told at startup.
type Config struct {
	Name    string
	Version string
	// Cluster scopes every name the driver makes, so that several clusters can share one nvweaver.
	Cluster string
	// NQNPrefix starts the NQN of each subsystem the driver makes.
	NQNPrefix string
	// Port is the nvweaver port that volumes are offered on, unless a StorageClass names another.
	Port int
	// CtrlLossTmo is how long, in seconds, a node keeps trying to reconnect before it gives a volume up; -1 is for ever.
	CtrlLossTmo int
	// MaxVolumesPerNode is what the node tells Kubernetes it can take.
	MaxVolumesPerNode int64
	Log               *slog.Logger
}

// MaxNamespaces is how many namespaces a subsystem can hold, on both targets: NVMET_MAX_NAMESPACES in the kernel's, and
// what nvweaver gives SPDK's subsystems. A node's volumes share its subsystem, so it's the most a node can have.
const MaxNamespaces = 1024

var clusterRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// Check says what's wrong with a configuration, if anything.
func (c Config) Check() error {
	if !clusterRE.MatchString(c.Cluster) {
		return fmt.Errorf("cluster %q must be 1 to 40 lowercase letters, digits and dashes", c.Cluster)
	}
	if c.Port < 0 {
		return fmt.Errorf("port %d", c.Port)
	}
	if c.CtrlLossTmo < -1 {
		return fmt.Errorf("ctrl-loss-tmo %d: it's seconds, or -1 for ever", c.CtrlLossTmo)
	}
	if c.MaxVolumesPerNode < 0 || c.MaxVolumesPerNode > MaxNamespaces {
		return fmt.Errorf("max-volumes-per-node %d: it's at most %d, the namespaces a subsystem can hold", c.MaxVolumesPerNode, MaxNamespaces)
	}
	return nil
}

// locks serializes work on one key, such as a volume or a node's subsystem. The
// API is safe to call at once, but find-then-create sequences aren't.
type locks struct {
	mu   sync.Mutex
	held map[string]*sync.Mutex
}

func (l *locks) lock(key string) func() {
	l.mu.Lock()
	if l.held == nil {
		l.held = map[string]*sync.Mutex{}
	}
	m, ok := l.held[key]
	if !ok {
		m = &sync.Mutex{}
		l.held[key] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// nvweaver's volume and snapshot names: up to 128 letters, digits, '.', '_' and '-', starting with a letter or digit.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// scopedName turns a name Kubernetes chose into one of nvweaver's, prefixed
// with the cluster. One too long, or with other characters, is hashed, so the
// same request always gives the same name.
func scopedName(cluster, name string) string {
	n := cluster + "-" + name
	if nameRE.MatchString(n) {
		return n
	}
	sum := sha256.Sum256([]byte(name))
	return cluster + "-h" + hex.EncodeToString(sum[:16])
}

// nodeSubsystem is the subsystem that every single-node volume on a node is exported in.
func (c Config) nodeSubsystem(hostNQN string) string {
	sum := sha256.Sum256([]byte(hostNQN))
	return fmt.Sprintf("%s:%s:node-%s", c.NQNPrefix, c.Cluster, hex.EncodeToString(sum[:8]))
}

// sharedSubsystem is the subsystem of its own that a volume used by several nodes at once is exported in.
func (c Config) sharedSubsystem(volume string) string {
	return fmt.Sprintf("%s:%s:vol-%s", c.NQNPrefix, c.Cluster, volume)
}

// shared reports whether a capability lets several nodes use a volume at once.
func shared(cap *csi.VolumeCapability) bool {
	switch cap.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return true
	}
	return false
}

// checkCapability refuses what the driver can't do safely: a filesystem written
// from several nodes at once, which would corrupt it.
func checkCapability(cap *csi.VolumeCapability) error {
	if cap == nil {
		return status.Error(codes.InvalidArgument, "no volume capability")
	}
	if cap.GetBlock() == nil && cap.GetMount() == nil {
		return status.Error(codes.InvalidArgument, "the capability must be block or mount")
	}
	switch cap.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_UNKNOWN:
		return status.Error(codes.InvalidArgument, "no access mode")
	case csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER, csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		if cap.GetMount() != nil {
			return status.Error(codes.InvalidArgument, "a filesystem can't be written from several nodes: use a block volume, or ReadOnlyMany")
		}
	}
	return nil
}

// apiError turns an API error into the gRPC status that CSI expects.
func apiError(err error, what string) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok && !errors.As(err, new(*client.Error)) {
		return err
	}
	msg := fmt.Sprintf("%s: %v", what, err)
	switch {
	case client.IsNotFound(err):
		return status.Error(codes.NotFound, msg)
	case client.IsConflict(err):
		return status.Error(codes.AlreadyExists, msg)
	case client.IsInvalid(err):
		return status.Error(codes.InvalidArgument, msg)
	case client.IsOutOfSpace(err):
		return status.Error(codes.ResourceExhausted, msg)
	case client.IsBusy(err):
		return status.Error(codes.Unavailable, msg)
	case client.IsUnauthorized(err):
		return status.Error(codes.Unauthenticated, msg)
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, msg)
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, msg)
	}
	return status.Error(codes.Internal, msg)
}
