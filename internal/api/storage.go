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
	State    string        `json:"state"`
	Warnings []string      `json:"warnings"`
	Capacity *capacityView `json:"capacity,omitempty"`
	Volumes  int           `json:"volumes"`
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

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	limit := defaultPageSize
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			return target.Invalid("limit must be between 1 and %d", maxPageSize)
		}
		limit = n
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

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name      string            `json:"name"`
		Pool      string            `json:"pool"`
		SizeBytes int64             `json:"size_bytes"`
		Labels    map[string]string `json:"labels"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := checkObjectName("volume", req.Name); err != nil {
		return err
	}
	if req.SizeBytes < 4096 || req.SizeBytes%4096 != 0 {
		return target.Invalid("size_bytes must be a positive multiple of 4096")
	}
	var vol *target.Volume
	var driver storage.Driver
	created := false
	err := s.m.mutate(func(st *target.State) error {
		pool, err := defaultPool(st, req.Pool)
		if err != nil {
			return err
		}
		// CSI retries, so the same request again is a success, not a conflict.
		if existing := st.Volumes[req.Name]; existing != nil {
			if existing.Pool != pool.Name || existing.SizeBytes != req.SizeBytes {
				return target.NewConflict("volume %s already exists with different settings", req.Name)
			}
			vol = existing
			return nil
		}
		if driver, err = s.m.newDriver(pool); err != nil {
			return err
		}
		internal := target.NewInternalName()
		path, err := driver.Create(internal, req.SizeBytes)
		if err != nil {
			if errors.Is(err, syscall.ENOSPC) {
				return &statusError{http.StatusInsufficientStorage, fmt.Sprintf("pool %s is full: %v", pool.Name, err)}
			}
			return err
		}
		vol = &target.Volume{
			Name:         req.Name,
			Pool:         pool.Name,
			InternalName: internal,
			DevicePath:   path,
			SizeBytes:    req.SizeBytes,
			UUID:         target.NewUUID(),
			NGUID:        target.NewNGUID(),
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
