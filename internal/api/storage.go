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
	// Only for pools with metadata of their own, whose filling up is as bad as data filling up.
	MetadataUsedPercent float64 `json:"metadata_used_percent,omitempty"`
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
	v.Capacity = &capacityView{TotalBytes: c.TotalBytes, UsedBytes: c.UsedBytes, ProvisionedBytes: provisioned, MetadataUsedPercent: c.MetadataUsedPercent}
	v.Warnings = append(v.Warnings, c.Warnings...)
	if c.TotalBytes > 0 {
		v.Capacity.UsedPercent = float64(c.UsedBytes) / float64(c.TotalBytes) * 100
	}
	if p.WarningPercent > 0 && v.Capacity.UsedPercent >= float64(p.WarningPercent) {
		v.Warnings = append(v.Warnings, fmt.Sprintf("pool is %.1f%% full, past its warning level of %d%%", v.Capacity.UsedPercent, p.WarningPercent))
	}
	if p.WarningPercent > 0 && c.MetadataUsedPercent >= float64(p.WarningPercent) {
		v.Warnings = append(v.Warnings, fmt.Sprintf("pool's metadata is %.1f%% full, past its warning level of %d%%", c.MetadataUsedPercent, p.WarningPercent))
	}
	// Storage comes and goes while this looks. In flight either before listing or after, or referred to
	// by the state either before or after, isn't a stray: a change that finishes meanwhile is one of those.
	maps.Copy(known, s.m.inFlightNames())
	if names, err := d.List(); err == nil {
		for _, name := range names {
			if !known[name] && !s.m.inFlightNames()[name] && !s.m.isReferenced(name) {
				v.Warnings = append(v.Warnings, fmt.Sprintf("%s is on disk, but no volume refers to it", name))
			}
		}
	}
	return v
}

