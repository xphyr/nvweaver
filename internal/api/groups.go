package api

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"nvweaver/internal/storage"
	"nvweaver/internal/target"
)

// Snapshot groups: snapshots of several volumes as of one instant. Hosts' I/O to the volumes is paused with ANA while
// they're taken, so that they're consistent with each other. See decision 56.
const (
	maxGroupMembers = 64
	maxGroupName    = 120
	// pauseKey is held by the one group being taken, as only one may pause hosts at a time.
	pauseKey = "the snapshot group pause"
)

// maxPause is how long hosts may be kept waiting. Past it, the group is undone, rather than keep them longer. A variable
// for tests.
var maxPause = 10 * time.Second

func groupKey(name string) string { return "snapshot group " + name }

// memberName is the name of the snapshot of a group's i'th volume, from 0.
func memberName(group string, i int) string { return fmt.Sprintf("%s-%d", group, i+1) }

type snapshotGroupPage struct {
	Items     []*target.SnapshotGroup `json:"items"`
	NextAfter string                  `json:"next_after,omitempty"`
}

func (s *Server) listSnapshotGroups(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	limit, err := pageLimit(q.Get("limit"))
	if err != nil {
		return err
	}
	after := q.Get("after")
	return s.m.view(func(st *target.State) error {
		page := snapshotGroupPage{Items: []*target.SnapshotGroup{}}
		for _, name := range slices.Sorted(maps.Keys(st.SnapshotGroups)) {
			if name <= after {
				continue
			}
			if len(page.Items) == limit {
				page.NextAfter = page.Items[limit-1].Name
				break
			}
			page.Items = append(page.Items, st.SnapshotGroups[name])
		}
		writeJSON(w, http.StatusOK, page)
		return nil
	})
}

func (s *Server) getSnapshotGroup(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("group")
	return s.m.view(func(st *target.State) error {
		g := st.SnapshotGroups[name]
		if g == nil {
			return notFound("no such snapshot group %s", name)
		}
		writeJSON(w, http.StatusOK, g)
		return nil
	})
}

