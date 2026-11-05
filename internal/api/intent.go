package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"nvweaver/internal/storage"
	"nvweaver/internal/target"
)

// The intent log makes changes to a pool safe against a crash. A pool and state.json can't change
// together, so before touching a pool, this records which internal name is about to change. Recovery
// then needs only one rule: if the saved state refers to the name, that storage is wanted, and
// otherwise it's removed. That settles a create or a delete interrupted at any point, because each
// saves the state at the moment it takes effect. A grow can't safely be undone, so it's finished.
const (
	opCreate = "create"
	opDelete = "delete"
	opGrow   = "grow"
)

// intent is one change to a pool that hasn't been confirmed finished.
type intent struct {
	Op        string `json:"op"`
	Pool      string `json:"pool"`
	Internal  string `json:"internal_name"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	At        string `json:"at"`
}

// The name becomes a file name, and comes back from disk, so this is also what stops a damaged
// log from naming a file outside the pool.
var internalNameRE = regexp.MustCompile("^" + target.InternalPrefix + "[0-9a-f]+$")

func (m *Manager) intentPath(internal string) string {
	return filepath.Join(m.intentDir, internal+".json")
}

// beginIntent durably records a change before it starts. Each is its own small file, so the cost
// doesn't grow with the number of volumes.
func (m *Manager) beginIntent(op, pool, internal string, sizeBytes int64) error {
	b, err := json.Marshal(intent{Op: op, Pool: pool, Internal: internal, SizeBytes: sizeBytes, At: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	if err := writeDurably(m.intentPath(internal), b); err != nil {
		return fmt.Errorf("record intent: %w", err)
	}
	return nil
}

// endIntent forgets a finished change. If this is lost in a crash, recovery settles it again, which
// changes nothing.
func (m *Manager) endIntent(internal string) {
	if err := os.Remove(m.intentPath(internal)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Error("couldn't remove a finished intent", "name", internal, "error", err)
	}
}

// abandonCreate removes what a failed create may have left. If that fails too, the intent stays, and
// the next start tries again.
func (m *Manager) abandonCreate(driver storage.Driver, internal, name string) {
	if err := driver.Delete(internal); err != nil {
		m.log.Error("couldn't clean up after a failed create", "name", name, "internal_name", internal, "error", err)
		return
	}
	m.endIntent(internal)
}

// crashPoint lets tests stop a request exactly where a crash would.
func (m *Manager) crashPoint(point string) {
	if m.crash != nil {
		m.crash(point)
	}
}

func (m *Manager) readIntents() ([]intent, error) {
	entries, err := os.ReadDir(m.intentDir)
	if err != nil {
		return nil, err
	}
	var out []intent
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.intentDir, e.Name()))
		if err != nil {
			return nil, err
		}
		var in intent
		if err := json.Unmarshal(b, &in); err != nil || in.Internal != name || !internalNameRE.MatchString(name) {
			m.log.Error("ignoring a damaged intent; check the pool for a file nothing refers to", "file", e.Name())
			continue
		}
		out = append(out, in)
	}
	return out, nil
}

// Recover settles whatever was interrupted, and must run before the service takes requests. An
// intent that can't be settled, say because a pool's disk is missing, stays for the next start.
func (m *Manager) Recover() {
	m.settleIntents("")
}

// settleIntents settles the intents of one pool, or of all of them.
func (m *Manager) settleIntents(pool string) {
	intents, err := m.readIntents()
	if err != nil {
		m.log.Error("couldn't read the intent log", "error", err)
		return
	}
	for _, in := range intents {
		if pool != "" && in.Pool != pool {
			continue
		}
		if err := m.settle(in); err != nil {
			m.log.Error("couldn't finish an interrupted change; will try again at the next start",
				"op", in.Op, "pool", in.Pool, "internal_name", in.Internal, "error", err)
			continue
		}
		m.log.Info("settled an interrupted change", "op", in.Op, "pool", in.Pool, "internal_name", in.Internal)
		m.endIntent(in.Internal)
	}
}

func (m *Manager) settle(in intent) error {
	var pool *target.Pool
	var referenced bool
	var volume string
	err := m.view(func(st *target.State) error {
		pool = st.Pools[in.Pool]
		for _, vol := range st.Volumes {
			if vol.InternalName == in.Internal {
				referenced, volume = true, vol.Name
			}
		}
		for _, snap := range st.Snapshots {
			if snap.InternalName == in.Internal {
				referenced = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if referenced && in.Op != opGrow {
		return nil
	}
	if !referenced && in.Op == opGrow {
		return nil // The volume has gone, and so has anything to finish.
	}
	if pool == nil {
		return fmt.Errorf("pool %s is unknown", in.Pool)
	}
	driver, err := m.newDriver(pool)
	if err != nil {
		return err
	}
	// A pool whose disk is missing must not look as if its files were already gone.
	if _, err := driver.Capacity(); err != nil {
		return err
	}
	if in.Op == opGrow {
		return m.finishGrow(driver, volume, in)
	}
	return driver.Delete(in.Internal)
}

// finishGrow completes a grow, as the client asked for it, and the file may already be larger.
func (m *Manager) finishGrow(driver storage.Driver, volume string, in intent) error {
	if err := driver.Grow(in.Internal, in.SizeBytes); err != nil {
		return err
	}
	var mapped *mappingView
	err := m.mutate(func(st *target.State) error {
		if vol := st.Volumes[volume]; vol != nil && vol.SizeBytes < in.SizeBytes {
			vol.SizeBytes = in.SizeBytes
		}
		mapped = mappings(st)[volume]
		return nil
	})
	if err != nil {
		return err
	}
	if mapped != nil && m.backend.LiveEnabled(mapped.Subsystem, mapped.NSID) {
		return m.backend.Revalidate(mapped.Subsystem, mapped.NSID)
	}
	return nil
}

func pendingFor(intents []intent, pool string) bool {
	for _, in := range intents {
		if in.Pool == pool {
			return true
		}
	}
	return false
}
