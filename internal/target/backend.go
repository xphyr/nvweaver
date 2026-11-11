package target

import "syscall"

// Capabilities says what a backend can do that the other might not, so that
// automation can check rather than discovering it through errors.
type Capabilities struct {
	// Passthrough can export a whole NVMe controller.
	Passthrough bool `json:"passthrough"`
	// SurvivesServiceExit keeps serving hosts with no container running.
	SurvivesServiceExit bool `json:"survives_service_exit"`
	// PerPortInlineData can set inline_data_size for each port.
	PerPortInlineData bool `json:"per_port_inline_data"`
	// BufferedIO can serve a namespace through the page cache.
	BufferedIO bool `json:"buffered_io"`
	// PerHostDHCHAPParams honours dhchap_hash and dhchap_dhgroup for each host.
	PerHostDHCHAPParams bool `json:"per_host_dhchap_params"`
	// WildcardListen can serve hosts on 0.0.0.0 or ::.
	WildcardListen bool `json:"wildcard_listen"`
	// IdentifiesConnectedHosts can say which host is connected to which subsystem.
	IdentifiesConnectedHosts bool `json:"identifies_connected_hosts"`
	// ListsAllConnections can list every connection at once, rather than one subsystem's. See decision 43.
	ListsAllConnections bool `json:"lists_all_connections"`
	// SnapshotGroups can pause hosts' I/O with ANA, for snapshot groups consistent across volumes. See decision 56.
	SnapshotGroups bool `json:"snapshot_groups"`
	// Transports lists the NVMe transports a port can use.
	Transports []string `json:"transports"`
}

// LiveChecker reports whether a namespace is really being served.
type LiveChecker func(nqn string, nsid uint32) bool

// Backend is an NVMe/TCP target implementation.
type Backend interface {
	// Name identifies the backend, as in NVWEAVER_BACKEND.
	Name() string
	Capabilities() Capabilities
	// Check rejects configuration this backend cannot express, before anything
	// is changed.
	Check(*State) error
	// Reconcile moves the target from prev to next. Hosts must never be able to
	// discover a subsystem that is only partly configured.
	Reconcile(prev, next *State) error
	// Revalidate tells the target that a namespace's backing size changed.
	Revalidate(nqn string, nsid uint32) error
	// LiveEnabled reports whether a namespace is really being served.
	LiveEnabled(nqn string, nsid uint32) bool
	// LiveEnabledAll is for checking many namespaces at once. It may take a snapshot, so don't
	// keep the result beyond one request.
	LiveEnabledAll() LiveChecker
	// Connections lists who's connected, to one subsystem's ports or to all of them if nqn is empty. It reads the target, so it may be slow.
	Connections(st *State, nqn string) ([]Connection, error)
	// Available reports whether the target is usable right now.
	Available() bool
	// Close releases anything the backend runs. Configuration removal is
	// separate: Reconcile to an empty state first if that is wanted.
	Close() error
	// Done is closed if the target stops unexpectedly. It may be nil, for a
	// target that cannot stop independently of the kernel.
	Done() <-chan struct{}
}

// Name implements Backend.
func (t *Target) Name() string { return "nvmet" }

// Capabilities implements Backend.
func (t *Target) Capabilities() Capabilities {
	return Capabilities{
		Passthrough:         true,
		SurvivesServiceExit: true,
		PerPortInlineData:   true,
		BufferedIO:          true,
		PerHostDHCHAPParams: true,
		WildcardListen:      true,
		ListsAllConnections: true,
		SnapshotGroups:      true,
		Transports:          []string{TransportTCP},
	}
}

// overlayfsMagic is OVERLAYFS_SUPER_MAGIC from linux/magic.h.
const overlayfsMagic = 0x794c7630

// Check implements Backend. nvmet enables a namespace backed by a file on
// overlayfs without complaint, but hosts then see a hidden disk of size zero with
// no device node. That is what a container's own filesystem is, so it is an easy
// mistake to make and otherwise a very hard one to diagnose.
func (t *Target) Check(s *State) error {
	for _, sub := range s.Subsystems {
		for _, ns := range sub.Namespaces {
			var fs syscall.Statfs_t
			if err := syscall.Statfs(ns.DevicePath, &fs); err != nil {
				continue // Reported when the namespace is enabled.
			}
			if int64(fs.Type) == overlayfsMagic {
				return Invalid("%s is on overlayfs, which the nvmet backend cannot serve to hosts; "+
					"keep it on a real filesystem, such as a volume mounted at the data directory", ns.DevicePath)
			}
		}
	}
	return nil
}

// LiveEnabledAll implements Backend. Each check reads one configfs file, so there's nothing to save.
func (t *Target) LiveEnabledAll() LiveChecker { return t.LiveEnabled }

// Close implements Backend. The kernel target needs no release.
func (t *Target) Close() error { return nil }

// Done implements Backend.
func (t *Target) Done() <-chan struct{} { return nil }
