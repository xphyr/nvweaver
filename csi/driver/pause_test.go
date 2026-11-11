package driver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWhetherANodeCanBePaused(t *testing.T) {
	dir := t.TempDir()
	for value, want := range map[string]bool{"Y\n": true, "N\n": false} {
		p := filepath.Join(dir, "multipath")
		if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if ok, reason := nvmePausable(p); ok != want || ok != (reason == "") {
			t.Errorf("%q: %v, %q", value, ok, reason)
		}
	}
	if ok, reason := nvmePausable(filepath.Join(dir, "none")); ok || !strings.Contains(reason, "can't tell") {
		t.Errorf("no such file: %v, %q", ok, reason)
	}
}

// fakeLeases is the Kubernetes API's leases, in one namespace, as much as the node plugin and controller use.
type fakeLeases struct {
	mu     sync.Mutex
	leases map[string]lease
	calls  []string
}

func (f *fakeLeases) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method)
	if r.Header.Get("Authorization") != "Bearer secret" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	const base = "/apis/coordination.k8s.io/v1/namespaces/nvweaver-csi/leases"
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")
	var in lease
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	switch {
	case r.Method == http.MethodPost && name == "":
		if _, ok := f.leases[in.Metadata.Name]; ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		in.Metadata.ResourceVersion = "1"
		f.leases[in.Metadata.Name] = in
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet:
		l, ok := f.leases[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(l)
	case r.Method == http.MethodPut:
		if in.Metadata.ResourceVersion != f.leases[name].Metadata.ResourceVersion {
			w.WriteHeader(http.StatusConflict)
			return
		}
		in.Metadata.ResourceVersion += "1"
		f.leases[name] = in
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func fakeKube(t *testing.T) (*kube, *fakeLeases) {
	t.Helper()
	f := &fakeLeases{leases: map[string]lease{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir() + "/"
	if err := os.WriteFile(dir+"token", []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &kube{base: srv.URL, hc: srv.Client(), saDir: dir, namespace: "nvweaver-csi"}, f
}

// Each node says whether it can be paused, in a Lease the controller reads; saying it again replaces it.
func TestNodesSayWhetherTheyCanBePaused(t *testing.T) {
	k, f := fakeKube(t)
	ctx := context.Background()
	cfg := testCfg
	read := k.pausable(cfg)
	if ok, reason, err := read(ctx, "worker-a"); ok || err != nil || !strings.Contains(reason, "hasn't said") {
		t.Errorf("a node that hasn't said: %v, %q, %v", ok, reason, err)
	}
	if err := k.reportPausable(ctx, cfg, "worker-a", false, "native NVMe multipath is off"); err != nil {
		t.Fatal(err)
	}
	if ok, reason, err := read(ctx, "worker-a"); ok || err != nil || reason != "native NVMe multipath is off" {
		t.Errorf("a node that can't: %v, %q, %v", ok, reason, err)
	}
	// Turned on, and the node plugin restarted.
	if err := k.reportPausable(ctx, cfg, "worker-a", true, ""); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := read(ctx, "worker-a"); !ok || err != nil {
		t.Errorf("a node that can: %v, %v", ok, err)
	}
	if l := f.leases["nvweaver-node-worker-a"]; l.Spec.HolderIdentity != "worker-a" || l.Metadata.Labels[cfg.Name+"/pause-nvme"] != "true" {
		t.Errorf("lease %+v", l)
	}
	if got := strings.Join(f.calls, " "); !strings.Contains(got, "POST GET PUT") {
		t.Errorf("replacing it called %s", got)
	}
}
