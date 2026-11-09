package client

// These follow the schemas in api/openapi.yaml. Tests in internal/api compare them with it, in both directions.

// Health is what /healthz reports.
type Health struct {
	Status string `json:"status"`
}

// Capabilities says what the running backend can do.
type Capabilities struct {
	Passthrough              bool     `json:"passthrough"`
	SurvivesServiceExit      bool     `json:"survives_service_exit"`
	PerPortInlineData        bool     `json:"per_port_inline_data"`
	BufferedIO               bool     `json:"buffered_io"`
	PerHostDHCHAPParams      bool     `json:"per_host_dhchap_params"`
	WildcardListen           bool     `json:"wildcard_listen"`
	IdentifiesConnectedHosts bool     `json:"identifies_connected_hosts"`
	ListsAllConnections      bool     `json:"lists_all_connections"`
	Transports               []string `json:"transports"`
}

// System describes the service and its target.
type System struct {
	Version      string          `json:"version"`
	Backend      string          `json:"backend"`
	Capabilities Capabilities    `json:"capabilities"`
	PoolTypes    []string        `json:"pool_types"`
	Kernel       string          `json:"kernel"`
	Modules      map[string]bool `json:"modules"`
	TargetReady  bool            `json:"target_ready"`
	Ports        int             `json:"ports"`
	Hosts        int             `json:"hosts"`
	Subsystems   int             `json:"subsystems"`
	Pools        int             `json:"pools"`
	Volumes      int             `json:"volumes"`
	Snapshots    int             `json:"snapshots"`
	Warnings     []string        `json:"warnings"`
}

// Port is a listener.
// Connection is one host's connection to a port, or to a subsystem where the target can tell.
type Connection struct {
	Port        int    `json:"port"`
	Transport   string `json:"transport"`
	HostAddress string `json:"host_address"`
	Queues      int    `json:"queues"`
	// Subsystem and HostNQN are empty if the target can't tell.
	Subsystem string `json:"subsystem,omitempty"`
	HostNQN   string `json:"host_nqn,omitempty"`
}

type Port struct {
	ID             int    `json:"id"`
	Transport      string `json:"transport"`
	Address        string `json:"address"`
	Service        int    `json:"service"`
	InlineDataSize *int   `json:"inline_data_size,omitempty"`
}

// PortCreate creates a port. With no ID, the lowest free one is used.
type PortCreate struct {
	ID             *int   `json:"id,omitempty"`
	Transport      string `json:"transport,omitempty"`
	Address        string `json:"address"`
	Service        int    `json:"service,omitempty"`
	InlineDataSize *int   `json:"inline_data_size,omitempty"`
}

// PortUpdate changes what's set.
type PortUpdate struct {
	Transport      *string `json:"transport,omitempty"`
	Address        *string `json:"address,omitempty"`
	Service        *int    `json:"service,omitempty"`
	InlineDataSize *int    `json:"inline_data_size,omitempty"`
}

// Host is an initiator's identity. The API never returns its keys.
type Host struct {
	NQN              string `json:"nqn"`
	HasDHCHAPKey     bool   `json:"has_dhchap_key"`
	HasDHCHAPCtrlKey bool   `json:"has_dhchap_ctrl_key"`
	DHCHAPHash       string `json:"dhchap_hash,omitempty"`
	DHCHAPDHGroup    string `json:"dhchap_dhgroup,omitempty"`
}

// HostCreate creates a host.
type HostCreate struct {
	NQN           string `json:"nqn"`
	DHCHAPKey     string `json:"dhchap_key,omitempty"`
	DHCHAPCtrlKey string `json:"dhchap_ctrl_key,omitempty"`
	DHCHAPHash    string `json:"dhchap_hash,omitempty"`
	DHCHAPDHGroup string `json:"dhchap_dhgroup,omitempty"`
}

// HostUpdate changes what's set.
type HostUpdate struct {
	DHCHAPKey     *string `json:"dhchap_key,omitempty"`
	DHCHAPCtrlKey *string `json:"dhchap_ctrl_key,omitempty"`
	DHCHAPHash    *string `json:"dhchap_hash,omitempty"`
	DHCHAPDHGroup *string `json:"dhchap_dhgroup,omitempty"`
}

// Subsystem is what hosts connect to.
type Subsystem struct {
	NQN            string      `json:"nqn"`
	Serial         string      `json:"serial"`
	Model          string      `json:"model"`
	AllowAnyHost   bool        `json:"allow_any_host"`
	AllowedHosts   []string    `json:"allowed_hosts"`
	Ports          []int       `json:"ports"`
	PassthruDevice string      `json:"passthru_device,omitempty"`
	Namespaces     []Namespace `json:"namespaces"`
}

// SubsystemCreate creates a subsystem.
type SubsystemCreate struct {
	NQN            string   `json:"nqn"`
	Serial         string   `json:"serial,omitempty"`
	Model          string   `json:"model,omitempty"`
	AllowAnyHost   bool     `json:"allow_any_host,omitempty"`
	AllowedHosts   []string `json:"allowed_hosts,omitempty"`
	Ports          []int    `json:"ports,omitempty"`
	PassthruDevice string   `json:"passthru_device,omitempty"`
}

// SubsystemUpdate changes what's set.
type SubsystemUpdate struct {
	AllowAnyHost *bool `json:"allow_any_host,omitempty"`
}

