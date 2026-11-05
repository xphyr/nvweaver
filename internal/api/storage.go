package api

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"syscall"
	"time"

	"nvweaver/internal/storage"
	"nvweaver/internal/target"
)

const (
	defaultPageSize = 500
	maxPageSize     = 1000
)

var objectNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func checkObjectName(kind, name string) error {
	if !objectNameRE.MatchString(name) {
		return target.Invalid("%s name %q must start with a letter or digit, and contain only letters, digits, '.', '_' and '-'", kind, name)
	}
	return nil
}

// --- pools ---

type capacityView struct {
	TotalBytes       int64   `json:"total_bytes"`
	UsedBytes        int64   `json:"used_bytes"`
	ProvisionedBytes int64   `json:"provisioned_bytes"`
	UsedPercent      float64 `json:"used_percent"`
}

type poolView struct {
	*target.Pool
	State     string        `json:"state"`
	Warnings  []string      `json:"warnings"`
	Capacity  *capacityView `json:"capacity,omitempty"`
	Volumes   int           `json:"volumes"`
	Snapshots int           `json:"snapshots"`
}

// viewPool asks the pool's driver for what can only be found out live.
func (s *Server) viewPool(st *target.State, p *target.Pool) poolView {
	v := poolView{Pool: p, State: "online", Warnings: []string{}}
	known := map[string]bool{}
	var provisioned int64
	for _, vol := range st.Volumes {
		if vol.Pool == p.Name {
			v.Volumes++
			provisioned += vol.SizeBytes
			known[vol.InternalName] = true
		}
	}
	for _, snap := range st.Snapshots {
		if snap.Pool == p.Name {
			v.Snapshots++
			known[snap.InternalName] = true
		}
	}
	d, err := s.m.newDriver(p)
	if err != nil {
		v.State = "missing"
		v.Warnings = append(v.Warnings, err.Error())
		return v
	}
	c, err := d.Capacity()
	if err != nil {
		v.State = "missing"
		v.Warnings = append(v.Warnings, err.Error())
		return v
	}
	v.Capacity = &capacityView{TotalBytes: c.TotalBytes, UsedBytes: c.UsedBytes, ProvisionedBytes: provisioned}
	if c.TotalBytes > 0 {
		v.Capacity.UsedPercent = float64(c.UsedBytes) / float64(c.TotalBytes) * 100
	}
	if p.WarningPercent > 0 && v.Capacity.UsedPercent >= float64(p.WarningPercent) {
		v.Warnings = append(v.Warnings, fmt.Sprintf("pool is %.1f%% full, past its warning level of %d%%", v.Capacity.UsedPercent, p.WarningPercent))
	}
	if names, err := d.List(); err == nil {
		for _, name := range names {
			if !known[name] {
				v.Warnings = append(v.Warnings, fmt.Sprintf("%s is on disk, but no volume refers to it", name))
			}
		}
	}
	return v
}

