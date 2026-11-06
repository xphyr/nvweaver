package api

import (
	"fmt"
	"maps"
	"net/http"

	"nvweaver/internal/target"
)

// Slow storage work, such as a full clone, runs outside the lock that serialises changes, so it can't
// hold up the rest of the API. A reservation stops the objects it depends on changing meanwhile: anything
// that clashes gets a 503, which clients are told to retry. Nothing waits on a reservation.
const (
	// maxWaitingChanges bounds how many requests queue for the lock. Beyond it, waiting only uses up a
	// client's timeout, and the server's own 60 s limit on writing a response.
	maxWaitingChanges = 256
	// maxPoolOperations bounds storage work running at once on one pool, each of which may be a process.
	maxPoolOperations = 32
	retryAfterSeconds = "1"
)

func busy(format string, a ...any) error {
	return &statusError{http.StatusServiceUnavailable, fmt.Sprintf(format, a...)}
}

func volumeKey(name string) string   { return "volume " + name }
func snapshotKey(name string) string { return "snapshot " + name }
func poolKey(name string) string     { return "pool " + name }

// admit refuses more work when too many changes are already waiting.
func (m *Manager) admit() error {
	if m.waiting.Load() >= maxWaitingChanges {
		return busy("%d changes are waiting; try again shortly", maxWaitingChanges)
	}
	return nil
}

// reservation is what one request holds. Release it before answering, or a client that acts straight
// away on the answer could find its own request in the way.
type reservation struct {
	m        *Manager
	held     []string
	sole     []string
	internal []string
}

func (m *Manager) reserve() *reservation {
	return &reservation{m: m}
}

// exclusive claims an object for changing. Holding a volume's name this way is what makes it safe to
// read the volume, work on its storage without the lock, and then save the result.
func (r *reservation) exclusive(key string) error {
	r.m.busyMu.Lock()
	defer r.m.busyMu.Unlock()
	if r.m.busy[key] != 0 {
		return busy("%s is in the middle of another change", key)
	}
	r.m.busy[key] = -1
	r.sole = append(r.sole, key)
	return nil
}

// shared stops an object changing or going away while something depends on it.
func (r *reservation) shared(key string) error {
	r.m.busyMu.Lock()
	defer r.m.busyMu.Unlock()
	if r.m.busy[key] < 0 {
		return busy("%s is in the middle of another change", key)
	}
	r.m.busy[key]++
	r.held = append(r.held, key)
	return nil
}

// pool holds a pool for storage work, which also stops recovery settling this work as if it had been
// interrupted, and stops the pool being forgotten.
func (r *reservation) pool(name string) error {
	key := poolKey(name)
	r.m.busyMu.Lock()
	over := r.m.busy[key] >= maxPoolOperations
	r.m.busyMu.Unlock()
	if over {
		return busy("pool %s already has %d operations running", name, maxPoolOperations)
	}
	if err := r.shared(key); err != nil {
		return err
	}
	if r.m.current().Pools[name] == nil {
		return target.Invalid("no such pool %s", name)
	}
	return nil
}

// track marks storage that exists without the state referring to it yet, or any longer, so that it
// isn't reported as a stray.
func (r *reservation) track(internal string) {
	r.m.busyMu.Lock()
	defer r.m.busyMu.Unlock()
	r.m.inFlight[internal] = true
	r.internal = append(r.internal, internal)
}

func (r *reservation) release() {
	r.m.busyMu.Lock()
	defer r.m.busyMu.Unlock()
	for _, key := range r.sole {
		delete(r.m.busy, key)
	}
	for _, key := range r.held {
		if r.m.busy[key]--; r.m.busy[key] == 0 {
			delete(r.m.busy, key)
		}
	}
	for _, name := range r.internal {
		delete(r.m.inFlight, name)
	}
	r.sole, r.held, r.internal = nil, nil, nil
}

func (m *Manager) inFlightNames() map[string]bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	return maps.Clone(m.inFlight)
}
