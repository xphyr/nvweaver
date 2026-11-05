package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"nvweaver/internal/storage"
	"nvweaver/internal/target"
	"nvweaver/internal/target/targettest"
)

const (
	subNQN  = "nqn.2026-11.net.example:sub1"
	hostNQN = "nqn.2026-11.net.example:host1"
)

type env struct {
	t    *testing.T
	fs   *targettest.FS
	dir  string
	http http.Handler
	// pools fakes the storage on disk, surviving a restart of the service.
	pools *fakePools
	m     *Manager
}

// fakePools is pool storage without needing a filesystem that has reflink.
type fakePools struct {
	dir       string
	files     map[string]int64
	checkErr  error
	createErr error
	deleteErr error
	growErr   error
	capErr    error
	alignTo   int64
}

func (f *fakePools) driver(p *target.Pool) (storage.Driver, error) {
	if p.Type != storage.TypeReflink {
		return nil, target.Invalid("pool type %q isn't available", p.Type)
	}
	return fakeDriver{f}, nil
}

type fakeDriver struct{ *fakePools }

func (d fakeDriver) Check() error { return d.checkErr }
func (d fakeDriver) AlignSize(size int64) (int64, error) {
	if d.alignTo == 0 {
		return size, nil
	}
	if rest := size % d.alignTo; rest != 0 {
		size += d.alignTo - rest
	}
	return size, nil
}
func (d fakeDriver) Activate() error { return nil }
func (d fakeDriver) Capacity() (storage.Capacity, error) {
	return storage.Capacity{TotalBytes: 1000, UsedBytes: 900}, d.capErr
}
func (d fakeDriver) Create(internal string, size int64) (string, error) {
	if d.createErr != nil {
		return "", d.createErr
	}
	if _, dup := d.files[internal]; dup {
		return "", fmt.Errorf("exists")
	}
	path := filepath.Join(d.dir, internal)
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		return "", err
	}
	d.files[internal] = size
	return path, nil
}
func (d fakeDriver) Delete(internal string) error {
	if d.deleteErr != nil {
		return d.deleteErr
	}
	delete(d.files, internal)
	if err := os.Remove(filepath.Join(d.dir, internal)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
func (d fakeDriver) Grow(internal string, size int64) error {
	if d.growErr != nil {
		return d.growErr
	}
	d.files[internal] = size
	return nil
}
func (d fakeDriver) copy(source, internal string, size int64) (string, error) {
	if d.createErr != nil {
		return "", d.createErr
	}
	if _, ok := d.files[source]; !ok {
		return "", fmt.Errorf("no such source %s", source)
	}
	if size == 0 {
		size = d.files[source]
	}
	return fakeDriver{d.fakePools}.Create(internal, size)
}
func (d fakeDriver) Snapshot(source, internal string) (string, error) {
	return d.copy(source, internal, 0)
}
func (d fakeDriver) Clone(source, internal string, size int64) (string, error) {
	return d.copy(source, internal, size)
}
func (d fakeDriver) List() ([]string, error) {
	var names []string
	for n := range d.files {
		names = append(names, n)
	}
	return names, nil
}

// newFakeDriver is for tests that only need pools to work.
func newFakeDriver(dir string) func(*target.Pool) (storage.Driver, error) {
	return (&fakePools{dir: dir, files: map[string]int64{}}).driver
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, fs: targettest.New(), dir: t.TempDir()}
	e.start()
	return e
}

func (e *env) start() {
	e.t.Helper()
	m, err := NewManager(&target.Target{FS: e.fs, Root: "/nvmet"}, e.dir, slog.New(slog.DiscardHandler))
	if err != nil {
		e.t.Fatal(err)
	}
	if e.pools == nil {
		e.pools = &fakePools{dir: e.t.TempDir(), files: map[string]int64{}}
	}
	m.newDriver = e.pools.driver
	m.Recover()
	e.m = m
	e.http = NewServer(m, "").Handler()
}

// do performs a request and fails the test unless it gets the wanted status.
func (e *env) do(method, path string, body any, want int) map[string]any {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	rec := httptest.NewRecorder()
	e.http.ServeHTTP(rec, httptest.NewRequest(method, path, r))
	if rec.Code != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body)
	}
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

type obj = map[string]any

func (e *env) file(name string) string {
	e.t.Helper()
	p := filepath.Join(e.t.TempDir(), name)
	if err := os.WriteFile(p, make([]byte, 4096), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// fullConfig creates a port, a host, a subsystem and a namespace.
func (e *env) fullConfig() {
	e.t.Helper()
	e.do("POST", "/v1/ports", obj{"id": 1, "address": "127.0.0.1"}, 201)
	e.do("POST", "/v1/hosts", obj{"nqn": hostNQN}, 201)
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN, "allowed_hosts": []string{hostNQN}, "ports": []int{1}}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"nsid": 1, "device_path": e.file("a.img")}, 201)
}

