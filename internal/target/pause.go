package target

import (
	"path/filepath"
	"strconv"
)

// Pausing hosts' I/O to some namespaces, for a snapshot group, with ANA. A paused namespace is moved into
// PauseANAGroup, which is inaccessible on every port, so hosts using native NVMe multipath queue their I/O until it's
// moved back. Everything else stays in group 1, the kernel's default. See decision 56.

// PausedAny reports whether any namespace is paused.
func PausedAny(s *State) bool {
	for _, sub := range s.Subsystems {
		for _, ns := range sub.Namespaces {
			if ns.Paused {
				return true
			}
		}
	}
	return false
}

// ANAGroup is the ANA group a namespace belongs in.
func ANAGroup(ns *Namespace) int {
	if ns.Paused {
		return PauseANAGroup
	}
	return 1
}

func (t *Target) pauseGroupDir(port uint16) string {
	return filepath.Join(t.portDir(port), "ana_groups", strconv.Itoa(PauseANAGroup))
}

// pauseOn makes the pause group on these ports, inaccessible, before anything is moved into it: the kernel won't put a
// namespace in a group that no port has. A port that's new is made here, which is harmless: its attributes are written
// later, and nothing is linked to it until the end.
func (t *Target) pauseOn(ports []uint16) error {
	for _, id := range ports {
		if err := t.mkdir(t.portDir(id)); err != nil {
			return err
		}
		dir := t.pauseGroupDir(id)
		if err := t.mkdir(dir); err != nil {
			return err
		}
		if p := filepath.Join(dir, "ana_state"); t.differs(p, "inaccessible", nil) {
			if err := t.write(p, "inaccessible"); err != nil {
				return err
			}
		}
	}
	return nil
}

// pauseOff removes the pause group from these ports, once nothing is in it. It's run at startup too, which removes one
// left by a crash, after the namespaces have been moved back to group 1.
func (t *Target) pauseOff(ports []uint16) error {
	for _, id := range ports {
		if err := t.rmdir(t.pauseGroupDir(id)); err != nil {
			return err
		}
	}
	return nil
}
