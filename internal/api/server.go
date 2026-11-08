package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"nvweaver/internal/auth"
	"nvweaver/internal/storage"
	"nvweaver/internal/target"
	"nvweaver/internal/webui"
)

const defaultModel = "nvweaver"

// Server is the HTTP API.
type Server struct {
	// Version is reported by /v1/system.
	Version string
	m       *Manager
	token   string
	// patterns lists the routes, for comparing with the OpenAPI spec.
	patterns []string
	// Auth signs people in with a password, if there's a users file.
	Auth *auth.Authenticator
	// AllowInsecureLogin lets passwords cross the network in clear text.
	AllowInsecureLogin bool
	// anyHost skips checking the Host header with no token. Only tests set it, whose requests have made-up names.
	anyHost bool
}

// NewServer returns an API handler. A non-empty token is required as a bearer
// token on everything except /healthz.
func NewServer(m *Manager, token string) *Server {
	return &Server{m: m, token: token}
}

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func notFound(format string, a ...any) error {
	return &statusError{http.StatusNotFound, fmt.Sprintf(format, a...)}
}

func errorStatus(err error) int {
	var se *statusError
	var ve *target.ValidationError
	var ce *target.ConflictError
	var ke *target.KernelError
	switch {
	case errors.As(err, &se):
		return se.code
	case errors.As(err, &ve):
		return http.StatusUnprocessableEntity
	case errors.As(err, &ce):
		return http.StatusConflict
	case errors.As(err, &ke):
		switch {
		case errors.Is(err, syscall.EADDRINUSE), errors.Is(err, syscall.EBUSY), errors.Is(err, syscall.EEXIST):
			return http.StatusConflict
		case errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENODEV),
			errors.Is(err, syscall.EADDRNOTAVAIL), errors.Is(err, syscall.ENOTBLK):
			return http.StatusUnprocessableEntity
		}
	}
	return http.StatusInternalServerError
}

type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			code := errorStatus(err)
			if code == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", retryAfterSeconds)
			} else if code >= 500 {
				s.m.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return &statusError{http.StatusBadRequest, "invalid request body: " + err.Error()}
	}
	return nil
}

// Handler returns the routed API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h handler) {
		s.patterns = append(s.patterns, pattern)
		mux.HandleFunc(pattern, s.wrap(h))
	}

	route("GET /v1/system", s.getSystem)

	route("GET /v1/connections", s.listConnections)
	route("GET /v1/ports", s.listPorts)
	route("POST /v1/ports", s.createPort)
	route("GET /v1/ports/{id}", s.getPort)
	route("PATCH /v1/ports/{id}", s.updatePort)
	route("DELETE /v1/ports/{id}", s.deletePort)

	route("GET /v1/hosts", s.listHosts)
	route("POST /v1/hosts", s.createHost)
	route("GET /v1/hosts/{nqn}", s.getHost)
	route("PATCH /v1/hosts/{nqn}", s.updateHost)
	route("DELETE /v1/hosts/{nqn}", s.deleteHost)

	route("GET /v1/subsystems", s.listSubsystems)
	route("POST /v1/subsystems", s.createSubsystem)
	route("GET /v1/subsystems/{nqn}", s.getSubsystem)
	route("PATCH /v1/subsystems/{nqn}", s.updateSubsystem)
	route("DELETE /v1/subsystems/{nqn}", s.deleteSubsystem)

	route("PUT /v1/subsystems/{nqn}/ports/{id}", s.attachPort)
	route("DELETE /v1/subsystems/{nqn}/ports/{id}", s.detachPort)
	route("PUT /v1/subsystems/{nqn}/hosts/{host}", s.allowHost)
	route("DELETE /v1/subsystems/{nqn}/hosts/{host}", s.denyHost)

	route("GET /v1/pools", s.listPools)
	route("POST /v1/pools", s.createPool)
	route("GET /v1/pools/{pool}", s.getPool)
	route("PATCH /v1/pools/{pool}", s.updatePool)
	route("DELETE /v1/pools/{pool}", s.deletePool)

	route("GET /v1/volumes", s.listVolumes)
	route("POST /v1/volumes", s.createVolume)
	route("GET /v1/volumes/{volume}", s.getVolume)
	route("PATCH /v1/volumes/{volume}", s.updateVolume)
	route("DELETE /v1/volumes/{volume}", s.deleteVolume)

	route("GET /v1/volumes/{volume}/usage", s.getVolumeUsage)
	route("GET /v1/snapshots", s.listSnapshots)
	route("POST /v1/snapshots", s.createSnapshot)
	route("GET /v1/snapshots/{snapshot}", s.getSnapshot)
	route("DELETE /v1/snapshots/{snapshot}", s.deleteSnapshot)

	route("GET /v1/subsystems/{nqn}/namespaces", s.listNamespaces)
	route("POST /v1/subsystems/{nqn}/namespaces", s.createNamespace)
	route("GET /v1/subsystems/{nqn}/namespaces/{nsid}", s.getNamespace)
	route("PATCH /v1/subsystems/{nqn}/namespaces/{nsid}", s.updateNamespace)
	route("DELETE /v1/subsystems/{nqn}/namespaces/{nsid}", s.deleteNamespace)

	authed := s.authenticate(mux)
	root := http.NewServeMux()
	s.patterns = append(s.patterns, "GET /healthz")
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.m.backend.Available() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "target unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	for _, session := range []struct {
		pattern string
		handler handler
	}{{"GET /v1/session", s.getSession}, {"POST /v1/session", s.signIn}, {"DELETE /v1/session", s.signOut}} {
		s.patterns = append(s.patterns, session.pattern)
		root.HandleFunc(session.pattern, s.wrap(session.handler))
	}
	root.Handle(webui.Path, webui.Handler())
	root.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, webui.Path, http.StatusFound)
	})
	root.Handle("/", authed)
	return s.guardBrowsers(root)
}

