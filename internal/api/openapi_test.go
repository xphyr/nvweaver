package api

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"nvweaver/internal/target"
	"nvweaver/internal/target/targettest"
)

// spec is the part of api/openapi.yaml these tests compare with the code. It is read by
// indentation, enough for a file in this one layout, to avoid needing a YAML dependency.
type spec struct {
	operations map[string]bool            // "GET /v1/ports"
	schemas    map[string]map[string]bool // schema name to property names
}

var httpMethods = []string{"get", "post", "put", "patch", "delete"}

func loadSpec(t *testing.T) spec {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := spec{operations: map[string]bool{}, schemas: map[string]map[string]bool{}}
	var section, path, schema string
	inProperties := false
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		line := lines.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		key, _, isKey := strings.Cut(trimmed, ":")
		switch {
		case indent == 0:
			section = key
		case indent == 2 && section == "paths" && isKey:
			path = key
		case indent == 4 && section == "paths" && slices.Contains(httpMethods, key):
			s.operations[strings.ToUpper(key)+" "+path] = true
		case indent == 2 && section == "components":
			section = "components." + key
		case indent == 2 && strings.HasPrefix(section, "components.") && isKey:
			section = "components." + key
		case indent == 4 && section == "components.schemas" && isKey:
			schema = key
			s.schemas[schema] = map[string]bool{}
			inProperties = false
		case indent == 6 && section == "components.schemas":
			inProperties = key == "properties"
		case indent == 8 && section == "components.schemas" && inProperties && isKey:
			s.schemas[schema][key] = true
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	if len(s.operations) == 0 || len(s.schemas) == 0 {
		t.Fatal("read nothing from the spec: has its layout changed?")
	}
	return s
}

func newSpecServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	m, err := NewManager(&target.Target{FS: targettest.New(), Root: "/nvmet"}, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	m.newDriver = newFakeDriver(t.TempDir())
	srv := fixtureServer(m)
	return srv, srv.Handler()
}

func TestSpecCoversExactlyTheRoutes(t *testing.T) {
	s := loadSpec(t)
	srv, _ := newSpecServer(t)
	routes := map[string]bool{}
	for _, r := range srv.patterns {
		routes[r] = true
		if !s.operations[r] {
			t.Errorf("the API serves %q, which isn't in the spec", r)
		}
	}
	for op := range s.operations {
		if !routes[op] {
			t.Errorf("the spec has %q, which the API doesn't serve", op)
		}
	}
}

// fieldsOf returns a JSON object's keys.
func fieldsOf(t *testing.T, data []byte) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("%v in %s", err, data)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestSpecDescribesResponses fills in every optional field, then checks that everything
// the API returns is in the matching schema, and that the schema lists nothing it doesn't.
func TestSpecDescribesResponses(t *testing.T) {
	s := loadSpec(t)
	_, h := newSpecServer(t)
	send := func(method, path, body string, want int) []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		if rec.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body)
		}
		return rec.Body.Bytes()
	}
	const sub = "nqn.2026-11.net.example:s"
	const host = "nqn.2026-11.net.example:h"
	send("POST", "/v1/hosts", `{"nqn":"`+host+`","dhchap_key":"k","dhchap_ctrl_key":"c","dhchap_hash":"hmac(sha256)","dhchap_dhgroup":"null","labels":{"csi-node":"n1"},"owner":"lab"}`, 201)
	send("POST", "/v1/ports", `{"id":1,"address":"127.0.0.1","inline_data_size":8192}`, 201)
	send("POST", "/v1/subsystems", `{"nqn":"`+sub+`","allowed_hosts":["`+host+`"],"ports":[1],"owner":"lab"}`, 201)
	send("POST", "/v1/subsystems/"+sub+"/namespaces", `{"file":{"name":"a","size_bytes":4096}}`, 201)
	send("POST", "/v1/pools", `{"name":"p","type":"reflink","path":"/fake","labels":{"a":"b"}}`, 201)
	send("POST", "/v1/volumes", `{"name":"v","size_bytes":4096,"labels":{"a":"b"},"owner":"lab"}`, 201)
	send("POST", "/v1/volumes", `{"name":"w","size_bytes":4096}`, 201)
	send("POST", "/v1/subsystems/"+sub+"/namespaces", `{"nsid":2,"volume":"v"}`, 201)
	send("POST", "/v1/snapshots", `{"name":"s","volume":"w","owner":"lab"}`, 201)
	send("POST", "/v1/snapshots", `{"name":"t","volume":"w"}`, 201)

	// A passthrough subsystem is the only way to get passthru_device, which needs a real
	// controller to create, so it is allowed to be absent here.
	// A namespace has managed_file or volume, depending on its backing: both are checked below.
	// A pool has path or volume_group and thin_pool, depending on its type: LVM needs real commands.
	optional := map[string][]string{
		"Subsystem": {"passthru_device"},
		"Namespace": {"volume", "managed_file"},
		"Pool":      {"volume_group", "thin_pool"},
	}

	for _, c := range []struct {
		schema string
		body   []byte
	}{
		{"System", send("GET", "/v1/system", "", 200)},
		{"Host", send("GET", "/v1/hosts/"+host, "", 200)},
		{"Port", send("GET", "/v1/ports/1", "", 200)},
		{"Subsystem", send("GET", "/v1/subsystems/"+sub, "", 200)},
		{"Namespace", send("GET", "/v1/subsystems/"+sub+"/namespaces/1", "", 200)},
		{"Namespace", send("GET", "/v1/subsystems/"+sub+"/namespaces/2", "", 200)},
		{"Pool", send("GET", "/v1/pools/p", "", 200)},
		{"Volume", send("GET", "/v1/volumes/v", "", 200)},
		{"VolumePage", send("GET", "/v1/volumes?limit=1", "", 200)},
		{"Snapshot", send("GET", "/v1/snapshots/s", "", 200)},
		{"SnapshotPage", send("GET", "/v1/snapshots?limit=1", "", 200)},
		{"Health", send("GET", "/healthz", "", 200)},
		{"Error", send("GET", "/v1/ports/9", "", 404)},
	} {
		props, ok := s.schemas[c.schema]
		if !ok {
			t.Errorf("no schema %s in the spec", c.schema)
			continue
		}
		got := fieldsOf(t, c.body)
		for _, f := range got {
			if !props[f] {
				t.Errorf("%s: the API returns %q, which isn't in the spec", c.schema, f)
			}
		}
		for f := range props {
			if !slices.Contains(got, f) && !slices.Contains(optional[c.schema], f) {
				t.Errorf("%s: the spec has %q, which the API didn't return", c.schema, f)
			}
		}
	}

	if got := fieldsOf(t, send("GET", "/v1/subsystems/"+sub+"/namespaces/1", "", 200)); !slices.Contains(got, "managed_file") {
		t.Errorf("a file namespace should have managed_file: %v", got)
	}
	if got := fieldsOf(t, send("GET", "/v1/subsystems/"+sub+"/namespaces/2", "", 200)); !slices.Contains(got, "volume") {
		t.Errorf("a volume namespace should have volume: %v", got)
	}

	var system struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(send("GET", "/v1/system", "", 200), &system); err != nil {
		t.Fatal(err)
	}
	for _, f := range fieldsOf(t, system.Capabilities) {
		if !s.schemas["Capabilities"][f] {
			t.Errorf("Capabilities: the API returns %q, which isn't in the spec", f)
		}
	}
}

