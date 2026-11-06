// Package target converges the kernel NVMe target (nvmet) on a desired state
// by way of its configfs interface.
package target

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"regexp"
	"slices"
)

const (
	// MaxNSID is NVMET_MAX_NAMESPACES in the kernel target.
	MaxNSID = 1024
	// MaxPortID keeps clear of 0xFFFF, which the NVMe spec reserves.
	MaxPortID = 65534
	// discoveryNQN is the well-known discovery subsystem; the kernel owns it.
	discoveryNQN = "nqn.2014-08.org.nvmexpress.discovery"
)

// State is the complete desired configuration of the target.
type State struct {
	Ports      map[uint16]*Port      `json:"ports"`
	Hosts      map[string]*Host      `json:"hosts"`
	Subsystems map[string]*Subsystem `json:"subsystems"`
	Pools      map[string]*Pool      `json:"pools,omitempty"`
	Volumes    map[string]*Volume    `json:"volumes,omitempty"`
	Snapshots  map[string]*Snapshot  `json:"snapshots,omitempty"`
}

// InternalPrefix starts the name of everything this service creates in a pool, which is how it
// tells its own from anything else that may be there.
const InternalPrefix = "nvw-"

// Pool is storage that was created outside this service, and adopted to allocate volumes from.
type Pool struct {
	Name           string            `json:"name"`
	Type           string            `json:"type"`
	Path           string            `json:"path,omitempty"`
	VolumeGroup    string            `json:"volume_group,omitempty"`
	ThinPool       string            `json:"thin_pool,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	WarningPercent int               `json:"warning_percent"`
}

// Volume is storage allocated from a pool, which exists whether or not it's exported. It owns
// the identity that hosts recognise it by, so that it keeps it wherever it's mapped.
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
}

// VolumeSource is what a clone was made from, by the name it had then, which may have gone since.
type VolumeSource struct {
	Snapshot string `json:"snapshot,omitempty"`
	Volume   string `json:"volume,omitempty"`
}

// Snapshot is a read-only copy of a volume at a moment in time. It's independent once taken, and
// can outlive its volume. To use one, clone it to a new volume.
type Snapshot struct {
	Name         string `json:"name"`
	Volume       string `json:"volume"`
	Pool         string `json:"pool"`
	InternalName string `json:"internal_name"`
	DevicePath   string `json:"device_path"`
	SizeBytes    int64  `json:"size_bytes"`
	CreatedAt    string `json:"created_at"`
}

// TransportTCP is NVMe/TCP, the only transport so far, and the default.
const TransportTCP = "tcp"

// Port is a listener on one transport. Discovery is served on every port by the kernel.
type Port struct {
	ID             uint16 `json:"id"`
	Transport      string `json:"transport"`
	Address        string `json:"address"`
	Service        int    `json:"service"`
	InlineDataSize *int   `json:"inline_data_size,omitempty"`
}

// Host is an initiator identity, with optional DH-HMAC-CHAP credentials.
type Host struct {
	NQN           string `json:"nqn"`
	DHCHAPKey     string `json:"dhchap_key,omitempty"`
	DHCHAPCtrlKey string `json:"dhchap_ctrl_key,omitempty"`
	DHCHAPHash    string `json:"dhchap_hash,omitempty"`
	DHCHAPDHGroup string `json:"dhchap_dhgroup,omitempty"`
}

// Subsystem is an NVM subsystem: what hosts connect to. A subsystem either
// has namespaces or passes a whole NVMe controller through, never both.
type Subsystem struct {
	NQN          string                `json:"nqn"`
	Serial       string                `json:"serial"`
	Model        string                `json:"model"`
	AllowAnyHost bool                  `json:"allow_any_host"`
	AllowedHosts []string              `json:"allowed_hosts"`
	Ports        []uint16              `json:"ports"`
	Namespaces   map[uint32]*Namespace `json:"namespaces"`
	Passthru     string                `json:"passthru_device,omitempty"`
}

// Namespace is one exported block device or file.
type Namespace struct {
	NSID       uint32 `json:"nsid"`
	DevicePath string `json:"device_path"`
	UUID       string `json:"uuid"`
	NGUID      string `json:"nguid"`
	Enabled    bool   `json:"enabled"`
	BufferedIO bool   `json:"buffered_io"`
	// ManagedFile marks a file this service created, under its volume directory.
	ManagedFile bool `json:"managed_file,omitempty"`
	// Volume names the volume this namespace exports, whose path and identity are copied here,
	// so that backends need only treat it as any other namespace.
	Volume string `json:"volume,omitempty"`
}

// NewState returns an empty state.
func NewState() *State {
	return &State{
		Ports:      map[uint16]*Port{},
		Hosts:      map[string]*Host{},
		Subsystems: map[string]*Subsystem{},
		Pools:      map[string]*Pool{},
		Volumes:    map[string]*Volume{},
		Snapshots:  map[string]*Snapshot{},
	}
}

// Normalize makes a state decoded from JSON safe to use, including one
// written by a version that lacked newer fields.
func (s *State) Normalize() {
	if s.Ports == nil {
		s.Ports = map[uint16]*Port{}
	}
	if s.Hosts == nil {
		s.Hosts = map[string]*Host{}
	}
	if s.Subsystems == nil {
		s.Subsystems = map[string]*Subsystem{}
	}
	if s.Pools == nil {
		s.Pools = map[string]*Pool{}
	}
	if s.Volumes == nil {
		s.Volumes = map[string]*Volume{}
	}
	if s.Snapshots == nil {
		s.Snapshots = map[string]*Snapshot{}
	}
	for _, p := range s.Ports {
		if p.Transport == "" {
			p.Transport = TransportTCP
		}
	}
	for _, sub := range s.Subsystems {
		if sub.Namespaces == nil {
			sub.Namespaces = map[uint32]*Namespace{}
		}
		if sub.AllowedHosts == nil {
			sub.AllowedHosts = []string{}
		}
		if sub.Ports == nil {
			sub.Ports = []uint16{}
		}
	}
}

// Clone returns a deep copy. Every change makes one, so at thousands of volumes it matters that
// this is direct: going through JSON took 160 ms at 5,000. A test checks that it covers every field.
func (s *State) Clone() *State {
	c := &State{
		Ports:      make(map[uint16]*Port, len(s.Ports)),
		Hosts:      make(map[string]*Host, len(s.Hosts)),
		Subsystems: make(map[string]*Subsystem, len(s.Subsystems)),
		Pools:      make(map[string]*Pool, len(s.Pools)),
		Volumes:    make(map[string]*Volume, len(s.Volumes)),
		Snapshots:  make(map[string]*Snapshot, len(s.Snapshots)),
	}
	for k, v := range s.Ports {
		p := *v
		if v.InlineDataSize != nil {
			size := *v.InlineDataSize
			p.InlineDataSize = &size
		}
		c.Ports[k] = &p
	}
	for k, v := range s.Hosts {
		h := *v
		c.Hosts[k] = &h
	}
	for k, v := range s.Subsystems {
		sub := *v
		sub.AllowedHosts = slices.Clone(v.AllowedHosts)
		sub.Ports = slices.Clone(v.Ports)
		sub.Namespaces = make(map[uint32]*Namespace, len(v.Namespaces))
		for id, ns := range v.Namespaces {
			n := *ns
			sub.Namespaces[id] = &n
		}
		c.Subsystems[k] = &sub
	}
	for k, v := range s.Pools {
		p := *v
		p.Labels = maps.Clone(v.Labels)
		c.Pools[k] = &p
	}
	for k, v := range s.Volumes {
		vol := *v
		vol.Labels = maps.Clone(v.Labels)
		if v.Source != nil {
			source := *v.Source
			vol.Source = &source
		}
		c.Volumes[k] = &vol
	}
	for k, v := range s.Snapshots {
		snap := *v
		c.Snapshots[k] = &snap
	}
	c.Normalize()
	return c
}

// ValidationError is a problem with what the client asked for.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, a...)}
}

// An NQN becomes a configfs path component, so this check is also what stops
// path traversal through the API. It accepts a deliberately conservative
// subset of what the spec allows, and covers the nvmexpress:uuid: form.
var nqnRE = regexp.MustCompile(`^nqn\.[0-9]{4}-[0-9]{2}\.[A-Za-z0-9][A-Za-z0-9.-]*(:[A-Za-z0-9._:-]+)?$`)

// ValidateNQN checks the format of an NVMe Qualified Name.
func ValidateNQN(nqn string) error {
	if len(nqn) > 223 {
		return invalid("NQN exceeds 223 bytes")
	}
	if !nqnRE.MatchString(nqn) {
		return invalid("%q is not a valid NQN (expected nqn.yyyy-mm.reverse.domain:name)", nqn)
	}
	if nqn == discoveryNQN {
		return invalid("%q is reserved for the discovery service", nqn)
	}
	return nil
}

func printableASCII(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// Validate checks a port in isolation.
func (p *Port) Validate() error {
	if p.ID > MaxPortID {
		return invalid("port id must be at most %d", MaxPortID)
	}
	if p.Transport != "" && p.Transport != TransportTCP {
		return invalid("transport %q isn't available: this service can use %s", p.Transport, TransportTCP)
	}
	ip := net.ParseIP(p.Address)
	if ip == nil {
		return invalid("address %q must be an IP address", p.Address)
	}
	if p.Service < 1 || p.Service > 65535 {
		return invalid("service must be a TCP port number")
	}
	if p.InlineDataSize != nil && *p.InlineDataSize < 0 {
		return invalid("inline_data_size must not be negative")
	}
	return nil
}

// Family returns the configfs address family for the port.
func (p *Port) Family() string {
	if ip := net.ParseIP(p.Address); ip != nil && ip.To4() == nil {
		return "ipv6"
	}
	return "ipv4"
}

// Validate checks a subsystem's own fields.
func (s *Subsystem) Validate() error {
	if err := ValidateNQN(s.NQN); err != nil {
		return err
	}
	if len(s.Serial) == 0 || len(s.Serial) > 20 || !printableASCII(s.Serial) {
		return invalid("serial must be 1-20 printable ASCII characters")
	}
	if len(s.Model) == 0 || len(s.Model) > 40 || !printableASCII(s.Model) {
		return invalid("model must be 1-40 printable ASCII characters")
	}
	if s.AllowAnyHost && len(s.AllowedHosts) > 0 {
		return invalid("allow_any_host cannot be combined with allowed_hosts")
	}
	if s.Passthru != "" && len(s.Namespaces) > 0 {
		return invalid("a passthrough subsystem cannot also have namespaces")
	}
	return nil
}

// Validate checks that references between objects resolve.
func (s *State) Validate() error {
	listeners := map[string]uint16{}
	for id, p := range s.Ports {
		if err := p.Validate(); err != nil {
			return err
		}
		key := net.JoinHostPort(p.Address, fmt.Sprint(p.Service))
		if other, dup := listeners[key]; dup {
			return &ConflictError{Msg: fmt.Sprintf("ports %d and %d both listen on %s", other, id, key)}
		}
		listeners[key] = id
	}
	for _, vol := range s.Volumes {
		if s.Pools[vol.Pool] == nil {
			return invalid("volume %s references unknown pool %s", vol.Name, vol.Pool)
		}
	}
	for _, snap := range s.Snapshots {
		if s.Pools[snap.Pool] == nil {
			return invalid("snapshot %s references unknown pool %s", snap.Name, snap.Pool)
		}
	}
	mappedTo := map[string]string{}
	for _, sub := range s.Subsystems {
		if err := sub.Validate(); err != nil {
			return err
		}
		for _, ns := range sub.Namespaces {
			if ns.Volume == "" {
				continue
			}
			if s.Volumes[ns.Volume] == nil {
				return invalid("subsystem %s references unknown volume %s", sub.NQN, ns.Volume)
			}
			if other, dup := mappedTo[ns.Volume]; dup {
				return NewConflict("volume %s is already mapped into %s; a volume is in one subsystem at a time, which may allow several hosts", ns.Volume, other)
			}
			mappedTo[ns.Volume] = sub.NQN
		}
		for _, h := range sub.AllowedHosts {
			if _, ok := s.Hosts[h]; !ok {
				return invalid("subsystem %s references unknown host %s", sub.NQN, h)
			}
		}
		for _, id := range sub.Ports {
			if _, ok := s.Ports[id]; !ok {
				return invalid("subsystem %s references unknown port %d", sub.NQN, id)
			}
		}
	}
	return nil
}

// ConflictError is a request that clashes with existing configuration.
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// NewConflict returns a ConflictError.
func NewConflict(format string, a ...any) error {
	return &ConflictError{Msg: fmt.Sprintf(format, a...)}
}

// Invalid returns a ValidationError.
func Invalid(format string, a ...any) error { return invalid(format, a...) }

// NewSerial returns a random serial. The kernel would pick its own, but one
// that changes on restart makes hosts see a different subsystem.
func NewSerial() string {
	b := make([]byte, 10)
	mustRand(b)
	return hex.EncodeToString(b)
}

// NewInternalName returns a name to use on disk, which is safe for any pool type.
func NewInternalName() string {
	b := make([]byte, 8)
	mustRand(b)
	return InternalPrefix + hex.EncodeToString(b)
}

// NewUUID returns a random RFC 9562 version 4 UUID.
func NewUUID() string {
	b := make([]byte, 16)
	mustRand(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewNGUID returns a random namespace globally unique identifier.
func NewNGUID() string {
	b := make([]byte, 16)
	mustRand(b)
	return hex.EncodeToString(b)
}

func mustRand(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("no system randomness: %v", err))
	}
}

func contains[T comparable](s []T, v T) bool { return slices.Contains(s, v) }
