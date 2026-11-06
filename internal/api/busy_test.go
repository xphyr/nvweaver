package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"nvweaver/internal/target"
)

// send is do for goroutines, and for looking at headers. It can't stop the test itself.
func (e *env) send(method, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	e.http.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader(b)))
	return rec
}

// wantBusy checks for the answer that tells a client to try again.
func (e *env) wantBusy(method, path string, body any) {
	e.t.Helper()
	rec := e.send(method, path, body)
	if rec.Code != http.StatusServiceUnavailable {
		e.t.Errorf("%s %s = %d, want 503: %s", method, path, rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != retryAfterSeconds {
		e.t.Errorf("%s %s: Retry-After = %q", method, path, got)
	}
}

// within fails rather than hanging if something is still waiting for the lock.
func (e *env) within(what string, fn func()) {
	e.t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		e.t.Fatalf("%s is stuck behind work in progress", what)
	}
}

// holdStorage makes storage work wait, and returns how to let it go.
func (e *env) holdStorage(capacity int) func() {
	e.pools.gate = make(chan struct{})
	e.pools.entered = make(chan string, capacity)
	var once sync.Once
	return func() {
		once.Do(func() { close(e.pools.gate) })
	}
}

func TestSlowStorageWorkHoldsUpNothingElse(t *testing.T) {
	e := newEnv(t)
	e.pool()
	src := e.do("POST", "/v1/volumes", obj{"name": "src", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/volumes", obj{"name": "other", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/snapshots", obj{"name": "snap", "volume": "other"}, 201)

	release := e.holdStorage(8)
	defer release()
	cloned := make(chan *httptest.ResponseRecorder, 1)
	clone := obj{"name": "c", "source": obj{"volume": "src"}}
	go func() { cloned <- e.send("POST", "/v1/volumes", clone) }()
	internal := <-e.pools.entered

	e.within("everything else", func() {
		e.do("GET", "/v1/volumes/src", nil, 200)
		e.do("GET", "/v1/volumes", nil, 200)

		// The same request, retried while it's still running: not created twice, and not a conflict.
		e.wantBusy("POST", "/v1/volumes", clone)
		// What the clone depends on can't change underneath it.
		e.wantBusy("DELETE", "/v1/volumes/src", nil)
		e.wantBusy("PATCH", "/v1/volumes/src", obj{"size_bytes": 8192})
		e.wantBusy("DELETE", "/v1/pools/p", nil)
		e.do("PATCH", "/v1/volumes/src", obj{"labels": obj{"a": "b"}}, 200)

		// Changes that don't touch it carry on, including ones on the same pool.
		e.do("POST", "/v1/hosts", obj{"nqn": hostNQN}, 201)
		e.do("PATCH", "/v1/volumes/other", obj{"size_bytes": 8192}, 200)
		e.do("DELETE", "/v1/snapshots/snap", nil, 204)
		e.do("DELETE", "/v1/volumes/other", nil, 204)
	})

	// Asking to forget the pool must not take the work in progress for an interrupted change.
	if !intentExists(t, e, internal) {
		t.Errorf("the intent for the clone in progress has gone")
	}

	// Its storage exists without a volume yet, and is no stray. A real one still shows up.
	e.pools.mu.Lock()
	e.pools.files[internal] = 4096
	e.pools.files[target.InternalPrefix+"stray"] = 1
	e.pools.mu.Unlock()
	warnings := fmt.Sprint(e.do("GET", "/v1/pools/p", nil, 200)["warnings"])
	if strings.Contains(warnings, internal) {
		t.Errorf("work in progress reported as a stray: %s", warnings)
	}
	if !strings.Contains(warnings, "stray") {
		t.Errorf("a real stray should still be reported: %s", warnings)
	}
	e.pools.mu.Lock()
	delete(e.pools.files, internal)
	delete(e.pools.files, target.InternalPrefix+"stray")
	e.pools.mu.Unlock()

	release()
	if rec := <-cloned; rec.Code != 201 {
		t.Fatalf("clone = %d: %s", rec.Code, rec.Body)
	}
	e.pools.gate = nil

	// A client that acts on the answer straight away mustn't meet its own request.
	got := e.do("GET", "/v1/volumes/c", nil, 200)
	if got["uuid"] == src["uuid"] || got["nguid"] == src["nguid"] {
		t.Errorf("a clone needs new identifiers: %v", got)
	}
	e.do("POST", "/v1/volumes", clone, 200)
	e.do("DELETE", "/v1/volumes/c", nil, 204)
	e.do("DELETE", "/v1/volumes/src", nil, 204)
	e.do("DELETE", "/v1/pools/p", nil, 204)
	if intents, err := e.m.readIntents(); err != nil || len(intents) != 0 {
		t.Errorf("intents left: %v, %v", intents, err)
	}
}

func intentExists(t *testing.T, e *env, internal string) bool {
	t.Helper()
	intents, err := e.m.readIntents()
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(intents, func(in intent) bool { return in.Internal == internal })
}

func TestAFailedCloneReleasesWhatItHeld(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "src", "size_bytes": 4096}, 201)
	e.pools.createErr = fmt.Errorf("it broke")
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"volume": "src"}}, 500)
	e.pools.createErr = nil
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"volume": "src"}}, 201)
	e.do("DELETE", "/v1/volumes/c", nil, 204)
	e.do("DELETE", "/v1/volumes/src", nil, 204)
	e.do("DELETE", "/v1/pools/p", nil, 204)
}

