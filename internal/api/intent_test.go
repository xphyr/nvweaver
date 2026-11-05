package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nvweaver/internal/target"
)

type crashed struct{}

// crash runs a request that dies at the named point, as a real crash would: nothing after it runs,
// and nothing that wasn't yet saved is kept. Then the service starts again.
func (e *env) crash(point string, request func()) {
	e.t.Helper()
	e.m.crash = func(p string) {
		if p == point {
			panic(crashed{})
		}
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				e.t.Errorf("the request never reached %q", point)
			} else if _, ok := r.(crashed); !ok {
				panic(r)
			}
		}()
		request()
	}()
	e.start()
}

func (e *env) intents() int {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.dir, "intents"))
	if err != nil {
		e.t.Fatal(err)
	}
	return len(entries)
}

// consistent checks what a crash must never break: the pool holds exactly what the state refers to.
func (e *env) consistent() {
	e.t.Helper()
	if n := e.intents(); n != 0 {
		e.t.Errorf("%d intents left", n)
	}
	pool := e.do("GET", "/v1/pools/p", nil, 200)
	if want := int(pool["volumes"].(float64) + pool["snapshots"].(float64)); len(e.pools.files) != want {
		e.t.Errorf("%d files for %d volumes and snapshots: %v", len(e.pools.files), want, e.pools.files)
	}
	if strings.Contains(fmt.Sprint(pool["warnings"]), "no volume refers") {
		e.t.Errorf("warnings = %v", pool["warnings"])
	}
}

func TestCrashWhileCreating(t *testing.T) {
	for _, tc := range []struct {
		kind string
		path string
		req  obj
		get  string
	}{
		{"volume", "/v1/volumes", obj{"name": "n", "size_bytes": 4096}, "/v1/volumes/n"},
		{"clone", "/v1/volumes", obj{"name": "n", "source": obj{"volume": "base"}}, "/v1/volumes/n"},
		{"snapshot", "/v1/snapshots", obj{"name": "n", "volume": "base"}, "/v1/snapshots/n"},
	} {
		// Before the state is saved, it never happened, and the client can simply retry.
		for _, point := range []string{"intent", "storage"} {
			t.Run(tc.kind+" at "+point, func(t *testing.T) {
				e := newEnv(t)
				e.pool()
				e.do("POST", "/v1/volumes", obj{"name": "base", "size_bytes": 4096}, 201)
				before := len(e.pools.files)
				e.crash(point, func() { e.do("POST", tc.path, tc.req, 201) })
				e.do("GET", tc.get, nil, 404)
				if len(e.pools.files) != before {
					t.Errorf("storage left behind: %v", e.pools.files)
				}
				e.consistent()
				e.do("POST", tc.path, tc.req, 201)
				e.consistent()
			})
		}
		// Once it's saved, it happened, even if the client never heard. A retry finds it.
		t.Run(tc.kind+" at saved", func(t *testing.T) {
			e := newEnv(t)
			e.pool()
			e.do("POST", "/v1/volumes", obj{"name": "base", "size_bytes": 4096}, 201)
			e.crash("saved", func() { e.do("POST", tc.path, tc.req, 201) })
			first := e.do("GET", tc.get, nil, 200)
			if _, ok := e.pools.files[first["internal_name"].(string)]; !ok {
				t.Errorf("its storage has gone: %v", e.pools.files)
			}
			e.consistent()
			if again := e.do("POST", tc.path, tc.req, 200); again["internal_name"] != first["internal_name"] {
				t.Errorf("a retry made another: %v, %v", first, again)
			}
		})
	}
}