// guardBrowsers stops web pages using the API through a browser on this machine. With no token, which is only allowed on loopback,
// nothing else would: any page could send a request here, and the API can export any unused disk. Programs send neither header
// that this looks at, so it doesn't affect them.
func (s *Server) guardBrowsers(next http.Handler) http.Handler {
	// A page from elsewhere can send a request without the server's permission, if it doesn't ask to read the answer.
	crossOrigin := http.NewCrossOriginProtection().Handler(next)
	// A session's cookie only goes to the name it came from, and a rebinding page doesn't have a token, so it's only with neither that the name matters.
	if s.token != "" || s.Auth != nil || s.anyHost {
		return crossOrigin
	}
	// DNS rebinding: a site can point its own name at 127.0.0.1, and to the browser, it's then the same origin, which lets it
	// read answers too. It can't change the name in the Host header, and nothing legitimate reaches loopback by another name.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "with no token, the API only answers to a loopback address"})
			return
		}
		crossOrigin.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.token == "" && s.Auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.signedIn(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token or session"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// signedIn reports whether a request may use the API, by token or session.
func (s *Server) signedIn(r *http.Request) bool {
	if s.token != "" {
		want := []byte("Bearer " + s.token)
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) == 1 {
			return true
		}
	}
	_, ok := s.sessionUser(r)
	return ok
}

func (s *Server) sessionUser(r *http.Request) (string, bool) {
	if s.Auth == nil {
		return "", false
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	return s.Auth.Check(cookie.Value)
}

func (s *Server) getSystem(w http.ResponseWriter, _ *http.Request) error {
	var uts syscall.Utsname
	_ = syscall.Uname(&uts)
	var b strings.Builder
	for _, c := range uts.Release {
		if c == 0 {
			break
		}
		b.WriteByte(byte(c))
	}
	modules := map[string]bool{}
	for _, name := range []string{"nvmet", "nvmet_tcp"} {
		_, err := os.Stat("/sys/module/" + name)
		modules[name] = err == nil
	}
	return s.m.view(func(st *target.State) error {
		writeJSON(w, http.StatusOK, map[string]any{
			"version":      s.Version,
			"backend":      s.m.backend.Name(),
			"capabilities": s.m.backend.Capabilities(),
			"pool_types":   storage.Types(),
			"pools":        len(st.Pools),
			"volumes":      len(st.Volumes),
			"snapshots":    len(st.Snapshots),
			"kernel":       b.String(),
			"modules":      modules,
			"target_ready": s.m.backend.Available(),
			"ports":        len(st.Ports),
			"hosts":        len(st.Hosts),
			"subsystems":   len(st.Subsystems),
			"warnings":     s.m.Warnings(),
		})
		return nil
	})
}

// --- ports ---

func pathPort(r *http.Request) (uint16, error) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 16)
	if err != nil {
		return 0, notFound("no such port %q", r.PathValue("id"))
	}
	return uint16(id), nil
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) error {
	nqn := r.URL.Query().Get("subsystem")
	return s.m.view(func(st *target.State) error {
		if nqn != "" {
			if _, ok := st.Subsystems[nqn]; !ok {
				return notFound("no subsystem %s", nqn)
			}
		}
		out, err := s.m.backend.Connections(st, nqn)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	})
}