func TestCreateAppliesToKernel(t *testing.T) {
	e := newEnv(t)
	e.fullConfig()
	for attr, want := range map[string]string{
		"/nvmet/ports/1/addr_trtype":                           "tcp",
		"/nvmet/ports/1/addr_adrfam":                           "ipv4",
		"/nvmet/ports/1/addr_trsvcid":                          "4420",
		"/nvmet/subsystems/" + subNQN + "/attr_allow_any_host": "0",
		"/nvmet/subsystems/" + subNQN + "/namespaces/1/enable": "1",
	} {
		if got, _ := e.fs.Attr(attr); got != want {
			t.Errorf("%s = %q, want %q", attr, got, want)
		}
	}
	paths := strings.Join(e.fs.Paths(), "\n")
	for _, link := range []string{
		"/nvmet/ports/1/subsystems/" + subNQN,
		"/nvmet/subsystems/" + subNQN + "/allowed_hosts/" + hostNQN,
	} {
		if !strings.Contains(paths, link) {
			t.Errorf("missing %s in:\n%s", link, paths)
		}
	}
}

func TestDeleteOrderingSatisfiesKernel(t *testing.T) {
	e := newEnv(t)
	e.fullConfig()
	e.do("DELETE", "/v1/ports/1", nil, 409)
	e.do("DELETE", "/v1/hosts/"+hostNQN, nil, 409)
	e.do("DELETE", "/v1/subsystems/"+subNQN, nil, 204)
	e.do("DELETE", "/v1/ports/1", nil, 204)
	e.do("DELETE", "/v1/hosts/"+hostNQN, nil, 204)
	if p := e.fs.Paths(); len(p) != 0 {
		t.Errorf("left behind: %v", p)
	}
}

func TestRestartDoesNotTouchRunningTarget(t *testing.T) {
	e := newEnv(t)
	e.fullConfig()
	before := e.fs.Writes
	e.start()
	if e.fs.Writes != before {
		t.Errorf("restart made %d writes to a target already in the desired state", e.fs.Writes-before)
	}
	e.do("GET", "/v1/subsystems/"+subNQN, nil, 200)
}

func TestRestartRestoresEmptyKernel(t *testing.T) {
	e := newEnv(t)
	e.fullConfig()
	e.fs = targettest.New() // as after a host reboot
	e.start()
	if got, _ := e.fs.Attr("/nvmet/subsystems/" + subNQN + "/namespaces/1/enable"); got != "1" {
		t.Errorf("namespace not restored, enable = %q", got)
	}
}

func TestIdentifiersAreStableAcrossRestart(t *testing.T) {
	e := newEnv(t)
	e.fullConfig()
	first := e.do("GET", "/v1/subsystems/"+subNQN, nil, 200)
	e.fs = targettest.New()
	e.start()
	second := e.do("GET", "/v1/subsystems/"+subNQN, nil, 200)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if !bytes.Equal(a, b) {
		t.Errorf("identity changed:\n%s\n%s", a, b)
	}
}

func TestFailedChangeRollsBack(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/v1/ports", obj{"id": 1, "address": "127.0.0.1"}, 201)
	e.fs.Fail["/ports/1/subsystems/"+subNQN] = syscall.EADDRINUSE
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN, "ports": []int{1}}, 409)
	delete(e.fs.Fail, "/ports/1/subsystems/"+subNQN)

	e.do("GET", "/v1/subsystems/"+subNQN, nil, 404)
	for _, p := range e.fs.Paths() {
		if strings.Contains(p, subNQN) {
			t.Errorf("rollback left %s", p)
		}
	}
	e.start()
	e.do("GET", "/v1/subsystems/"+subNQN, nil, 404)
}

func TestPortChangeRelinksSubsystems(t *testing.T) {
	e := newEnv(t)
	e.fullConfig()
	e.do("PATCH", "/v1/ports/1", obj{"service": 4421}, 200)
	if got, _ := e.fs.Attr("/nvmet/ports/1/addr_trsvcid"); got != "4421" {
		t.Errorf("trsvcid = %q", got)
	}
	if !strings.Contains(strings.Join(e.fs.Paths(), "\n"), "/nvmet/ports/1/subsystems/"+subNQN) {
		t.Error("subsystem not relinked after port change")
	}
}