func TestCrashWhileDeleting(t *testing.T) {
	for _, tc := range []struct {
		kind string
		path string
	}{
		{"volume", "/v1/volumes/v"},
		{"snapshot", "/v1/snapshots/s"},
	} {
		setup := func(t *testing.T) *env {
			e := newEnv(t)
			e.pool()
			e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
			e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "v"}, 201)
			return e
		}
		// Before the state is saved, the object is still there, and must keep its data.
		t.Run(tc.kind+" at intent", func(t *testing.T) {
			e := setup(t)
			e.crash("intent", func() { e.do("DELETE", tc.path, nil, 204) })
			got := e.do("GET", tc.path, nil, 200)
			if _, ok := e.pools.files[got["internal_name"].(string)]; !ok {
				t.Errorf("a %s that's still listed lost its storage", tc.kind)
			}
			e.consistent()
			e.do("DELETE", tc.path, nil, 204)
			e.consistent()
		})
		for _, point := range []string{"saved", "storage"} {
			t.Run(tc.kind+" at "+point, func(t *testing.T) {
				e := setup(t)
				e.crash(point, func() { e.do("DELETE", tc.path, nil, 204) })
				e.do("GET", tc.path, nil, 404)
				e.consistent()
			})
		}
	}
}

func TestCrashWhileGrowing(t *testing.T) {
	for _, point := range []string{"intent", "storage", "saved"} {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t)
			e.pool()
			vol := e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
			e.crash(point, func() { e.do("PATCH", "/v1/volumes/v", obj{"size_bytes": 8192}, 200) })
			// Shrinking back could lose data, so it's finished, whichever way it went.
			got := e.do("GET", "/v1/volumes/v", nil, 200)
			if got["size_bytes"] != float64(8192) {
				t.Errorf("size_bytes = %v", got["size_bytes"])
			}
			if size := e.pools.files[vol["internal_name"].(string)]; size != 8192 {
				t.Errorf("the file is %d bytes", size)
			}
			e.consistent()
		})
	}
}

func TestDeleteThatFailsIsRetriedAtTheNextStart(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	e.pools.deleteErr = fmt.Errorf("disk trouble")
	e.do("DELETE", "/v1/volumes/v", nil, 500)
	e.do("GET", "/v1/volumes/v", nil, 404)
	if e.intents() != 1 {
		t.Fatalf("the failure should be remembered: %d intents", e.intents())
	}
	// Forgetting the pool would lose track of the file.
	if got := e.do("DELETE", "/v1/pools/p", nil, 409); !strings.Contains(fmt.Sprint(got["error"]), "couldn't be finished") {
		t.Errorf("error = %v", got["error"])
	}
	e.start() // It still fails, so it must still be remembered.
	if e.intents() != 1 {
		t.Errorf("%d intents", e.intents())
	}
	e.pools.deleteErr = nil
	e.start()
	e.consistent()
	e.do("DELETE", "/v1/pools/p", nil, 204)
}

func TestForgettingAPoolRetriesFirst(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	e.pools.deleteErr = fmt.Errorf("disk trouble")
	e.do("DELETE", "/v1/volumes/v", nil, 500)
	e.pools.deleteErr = nil
	e.do("DELETE", "/v1/pools/p", nil, 204)
	if len(e.pools.files) != 0 || e.intents() != 0 {
		t.Errorf("files %v, intents %d", e.pools.files, e.intents())
	}
}

func TestMissingPoolKeepsTheIntent(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.crash("storage", func() {
		e.pools.capErr = fmt.Errorf("the disk has gone")
		e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	})
	if e.intents() != 1 || len(e.pools.files) != 1 {
		t.Errorf("nothing can be known while the disk is missing: intents %d, files %v", e.intents(), e.pools.files)
	}
	e.pools.capErr = nil
	e.start()
	e.consistent()
	if len(e.pools.files) != 0 {
		t.Errorf("files = %v", e.pools.files)
	}
}

func TestFailedCreateLeavesNothing(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.pools.createErr = fmt.Errorf("disk trouble")
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 500)
	e.pools.createErr = nil
	e.consistent()
}

func TestDamagedIntentsAreIgnored(t *testing.T) {
	e := newEnv(t)
	e.pool()
	dir := filepath.Join(e.dir, "intents")
	outside := filepath.Join(e.dir, "precious")
	if err := os.WriteFile(outside, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"garbage.json":                    "not json",
		target.InternalPrefix + "aa.json": `{"op":"delete","pool":"p","internal_name":"../precious"}`,
		"../precious.json":                "",
	} {
		if strings.Contains(name, "/") {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.start()
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a damaged intent reached outside the pool: %v", err)
	}
}