func TestWorkOnAPoolIsLimited(t *testing.T) {
	e := newEnv(t)
	e.pool()
	release := e.holdStorage(maxPoolOperations + 1)
	defer release()
	var wg sync.WaitGroup
	codes := make([]int, maxPoolOperations)
	for i := range maxPoolOperations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = e.send("POST", "/v1/volumes", obj{"name": fmt.Sprintf("v%d", i), "size_bytes": 4096}).Code
		}()
	}
	for range maxPoolOperations {
		<-e.pools.entered
	}
	e.wantBusy("POST", "/v1/volumes", obj{"name": "one-too-many", "size_bytes": 4096})
	release()
	wg.Wait()
	for i, code := range codes {
		if code != 201 {
			t.Errorf("v%d = %d", i, code)
		}
	}
	e.do("GET", "/v1/volumes/one-too-many", nil, 404)
	e.do("POST", "/v1/volumes", obj{"name": "one-too-many", "size_bytes": 4096}, 201)
}

// stuckBackend holds a change where a slow target would.
type stuckBackend struct {
	target.Backend
	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
}

func (b *stuckBackend) Reconcile(prev, next *target.State) error {
	b.mu.Lock()
	gate := b.gate
	b.mu.Unlock()
	if gate != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		<-gate
	}
	return b.Backend.Reconcile(prev, next)
}

func TestReadsDontWaitForAChangeAndTheQueueIsBounded(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/v1/hosts", obj{"nqn": hostNQN}, 201)
	stuck := &stuckBackend{Backend: e.m.backend, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	e.m.backend = stuck

	var wg sync.WaitGroup
	first := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		first <- e.send("POST", "/v1/hosts", obj{"nqn": "nqn.2026-11.net.example:first"}).Code
	}()
	<-stuck.entered

	e.within("a read", func() {
		e.do("GET", "/v1/hosts/"+hostNQN, nil, 200)
		// A change that hasn't happened isn't visible yet.
		e.do("GET", "/v1/hosts/nqn.2026-11.net.example:first", nil, 404)
		e.do("GET", "/v1/system", nil, 200)
	})

	// Fill the queue for real, rather than setting the count.
	const total = maxWaitingChanges + 50
	nqn := func(i int) string { return fmt.Sprintf("nqn.2026-11.net.example:h%d", i) }
	codes := make([]int, total)
	for i := range maxWaitingChanges {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = e.send("POST", "/v1/hosts", obj{"nqn": nqn(i)}).Code
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.m.waiting.Load() < maxWaitingChanges {
		if time.Now().After(deadline) {
			t.Fatalf("only %d changes queued", e.m.waiting.Load())
		}
		time.Sleep(time.Millisecond)
	}
	for i := maxWaitingChanges; i < total; i++ {
		rec := e.send("POST", "/v1/hosts", obj{"nqn": nqn(i)})
		codes[i] = rec.Code
		if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
			t.Errorf("past the limit: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
		}
	}

	stuck.mu.Lock()
	close(stuck.gate)
	stuck.gate = nil
	stuck.mu.Unlock()
	wg.Wait()
	if code := <-first; code != 201 {
		t.Errorf("first = %d", code)
	}
	// A 503 must mean that nothing happened, and anything else that it did.
	for i, code := range codes {
		want := 404
		if code == 201 {
			want = 200
		} else if code != 503 {
			t.Errorf("%d: %d", i, code)
		}
		e.do("GET", "/v1/hosts/"+nqn(i), nil, want)
	}
	if e.m.waiting.Load() != 0 {
		t.Errorf("waiting = %d", e.m.waiting.Load())
	}
}

// TestManyChangesAtOnce is mainly for the race detector. Retrying on a 503 as a client should, every request
// has to end up done exactly once, and the pool must hold just what the state refers to.
func TestManyChangesAtOnce(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "src", "size_bytes": 4096}, 201)
	retry := func(method, path string, body any, want ...int) error {
		for range 2000 {
			rec := e.send(method, path, body)
			if rec.Code == 503 {
				time.Sleep(time.Millisecond)
				continue
			}
			if !slices.Contains(want, rec.Code) {
				return fmt.Errorf("%s %s = %d: %s", method, path, rec.Code, rec.Body)
			}
			return nil
		}
		return fmt.Errorf("%s %s stayed busy", method, path)
	}
	const workers = 24
	errs := make(chan error, workers*8)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, c, s := fmt.Sprintf("v%d", i), fmt.Sprintf("c%d", i), fmt.Sprintf("s%d", i)
			for _, step := range []func() error{
				func() error { return retry("POST", "/v1/volumes", obj{"name": v, "size_bytes": 4096}, 201) },
				func() error { return retry("POST", "/v1/snapshots", obj{"name": s, "volume": "src"}, 201) },
				func() error {
					return retry("POST", "/v1/volumes", obj{"name": c, "source": obj{"snapshot": s}}, 201)
				},
				func() error { return retry("PATCH", "/v1/volumes/"+v, obj{"size_bytes": 8192}, 200) },
				func() error { return retry("GET", "/v1/pools/p", nil, 200) },
				func() error { return retry("GET", "/v1/volumes", nil, 200) },
				func() error { return retry("DELETE", "/v1/snapshots/"+s, nil, 204) },
				func() error {
					if i%2 == 0 {
						return retry("DELETE", "/v1/volumes/"+c, nil, 204)
					}
					return nil
				},
			} {
				if err := step(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	e.consistent()
	want := 1 + workers + workers/2
	if got := len(e.m.current().Volumes); got != want {
		t.Errorf("volumes = %d, want %d", got, want)
	}
	if got := fmt.Sprint(e.do("GET", "/v1/pools/p", nil, 200)["warnings"]); strings.Contains(got, "no volume refers") {
		t.Errorf("strays: %s", got)
	}
}