func TestNamespaceChangeCyclesEnable(t *testing.T) {
	e := newEnv(t)
	e.fullConfig()
	path := e.file("b.img")
	e.do("PATCH", "/v1/subsystems/"+subNQN+"/namespaces/1", obj{"device_path": path}, 200)
	prefix := "/nvmet/subsystems/" + subNQN + "/namespaces/1/"
	if got, _ := e.fs.Attr(prefix + "device_path"); got != path {
		t.Errorf("device_path = %q", got)
	}
	if got, _ := e.fs.Attr(prefix + "enable"); got != "1" {
		t.Errorf("enable = %q", got)
	}
	e.do("PATCH", "/v1/subsystems/"+subNQN+"/namespaces/1", obj{"enabled": false}, 200)
	if got, _ := e.fs.Attr(prefix + "enable"); got != "0" {
		t.Errorf("enable after disable = %q", got)
	}
}

func TestAccessModeSwitch(t *testing.T) {
	e := newEnv(t)
	e.fullConfig()
	e.do("PATCH", "/v1/subsystems/"+subNQN, obj{"allow_any_host": true}, 422)
	e.do("DELETE", "/v1/subsystems/"+subNQN+"/hosts/"+hostNQN, nil, 204)
	e.do("PATCH", "/v1/subsystems/"+subNQN, obj{"allow_any_host": true}, 200)
	e.do("PUT", "/v1/subsystems/"+subNQN+"/hosts/"+hostNQN, nil, 422)
}

func TestLinksAreIdempotent(t *testing.T) {
	e := newEnv(t)
	e.fullConfig()
	e.do("PUT", "/v1/subsystems/"+subNQN+"/ports/1", nil, 204)
	e.do("DELETE", "/v1/subsystems/"+subNQN+"/ports/1", nil, 204)
	e.do("DELETE", "/v1/subsystems/"+subNQN+"/ports/1", nil, 204)
	e.do("PUT", "/v1/subsystems/"+subNQN+"/ports/9", nil, 422)
}

func TestRejections(t *testing.T) {
	e := newEnv(t)
	shared := e.file("shared.img")
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"device_path": shared}, 201)
	cases := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"traversal NQN", "POST", "/v1/subsystems", obj{"nqn": "nqn.2026-11.x:../../etc"}, 422},
		{"slash NQN", "POST", "/v1/hosts", obj{"nqn": "nqn.2026-11.x:a/b"}, 422},
		{"discovery NQN", "POST", "/v1/subsystems", obj{"nqn": "nqn.2014-08.org.nvmexpress.discovery"}, 422},
		{"duplicate subsystem", "POST", "/v1/subsystems", obj{"nqn": subNQN}, 409},
		{"unknown host", "POST", "/v1/subsystems", obj{"nqn": subNQN + "2", "allowed_hosts": []string{hostNQN}}, 422},
		{"hostname address", "POST", "/v1/ports", obj{"address": "localhost"}, 422},
		{"unknown field", "POST", "/v1/ports", obj{"address": "127.0.0.1", "bogus": 1}, 400},
		{"shared backing", "POST", "/v1/subsystems/" + subNQN + "/namespaces", obj{"device_path": shared}, 409},
		{"no backing", "POST", "/v1/subsystems/" + subNQN + "/namespaces", obj{}, 422},
		{"both backings", "POST", "/v1/subsystems/" + subNQN + "/namespaces", obj{"device_path": shared, "file": obj{"name": "x", "size_bytes": 4096}}, 422},
		{"relative path", "POST", "/v1/subsystems/" + subNQN + "/namespaces", obj{"device_path": "a.img"}, 422},
		{"directory", "POST", "/v1/subsystems/" + subNQN + "/namespaces", obj{"device_path": "/tmp"}, 422},
		{"nsid too big", "POST", "/v1/subsystems/" + subNQN + "/namespaces", obj{"nsid": 1025, "device_path": e.file("z")}, 422},
		{"volume traversal", "POST", "/v1/subsystems/" + subNQN + "/namespaces", obj{"file": obj{"name": "../x", "size_bytes": 4096}}, 422},
		{"not a controller", "POST", "/v1/subsystems", obj{"nqn": subNQN + "3", "passthru_device": "/dev/sda"}, 422},
		{"missing port", "GET", "/v1/ports/7", nil, 404},
		{"bad port id", "GET", "/v1/ports/abc", nil, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e.t = t
			e.do(c.method, c.path, c.body, c.want)
		})
	}
}

