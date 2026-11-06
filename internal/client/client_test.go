package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, o Options, h http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	o.URL = srv.URL
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	}
	return c, &slept
}

func TestBadAddressesAreRefused(t *testing.T) {
	for _, u := range []string{"127.0.0.1:8420", "ftp://host", "https://", "://nope"} {
		if _, err := New(Options{URL: u}); err == nil {
			t.Errorf("%q was accepted", u)
		}
	}
	if _, err := New(Options{CAFile: "/no/such/file"}); err == nil {
		t.Error("a missing CA file was accepted")
	}
}

func TestTokenIsSent(t *testing.T) {
	var got string
	c, _ := testClient(t, Options{Token: "secret"}, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	})
	if _, err := c.ListPorts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer secret" {
		t.Errorf("got %q", got)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var followed atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	t.Cleanup(other.Close)
	c, _ := testClient(t, Options{Token: "secret"}, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	})
	if _, err := c.ListPorts(context.Background()); err == nil {
		t.Error("a redirect was treated as success")
	}
	if followed.Load() {
		t.Error("the redirect was followed, with the request")
	}
}

func TestErrorsCarryTheirStatusAndMessage(t *testing.T) {
	cases := []struct {
		status int
		check  func(error) bool
	}{
		{404, IsNotFound}, {409, IsConflict}, {422, IsInvalid}, {401, IsUnauthorized}, {507, IsOutOfSpace},
	}
	for _, tc := range cases {
		c, _ := testClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"what went wrong"}`))
		})
		err := c.DeletePort(context.Background(), 1)
		if !tc.check(err) || err.Error() != "what went wrong" {
			t.Errorf("%d: got %v", tc.status, err)
		}
		if tc.status != 404 && IsNotFound(err) {
			t.Errorf("%d was reported as not found", tc.status)
		}
	}
}

func TestAnErrorFromSomethingElseStillReads(t *testing.T) {
	c, _ := testClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("<html>" + strings.Repeat("x", 2*errorBodyLimit)))
	})
	err := c.DeletePort(context.Background(), 1)
	var e *Error
	if !errors.As(err, &e) || e.Status != 502 || len(e.Message) > errorBodyLimit {
		t.Errorf("got %#v", err)
	}
}

func TestBusyIsRetriedWithTheSameRequest(t *testing.T) {
	var attempts atomic.Int32
	var bodies []string
	c, slept := testClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 256)
		n, _ := r.Body.Read(b)
		bodies = append(bodies, string(b[:n]))
		if attempts.Add(1) < 3 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"name":"v"}`))
	})
	got, err := c.CreateVolume(context.Background(), VolumeCreate{Name: "v", SizeBytes: 4096})
	if err != nil || got.Name != "v" {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(*slept) != 2 || (*slept)[0] != 2*time.Second {
		t.Errorf("slept %v, want 2 s twice, as Retry-After said", *slept)
	}
	for _, b := range bodies {
		if !strings.Contains(b, `"size_bytes":4096`) {
			t.Errorf("a retry sent %q", b)
		}
	}
}

func TestBusyGivesUpEventually(t *testing.T) {
	var attempts atomic.Int32
	c, _ := testClient(t, Options{BusyRetry: 50 * time.Millisecond}, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"too much is in progress"}`))
	})
	err := c.DeleteVolume(context.Background(), "v")
	if !IsBusy(err) || !strings.Contains(err.Error(), "too much") {
		t.Errorf("got %v", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("made %d attempts, when the wait wouldn't fit in the time allowed", attempts.Load())
	}
}

func TestBusyRetryStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, _ := testClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(503)
	})
	if err := c.DeleteVolume(ctx, "v"); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
}

func TestOtherFailuresAreNotRetried(t *testing.T) {
	var attempts atomic.Int32
	c, _ := testClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(500)
	})
	if err := c.DeleteVolume(context.Background(), "v"); err == nil {
		t.Fatal("no error")
	}
	if attempts.Load() != 1 {
		t.Errorf("made %d attempts: the first may have done something", attempts.Load())
	}
}

func TestNamesAreEscapedInPaths(t *testing.T) {
	var got string
	c, _ := testClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
		w.WriteHeader(204)
	})
	if err := c.DeleteVolume(context.Background(), "a/b c"); err != nil {
		t.Fatal(err)
	}
	if got != "/v1/volumes/a%2Fb%20c" {
		t.Errorf("got %q", got)
	}
}

func TestListsFollowPages(t *testing.T) {
	var queries []string
	c, _ := testClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("after") == "" {
			_, _ = w.Write([]byte(`{"items":[{"name":"a"}],"next_after":"a"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"name":"b"}]}`))
	})
	got, err := c.ListVolumes(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Name != "b" {
		t.Errorf("got %v", got)
	}
	if len(queries) != 2 || !strings.Contains(queries[1], "after=a") || !strings.Contains(queries[1], "pool=p") {
		t.Errorf("asked with %v", queries)
	}
}

func TestUnhealthyIsAnAnswerAndIsntRetried(t *testing.T) {
	var attempts atomic.Int32
	c, slept := testClient(t, Options{}, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"status":"target unavailable"}`))
	})
	got, err := c.Health(context.Background())
	if err != nil || got.Status != "unavailable" {
		t.Errorf("got %+v, %v", got, err)
	}
	if attempts.Load() != 1 || len(*slept) != 0 {
		t.Errorf("made %d attempts and slept %v", attempts.Load(), *slept)
	}
}