func (s *Server) listPorts(w http.ResponseWriter, _ *http.Request) error {
	return s.m.view(func(st *target.State) error {
		out := []*target.Port{}
		for _, id := range slices.Sorted(maps.Keys(st.Ports)) {
			out = append(out, st.Ports[id])
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	})
}

func (s *Server) createPort(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		ID             *uint16 `json:"id"`
		Transport      string  `json:"transport"`
		Address        string  `json:"address"`
		Service        int     `json:"service"`
		InlineDataSize *int    `json:"inline_data_size"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if req.Service == 0 {
		req.Service = 4420 // IANA-assigned NVMe/TCP I/O port.
	}
	if req.Transport == "" {
		req.Transport = target.TransportTCP
	}
	var created *target.Port
	err := s.m.mutate(func(st *target.State) error {
		var id uint16
		if req.ID != nil {
			id = *req.ID
			if _, dup := st.Ports[id]; dup {
				return target.NewConflict("port %d already exists", id)
			}
		} else {
			id = 1
			for st.Ports[id] != nil {
				id++
			}
		}
		created = &target.Port{ID: id, Transport: req.Transport, Address: req.Address, Service: req.Service, InlineDataSize: req.InlineDataSize}
		st.Ports[id] = created
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, created)
	return nil
}

func (s *Server) getPort(w http.ResponseWriter, r *http.Request) error {
	id, err := pathPort(r)
	if err != nil {
		return err
	}
	return s.m.view(func(st *target.State) error {
		p := st.Ports[id]
		if p == nil {
			return notFound("no such port %d", id)
		}
		writeJSON(w, http.StatusOK, p)
		return nil
	})
}

func (s *Server) updatePort(w http.ResponseWriter, r *http.Request) error {
	id, err := pathPort(r)
	if err != nil {
		return err
	}
	var req struct {
		Transport      *string `json:"transport"`
		Address        *string `json:"address"`
		Service        *int    `json:"service"`
		InlineDataSize *int    `json:"inline_data_size"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	var updated *target.Port
	err = s.m.mutate(func(st *target.State) error {
		p := st.Ports[id]
		if p == nil {
			return notFound("no such port %d", id)
		}
		if req.Transport != nil {
			p.Transport = *req.Transport
		}
		if req.Address != nil {
			p.Address = *req.Address
		}
		if req.Service != nil {
			p.Service = *req.Service
		}
		if req.InlineDataSize != nil {
			p.InlineDataSize = req.InlineDataSize
		}
		updated = p
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, updated)
	return nil
}

func (s *Server) deletePort(w http.ResponseWriter, r *http.Request) error {
	id, err := pathPort(r)
	if err != nil {
		return err
	}
	err = s.m.mutate(func(st *target.State) error {
		if st.Ports[id] == nil {
			return notFound("no such port %d", id)
		}
		for _, sub := range st.Subsystems {
			if slices.Contains(sub.Ports, id) {
				return target.NewConflict("port %d is attached to subsystem %s", id, sub.NQN)
			}
		}
		delete(st.Ports, id)
		return nil
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- hosts ---

// hostView never echoes secrets back.
type hostView struct {
	NQN           string `json:"nqn"`
	HasKey        bool   `json:"has_dhchap_key"`
	HasCtrlKey    bool   `json:"has_dhchap_ctrl_key"`
	DHCHAPHash    string `json:"dhchap_hash,omitempty"`
	DHCHAPDHGroup string `json:"dhchap_dhgroup,omitempty"`
}

func viewHost(h *target.Host) hostView {
	return hostView{h.NQN, h.DHCHAPKey != "", h.DHCHAPCtrlKey != "", h.DHCHAPHash, h.DHCHAPDHGroup}
}

func (s *Server) listHosts(w http.ResponseWriter, _ *http.Request) error {
	return s.m.view(func(st *target.State) error {
		out := []hostView{}
		for _, nqn := range slices.Sorted(maps.Keys(st.Hosts)) {
			out = append(out, viewHost(st.Hosts[nqn]))
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	})
}

func (s *Server) createHost(w http.ResponseWriter, r *http.Request) error {
	var req target.Host
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := target.ValidateNQN(req.NQN); err != nil {
		return err
	}
	err := s.m.mutate(func(st *target.State) error {
		if st.Hosts[req.NQN] != nil {
			return target.NewConflict("host %s already exists", req.NQN)
		}
		st.Hosts[req.NQN] = &req
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, viewHost(&req))
	return nil
}

func (s *Server) getHost(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	return s.m.view(func(st *target.State) error {
		h := st.Hosts[nqn]
		if h == nil {
			return notFound("no such host %s", nqn)
		}
		writeJSON(w, http.StatusOK, viewHost(h))
		return nil
	})
}

func (s *Server) updateHost(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	var req struct {
		DHCHAPKey     *string `json:"dhchap_key"`
		DHCHAPCtrlKey *string `json:"dhchap_ctrl_key"`
		DHCHAPHash    *string `json:"dhchap_hash"`
		DHCHAPDHGroup *string `json:"dhchap_dhgroup"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	var view hostView
	err := s.m.mutate(func(st *target.State) error {
		h := st.Hosts[nqn]
		if h == nil {
			return notFound("no such host %s", nqn)
		}
		for _, f := range []struct {
			src *string
			dst *string
		}{
			{req.DHCHAPKey, &h.DHCHAPKey},
			{req.DHCHAPCtrlKey, &h.DHCHAPCtrlKey},
			{req.DHCHAPHash, &h.DHCHAPHash},
			{req.DHCHAPDHGroup, &h.DHCHAPDHGroup},
		} {
			if f.src == nil {
				continue
			}
			if *f.src == "" {
				return target.Invalid("credentials cannot be cleared; delete and recreate the host")
			}
			*f.dst = *f.src
		}
		view = viewHost(h)
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	err := s.m.mutate(func(st *target.State) error {
		if st.Hosts[nqn] == nil {
			return notFound("no such host %s", nqn)
		}
		for _, sub := range st.Subsystems {
			if slices.Contains(sub.AllowedHosts, nqn) {
				return target.NewConflict("host is allowed on subsystem %s", sub.NQN)
			}
		}
		delete(st.Hosts, nqn)
		return nil
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- subsystems ---

type namespaceView struct {
	*target.Namespace
	LiveEnabled bool   `json:"live_enabled"`
	SizeBytes   *int64 `json:"size_bytes,omitempty"`
}

type subsystemView struct {
	*target.Subsystem
	Namespaces []namespaceView `json:"namespaces"`
}

func (s *Server) viewNamespace(nqn string, n *target.Namespace) namespaceView {
	return viewNamespaceWith(s.m.backend.LiveEnabled, nqn, n)
}

func viewNamespaceWith(live target.LiveChecker, nqn string, n *target.Namespace) namespaceView {
	v := namespaceView{Namespace: n, LiveEnabled: live(nqn, n.NSID)}
	if size, ok := backingSize(n.DevicePath); ok {
		v.SizeBytes = &size
	}
	return v
}

func (s *Server) viewSubsystem(sub *target.Subsystem) subsystemView {
	return viewSubsystemWith(s.m.backend.LiveEnabled, sub)
}

func viewSubsystemWith(live target.LiveChecker, sub *target.Subsystem) subsystemView {
	v := subsystemView{Subsystem: sub, Namespaces: []namespaceView{}}
	for _, nsid := range slices.Sorted(maps.Keys(sub.Namespaces)) {
		v.Namespaces = append(v.Namespaces, viewNamespaceWith(live, sub.NQN, sub.Namespaces[nsid]))
	}
	return v
}

func (s *Server) listSubsystems(w http.ResponseWriter, _ *http.Request) error {
	return s.m.view(func(st *target.State) error {
		out := []subsystemView{}
		live := s.m.backend.LiveEnabledAll()
		for _, nqn := range slices.Sorted(maps.Keys(st.Subsystems)) {
			out = append(out, viewSubsystemWith(live, st.Subsystems[nqn]))
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	})
}

func (s *Server) createSubsystem(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		NQN            string   `json:"nqn"`
		Serial         string   `json:"serial"`
		Model          string   `json:"model"`
		AllowAnyHost   bool     `json:"allow_any_host"`
		AllowedHosts   []string `json:"allowed_hosts"`
		Ports          []uint16 `json:"ports"`
		PassthruDevice string   `json:"passthru_device"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if req.Serial == "" {
		req.Serial = target.NewSerial()
	}
	if req.Model == "" {
		req.Model = defaultModel
	}
	if req.PassthruDevice != "" {
		if err := checkPassthru(req.PassthruDevice); err != nil {
			return err
		}
	}
	sub := &target.Subsystem{
		NQN:          req.NQN,
		Serial:       req.Serial,
		Model:        req.Model,
		AllowAnyHost: req.AllowAnyHost,
		AllowedHosts: append([]string{}, req.AllowedHosts...),
		Ports:        append([]uint16{}, req.Ports...),
		Namespaces:   map[uint32]*target.Namespace{},
		Passthru:     req.PassthruDevice,
	}
	err := s.m.mutate(func(st *target.State) error {
		if st.Subsystems[req.NQN] != nil {
			return target.NewConflict("subsystem %s already exists", req.NQN)
		}
		if sub.Passthru != "" {
			if err := checkPassthruFree(st, sub.Passthru); err != nil {
				return err
			}
		}
		st.Subsystems[req.NQN] = sub
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, s.viewSubsystem(sub))
	return nil
}

func (s *Server) getSubsystem(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	return s.m.view(func(st *target.State) error {
		sub := st.Subsystems[nqn]
		if sub == nil {
			return notFound("no such subsystem %s", nqn)
		}
		writeJSON(w, http.StatusOK, s.viewSubsystem(sub))
		return nil
	})
}

// updateSubsystem changes access policy only: the kernel freezes serial and
// model once a host has identified the controller.
func (s *Server) updateSubsystem(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	var req struct {
		AllowAnyHost *bool `json:"allow_any_host"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	var updated *target.Subsystem
	err := s.m.mutate(func(st *target.State) error {
		sub := st.Subsystems[nqn]
		if sub == nil {
			return notFound("no such subsystem %s", nqn)
		}
		if req.AllowAnyHost != nil {
			sub.AllowAnyHost = *req.AllowAnyHost
		}
		updated = sub
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.viewSubsystem(updated))
	return nil
}

func (s *Server) deleteSubsystem(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	err := s.m.mutate(func(st *target.State) error {
		if st.Subsystems[nqn] == nil {
			return notFound("no such subsystem %s", nqn)
		}
		delete(st.Subsystems, nqn)
		return nil
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- subsystem links ---

// link adds or removes value in the list that pick selects. PUT and DELETE are
// both idempotent, which suits automation that retries.
func link[T comparable](s *Server, w http.ResponseWriter, nqn string, value T, add bool, pick func(*target.Subsystem) *[]T) error {
	err := s.m.mutate(func(st *target.State) error {
		sub := st.Subsystems[nqn]
		if sub == nil {
			return notFound("no such subsystem %s", nqn)
		}
		list := pick(sub)
		i := slices.Index(*list, value)
		switch {
		case add && i < 0:
			*list = append(*list, value)
		case !add && i >= 0:
			*list = slices.Delete(*list, i, i+1)
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func subsysPorts(sub *target.Subsystem) *[]uint16 { return &sub.Ports }
func subsysHosts(sub *target.Subsystem) *[]string { return &sub.AllowedHosts }

func (s *Server) attachPort(w http.ResponseWriter, r *http.Request) error {
	id, err := pathPort(r)
	if err != nil {
		return err
	}
	return link(s, w, r.PathValue("nqn"), id, true, subsysPorts)
}

func (s *Server) detachPort(w http.ResponseWriter, r *http.Request) error {
	id, err := pathPort(r)
	if err != nil {
		return err
	}
	return link(s, w, r.PathValue("nqn"), id, false, subsysPorts)
}

func (s *Server) allowHost(w http.ResponseWriter, r *http.Request) error {
	return link(s, w, r.PathValue("nqn"), r.PathValue("host"), true, subsysHosts)
}

func (s *Server) denyHost(w http.ResponseWriter, r *http.Request) error {
	return link(s, w, r.PathValue("nqn"), r.PathValue("host"), false, subsysHosts)
}

// --- namespaces ---

func pathNSID(r *http.Request) (uint32, error) {
	nsid, err := strconv.ParseUint(r.PathValue("nsid"), 10, 32)
	if err != nil {
		return 0, notFound("no such namespace %q", r.PathValue("nsid"))
	}
	return uint32(nsid), nil
}

func (s *Server) listNamespaces(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	return s.m.view(func(st *target.State) error {
		sub := st.Subsystems[nqn]
		if sub == nil {
			return notFound("no such subsystem %s", nqn)
		}
		writeJSON(w, http.StatusOK, s.viewSubsystem(sub).Namespaces)
		return nil
	})
}

func (s *Server) createNamespace(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	var req struct {
		NSID       uint32 `json:"nsid"`
		DevicePath string `json:"device_path"`
		Volume     string `json:"volume"`
		File       *struct {
			Name      string `json:"name"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"file"`
		UUID       string `json:"uuid"`
		NGUID      string `json:"nguid"`
		BufferedIO bool   `json:"buffered_io"`
		Enabled    *bool  `json:"enabled"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	backings := 0
	for _, given := range []bool{req.DevicePath != "", req.File != nil, req.Volume != ""} {
		if given {
			backings++
		}
	}
	if backings != 1 {
		return target.Invalid("specify exactly one of device_path, file or volume")
	}
	if req.Volume != "" && (req.UUID != "" || req.NGUID != "") {
		return target.Invalid("a volume brings its own uuid and nguid, so they can't be given here")
	}
	if req.NSID > target.MaxNSID {
		return target.Invalid("nsid must be between 1 and %d", target.MaxNSID)
	}
	if req.UUID == "" {
		req.UUID = target.NewUUID()
	}
	if req.NGUID == "" {
		req.NGUID = target.NewNGUID()
	}
	ns := &target.Namespace{
		NSID:       req.NSID,
		DevicePath: req.DevicePath,
		UUID:       req.UUID,
		NGUID:      req.NGUID,
		BufferedIO: req.BufferedIO,
		Enabled:    req.Enabled == nil || *req.Enabled,
	}
	var intent string // a managed file's, once recorded
	err := s.m.mutate(func(st *target.State) error {
		sub := st.Subsystems[nqn]
		if sub == nil {
			return notFound("no such subsystem %s", nqn)
		}
		if sub.Passthru != "" {
			return target.NewConflict("subsystem %s is a passthrough subsystem", nqn)
		}
		if ns.NSID == 0 {
			ns.NSID = 1
			for sub.Namespaces[ns.NSID] != nil {
				ns.NSID++
			}
			if ns.NSID > target.MaxNSID {
				return target.NewConflict("subsystem %s has no free namespace IDs", nqn)
			}
		} else if sub.Namespaces[ns.NSID] != nil {
			return target.NewConflict("namespace %d already exists", ns.NSID)
		}
		if req.Volume != "" {
			vol := st.Volumes[req.Volume]
			if vol == nil {
				return target.Invalid("no such volume %s", req.Volume)
			}
			// Its file, reached by another name, may already be exported.
			if err := checkUnshared(st, vol.DevicePath, "", 0, vol.Name); err != nil {
				return err
			}
			ns.Volume, ns.DevicePath, ns.UUID, ns.NGUID = vol.Name, vol.DevicePath, vol.UUID, vol.NGUID
		} else if req.File != nil {
			// Refused before the intent, which would otherwise make recovery remove a file that was there already.
			if err := s.m.checkNewFile(req.File.Name); err != nil {
				return err
			}
			if err := s.m.beginFileIntent(opFileCreate, req.File.Name); err != nil {
				return err
			}
			intent = fileKey(req.File.Name)
			path, err := s.m.createVolume(req.File.Name, req.File.SizeBytes)
			if err != nil {
				return err
			}
			ns.DevicePath, ns.ManagedFile = path, true
			s.m.crashPoint("file created")
		} else {
			if err := checkBacking(ns.DevicePath); err != nil {
				return err
			}
			if err := checkUnshared(st, ns.DevicePath, "", 0, ""); err != nil {
				return err
			}
		}
		sub.Namespaces[ns.NSID] = ns
		return nil
	})
	if err != nil && ns.ManagedFile {
		os.Remove(ns.DevicePath)
	}
	if intent != "" {
		s.m.endIntent(intent)
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, s.viewNamespaceLocked(nqn, ns))
	return nil
}

func (s *Server) viewNamespaceLocked(nqn string, ns *target.Namespace) namespaceView {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	return s.viewNamespace(nqn, ns)
}

func (s *Server) getNamespace(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	nsid, err := pathNSID(r)
	if err != nil {
		return err
	}
	return s.m.view(func(st *target.State) error {
		sub := st.Subsystems[nqn]
		if sub == nil || sub.Namespaces[nsid] == nil {
			return notFound("no such namespace %d on %s", nsid, nqn)
		}
		writeJSON(w, http.StatusOK, s.viewNamespace(nqn, sub.Namespaces[nsid]))
		return nil
	})
}

func (s *Server) updateNamespace(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	nsid, err := pathNSID(r)
	if err != nil {
		return err
	}
	var req struct {
		Enabled    *bool   `json:"enabled"`
		DevicePath *string `json:"device_path"`
		BufferedIO *bool   `json:"buffered_io"`
		SizeBytes  *int64  `json:"size_bytes"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	var updated *target.Namespace
	err = s.m.mutate(func(st *target.State) error {
		sub := st.Subsystems[nqn]
		if sub == nil || sub.Namespaces[nsid] == nil {
			return notFound("no such namespace %d on %s", nsid, nqn)
		}
		ns := sub.Namespaces[nsid]
		if ns.Volume != "" && (req.DevicePath != nil || req.SizeBytes != nil) {
			return target.Invalid("namespace %d exports volume %s: change the volume, or map another", nsid, ns.Volume)
		}
		if req.Enabled != nil {
			ns.Enabled = *req.Enabled
		}
		if req.BufferedIO != nil {
			ns.BufferedIO = *req.BufferedIO
		}
		if req.DevicePath != nil && *req.DevicePath != ns.DevicePath {
			if ns.ManagedFile {
				return target.NewConflict("namespace %d's file was created by this service, and would be left behind: delete the namespace, with delete_file=true to remove its file, and create another", nsid)
			}
			if err := checkBacking(*req.DevicePath); err != nil {
				return err
			}
			if err := checkUnshared(st, *req.DevicePath, nqn, nsid, ""); err != nil {
				return err
			}
			ns.DevicePath, ns.ManagedFile = *req.DevicePath, false
		}
		if req.SizeBytes != nil {
			if !ns.ManagedFile {
				return target.Invalid("size_bytes applies only to file-backed volumes created by this service")
			}
			current, ok := backingSize(ns.DevicePath)
			switch {
			case !ok:
				return fmt.Errorf("cannot read size of %s", ns.DevicePath)
			case *req.SizeBytes < current:
				return target.Invalid("volumes can grow but not shrink (currently %d bytes)", current)
			case *req.SizeBytes%4096 != 0:
				return target.Invalid("size_bytes must be a multiple of 4096")
			}
			if err := os.Truncate(ns.DevicePath, *req.SizeBytes); err != nil {
				return err
			}
		}
		updated = ns
		return nil
	})
	if err != nil {
		return err
	}
	if req.SizeBytes != nil && s.m.backend.LiveEnabled(nqn, nsid) {
		if err := s.m.backend.Revalidate(nqn, nsid); err != nil {
			return err
		}
	}
	writeJSON(w, http.StatusOK, s.viewNamespaceLocked(nqn, updated))
	return nil
}

func (s *Server) deleteNamespace(w http.ResponseWriter, r *http.Request) error {
	nqn := r.PathValue("nqn")
	nsid, err := pathNSID(r)
	if err != nil {
		return err
	}
	removeFile := r.URL.Query().Get("delete_file") == "true"
	var path string
	var managed, began bool
	err = s.m.mutate(func(st *target.State) error {
		sub := st.Subsystems[nqn]
		if sub == nil || sub.Namespaces[nsid] == nil {
			return notFound("no such namespace %d on %s", nsid, nqn)
		}
		path, managed = sub.Namespaces[nsid].DevicePath, sub.Namespaces[nsid].ManagedFile
		if removeFile && !managed {
			return target.Invalid("delete_file applies only to file-backed volumes created by this service")
		}
		if removeFile {
			if err := s.m.beginFileIntent(opFileDelete, filepath.Base(path)); err != nil {
				return err
			}
			began = true
		}
		delete(sub.Namespaces, nsid)
		return nil
	})
	if began {
		key := fileKey(filepath.Base(path))
		if err != nil {
			s.m.endIntent(key) // The namespace is still there, so its file stays.
			return err
		}
		s.m.crashPoint("file forgotten")
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("namespace %d was deleted, and removing its file failed, which the next start tries again: %w", nsid, err)
		}
		s.m.endIntent(key)
	}
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