func (s *Server) listPools(w http.ResponseWriter, _ *http.Request) error {
	return s.m.view(func(st *target.State) error {
		out := []poolView{}
		for _, name := range slices.Sorted(maps.Keys(st.Pools)) {
			out = append(out, s.viewPool(st, st.Pools[name]))
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	})
}

func samePool(a, b *target.Pool) bool { return a.Type == b.Type && a.Path == b.Path }

func (s *Server) createPool(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name           string            `json:"name"`
		Type           string            `json:"type"`
		Path           string            `json:"path"`
		Labels         map[string]string `json:"labels"`
		WarningPercent *int              `json:"warning_percent"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := checkObjectName("pool", req.Name); err != nil {
		return err
	}
	pool := &target.Pool{Name: req.Name, Type: req.Type, Path: req.Path, Labels: req.Labels, WarningPercent: 80}
	if req.WarningPercent != nil {
		pool.WarningPercent = *req.WarningPercent
	}
	if pool.WarningPercent < 0 || pool.WarningPercent > 100 {
		return target.Invalid("warning_percent must be between 0 and 100")
	}
	d, err := s.m.newDriver(pool)
	if err != nil {
		return err
	}
	if err := d.Check(); err != nil {
		return err
	}
	created := false
	err = s.m.mutate(func(st *target.State) error {
		if existing := st.Pools[req.Name]; existing != nil {
			if !samePool(existing, pool) {
				return target.NewConflict("pool %s already exists with different settings", req.Name)
			}
			pool = existing
			return nil
		}
		for _, other := range st.Pools {
			if other.Type == pool.Type && other.Path == pool.Path {
				return target.NewConflict("pool %s already uses %s", other.Name, pool.Path)
			}
		}
		st.Pools[req.Name] = pool
		created = true
		return nil
	})
	if err != nil {
		return err
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	return s.m.view(func(st *target.State) error {
		writeJSON(w, code, s.viewPool(st, pool))
		return nil
	})
}

func (s *Server) getPool(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("pool")
	return s.m.view(func(st *target.State) error {
		p := st.Pools[name]
		if p == nil {
			return notFound("no such pool %s", name)
		}
		writeJSON(w, http.StatusOK, s.viewPool(st, p))
		return nil
	})
}

func (s *Server) updatePool(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("pool")
	var req struct {
		Labels         map[string]string `json:"labels"`
		WarningPercent *int              `json:"warning_percent"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	err := s.m.mutate(func(st *target.State) error {
		p := st.Pools[name]
		if p == nil {
			return notFound("no such pool %s", name)
		}
		if req.Labels != nil {
			p.Labels = req.Labels
		}
		if req.WarningPercent != nil {
			if *req.WarningPercent < 0 || *req.WarningPercent > 100 {
				return target.Invalid("warning_percent must be between 0 and 100")
			}
			p.WarningPercent = *req.WarningPercent
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.getPool(w, r)
}

// deletePool only forgets the pool: nothing on disk changes.
func (s *Server) deletePool(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("pool")
	err := s.m.mutate(func(st *target.State) error {
		if st.Pools[name] == nil {
			return notFound("no such pool %s", name)
		}
		for _, vol := range st.Volumes {
			if vol.Pool == name {
				return target.NewConflict("pool %s still has volumes, such as %s", name, vol.Name)
			}
		}
		for _, snap := range st.Snapshots {
			if snap.Pool == name {
				return target.NewConflict("pool %s still has snapshots, such as %s", name, snap.Name)
			}
		}
		delete(st.Pools, name)
		return nil
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- volumes ---

type mappingView struct {
	Subsystem string `json:"subsystem"`
	NSID      uint32 `json:"nsid"`
}

type volumeView struct {
	*target.Volume
	State    string       `json:"state"`
	MappedTo *mappingView `json:"mapped_to"`
}

type volumePage struct {
	Items     []volumeView `json:"items"`
	NextAfter string       `json:"next_after,omitempty"`
}

// mappings finds what each volume is mapped to.
func mappings(st *target.State) map[string]*mappingView {
	out := map[string]*mappingView{}
	for _, sub := range st.Subsystems {
		for _, ns := range sub.Namespaces {
			if ns.Volume != "" {
				out[ns.Volume] = &mappingView{Subsystem: sub.NQN, NSID: ns.NSID}
			}
		}
	}
	return out
}

func viewVolume(vol *target.Volume, mapped map[string]*mappingView) volumeView {
	return volumeView{Volume: vol, State: "online", MappedTo: mapped[vol.Name]}
}

func pageLimit(v string) (int, error) {
	if v == "" {
		return defaultPageSize, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxPageSize {
		return 0, target.Invalid("limit must be between 1 and %d", maxPageSize)
	}
	return n, nil
}

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	limit, err := pageLimit(q.Get("limit"))
	if err != nil {
		return err
	}
	after, pool := q.Get("after"), q.Get("pool")
	return s.m.view(func(st *target.State) error {
		if pool != "" && st.Pools[pool] == nil {
			return notFound("no such pool %s", pool)
		}
		page := volumePage{Items: []volumeView{}}
		mapped := mappings(st)
		for _, name := range slices.Sorted(maps.Keys(st.Volumes)) {
			vol := st.Volumes[name]
			if name <= after || (pool != "" && vol.Pool != pool) {
				continue
			}
			if len(page.Items) == limit {
				page.NextAfter = page.Items[limit-1].Name
				break
			}
			page.Items = append(page.Items, viewVolume(vol, mapped))
		}
		writeJSON(w, http.StatusOK, page)
		return nil
	})
}

// defaultPool lets a request leave out the pool when there's only one to choose from.
func defaultPool(st *target.State, requested string) (*target.Pool, error) {
	if requested != "" {
		p := st.Pools[requested]
		if p == nil {
			return nil, target.Invalid("no such pool %s", requested)
		}
		return p, nil
	}
	switch len(st.Pools) {
	case 0:
		return nil, target.Invalid("there are no pools; adopt one first")
	case 1:
		for _, p := range st.Pools {
			return p, nil
		}
	}
	return nil, target.Invalid("there are %d pools, so pool is required", len(st.Pools))
}

// poolFull turns running out of space into the status that says so.
func poolFull(pool string, err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return &statusError{http.StatusInsufficientStorage, fmt.Sprintf("pool %s is full: %v", pool, err)}
	}
	return err
}

// cloneSource is what a clone is made from.
type cloneSource struct {
	pool, internal string
	sizeBytes      int64
}

func findSource(st *target.State, src *target.VolumeSource) (cloneSource, error) {
	if src.Snapshot != "" {
		snap := st.Snapshots[src.Snapshot]
		if snap == nil {
			return cloneSource{}, target.Invalid("no such snapshot %s", src.Snapshot)
		}
		return cloneSource{snap.Pool, snap.InternalName, snap.SizeBytes}, nil
	}
	vol := st.Volumes[src.Volume]
	if vol == nil {
		return cloneSource{}, target.Invalid("no such volume %s", src.Volume)
	}
	return cloneSource{vol.Pool, vol.InternalName, vol.SizeBytes}, nil
}

func sameSource(a, b *target.VolumeSource) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name      string               `json:"name"`
		Pool      string               `json:"pool"`
		SizeBytes int64                `json:"size_bytes"`
		Source    *target.VolumeSource `json:"source"`
		Labels    map[string]string    `json:"labels"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := checkObjectName("volume", req.Name); err != nil {
		return err
	}
	if src := req.Source; src != nil && (src.Snapshot == "") == (src.Volume == "") {
		return target.Invalid("source must name either a snapshot or a volume")
	}
	sizeGiven := req.SizeBytes != 0 || req.Source == nil
	if sizeGiven && (req.SizeBytes < 4096 || req.SizeBytes%4096 != 0) {
		return target.Invalid("size_bytes must be a positive multiple of 4096")
	}
	var vol *target.Volume
	var driver storage.Driver
	created := false
	err := s.m.mutate(func(st *target.State) error {
		// CSI retries, so the same request again is a success, not a conflict. This comes first
		// because a clone's source may have gone by then.
		if existing := st.Volumes[req.Name]; existing != nil {
			wantPool := req.Pool
			if req.Source == nil {
				p, err := defaultPool(st, req.Pool)
				if err != nil {
					return err
				}
				wantPool = p.Name
			}
			if (wantPool != "" && existing.Pool != wantPool) ||
				(sizeGiven && existing.SizeBytes != req.SizeBytes) ||
				!sameSource(existing.Source, req.Source) {
				return target.NewConflict("volume %s already exists with different settings", req.Name)
			}
			vol = existing
			return nil
		}
		var pool *target.Pool
		var source cloneSource
		var err error
		if req.Source != nil {
			if source, err = findSource(st, req.Source); err != nil {
				return err
			}
			if req.Pool != "" && req.Pool != source.pool {
				return target.Invalid("a clone stays in its source's pool, %s", source.pool)
			}
			pool = st.Pools[source.pool]
			if sizeGiven && req.SizeBytes < source.sizeBytes {
				return target.Invalid("a clone can't be smaller than its source (%d bytes)", source.sizeBytes)
			}
			if !sizeGiven {
				req.SizeBytes = source.sizeBytes
			}
		} else if pool, err = defaultPool(st, req.Pool); err != nil {
			return err
		}
		if driver, err = s.m.newDriver(pool); err != nil {
			return err
		}
		internal := target.NewInternalName()
		var path string
		if req.Source != nil {
			path, err = driver.Clone(source.internal, internal, req.SizeBytes)
		} else {
			path, err = driver.Create(internal, req.SizeBytes)
		}
		if err != nil {
			return poolFull(pool.Name, err)
		}
		vol = &target.Volume{
			Name:         req.Name,
			Pool:         pool.Name,
			InternalName: internal,
			DevicePath:   path,
			SizeBytes:    req.SizeBytes,
			UUID:         target.NewUUID(),
			NGUID:        target.NewNGUID(),
			Source:       req.Source,
			Labels:       req.Labels,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		st.Volumes[req.Name] = vol
		created = true
		return nil
	})
	if err != nil {
		if created && driver != nil {
			if derr := driver.Delete(vol.InternalName); derr != nil {
				s.m.log.Error("couldn't clean up a volume after a failed create", "volume", req.Name, "error", derr)
			}
		}
		return err
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	return s.m.view(func(st *target.State) error {
		writeJSON(w, code, viewVolume(vol, mappings(st)))
		return nil
	})
}

func (s *Server) getVolume(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("volume")
	return s.m.view(func(st *target.State) error {
		vol := st.Volumes[name]
		if vol == nil {
			return notFound("no such volume %s", name)
		}
		writeJSON(w, http.StatusOK, viewVolume(vol, mappings(st)))
		return nil
	})
}

func (s *Server) updateVolume(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("volume")
	var req struct {
		SizeBytes *int64            `json:"size_bytes"`
		Labels    map[string]string `json:"labels"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	var mapped *mappingView
	grew := false
	err := s.m.mutate(func(st *target.State) error {
		vol := st.Volumes[name]
		if vol == nil {
			return notFound("no such volume %s", name)
		}
		if req.Labels != nil {
			vol.Labels = req.Labels
		}
		if req.SizeBytes == nil || *req.SizeBytes == vol.SizeBytes {
			return nil
		}
		switch {
		case *req.SizeBytes < vol.SizeBytes:
			return target.Invalid("volumes can grow but not shrink (currently %d bytes)", vol.SizeBytes)
		case *req.SizeBytes%4096 != 0:
			return target.Invalid("size_bytes must be a multiple of 4096")
		}
		driver, err := s.m.newDriver(st.Pools[vol.Pool])
		if err != nil {
			return err
		}
		if err := driver.Grow(vol.InternalName, *req.SizeBytes); err != nil {
			return err
		}
		vol.SizeBytes = *req.SizeBytes
		mapped = mappings(st)[name]
		grew = true
		return nil
	})
	if err != nil {
		return err
	}
	if grew && mapped != nil && s.m.backend.LiveEnabled(mapped.Subsystem, mapped.NSID) {
		if err := s.m.backend.Revalidate(mapped.Subsystem, mapped.NSID); err != nil {
			return err
		}
	}
	return s.getVolume(w, r)
}

func (s *Server) deleteVolume(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("volume")
	var driver storage.Driver
	var internal string
	err := s.m.mutate(func(st *target.State) error {
		vol := st.Volumes[name]
		if vol == nil {
			return notFound("no such volume %s", name)
		}
		if m := mappings(st)[name]; m != nil {
			return target.NewConflict("volume %s is mapped into %s as namespace %d", name, m.Subsystem, m.NSID)
		}
		var err error
		if driver, err = s.m.newDriver(st.Pools[vol.Pool]); err != nil {
			return err
		}
		internal = vol.InternalName
		delete(st.Volumes, name)
		return nil
	})
	if err != nil {
		return err
	}
	// Saved first: a failure here leaves a file that the pool reports, not a volume with no storage.
	if err := driver.Delete(internal); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- snapshots ---

type snapshotView struct {
	*target.Snapshot
	State string `json:"state"`
}

type snapshotPage struct {
	Items     []snapshotView `json:"items"`
	NextAfter string         `json:"next_after,omitempty"`
}

func viewSnapshot(snap *target.Snapshot) snapshotView {
	return snapshotView{Snapshot: snap, State: "online"}
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	limit, err := pageLimit(q.Get("limit"))
	if err != nil {
		return err
	}
	after, volume := q.Get("after"), q.Get("volume")
	return s.m.view(func(st *target.State) error {
		page := snapshotPage{Items: []snapshotView{}}
		for _, name := range slices.Sorted(maps.Keys(st.Snapshots)) {
			snap := st.Snapshots[name]
			// The volume needn't exist: a snapshot can outlive it.
			if name <= after || (volume != "" && snap.Volume != volume) {
				continue
			}
			if len(page.Items) == limit {
				page.NextAfter = page.Items[limit-1].Name
				break
			}
			page.Items = append(page.Items, viewSnapshot(snap))
		}
		writeJSON(w, http.StatusOK, page)
		return nil
	})
}

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name   string `json:"name"`
		Volume string `json:"volume"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := checkObjectName("snapshot", req.Name); err != nil {
		return err
	}
	var snap *target.Snapshot
	var driver storage.Driver
	created := false
	err := s.m.mutate(func(st *target.State) error {
		// This comes first because the volume may have gone by the time CSI retries.
		if existing := st.Snapshots[req.Name]; existing != nil {
			if existing.Volume != req.Volume {
				return target.NewConflict("snapshot %s already exists, of volume %s", req.Name, existing.Volume)
			}
			snap = existing
			return nil
		}
		vol := st.Volumes[req.Volume]
		if vol == nil {
			return target.Invalid("no such volume %s", req.Volume)
		}
		var err error
		if driver, err = s.m.newDriver(st.Pools[vol.Pool]); err != nil {
			return err
		}
		internal := target.NewInternalName()
		path, err := driver.Snapshot(vol.InternalName, internal)
		if err != nil {
			return poolFull(vol.Pool, err)
		}
		snap = &target.Snapshot{
			Name:         req.Name,
			Volume:       vol.Name,
			Pool:         vol.Pool,
			InternalName: internal,
			DevicePath:   path,
			SizeBytes:    vol.SizeBytes,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		st.Snapshots[req.Name] = snap
		created = true
		return nil
	})
	if err != nil {
		if created && driver != nil {
			if derr := driver.Delete(snap.InternalName); derr != nil {
				s.m.log.Error("couldn't clean up a snapshot after a failed create", "snapshot", req.Name, "error", derr)
			}
		}
		return err
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, viewSnapshot(snap))
	return nil
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("snapshot")
	return s.m.view(func(st *target.State) error {
		snap := st.Snapshots[name]
		if snap == nil {
			return notFound("no such snapshot %s", name)
		}
		writeJSON(w, http.StatusOK, viewSnapshot(snap))
		return nil
	})
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("snapshot")
	var driver storage.Driver
	var internal string
	err := s.m.mutate(func(st *target.State) error {
		snap := st.Snapshots[name]
		if snap == nil {
			return notFound("no such snapshot %s", name)
		}
		var err error
		if driver, err = s.m.newDriver(st.Pools[snap.Pool]); err != nil {
			return err
		}
		internal = snap.InternalName
		delete(st.Snapshots, name)
		return nil
	})
	if err != nil {
		return err
	}
	// Saved first, as with volumes: a failure here leaves a file that the pool reports.
	if err := driver.Delete(internal); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
