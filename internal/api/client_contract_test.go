package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"nvweaver/internal/client"
)

// The client is written by hand, so these check it against the spec and against the real handlers.

func TestClientTypesMatchTheSpec(t *testing.T) {
	s := loadSpec(t)
	types := map[string]any{
		"Health": client.Health{}, "Capabilities": client.Capabilities{}, "System": client.System{},
		"Port": client.Port{}, "PortCreate": client.PortCreate{}, "PortUpdate": client.PortUpdate{},
		"Host": client.Host{}, "HostCreate": client.HostCreate{}, "HostUpdate": client.HostUpdate{},
		"Subsystem": client.Subsystem{}, "SubsystemCreate": client.SubsystemCreate{}, "SubsystemUpdate": client.SubsystemUpdate{},
		"Namespace": client.Namespace{}, "NamespaceCreate": client.NamespaceCreate{}, "NamespaceUpdate": client.NamespaceUpdate{},
		"PoolCapacity": client.PoolCapacity{}, "Pool": client.Pool{}, "PoolCreate": client.PoolCreate{}, "PoolUpdate": client.PoolUpdate{},
		"VolumeSource": client.VolumeSource{}, "VolumeMapping": client.VolumeMapping{}, "Volume": client.Volume{},
		"VolumePage": client.VolumePage{}, "VolumeCreate": client.VolumeCreate{}, "VolumeUpdate": client.VolumeUpdate{},
		"Snapshot": client.Snapshot{}, "SnapshotPage": client.SnapshotPage{}, "SnapshotCreate": client.SnapshotCreate{},
	}
	for schema, props := range s.schemas {
		if len(props) == 0 {
			continue // Such as NQN, which is only a string.
		}
		if schema == "Error" {
			continue // Handled by client.Error.
		}
		v, ok := types[schema]
		if !ok {
			t.Errorf("the client has no type for schema %s", schema)
			continue
		}
		fields := map[string]bool{}
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			fields[name] = true
		}
		for prop := range props {
			if !fields[prop] {
				t.Errorf("client.%s has no field for %s in the spec", schema, prop)
			}
		}
		for field := range fields {
			if !props[field] {
				t.Errorf("client.%s has %s, which isn't in the spec", schema, field)
			}
		}
	}
	for schema := range types {
		if _, ok := s.schemas[schema]; !ok {
			t.Errorf("client type for %s, which isn't in the spec", schema)
		}
	}
}

