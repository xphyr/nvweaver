package target

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Connection is one host's connection to a port, or to a subsystem where the target can tell.
type Connection struct {
	Port        uint16 `json:"port"`
	Transport   string `json:"transport"`
	HostAddress string `json:"host_address"`
	// Queues counts its queues, including the admin queue.
	Queues int `json:"queues"`
	// Subsystem and HostNQN are empty if the target can't tell.
	Subsystem string `json:"subsystem,omitempty"`
	HostNQN   string `json:"host_nqn,omitempty"`
}

// SortConnections puts connections in the order that the API promises.
func SortConnections(c []Connection) {
	slices.SortFunc(c, func(a, b Connection) int {
		return cmp.Or(cmp.Compare(a.Port, b.Port), cmp.Compare(a.Subsystem, b.Subsystem), cmp.Compare(a.HostNQN, b.HostNQN), cmp.Compare(a.HostAddress, b.HostAddress))
	})
}

const tcpEstablished = "01"

// Connections implements Backend. The kernel only says which host has which controller in debugfs, and only if it's built with CONFIG_NVME_TARGET_DEBUGFS, which
// Fedora's isn't. Its log can't be used, as it's overwritten and doesn't say when hosts leave. What it does always show is its sockets, one for each queue.
// These are in the network namespace of this process, so this needs the host's, as nvmet does.
func (t *Target) Connections(st *State, nqn string) ([]Connection, error) {
	ids := slices.Sorted(maps.Keys(st.Ports))
	if nqn != "" {
		sub, ok := st.Subsystems[nqn]
		if !ok {
			return nil, fmt.Errorf("no subsystem %s", nqn)
		}
		ids = sub.Ports
	}
	dir := t.ProcNet
	if dir == "" {
		dir = "/proc/net"
	}
	var sockets []tcpSocket
	for _, name := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue // A kernel with no IPv6.
		}
		if err != nil {
			return nil, err
		}
		found, err := parseProcNetTCP(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		sockets = append(sockets, found...)
	}
	type key struct {
		port uint16
		host netip.Addr
	}
	queues := map[key]int{}
	transports := map[uint16]string{}
	for _, id := range ids {
		port, ok := st.Ports[id]
		if !ok {
			continue
		}
		transports[id] = port.Transport
		// An address that doesn't parse is a port on a name, which nothing connects to as it can't listen.
		listen, err := netip.ParseAddr(port.Address)
		if err != nil {
			continue
		}
		listen = listen.Unmap()
		for _, s := range sockets {
			if s.local.Port() != uint16(port.Service) || (!listen.IsUnspecified() && s.local.Addr() != listen) {
				continue
			}
			queues[key{id, s.remote.Addr()}]++
		}
	}
	out := []Connection{}
	for k, n := range queues {
		out = append(out, Connection{Port: k.port, Transport: transports[k.port], HostAddress: k.host.String(), Queues: n})
	}
	SortConnections(out)
	return out, nil
}

type tcpSocket struct{ local, remote netip.AddrPort }

// parseProcNetTCP reads the established sockets from /proc/net/tcp or tcp6.
func parseProcNetTCP(b []byte) ([]tcpSocket, error) {
	var out []tcpSocket
	lines := bufio.NewScanner(bytes.NewReader(b))
	lines.Buffer(make([]byte, 4096), 1<<20)
	lines.Scan() // Its heading.
	for lines.Scan() {
		f := strings.Fields(lines.Text())
		if len(f) < 4 {
			return nil, fmt.Errorf("a line with %d fields", len(f))
		}
		if f[3] != tcpEstablished {
			continue
		}
		local, err := parseProcAddr(f[1])
		if err != nil {
			return nil, err
		}
		remote, err := parseProcAddr(f[2])
		if err != nil {
			return nil, err
		}
		out = append(out, tcpSocket{local, remote})
	}
	return out, lines.Err()
}

// parseProcAddr reads ADDRESS:PORT in hex. The kernel prints each 32-bit word of an address as a native integer, which is little-endian on both architectures that
// this is released for. It would need changing for a big-endian one.
func parseProcAddr(s string) (netip.AddrPort, error) {
	addr, port, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("bad address %q", s)
	}
	raw, err := hex.DecodeString(addr)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, fmt.Errorf("bad address %q", s)
	}
	for word := raw; len(word) > 0; word = word[4:] {
		slices.Reverse(word[:4])
	}
	ip, _ := netip.AddrFromSlice(raw)
	p, err := hex.DecodeString(port)
	if err != nil || len(p) != 2 {
		return netip.AddrPort{}, fmt.Errorf("bad port %q", s)
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(p[0])<<8|uint16(p[1])), nil
}
