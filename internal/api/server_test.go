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
	"sync"
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
	mu sync.Mutex
	// While gate is set, creating storage says so on entered, then waits for gate to close.
	gate      chan struct{}
	entered   chan string
	dir       string
	files     map[string]int64
	checkErr  error
	createErr error
	deleteErr error
	growErr   error
	capErr    error
	alignTo   int64
	// What Allocated and Changed report, and the internal names they were last asked about.
	blocks    storage.Blocks
	blocksErr error
	asked     []string
}

func (f *fakePools) driver(p *target.Pool) (storage.Driver, error) {
	if p.Type != storage.TypeReflink {
		return nil, target.Invalid("pool type %q isn't available", p.Type)
	}
	return fakeDriver{f}, nil
}

type fakeDriver struct{ *fakePools }

func (d fakeDriver) Check() error { return d.checkErr }
func (d fakeDriver) Allocated(internal string) (storage.Blocks, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = []string{internal}
	return d.blocks, d.blocksErr
}
func (d fakeDriver) Changed(base, internal string) (storage.Blocks, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = []string{base, internal}
	return d.blocks, d.blocksErr
}
func (d fakeDriver) Used(internal string) (int64, error) {
	size, ok := d.files[internal]
	if !ok {
		return 0, errors.New("no such volume")
	}
	return size / 2, nil
}
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
	if d.gate != nil {
		d.entered <- internal
		<-d.gate
	}
	if d.createErr != nil {
		return "", d.createErr
	}
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files[internal] = size
	return nil
}
func (d fakeDriver) copy(source, internal string, size int64) (string, error) {
	if d.createErr != nil {
		return "", d.createErr
	}
	d.mu.Lock()
	have, ok := d.files[source]
	d.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no such source %s", source)
	}
	if size == 0 {
		size = have
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	e.http = fixtureServer(m).Handler()
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

// A namespace's file is in the intent log, like a pool's volumes, so a crash leaves nothing behind that can't be made
// again, and nothing that was meant to go.
func TestManagedFilesSurviveACrash(t *testing.T) {
	file := func(e *env) string { return filepath.Join(e.dir, "volumes", "vol") }
	exists := func(e *env) bool { _, err := os.Stat(file(e)); return err == nil }
	ns := subNQN + "/namespaces"
	create := obj{"nsid": 1, "file": obj{"name": "vol", "size_bytes": 8192}}

	t.Run("created, and not yet saved", func(t *testing.T) {
		e := newEnv(t)
		e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
		e.crash("file created", func() { e.do("POST", "/v1/subsystems/"+ns, create, 201) })
		e.do("GET", "/v1/subsystems/"+ns+"/1", nil, 404)
		if exists(e) {
			t.Error("a file that nothing refers to was left")
		}
		e.do("POST", "/v1/subsystems/"+ns, create, 201)
		if n := e.intents(); n != 0 {
			t.Errorf("%d intents left", n)
		}
	})

	t.Run("forgotten, and not yet deleted", func(t *testing.T) {
		e := newEnv(t)
		e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
		e.do("POST", "/v1/subsystems/"+ns, create, 201)
		e.crash("file forgotten", func() { e.do("DELETE", "/v1/subsystems/"+ns+"/1?delete_file=true", nil, 204) })
		e.do("GET", "/v1/subsystems/"+ns+"/1", nil, 404)
		if exists(e) {
			t.Error("the file asked to be deleted was left")
		}
		if n := e.intents(); n != 0 {
			t.Errorf("%d intents left", n)
		}
	})

	t.Run("kept on purpose", func(t *testing.T) {
		e := newEnv(t)
		e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
		e.do("POST", "/v1/subsystems/"+ns, create, 201)
		e.do("DELETE", "/v1/subsystems/"+ns+"/1", nil, 204)
		e.start()
		if !exists(e) {
			t.Error("a file kept without delete_file was removed")
		}
	})
}

// Changing where a managed file's namespace points would orphan the file, or destroy it, so it's refused.
func TestAManagedFilesNamespaceKeepsItsFile(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	ns := "/v1/subsystems/" + subNQN + "/namespaces"
	e.do("POST", ns, obj{"nsid": 1, "file": obj{"name": "vol", "size_bytes": 8192}}, 201)
	e.do("PATCH", ns+"/1", obj{"device_path": e.file("other.img")}, 409)
	if _, err := os.Stat(filepath.Join(e.dir, "volumes", "vol")); err != nil {
		t.Errorf("the file: %v", err)
	}
	if got := e.do("GET", ns+"/1", nil, 200); got["device_path"] != filepath.Join(e.dir, "volumes", "vol") {
		t.Errorf("device_path = %v", got["device_path"])
	}
}

// A key the kernel refuses mustn't come back in the error, which reaches the caller and, for a 500, the log.
func TestAFailedKeyWriteDoesntQuoteTheKey(t *testing.T) {
	const key = "DHHC-1:00:c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0:"
	const ctrl = "DHHC-1:00:Y3RybGN0cmxjdHJsY3RybGN0cmxjdHJsY3RybGN0:"
	for _, attr := range []string{"/dhchap_key", "/dhchap_ctrl_key"} {
		for _, errno := range []error{syscall.EINVAL, syscall.EIO} {
			e := newEnv(t)
			e.fs.Fail[attr] = errno
			b, _ := json.Marshal(obj{"nqn": hostNQN, "dhchap_key": key, "dhchap_ctrl_key": ctrl})
			rec := httptest.NewRecorder()
			e.http.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/hosts", bytes.NewReader(b)))
			if rec.Code < 400 {
				t.Fatalf("%s: %d", attr, rec.Code)
			}
			if body := rec.Body.String(); strings.Contains(body, "c2VjcmV0") || strings.Contains(body, "Y3RybGN0") || !strings.Contains(body, attr[1:]) {
				t.Errorf("%s, %v: %s", attr, errno, body)
			}
		}
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
	e := &env{t: t, fs: fs, dir: dir, http: fixtureServer(m).Handler()}
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
	srv := fixtureServer(m)
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
	e := &env{t: t, http: fixtureServer(m).Handler()}
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

// fixtureServer is for tests whose requests have httptest's made-up host name, which a real server refuses with no token.
func fixtureServer(m *Manager) *Server {
	s := NewServer(m, "")
	s.anyHost = true
	return s
}

func TestConnections(t *testing.T) {
	e := newEnv(t)
	e.do(http.MethodGet, "/v1/connections", nil, http.StatusOK)
	e.do(http.MethodGet, "/v1/connections?subsystem=nqn.2026-11.net.example:none", nil, http.StatusNotFound)
}

// A failed create whose file can't be removed keeps its intent, so that the next start removes it.
func TestAFileThatCantBeRemovedKeepsItsIntent(t *testing.T) {
	ns := subNQN + "/namespaces"
	failRemove := func(t *testing.T) {
		real := removeManagedFile
		removeManagedFile = func(string) error { return errors.New("injected") }
		t.Cleanup(func() { removeManagedFile = real })
	}
	t.Run("the save fails", func(t *testing.T) {
		e := newEnv(t)
		e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
		failRemove(t)
		restore := e.breakSaving()
		body := e.do("POST", "/v1/subsystems/"+ns, obj{"nsid": 1, "file": obj{"name": "vol", "size_bytes": 8192}}, 500)
		restore()
		if !strings.Contains(fmt.Sprint(body["error"]), "tried again") {
			t.Errorf("the error doesn't say the file was left: %s", body)
		}
		if n := e.intents(); n != 1 {
			t.Fatalf("%d intents, want the create's", n)
		}
		removeManagedFile = os.Remove
		e.start()
		if _, err := os.Stat(filepath.Join(e.dir, "volumes", "vol")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the next start left the file: %v", err)
		}
		if n := e.intents(); n != 0 {
			t.Errorf("%d intents after the next start", n)
		}
	})
	t.Run("sizing the file fails", func(t *testing.T) {
		e := newEnv(t)
		e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
		failRemove(t)
		real := sizeFile
		sizeFile = func(*os.File, int64) error { return errors.New("injected") }
		t.Cleanup(func() { sizeFile = real })
		e.do("POST", "/v1/subsystems/"+ns, obj{"nsid": 1, "file": obj{"name": "vol", "size_bytes": 8192}}, 500)
		if n := e.intents(); n != 1 {
			t.Errorf("%d intents, want the create's", n)
		}
	})
	t.Run("the file is removed", func(t *testing.T) {
		e := newEnv(t)
		e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
		restore := e.breakSaving()
		e.do("POST", "/v1/subsystems/"+ns, obj{"nsid": 1, "file": obj{"name": "vol", "size_bytes": 8192}}, 500)
		restore()
		if n := e.intents(); n != 0 {
			t.Errorf("%d intents, with the file gone", n)
		}
	})
}

// oneSubsystemAtATime is the kernel target, but saying what SPDK says: that it
// can't list every connection at once. It counts the lists it's asked for.
type oneSubsystemAtATime struct {
	*target.Target
	asked int
}

func (b *oneSubsystemAtATime) Capabilities() target.Capabilities {
	c := b.Target.Capabilities()
	c.ListsAllConnections = false
	return c
}

func (b *oneSubsystemAtATime) Connections(st *target.State, nqn string) ([]target.Connection, error) {
	b.asked++
	return b.Target.Connections(st, nqn)
}

func TestListingEveryConnectionNeedsATargetThatCan(t *testing.T) {
	e := newEnv(t)
	e.do(http.MethodGet, "/v1/connections", nil, http.StatusOK)

	b := &oneSubsystemAtATime{Target: &target.Target{FS: e.fs, Root: "/nvmet"}}
	m, err := NewManager(b, e.dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	m.newDriver = e.pools.driver
	m.Recover()
	e.m, e.http = m, fixtureServer(m).Handler()
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	b.asked = 0
	body := e.do(http.MethodGet, "/v1/connections", nil, http.StatusUnprocessableEntity)
	if !strings.Contains(fmt.Sprint(body["error"]), "subsystem=") {
		t.Errorf("the refusal doesn't say what to pass: %v", body["error"])
	}
	if b.asked != 0 {
		t.Errorf("the target was asked for every connection, %d times", b.asked)
	}
	e.do(http.MethodGet, "/v1/connections?subsystem="+subNQN, nil, http.StatusOK)
	if b.asked != 1 {
		t.Errorf("one subsystem: asked %d times", b.asked)
	}
	if !(&target.Target{}).Capabilities().ListsAllConnections {
		t.Error("the kernel target says it can't list every connection")
	}
}
