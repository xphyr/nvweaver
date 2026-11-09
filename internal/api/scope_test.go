package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"nvweaver/internal/auth"
)

// scopedEnv is a server with an admin token, and two scoped tokens: one for each of two clusters.
type scopedEnv struct {
	*env
	admin, lab, other string
}

func newScopedEnv(t *testing.T) *scopedEnv {
	t.Helper()
	e := newEnv(t)
	lab, labLine, _ := auth.NewToken("lab")
	other, otherLine, _ := auth.NewToken("other")
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte(labLine+"\n"+otherLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	toks, err := auth.NewTokens(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(e.m, "admin-secret")
	s.anyHost, s.Tokens = true, toks
	e.http = s.Handler()
	se := &scopedEnv{env: e, admin: "admin-secret", lab: lab, other: other}
	se.as(se.admin, "POST", "/v1/pools", obj{"name": "p", "type": "reflink", "path": "/pool"}, http.StatusCreated)
	return se
}

// as sends a request with a token, and checks the status.
func (e *scopedEnv) as(token, method, path string, body any, want int) map[string]any {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.http.ServeHTTP(rec, req)
	if rec.Code != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body)
	}
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestAScopedTokenOwnsWhatItMakes(t *testing.T) {
	e := newScopedEnv(t)
	e.as("nvw_wrong", "GET", "/v1/volumes", nil, http.StatusUnauthorized)
	e.as(e.lab, "GET", "/v1/volumes", nil, http.StatusOK)

	v := e.as(e.lab, "POST", "/v1/volumes", obj{"name": "lab-a", "size_bytes": 8192}, http.StatusCreated)
	if v["owner"] != "lab" {
		t.Errorf("a scoped token's volume has owner %v", v["owner"])
	}
	e.as(e.lab, "POST", "/v1/volumes", obj{"name": "lab-a", "size_bytes": 8192}, http.StatusOK) // CSI's retry
	e.as(e.other, "POST", "/v1/volumes", obj{"name": "lab-a", "size_bytes": 8192}, http.StatusForbidden)
	e.as(e.other, "PATCH", "/v1/volumes/lab-a", obj{"size_bytes": 16384}, http.StatusForbidden)
	e.as(e.other, "DELETE", "/v1/volumes/lab-a", nil, http.StatusForbidden)
	e.as(e.other, "POST", "/v1/snapshots", obj{"name": "x", "volume": "lab-a"}, http.StatusForbidden)
	e.as(e.other, "POST", "/v1/volumes", obj{"name": "x", "source": obj{"volume": "lab-a"}}, http.StatusForbidden)
	e.as(e.lab, "POST", "/v1/volumes", obj{"name": "lab-b", "size_bytes": 8192, "owner": "other"}, http.StatusForbidden)

	s := e.as(e.lab, "POST", "/v1/snapshots", obj{"name": "lab-s", "volume": "lab-a"}, http.StatusCreated)
	if s["owner"] != "lab" {
		t.Errorf("snapshot owner %v", s["owner"])
	}
	e.as(e.other, "POST", "/v1/volumes", obj{"name": "x", "source": obj{"snapshot": "lab-s"}}, http.StatusForbidden)
	e.as(e.other, "DELETE", "/v1/snapshots/lab-s", nil, http.StatusForbidden)
	c := e.as(e.lab, "POST", "/v1/volumes", obj{"name": "lab-c", "source": obj{"snapshot": "lab-s"}}, http.StatusCreated)
	if c["owner"] != "lab" {
		t.Errorf("clone owner %v", c["owner"])
	}
	e.as(e.lab, "PATCH", "/v1/volumes/lab-a", obj{"labels": obj{"k": "v"}}, http.StatusOK)
	e.as(e.lab, "PATCH", "/v1/volumes/lab-a", obj{"owner": "other"}, http.StatusForbidden)

	// What an admin made, with no owner, is only for admins.
	e.as(e.admin, "POST", "/v1/volumes", obj{"name": "unowned", "size_bytes": 8192}, http.StatusCreated)
	e.as(e.lab, "DELETE", "/v1/volumes/unowned", nil, http.StatusForbidden)
	// An admin can give it to a token, and take it back.
	e.as(e.admin, "PATCH", "/v1/volumes/unowned", obj{"owner": "lab"}, http.StatusOK)
	e.as(e.lab, "PATCH", "/v1/volumes/unowned", obj{"size_bytes": 16384}, http.StatusOK)
	e.as(e.admin, "PATCH", "/v1/volumes/unowned", obj{"owner": ""}, http.StatusOK)
	e.as(e.lab, "DELETE", "/v1/volumes/unowned", nil, http.StatusForbidden)
	e.as(e.admin, "POST", "/v1/volumes", obj{"name": "given", "size_bytes": 8192, "owner": "other"}, http.StatusCreated)
	e.as(e.admin, "POST", "/v1/volumes", obj{"name": "badowner", "size_bytes": 8192, "owner": "Not OK"}, http.StatusUnprocessableEntity)

	e.as(e.other, "DELETE", "/v1/volumes/given", nil, http.StatusNoContent)
	e.as(e.lab, "DELETE", "/v1/volumes/lab-c", nil, http.StatusNoContent)
	e.as(e.lab, "DELETE", "/v1/snapshots/lab-s", nil, http.StatusNoContent)
	e.as(e.lab, "DELETE", "/v1/volumes/lab-a", nil, http.StatusNoContent)
}

func TestAScopedTokensSubsystems(t *testing.T) {
	e := newScopedEnv(t)
	host := "nqn.2014-08.org.nvmexpress:uuid:node-1"
	e.as(e.lab, "POST", "/v1/hosts", obj{"nqn": host}, http.StatusCreated)
	e.as(e.lab, "POST", "/v1/hosts", obj{"nqn": host + "2", "dhchap_key": "DHHC-1:00:" + "c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0MTIzNDU2Nzg=" + ":"}, http.StatusForbidden)
	e.as(e.admin, "POST", "/v1/ports", obj{"id": 1, "address": "127.0.0.1", "service": 4420}, http.StatusCreated)
	e.as(e.lab, "POST", "/v1/volumes", obj{"name": "lab-v", "size_bytes": 8192}, http.StatusCreated)
	e.as(e.other, "POST", "/v1/volumes", obj{"name": "other-v", "size_bytes": 8192}, http.StatusCreated)

	sub := "nqn.2026-11.net.example:lab-node"
	s := e.as(e.lab, "POST", "/v1/subsystems", obj{"nqn": sub, "allowed_hosts": []string{host}, "ports": []int{1}}, http.StatusCreated)
	if s["owner"] != "lab" {
		t.Errorf("subsystem owner %v", s["owner"])
	}
	e.as(e.lab, "POST", "/v1/subsystems", obj{"nqn": sub + "-any", "allow_any_host": true}, http.StatusForbidden)
	e.as(e.lab, "POST", "/v1/subsystems", obj{"nqn": sub + "-pt", "passthru_device": "/dev/nvme0"}, http.StatusForbidden)
	e.as(e.lab, "PATCH", "/v1/subsystems/"+sub, obj{"allow_any_host": true}, http.StatusForbidden)
	e.as(e.lab, "POST", "/v1/subsystems/"+sub+"/namespaces", obj{"volume": "other-v"}, http.StatusForbidden)
	e.as(e.lab, "POST", "/v1/subsystems/"+sub+"/namespaces", obj{"device_path": e.file("raw")}, http.StatusForbidden)
	e.as(e.lab, "POST", "/v1/subsystems/"+sub+"/namespaces", obj{"file": obj{"name": "f.img", "size_bytes": 8192}}, http.StatusForbidden)
	e.as(e.lab, "POST", "/v1/subsystems/"+sub+"/namespaces", obj{"volume": "lab-v"}, http.StatusCreated)

	for _, call := range []struct{ method, path string }{
		{"DELETE", "/v1/subsystems/" + sub},
		{"PUT", "/v1/subsystems/" + sub + "/ports/1"},
		{"DELETE", "/v1/subsystems/" + sub + "/ports/1"},
		{"PUT", "/v1/subsystems/" + sub + "/hosts/" + host},
		{"DELETE", "/v1/subsystems/" + sub + "/hosts/" + host},
		{"POST", "/v1/subsystems/" + sub + "/namespaces"},
		{"DELETE", "/v1/subsystems/" + sub + "/namespaces/1"},
	} {
		var body any
		if call.method == "POST" {
			body = obj{"volume": "other-v"}
		}
		e.as(e.other, call.method, call.path, body, http.StatusForbidden)
	}
	e.as(e.lab, "PUT", "/v1/subsystems/"+sub+"/hosts/"+host, nil, http.StatusNoContent)
	e.as(e.lab, "DELETE", "/v1/subsystems/"+sub+"/namespaces/1", nil, http.StatusNoContent)
	e.as(e.lab, "DELETE", "/v1/subsystems/"+sub, nil, http.StatusNoContent)
}

// Anything else that changes the service is for admins only, and refused before it's looked at.
func TestAScopedTokenCantChangeTheService(t *testing.T) {
	e := newScopedEnv(t)
	host := "nqn.2014-08.org.nvmexpress:uuid:node-1"
	e.as(e.admin, "POST", "/v1/hosts", obj{"nqn": host}, http.StatusCreated)
	e.as(e.admin, "POST", "/v1/ports", obj{"id": 1, "address": "127.0.0.1", "service": 4420}, http.StatusCreated)
	sub := "nqn.2026-11.net.example:admin"
	e.as(e.admin, "POST", "/v1/subsystems", obj{"nqn": sub}, http.StatusCreated)
	e.as(e.admin, "POST", "/v1/subsystems/"+sub+"/namespaces", obj{"nsid": 1, "device_path": e.file("raw")}, http.StatusCreated)
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/pools", obj{"name": "p", "type": "reflink", "path": "/tmp"}},
		{"PATCH", "/v1/pools/p", obj{"labels": obj{}}},
		{"DELETE", "/v1/pools/p", nil},
		{"POST", "/v1/ports", obj{"id": 2, "address": "127.0.0.1", "service": 4421}},
		{"PATCH", "/v1/ports/1", obj{"service": 4422}},
		{"DELETE", "/v1/ports/1", nil},
		{"PATCH", "/v1/hosts/" + host, obj{"dhchap_hash": "hmac(sha256)"}},
		{"DELETE", "/v1/hosts/" + host, nil},
		{"PATCH", "/v1/subsystems/" + sub + "/namespaces/1", obj{"enabled": false}},
		{"DELETE", "/v1/subsystems/" + sub, nil},
	} {
		e.as(e.lab, call.method, call.path, call.body, http.StatusForbidden)
	}
	// Reads are for everyone.
	e.as(e.lab, "GET", "/v1/subsystems/"+sub, nil, http.StatusOK)
	e.as(e.lab, "GET", "/v1/system", nil, http.StatusOK)
}