// Namespace is what a subsystem exports.
type Namespace struct {
	NSID        int    `json:"nsid"`
	DevicePath  string `json:"device_path"`
	UUID        string `json:"uuid"`
	NGUID       string `json:"nguid"`
	Enabled     bool   `json:"enabled"`
	BufferedIO  bool   `json:"buffered_io"`
	ManagedFile bool   `json:"managed_file,omitempty"`
	Volume      string `json:"volume,omitempty"`
	LiveEnabled bool   `json:"live_enabled"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
}

// NamespaceFile asks for a new sparse file in the service's volume directory.
type NamespaceFile struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// NamespaceCreate exports one of a device, a volume or a new file. An NSID of 0 means the lowest free one.
type NamespaceCreate struct {
	NSID       int            `json:"nsid,omitempty"`
	DevicePath string         `json:"device_path,omitempty"`
	Volume     string         `json:"volume,omitempty"`
	File       *NamespaceFile `json:"file,omitempty"`
	UUID       string         `json:"uuid,omitempty"`
	NGUID      string         `json:"nguid,omitempty"`
	BufferedIO bool           `json:"buffered_io,omitempty"`
	Enabled    *bool          `json:"enabled,omitempty"`
}

// NamespaceUpdate changes what's set.
type NamespaceUpdate struct {
	Enabled    *bool   `json:"enabled,omitempty"`
	DevicePath *string `json:"device_path,omitempty"`
	BufferedIO *bool   `json:"buffered_io,omitempty"`
	SizeBytes  *int64  `json:"size_bytes,omitempty"`
}

// PoolCapacity is a pool's space, where its storage can report it.
type PoolCapacity struct {
	TotalBytes          int64   `json:"total_bytes"`
	UsedBytes           int64   `json:"used_bytes"`
	ProvisionedBytes    int64   `json:"provisioned_bytes"`
	UsedPercent         float64 `json:"used_percent"`
	MetadataUsedPercent float64 `json:"metadata_used_percent,omitempty"`
}

// Pool is adopted storage that volumes come from.
type Pool struct {
	Name           string            `json:"name"`
	Type           string            `json:"type"`
	Path           string            `json:"path,omitempty"`
	VolumeGroup    string            `json:"volume_group,omitempty"`
	ThinPool       string            `json:"thin_pool,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	WarningPercent int               `json:"warning_percent"`
	State          string            `json:"state"`
	Warnings       []string          `json:"warnings"`
	Capacity       *PoolCapacity     `json:"capacity,omitempty"`
	Volumes        int               `json:"volumes"`
	Snapshots      int               `json:"snapshots"`
}

// PoolCreate adopts a pool.
type PoolCreate struct {
	Name           string            `json:"name"`
	Type           string            `json:"type"`
	Path           string            `json:"path,omitempty"`
	VolumeGroup    string            `json:"volume_group,omitempty"`
	ThinPool       string            `json:"thin_pool,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	WarningPercent *int              `json:"warning_percent,omitempty"`
}

// PoolUpdate changes what's set. Pointing Labels at an empty map removes them all.
type PoolUpdate struct {
	Labels         *map[string]string `json:"labels,omitempty"`
	WarningPercent *int               `json:"warning_percent,omitempty"`
}

// VolumeSource says what a volume was cloned from.
type VolumeSource struct {
	Snapshot string `json:"snapshot,omitempty"`
	Volume   string `json:"volume,omitempty"`
}

// VolumeMapping says where a volume is exported.
type VolumeMapping struct {
	Subsystem string `json:"subsystem"`
	NSID      int    `json:"nsid"`
}

// Volume is storage from a pool, which keeps its identity wherever it's exported.
type Volume struct {
	Name         string            `json:"name"`
	Pool         string            `json:"pool"`
	InternalName string            `json:"internal_name"`
	DevicePath   string            `json:"device_path"`
	SizeBytes    int64             `json:"size_bytes"`
	UUID         string            `json:"uuid"`
	NGUID        string            `json:"nguid"`
	Source       *VolumeSource     `json:"source"`
	Labels       map[string]string `json:"labels,omitempty"`
	CreatedAt    string            `json:"created_at"`
	State        string            `json:"state"`
	MappedTo     *VolumeMapping    `json:"mapped_to"`
}

// VolumeUsage says how much of a volume is really allocated.
type VolumeUsage struct {
	SizeBytes int64 `json:"size_bytes"`
	UsedBytes int64 `json:"used_bytes"`
}

// VolumePage is one page of volumes. NextAfter is empty on the last.
type VolumePage struct {
	Items     []Volume `json:"items"`
	NextAfter string   `json:"next_after,omitempty"`
}

// VolumeCreate creates a volume, or a clone if Source is set.
type VolumeCreate struct {
	Name      string            `json:"name"`
	Pool      string            `json:"pool,omitempty"`
	SizeBytes int64             `json:"size_bytes,omitempty"`
	Source    *VolumeSource     `json:"source,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// VolumeUpdate changes what's set. Pointing Labels at an empty map removes them all.
type VolumeUpdate struct {
	SizeBytes *int64             `json:"size_bytes,omitempty"`
	Labels    *map[string]string `json:"labels,omitempty"`
}

// Snapshot is a read-only copy of a volume.
type Snapshot struct {
	Name         string `json:"name"`
	Volume       string `json:"volume"`
	Pool         string `json:"pool"`
	InternalName string `json:"internal_name"`
	DevicePath   string `json:"device_path"`
	SizeBytes    int64  `json:"size_bytes"`
	CreatedAt    string `json:"created_at"`
	State        string `json:"state"`
}

// SnapshotPage is one page of snapshots. NextAfter is empty on the last.
type SnapshotPage struct {
	Items     []Snapshot `json:"items"`
	NextAfter string     `json:"next_after,omitempty"`
}

// SnapshotCreate snapshots a volume.
type SnapshotCreate struct {
	Name   string `json:"name"`
	Volume string `json:"volume"`
}
