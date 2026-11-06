package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nvweaver/internal/target"
	"nvweaver/internal/target/targettest"
)

// A web page open in a browser on the machine can send requests to the API on loopback. With no token, nothing else stops them,
// and the API can export any unused disk. These are the requests a browser would really send.

func browserServer(t *testing.T, token string) http.Handler {
	t.Helper()
	m, err := NewManager(&target.Target{FS: targettest.New(), Root: "/nvmet"}, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(m, token).Handler()
}

const hostBody = `{"nqn":"nqn.2026-11.net.example:attacker"}`

func hostExists(t *testing.T, h http.Handler, host string) bool {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/hosts/nqn.2026-11.net.example:attacker", nil)
	r.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code == http.StatusOK
}

func TestAWebPageCantChangeAnything(t *testing.T) {
	const local = "127.0.0.1:8420"
	cases := []struct {
		name    string
		headers map[string]string
	}{
		// A form or fetch with a type that needs no permission from the server, as this body doesn't.
		{"from another site, on a current browser", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example", "Content-Type": "text/plain"}},
		{"from another port on this machine", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://127.0.0.1:9999", "Content-Type": "text/plain"}},
		{"on an old browser, with only Origin", map[string]string{"Origin": "https://evil.example", "Content-Type": "text/plain"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := browserServer(t, "")
			r := httptest.NewRequest(http.MethodPost, "/v1/hosts", strings.NewReader(hostBody))
			r.Host = local
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status %d, want 403", rec.Code)
			}
			if hostExists(t, h, local) {
				t.Error("the request took effect")
			}
		})
	}
}

// DNS rebinding: a site points its own name at 127.0.0.1, which makes it the same origin as far as the browser can tell.
// It can then read as well as write, but it can't change the name in the Host header.
func TestDNSRebindingIsRefused(t *testing.T) {
	h := browserServer(t, "")
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		r := httptest.NewRequest(method, "/v1/system", nil)
		r.Host = "evil.example:8420"
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", method, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "backend") {
			t.Errorf("%s: it answered with data: %s", method, rec.Body)
		}
	}
}

func TestLegitimateRequestsStillWork(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		headers map[string]string
	}{
		{"curl or the command line, which send neither header", "127.0.0.1:8420", nil},
		{"by name", "localhost:8420", nil},
		{"on IPv6", "[::1]:8420", nil},
		{"with no port", "localhost", nil},
		{"another loopback address", "127.0.0.2:8420", nil},
		{"the interface, from itself", "127.0.0.1:8420", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:8420"}},
		{"a tab opened directly", "localhost:8420", map[string]string{"Sec-Fetch-Site": "none"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := browserServer(t, "")
			r := httptest.NewRequest(http.MethodPost, "/v1/hosts", strings.NewReader(hostBody))
			r.Host = c.host
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusCreated {
				t.Errorf("status %d, want 201: %s", rec.Code, rec.Body)
			}
		})
	}
}

// With a token, the API can be on any address, so its name can be anything, and the token is what protects it.
func TestWithATokenAnyNameWorksAndCrossSiteStillDoesnt(t *testing.T) {
	h := browserServer(t, "secret")
	r := httptest.NewRequest(http.MethodPost, "/v1/hosts", strings.NewReader(hostBody))
	r.Host = "storage.example:8420"
	r.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Errorf("by name, with the token: status %d: %s", rec.Code, rec.Body)
	}
	r = httptest.NewRequest(http.MethodPost, "/v1/hosts", strings.NewReader(hostBody))
	r.Host = "storage.example:8420"
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site, even with the token: status %d", rec.Code)
	}
}