// TestClientReachesEveryOperation runs every method against the real handlers, and fails if any operation in the spec isn't
// reached, or isn't successful. So adding an operation to the API without adding it to the client fails here.
func TestClientReachesEveryOperation(t *testing.T) {
	s := loadSpec(t)
	_, handler := newSpecServer(t)
	reached := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		handler.ServeHTTP(rec, r)
		if rec.status < 300 {
			reached[operationOf(s, r)] = true
		}
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(client.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	ptr := func(i int) *int { return &i }
	const nqn, hostNQN = "nqn.2026-11.net.example:client", "nqn.2026-11.net.example:host"
	dir := t.TempDir()

	health, err := c.Health(ctx)
	must("health", err)
	if health.Status == "" {
		t.Error("no health status")
	}
	sys, err := c.System(ctx)
	must("system", err)
	if sys.Backend == "" || sys.Warnings == nil {
		t.Errorf("system came back as %+v", sys)
	}

	port, err := c.CreatePort(ctx, client.PortCreate{ID: ptr(0), Address: "127.0.0.1", Service: 4420})
	must("create port", err)
	if port.ID != 0 || port.Transport != "tcp" {
		t.Errorf("port came back as %+v", port)
	}
	_, err = c.ListPorts(ctx)
	must("list ports", err)
	_, err = c.GetPort(ctx, port.ID)
	must("get port", err)
	service := 4421
	updated, err := c.UpdatePort(ctx, port.ID, client.PortUpdate{Service: &service})
	must("update port", err)
	if updated.Service != service || updated.Address != "127.0.0.1" {
		t.Errorf("port came back as %+v", updated)
	}

	_, err = c.CreateHost(ctx, client.HostCreate{NQN: hostNQN})
	must("create host", err)
	_, err = c.ListHosts(ctx)
	must("list hosts", err)
	_, err = c.GetHost(ctx, hostNQN)
	must("get host", err)
	hash := "hmac(sha256)"
	_, err = c.UpdateHost(ctx, hostNQN, client.HostUpdate{DHCHAPHash: &hash})
	must("update host", err)

	sub, err := c.CreateSubsystem(ctx, client.SubsystemCreate{NQN: nqn, AllowedHosts: []string{}, Ports: []int{}})
	must("create subsystem", err)
	if sub.NQN != nqn || sub.Serial == "" {
		t.Errorf("subsystem came back as %+v", sub)
	}
	_, err = c.ListSubsystems(ctx)
	must("list subsystems", err)
	_, err = c.GetSubsystem(ctx, nqn)
	must("get subsystem", err)
	must("attach port", c.AttachPort(ctx, nqn, port.ID))
	must("allow host", c.AllowHost(ctx, nqn, hostNQN))
	got, err := c.GetSubsystem(ctx, nqn)
	must("get subsystem", err)
	if len(got.Ports) != 1 || len(got.AllowedHosts) != 1 {
		t.Errorf("subsystem came back as %+v", got)
	}

	ns, err := c.CreateNamespace(ctx, nqn, client.NamespaceCreate{File: &client.NamespaceFile{Name: "f.img", SizeBytes: 1 << 20}})
	must("create namespace", err)
	if ns.NSID == 0 || !ns.ManagedFile || ns.UUID == "" {
		t.Errorf("namespace came back as %+v", ns)
	}
	_, err = c.ListNamespaces(ctx, nqn)
	must("list namespaces", err)
	_, err = c.GetNamespace(ctx, nqn, ns.NSID)
	must("get namespace", err)
	bigger := int64(2 << 20)
	grown, err := c.UpdateNamespace(ctx, nqn, ns.NSID, client.NamespaceUpdate{SizeBytes: &bigger})
	must("update namespace", err)
	if grown.SizeBytes != bigger {
		t.Errorf("namespace came back as %+v", grown)
	}
	must("delete namespace", c.DeleteNamespace(ctx, nqn, ns.NSID, true))

	pool, err := c.CreatePool(ctx, client.PoolCreate{Name: "p", Type: "reflink", Path: dir})
	must("create pool", err)
	if pool.State == "" || pool.Warnings == nil {
		t.Errorf("pool came back as %+v", pool)
	}
	_, err = c.ListPools(ctx)
	must("list pools", err)
	_, err = c.GetPool(ctx, "p")
	must("get pool", err)
	labels := map[string]string{"a": "b"}
	labelled, err := c.UpdatePool(ctx, "p", client.PoolUpdate{Labels: &labels})
	must("update pool", err)
	if labelled.Labels["a"] != "b" {
		t.Errorf("pool came back as %+v", labelled)
	}
	cleared, err := c.UpdatePool(ctx, "p", client.PoolUpdate{Labels: &map[string]string{}})
	must("clear pool labels", err)
	if len(cleared.Labels) != 0 {
		t.Errorf("labels weren't cleared: %+v", cleared)
	}

	vol, err := c.CreateVolume(ctx, client.VolumeCreate{Name: "v", SizeBytes: 1 << 20})
	must("create volume", err)
	if vol.Pool != "p" || vol.NGUID == "" {
		t.Errorf("volume came back as %+v", vol)
	}
	vols, err := c.ListVolumes(ctx, "p")
	must("list volumes", err)
	if len(vols) != 1 {
		t.Errorf("got %d volumes", len(vols))
	}
	_, err = c.GetVolume(ctx, "v")
	must("get volume", err)
	grownVol, err := c.UpdateVolume(ctx, "v", client.VolumeUpdate{SizeBytes: &bigger})
	must("update volume", err)
	if grownVol.SizeBytes != bigger {
		t.Errorf("volume came back as %+v", grownVol)
	}
	mapped, err := c.CreateNamespace(ctx, nqn, client.NamespaceCreate{Volume: "v"})
	must("map volume", err)
	vol, err = c.GetVolume(ctx, "v")
	must("get volume", err)
	if vol.MappedTo == nil || vol.MappedTo.Subsystem != nqn {
		t.Errorf("volume's mapping came back as %+v", vol.MappedTo)
	}
	if err := c.DeleteVolume(ctx, "v"); !client.IsConflict(err) {
		t.Errorf("deleting a mapped volume: got %v", err)
	}
	must("unmap volume", c.DeleteNamespace(ctx, nqn, mapped.NSID, false))

	snap, err := c.CreateSnapshot(ctx, client.SnapshotCreate{Name: "s", Volume: "v"})
	must("create snapshot", err)
	if snap.Volume != "v" {
		t.Errorf("snapshot came back as %+v", snap)
	}
	snaps, err := c.ListSnapshots(ctx, "v")
	must("list snapshots", err)
	if len(snaps) != 1 {
		t.Errorf("got %d snapshots", len(snaps))
	}
	_, err = c.GetSnapshot(ctx, "s")
	must("get snapshot", err)
	clone, err := c.CreateVolume(ctx, client.VolumeCreate{Name: "c", Source: &client.VolumeSource{Snapshot: "s"}})
	must("clone", err)
	if clone.Source == nil || clone.Source.Snapshot != "s" {
		t.Errorf("clone came back as %+v", clone)
	}
	must("delete clone", c.DeleteVolume(ctx, "c"))
	must("delete snapshot", c.DeleteSnapshot(ctx, "s"))
	must("delete volume", c.DeleteVolume(ctx, "v"))
	must("delete pool", c.DeletePool(ctx, "p"))

	subNow, err := c.UpdateSubsystem(ctx, nqn, client.SubsystemUpdate{AllowAnyHost: new(bool)})
	must("update subsystem", err)
	if subNow.AllowAnyHost {
		t.Error("allow_any_host didn't change")
	}
	must("deny host", c.DenyHost(ctx, nqn, hostNQN))
	must("detach port", c.DetachPort(ctx, nqn, port.ID))
	must("delete subsystem", c.DeleteSubsystem(ctx, nqn))
	must("delete host", c.DeleteHost(ctx, hostNQN))
	must("delete port", c.DeletePort(ctx, port.ID))

	if _, err := c.GetVolume(ctx, "gone"); !client.IsNotFound(err) {
		t.Errorf("a missing volume: got %v", err)
	}
	for op := range s.operations {
		if !reached[op] {
			t.Errorf("the client never reached %s", op)
		}
	}
	delete(reached, "")
	if len(reached) != len(s.operations) {
		t.Errorf("reached %d operations, of %d", len(reached), len(s.operations))
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// operationOf turns a request back into the spec's name for it, such as "GET /v1/ports/{id}".
func operationOf(s spec, r *http.Request) string {
	segments := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
	for op := range s.operations {
		method, path, _ := strings.Cut(op, " ")
		if method != r.Method {
			continue
		}
		want := strings.Split(strings.Trim(path, "/"), "/")
		if len(want) != len(segments) {
			continue
		}
		match := true
		for i := range want {
			if !strings.HasPrefix(want[i], "{") && want[i] != segments[i] {
				match = false
				break
			}
		}
		if match {
			return op
		}
	}
	return ""
}