func (s *Server) createSnapshotGroup(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name    string   `json:"name"`
		Volumes []string `json:"volumes"`
		Owner   string   `json:"owner"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := checkObjectName("snapshot group", req.Name); err != nil {
		return err
	}
	if len(req.Name) > maxGroupName {
		return target.Invalid("a snapshot group's name can be at most %d characters, as its snapshots are named from it", maxGroupName)
	}
	if len(req.Volumes) == 0 || len(req.Volumes) > maxGroupMembers {
		return target.Invalid("a snapshot group has 1 to %d volumes, not %d", maxGroupMembers, len(req.Volumes))
	}
	seen := map[string]bool{}
	for _, v := range req.Volumes {
		if seen[v] {
			return target.Invalid("volume %s is named twice", v)
		}
		seen[v] = true
	}
	scope := scopeOf(r)
	owner, err := ownerFor(scope, req.Owner)
	if err != nil {
		return err
	}
	g, created, err := s.takeSnapshotGroup(req.Name, req.Volumes, owner, scope)
	if err != nil {
		return err
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, g)
	return nil
}

// member is one volume of a group being taken, and its snapshot.
type member struct {
	vol      *target.Volume
	name     string // the snapshot's
	internal string
	driver   storage.Driver
	path     string
	intent   bool // an intent was recorded, so it has to be settled
}

func (s *Server) takeSnapshotGroup(name string, volumes []string, owner, scope string) (*target.SnapshotGroup, bool, error) {
	res := s.m.reserve()
	defer res.release()
	if err := res.exclusive(groupKey(name)); err != nil {
		return nil, false, err
	}
	// This comes first, as with a snapshot, because the volumes may have gone by the time CSI retries.
	if g := s.m.current().SnapshotGroups[name]; g != nil {
		if err := mayChange(scope, g.Owner, "snapshot group "+name); err != nil {
			return nil, false, err
		}
		if !slices.Equal(g.Volumes, volumes) || g.Owner != owner {
			return nil, false, target.NewConflict("snapshot group %s already exists, of volumes %v", name, g.Volumes)
		}
		return g, false, nil
	}
	if err := s.m.admit(); err != nil {
		return nil, false, err
	}
	if res.exclusive(pauseKey) != nil {
		return nil, false, busy("another snapshot group is being taken; try again shortly")
	}
	members := make([]*member, len(volumes))
	for i, v := range volumes {
		m := &member{name: memberName(name, i)}
		members[i] = m
		if err := res.exclusive(snapshotKey(m.name)); err != nil {
			return nil, false, err
		}
		if s.m.current().Snapshots[m.name] != nil {
			return nil, false, target.NewConflict("snapshot %s already exists, which snapshot group %s would make", m.name, name)
		}
		if s.m.current().Volumes[v] == nil {
			return nil, false, target.Invalid("no such volume %s", v)
		}
		if err := res.shared(volumeKey(v)); err != nil {
			return nil, false, err
		}
	}
	// Read after the reservations, as the volumes can't change from here on.
	cur := s.m.current()
	for i, v := range volumes {
		m := members[i]
		if m.vol = cur.Volumes[v]; m.vol == nil {
			return nil, false, target.Invalid("no such volume %s", v)
		}
		if err := mayChange(scope, m.vol.Owner, "volume "+v); err != nil {
			return nil, false, err
		}
		if err := res.pool(m.vol.Pool); err != nil {
			return nil, false, err
		}
		driver, err := s.m.newDriver(cur.Pools[m.vol.Pool])
		if err != nil {
			return nil, false, err
		}
		m.driver = driver
	}
	// From here, anything made is removed again on failure, and the intents make that safe against a crash. Not in a
	// defer, which a test's simulated crash would run, as a real one wouldn't.
	fail := func(err error) (*target.SnapshotGroup, bool, error) {
		for _, m := range members {
			if m.intent {
				s.m.abandonCreate(m.driver, m.internal, m.name)
			}
		}
		return nil, false, err
	}
	for _, m := range members {
		m.internal = target.NewInternalName()
		res.track(m.internal)
		if err := s.m.beginIntent(opCreate, m.vol.Pool, m.internal, 0); err != nil {
			return fail(err)
		}
		m.intent = true
	}
	s.m.crashPoint("intent")

	pausedMS, err := s.withPaused(volumes, func(start time.Time) error {
		for _, m := range members {
			if time.Since(start) > maxPause {
				return busy("hosts were paused for more than %s, so snapshot group %s was undone; try again", maxPause, name)
			}
			path, err := m.driver.Snapshot(m.vol.InternalName, m.internal)
			if err != nil {
				return poolFull(m.vol.Pool, err)
			}
			m.path = path
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	s.m.crashPoint("storage")
	now := time.Now().UTC().Format(time.RFC3339)
	g := &target.SnapshotGroup{Name: name, Volumes: slices.Clone(volumes), PausedMS: pausedMS, Owner: owner, CreatedAt: now}
	for _, m := range members {
		g.Snapshots = append(g.Snapshots, m.name)
	}
	if err := s.m.commit(func(st *target.State) error {
		for _, m := range members {
			st.Snapshots[m.name] = &target.Snapshot{
				Name: m.name, Volume: m.vol.Name, Pool: m.vol.Pool, InternalName: m.internal, DevicePath: m.path,
				SizeBytes: m.vol.SizeBytes, Owner: owner, CreatedAt: now, Group: name,
			}
		}
		st.SnapshotGroups[name] = g
		return nil
	}); err != nil {
		return fail(err)
	}
	s.m.crashPoint("saved")
	for _, m := range members {
		s.m.endIntent(m.internal)
	}
	return g, true, nil
}

// withPaused runs fn with hosts' I/O to the volumes' namespaces paused, and returns how long they were paused, in ms. A
// volume that isn't exported has nothing to pause. Resuming is tried hard, as hosts wait until it works; if it can't,
// the error says so, and a restart resumes them, as a pause is never saved.
func (s *Server) withPaused(volumes []string, fn func(start time.Time) error) (int64, error) {
	members := map[string]bool{}
	for _, v := range volumes {
		members[v] = true
	}
	exported := false
	for _, sub := range s.m.current().Subsystems {
		for _, ns := range sub.Namespaces {
			exported = exported || members[ns.Volume] && ns.Enabled
		}
	}
	if !exported {
		return 0, fn(time.Now())
	}
	setPaused := func(on bool) func(*target.State) error {
		return func(st *target.State) error {
			for _, sub := range st.Subsystems {
				for _, ns := range sub.Namespaces {
					ns.Paused = on && members[ns.Volume] && ns.Enabled
				}
			}
			return nil
		}
	}
	start := time.Now()
	if err := s.m.apply(setPaused(true)); err != nil {
		// apply rolled back, so nothing is paused.
		return 0, fmt.Errorf("pausing hosts: %w", err)
	}
	s.m.crashPoint("paused")
	err := fn(start)
	var rerr error
	for attempt := range 5 {
		if rerr = s.m.apply(setPaused(false)); rerr == nil {
			break
		}
		s.m.log.Error("resuming hosts paused for a snapshot group", "attempt", attempt+1, "error", rerr)
		time.Sleep(200 * time.Millisecond)
	}
	paused := time.Since(start).Milliseconds()
	if rerr != nil {
		return paused, fmt.Errorf("hosts' I/O to %v is still paused, and restarting nvweaver resumes it: %w", volumes, rerr)
	}
	return paused, err
}

func (s *Server) deleteSnapshotGroup(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("group")
	scope := scopeOf(r)
	res := s.m.reserve()
	defer res.release()
	if err := s.m.admit(); err != nil {
		return err
	}
	if err := res.exclusive(groupKey(name)); err != nil {
		return err
	}
	g := s.m.current().SnapshotGroups[name]
	if g == nil {
		return notFound("no such snapshot group %s", name)
	}
	if err := mayChange(scope, g.Owner, "snapshot group "+name); err != nil {
		return err
	}
	// One at a time, each saved as it goes, as a snapshot is deleted. If one fails, the group keeps the rest, and
	// deleting it again carries on.
	for _, snapName := range slices.Clone(g.Snapshots) {
		if err := res.exclusive(snapshotKey(snapName)); err != nil {
			return err
		}
		snap := s.m.current().Snapshots[snapName]
		if snap == nil {
			continue
		}
		if err := s.removeStorage(res, snap.Pool, snap.InternalName, func(st *target.State) error {
			delete(st.Snapshots, snapName)
			if g := st.SnapshotGroups[name]; g != nil {
				g.Snapshots = slices.DeleteFunc(g.Snapshots, func(n string) bool { return n == snapName })
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := s.m.commit(func(st *target.State) error {
		delete(st.SnapshotGroups, name)
		return nil
	}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
