package spdk

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"nvweaver/internal/target"
)

type liveQueue struct {
	Controller int    `json:"cntlid"`
	HostNQN    string `json:"hostnqn"`
	Listen     struct {
		Address string `json:"traddr"`
		Service string `json:"trsvcid"`
	} `json:"listen_address"`
	Peer struct {
		Address string `json:"traddr"`
	} `json:"peer_address"`
}

// Connections implements target.Backend. It's one call for each subsystem, and a host that's only using discovery isn't listed, as it hasn't connected to one.
func (b *Backend) Connections(st *target.State, nqn string) ([]target.Connection, error) {
	nqns := slices.Sorted(maps.Keys(st.Subsystems))
	if nqn != "" {
		nqns = []string{nqn}
	}
	listeners := map[string]*target.Port{}
	for _, p := range st.Ports {
		listeners[p.Address+"/"+strconv.Itoa(p.Service)] = p
	}
	out := []target.Connection{}
	for _, name := range nqns {
		var queues []liveQueue
		if err := b.call("nvmf_subsystem_get_qpairs", map[string]any{"nqn": name}, &queues); err != nil {
			// It was removed after the state that this was given, so it has no connections.
			if strings.Contains(err.Error(), "not found") {
				continue
			}
			return nil, fmt.Errorf("connections to %s: %w", name, err)
		}
		// Each controller is one host's connection to this subsystem, and a host can have several, through different ports.
		controllers := map[int]*target.Connection{}
		for _, q := range queues {
			c, ok := controllers[q.Controller]
			if !ok {
				port, known := listeners[q.Listen.Address+"/"+q.Listen.Service]
				if !known {
					continue // A listener removed after that state.
				}
				c = &target.Connection{Port: port.ID, Transport: port.Transport, HostAddress: q.Peer.Address, Subsystem: name, HostNQN: q.HostNQN}
				controllers[q.Controller] = c
			}
			c.Queues++
		}
		for _, c := range controllers {
			out = append(out, *c)
		}
	}
	target.SortConnections(out)
	return out, nil
}