// isReferenced looks again, as a volume may have been finished since st was read.
func (m *Manager) isReferenced(internal string) bool {
	st := m.current()
	for _, vol := range st.Volumes {
		if vol.InternalName == internal {
			return true
		}
	}
	for _, snap := range st.Snapshots {
		if snap.InternalName == internal {
			return true
		}
	}
	return false
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

func samePool(a, b *target.Pool) bool {
	return a.Type == b.Type && a.Path == b.Path && a.VolumeGroup == b.VolumeGroup && a.ThinPool == b.ThinPool
}

func (s *Server) createPool(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name           string            `json:"name"`
		Type           string            `json:"type"`
		Path           string            `json:"path"`
		VolumeGroup    string            `json:"volume_group"`
		ThinPool       string            `json:"thin_pool"`
		Labels         map[string]string `json:"labels"`
		WarningPercent *int              `json:"warning_percent"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := checkObjectName("pool", req.Name); err != nil {
		return err
	}
	pool := &target.Pool{Name: req.Name, Type: req.Type, Path: req.Path, VolumeGroup: req.VolumeGroup, ThinPool: req.ThinPool, Labels: req.Labels, WarningPercent: 80}
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
			if samePool(other, pool) {
				return target.NewConflict("pool %s already uses that storage", other.Name)
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
	// With this held nothing is at work on the pool, so any intent left was really interrupted.
	res := s.m.reserve()
	defer res.release()
	if err := res.exclusive(poolKey(name)); err != nil {
		return err
	}
	s.m.settleIntents(name)
	err := s.m.mutate(func(st *target.State) error {
		if st.Pools[name] == nil {
			return notFound("no such pool %s", name)
		}
		intents, err := s.m.readIntents()
		if err != nil {
			return err
		}
		if pendingFor(intents, name) {
			return target.NewConflict("pool %s has a change that couldn't be finished; see the service's log", name)
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
	res.release()
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

type createVolumeRequest struct {
	Name      string               `json:"name"`
	Pool      string               `json:"pool"`
	SizeBytes int64                `json:"size_bytes"`
	Source    *target.VolumeSource `json:"source"`
	Labels    map[string]string    `json:"labels"`
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) error {
	var req createVolumeRequest
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := checkObjectName("volume", req.Name); err != nil {
		return err
	}
	if src := req.Source; src != nil && (src.Snapshot == "") == (src.Volume == "") {
		return target.Invalid("source must name either a snapshot or a volume")
	}
	if (req.SizeBytes != 0 || req.Source == nil) && (req.SizeBytes < 4096 || req.SizeBytes%4096 != 0) {
		return target.Invalid("size_bytes must be a positive multiple of 4096")
	}
	vol, created, err := s.provisionVolume(req)
	if err != nil {
		return err
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, viewVolume(vol, mappings(s.m.current())))
	return nil
}

// existingVolume answers a retry. CSI retries, so the same request again is a success, not a conflict.
func (s *Server) existingVolume(req createVolumeRequest, existing *target.Volume) error {
	st := s.m.current()
	sizeGiven := req.SizeBytes != 0 || req.Source == nil
	wantPool := req.Pool
	if req.Source == nil {
		p, err := defaultPool(st, req.Pool)
		if err != nil {
			return err
		}
		wantPool = p.Name
	}
	// A size that the pool rounded up the first time has to match on a retry.
	wantSize := req.SizeBytes
	if sizeGiven {
		d, err := s.m.newDriver(st.Pools[existing.Pool])
		if err != nil {
			return err
		}
		if wantSize, err = d.AlignSize(req.SizeBytes); err != nil {
			return err
		}
	}
	if (wantPool != "" && existing.Pool != wantPool) ||
		(sizeGiven && existing.SizeBytes != wantSize) ||
		!sameSource(existing.Source, req.Source) {
		return target.NewConflict("volume %s already exists with different settings", req.Name)
	}
	return nil
}

// provisionVolume makes the storage without the lock, which for a full clone can take minutes, then
// saves the volume. It returns once its reservations are released.
func (s *Server) provisionVolume(req createVolumeRequest) (*target.Volume, bool, error) {
	res := s.m.reserve()
	defer res.release()
	if err := res.exclusive(volumeKey(req.Name)); err != nil {
		return nil, false, err
	}
	// This comes first because a clone's source may have gone by the time of a retry.
	if existing := s.m.current().Volumes[req.Name]; existing != nil {
		if err := s.existingVolume(req, existing); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	if err := s.m.admit(); err != nil {
		return nil, false, err
	}
	if src := req.Source; src != nil {
		key := volumeKey(src.Volume)
		if src.Snapshot != "" {
			key = snapshotKey(src.Snapshot)
		}
		if _, err := findSource(s.m.current(), src); err != nil {
			return nil, false, err
		}
		if err := res.shared(key); err != nil {
			return nil, false, err
		}
	}
	// Read after the reservations, because the source can't change from here on.
	st := s.m.current()
	var pool *target.Pool
	var source cloneSource
	var err error
	if req.Source != nil {
		if source, err = findSource(st, req.Source); err != nil {
			return nil, false, err
		}
		if req.Pool != "" && req.Pool != source.pool {
			return nil, false, target.Invalid("a clone stays in its source's pool, %s", source.pool)
		}
		pool = st.Pools[source.pool]
	} else if pool, err = defaultPool(st, req.Pool); err != nil {
		return nil, false, err
	}
	if err := res.pool(pool.Name); err != nil {
		return nil, false, err
	}
	driver, err := s.m.newDriver(pool)
	if err != nil {
		return nil, false, err
	}
	sizeGiven := req.SizeBytes != 0 || req.Source == nil
	size := req.SizeBytes
	if sizeGiven {
		if size, err = driver.AlignSize(req.SizeBytes); err != nil {
			return nil, false, err
		}
	}
	if req.Source != nil {
		if sizeGiven && size < source.sizeBytes {
			return nil, false, target.Invalid("a clone can't be smaller than its source (%d bytes)", source.sizeBytes)
		}
		if !sizeGiven {
			size = source.sizeBytes
		}
	}
	internal := target.NewInternalName()
	res.track(internal)
	if err := s.m.beginIntent(opCreate, pool.Name, internal, 0); err != nil {
		return nil, false, err
	}
	s.m.crashPoint("intent")
	var path string
	if req.Source != nil {
		path, err = driver.Clone(source.internal, internal, size)
	} else {
		path, err = driver.Create(internal, size)
	}
	if err != nil {
		s.m.abandonCreate(driver, internal, req.Name)
		return nil, false, poolFull(pool.Name, err)
	}
	s.m.crashPoint("storage")
	vol := &target.Volume{
		Name:         req.Name,
		Pool:         pool.Name,
		InternalName: internal,
		DevicePath:   path,
		SizeBytes:    size,
		UUID:         target.NewUUID(),
		NGUID:        target.NewNGUID(),
		Source:       req.Source,
		Labels:       req.Labels,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.m.commit(func(st *target.State) error {
		st.Volumes[req.Name] = vol
		return nil
	}); err != nil {
		s.m.abandonCreate(driver, internal, req.Name)
		return nil, false, err
	}
	s.m.crashPoint("saved")
	s.m.endIntent(internal)
	return vol, true, nil
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

// getVolumeUsage asks the storage, outside any lock, so that a slow answer holds up nothing else.
func (s *Server) getVolumeUsage(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("volume")
	var vol target.Volume
	var pool *target.Pool
	if err := s.m.view(func(st *target.State) error {
		found := st.Volumes[name]
		if found == nil {
			return notFound("no such volume %s", name)
		}
		vol, pool = *found, st.Pools[found.Pool]
		return nil
	}); err != nil {
		return err
	}
	if pool == nil {
		return notFound("no such pool %s", vol.Pool)
	}
	driver, err := s.m.newDriver(pool)
	if err != nil {
		return err
	}
	used, err := driver.Used(vol.InternalName)
	if err != nil {
		return fmt.Errorf("volume %s: %w", name, err)
	}
	writeJSON(w, http.StatusOK, map[string]int64{"size_bytes": vol.SizeBytes, "used_bytes": used})
	return nil
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
	if err := s.changeVolume(name, req.SizeBytes, req.Labels); err != nil {
		return err
	}
	return s.getVolume(w, r)
}

// changeVolume grows the storage, if asked to, without the lock. Labels alone don't need to wait for
// anything.
func (s *Server) changeVolume(name string, sizeBytes *int64, labels map[string]string) error {
	res := s.m.reserve()
	defer res.release()
	label := func(st *target.State) (*target.Volume, error) {
		vol := st.Volumes[name]
		if vol == nil {
			return nil, notFound("no such volume %s", name)
		}
		if labels != nil {
			vol.Labels = labels
		}
		return vol, nil
	}
	var size int64
	var growing string
	if sizeBytes != nil {
		vol := s.m.current().Volumes[name]
		if vol == nil {
			return notFound("no such volume %s", name)
		}
		if *sizeBytes != vol.SizeBytes {
			if err := s.m.admit(); err != nil {
				return err
			}
			if err := res.exclusive(volumeKey(name)); err != nil {
				return err
			}
			if vol = s.m.current().Volumes[name]; vol == nil {
				return notFound("no such volume %s", name)
			}
			switch {
			case *sizeBytes < vol.SizeBytes:
				return target.Invalid("volumes can grow but not shrink (currently %d bytes)", vol.SizeBytes)
			case *sizeBytes%4096 != 0:
				return target.Invalid("size_bytes must be a multiple of 4096")
			}
			driver, err := s.m.newDriver(s.m.current().Pools[vol.Pool])
			if err != nil {
				return err
			}
			if size, err = driver.AlignSize(*sizeBytes); err != nil {
				return err
			}
			if size != vol.SizeBytes {
				if err := res.pool(vol.Pool); err != nil {
					return err
				}
				if err := s.m.beginIntent(opGrow, vol.Pool, vol.InternalName, size); err != nil {
					return err
				}
				growing = vol.InternalName
				s.m.crashPoint("intent")
				if err := driver.Grow(vol.InternalName, size); err != nil {
					s.m.endIntent(growing)
					return err
				}
				s.m.crashPoint("storage")
			}
		}
	}
	if growing == "" {
		return s.m.mutate(func(st *target.State) error {
			_, err := label(st)
			return err
		})
	}
	var mapped *mappingView
	err := s.m.commit(func(st *target.State) error {
		vol, err := label(st)
		if err != nil {
			return err
		}
		vol.SizeBytes = size
		mapped = mappings(st)[name]
		return nil
	})
	if err == nil {
		s.m.crashPoint("saved")
	}
	s.m.endIntent(growing)
	if err != nil {
		return err
	}
	if mapped != nil && s.m.backend.LiveEnabled(mapped.Subsystem, mapped.NSID) {
		return s.m.backend.Revalidate(mapped.Subsystem, mapped.NSID)
	}
	return nil
}

func (s *Server) deleteVolume(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("volume")
	if err := s.removeVolume(name); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) removeVolume(name string) error {
	res := s.m.reserve()
	defer res.release()
	if err := s.m.admit(); err != nil {
		return err
	}
	if err := res.exclusive(volumeKey(name)); err != nil {
		return err
	}
	unmapped := func(st *target.State) (*target.Volume, error) {
		vol := st.Volumes[name]
		if vol == nil {
			return nil, notFound("no such volume %s", name)
		}
		if m := mappings(st)[name]; m != nil {
			return nil, target.NewConflict("volume %s is mapped into %s as namespace %d", name, m.Subsystem, m.NSID)
		}
		return vol, nil
	}
	vol, err := unmapped(s.m.current())
	if err != nil {
		return err
	}
	return s.removeStorage(res, vol.Pool, vol.InternalName, func(st *target.State) error {
		if _, err := unmapped(st); err != nil {
			return err
		}
		delete(st.Volumes, name)
		return nil
	})
}

// removeStorage saves that something has gone, then removes its storage.
func (s *Server) removeStorage(res *reservation, pool, internal string, forget func(*target.State) error) error {
	if err := res.pool(pool); err != nil {
		return err
	}
	driver, err := s.m.newDriver(s.m.current().Pools[pool])
	if err != nil {
		return err
	}
	res.track(internal)
	if err := s.m.beginIntent(opDelete, pool, internal, 0); err != nil {
		return err
	}
	s.m.crashPoint("intent")
	if err := s.m.commit(forget); err != nil {
		s.m.endIntent(internal) // It's still in use, so its storage stays too.
		return err
	}
	s.m.crashPoint("saved")
	// Saved first: if this fails, the intent stays, and the next start tries again. So a volume never
	// loses its storage while it's still listed.
	if err := driver.Delete(internal); err != nil {
		return err
	}
	s.m.crashPoint("storage")
	s.m.endIntent(internal)
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
	snap, created, err := s.takeSnapshot(req.Name, req.Volume)
	if err != nil {
		return err
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, viewSnapshot(snap))
	return nil
}

func (s *Server) takeSnapshot(name, volume string) (*target.Snapshot, bool, error) {
	res := s.m.reserve()
	defer res.release()
	if err := res.exclusive(snapshotKey(name)); err != nil {
		return nil, false, err
	}
	// This comes first because the volume may have gone by the time CSI retries.
	if existing := s.m.current().Snapshots[name]; existing != nil {
		if existing.Volume != volume {
			return nil, false, target.NewConflict("snapshot %s already exists, of volume %s", name, existing.Volume)
		}
		return existing, false, nil
	}
	if err := s.m.admit(); err != nil {
		return nil, false, err
	}
	if s.m.current().Volumes[volume] == nil {
		return nil, false, target.Invalid("no such volume %s", volume)
	}
	if err := res.shared(volumeKey(volume)); err != nil {
		return nil, false, err
	}
	// Read after the reservation, because the volume can't change from here on.
	vol := s.m.current().Volumes[volume]
	if vol == nil {
		return nil, false, target.Invalid("no such volume %s", volume)
	}
	if err := res.pool(vol.Pool); err != nil {
		return nil, false, err
	}
	driver, err := s.m.newDriver(s.m.current().Pools[vol.Pool])
	if err != nil {
		return nil, false, err
	}
	internal := target.NewInternalName()
	res.track(internal)
	if err := s.m.beginIntent(opCreate, vol.Pool, internal, 0); err != nil {
		return nil, false, err
	}
	s.m.crashPoint("intent")
	path, err := driver.Snapshot(vol.InternalName, internal)
	if err != nil {
		s.m.abandonCreate(driver, internal, name)
		return nil, false, poolFull(vol.Pool, err)
	}
	s.m.crashPoint("storage")
	snap := &target.Snapshot{
		Name:         name,
		Volume:       vol.Name,
		Pool:         vol.Pool,
		InternalName: internal,
		DevicePath:   path,
		SizeBytes:    vol.SizeBytes,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.m.commit(func(st *target.State) error {
		st.Snapshots[name] = snap
		return nil
	}); err != nil {
		s.m.abandonCreate(driver, internal, name)
		return nil, false, err
	}
	s.m.crashPoint("saved")
	s.m.endIntent(internal)
	return snap, true, nil
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
	if err := s.removeSnapshot(name); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) removeSnapshot(name string) error {
	res := s.m.reserve()
	defer res.release()
	if err := s.m.admit(); err != nil {
		return err
	}
	if err := res.exclusive(snapshotKey(name)); err != nil {
		return err
	}
	snap := s.m.current().Snapshots[name]
	if snap == nil {
		return notFound("no such snapshot %s", name)
	}
	return s.removeStorage(res, snap.Pool, snap.InternalName, func(st *target.State) error {
		delete(st.Snapshots, name)
		return nil
	})
}