func TestManagedVolumeLifecycle(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	ns := subNQN + "/namespaces"
	got := e.do("POST", "/v1/subsystems/"+ns, obj{"file": obj{"name": "vol", "size_bytes": 8192}}, 201)
	if got["size_bytes"] != float64(8192) {
		t.Errorf("size_bytes = %v", got["size_bytes"])
	}
	e.do("POST", "/v1/subsystems/"+ns, obj{"file": obj{"name": "vol", "size_bytes": 8192}}, 409)
	if _, err := os.Stat(filepath.Join(e.dir, "volumes", "vol")); err != nil {
		t.Fatalf("a rejected duplicate must not remove the existing volume: %v", err)
	}
	e.do("PATCH", "/v1/subsystems/"+ns+"/1", obj{"size_bytes": 4096}, 422)
	got = e.do("PATCH", "/v1/subsystems/"+ns+"/1", obj{"size_bytes": 16384}, 200)
	if got["size_bytes"] != float64(16384) {
		t.Errorf("size_bytes after grow = %v", got["size_bytes"])
	}
	e.do("DELETE", "/v1/subsystems/"+ns+"/1?delete_file=true", nil, 204)
	if _, err := os.Stat(filepath.Join(e.dir, "volumes", "vol")); !os.IsNotExist(err) {
		t.Errorf("volume file should be gone: %v", err)
	}
}

func TestSecretsAreNotReturned(t *testing.T) {
	e := newEnv(t)
	const key = "DHHC-1:00:c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0:"
	got := e.do("POST", "/v1/hosts", obj{"nqn": hostNQN, "dhchap_key": key}, 201)
	if got["has_dhchap_key"] != true {
		t.Errorf("has_dhchap_key = %v", got["has_dhchap_key"])
	}
	b, _ := json.Marshal(e.do("GET", "/v1/hosts/"+hostNQN, nil, 200))
	if strings.Contains(string(b), key) {
		t.Errorf("secret returned by API: %s", b)
	}
	info, err := os.Stat(filepath.Join(e.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestAuthentication(t *testing.T) {
	m, err := NewManager(&target.Target{FS: targettest.New(), Root: "/nvmet"}, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	h := NewServer(m, "s3cret").Handler()
	check := func(path, auth string, want int) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("GET %s with %q = %d, want %d", path, auth, rec.Code, want)
		}
	}
	check("/v1/ports", "", 401)
	check("/v1/ports", "Bearer wrong", 401)
	check("/v1/ports", "Bearer s3cret", 200)
}

func TestTeardownKeepsSavedState(t *testing.T) {
	dir := t.TempDir()
	fs := targettest.New()
	m, err := NewManager(&target.Target{FS: fs, Root: "/nvmet"}, dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, fs: fs, dir: dir, http: NewServer(m, "").Handler()}
	e.fullConfig()
	if err := m.Teardown(); err != nil {
		t.Fatal(err)
	}
	if p := fs.Paths(); len(p) != 0 {
		t.Errorf("teardown left: %v", p)
	}
	e.start()
	if got, _ := fs.Attr("/nvmet/subsystems/" + subNQN + "/namespaces/1/enable"); got != "1" {
		t.Error("configuration not restored after teardown and restart")
	}
}

func TestSystemReportsVersion(t *testing.T) {
	m, err := NewManager(&target.Target{FS: targettest.New(), Root: "/nvmet"}, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(m, "")
	srv.Version = "1.2.3"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/system", nil))
	var got struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.2.3" {
		t.Errorf("version = %q", got.Version)
	}
}

// countingBackend counts how a listing checks whether namespaces are live.
type countingBackend struct {
	target.Backend
	single, all int
}

func (c *countingBackend) LiveEnabled(nqn string, nsid uint32) bool {
	c.single++
	return c.Backend.LiveEnabled(nqn, nsid)
}

func (c *countingBackend) LiveEnabledAll() target.LiveChecker {
	c.all++
	return c.Backend.LiveEnabledAll()
}

func TestListingChecksLiveStateOnce(t *testing.T) {
	backend := &countingBackend{Backend: &target.Target{FS: targettest.New(), Root: "/nvmet"}}
	m, err := NewManager(backend, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, http: NewServer(m, "").Handler()}
	for i := range 3 {
		sub := fmt.Sprintf("%s%d", subNQN, i)
		e.do("POST", "/v1/subsystems", obj{"nqn": sub}, 201)
		e.do("POST", "/v1/subsystems/"+sub+"/namespaces", obj{"nsid": 1, "file": obj{"name": sub[len(sub)-1:], "size_bytes": 4096}}, 201)
	}
	backend.single, backend.all = 0, 0
	rec := httptest.NewRecorder()
	e.http.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/subsystems", nil))
	if rec.Code != 200 {
		t.Fatalf("list = %d", rec.Code)
	}
	if backend.all != 1 || backend.single != 0 {
		t.Errorf("listing made %d snapshots and %d single checks, want 1 and 0", backend.all, backend.single)
	}
	if !strings.Contains(rec.Body.String(), `"live_enabled": true`) {
		t.Errorf("listing lost live state: %s", rec.Body)
	}
}