// TestSpecFieldsAreAccepted sends each request schema's fields, set to null so as to change
// nothing. The decoder refuses a field it doesn't know, so this finds any the API has lost.
func TestSpecFieldsAreAccepted(t *testing.T) {
	s := loadSpec(t)
	const sub = "nqn.2026-11.net.example:s"
	const host = "nqn.2026-11.net.example:h"
	for _, c := range []struct{ schema, method, path string }{
		{"PortCreate", "POST", "/v1/ports"},
		{"PortUpdate", "PATCH", "/v1/ports/1"},
		{"HostCreate", "POST", "/v1/hosts"},
		{"HostUpdate", "PATCH", "/v1/hosts/" + host},
		{"SubsystemCreate", "POST", "/v1/subsystems"},
		{"SubsystemUpdate", "PATCH", "/v1/subsystems/" + sub},
		{"NamespaceCreate", "POST", "/v1/subsystems/" + sub + "/namespaces"},
		{"NamespaceUpdate", "PATCH", "/v1/subsystems/" + sub + "/namespaces/1"},
		{"PoolCreate", "POST", "/v1/pools"},
		{"PoolUpdate", "PATCH", "/v1/pools/p"},
		{"VolumeCreate", "POST", "/v1/volumes"},
		{"VolumeUpdate", "PATCH", "/v1/volumes/v"},
	} {
		props := s.schemas[c.schema]
		if len(props) == 0 {
			t.Errorf("no properties for schema %s", c.schema)
		}
		for field := range props {
			_, h := newSpecServer(t)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(`{"`+field+`":null}`)))
			body, _ := io.ReadAll(rec.Body)
			if strings.Contains(string(body), "unknown field") {
				t.Errorf("%s: %s %s refuses %q: %s", c.schema, c.method, c.path, field, body)
			}
		}
	}
	// A check that can't fail proves nothing: a field that doesn't exist must be refused.
	_, h := newSpecServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/ports", strings.NewReader(`{"no_such_field":null}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field gave %d, want 400", rec.Code)
	}
}
