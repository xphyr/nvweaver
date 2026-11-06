package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nvweaver/internal/auth"
	"nvweaver/internal/target"
	"nvweaver/internal/target/targettest"
)

// Made by a real htpasswd: htpasswd -nbB -C 5 admin "correct horse".
const testUsers = "admin:$2y$05$S59rbe.HBNjlebyPt896r.18ioUcnHVsPZfgr3rL.nNQujixCO3by\n"

func sessionServer(t *testing.T, token string) *Server {
	t.Helper()
	m, err := NewManager(&target.Target{FS: targettest.New(), Root: "/nvmet"}, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "users")
	if err := os.WriteFile(path, []byte(testUsers), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(m, token)
	s.Auth = a
	return s
}

const goodLogin = `{"username":"admin","password":"correct horse"}`

// onAddress makes a request as if it had arrived on this address, as a real connection does.
func onAddress(ip string, method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:8420"
	return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(ip), Port: 8420}))
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func sessionCookieOf(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no session cookie: status %d, %s", rec.Code, rec.Body)
	return nil
}

func TestSigningInAndOut(t *testing.T) {
	h := sessionServer(t, "").Handler()
	api := func(cookie *http.Cookie) int {
		r := onAddress("127.0.0.1", http.MethodGet, "/v1/system", "")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		return do(h, r).Code
	}
	if code := api(nil); code != http.StatusUnauthorized {
		t.Errorf("with no session: status %d", code)
	}
	rec := do(h, onAddress("127.0.0.1", http.MethodPost, "/v1/session", goodLogin))
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in: status %d: %s", rec.Code, rec.Body)
	}
	c := sessionCookieOf(t, rec)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge <= 0 {
		t.Errorf("cookie %+v", c)
	}
	if c.Secure {
		t.Error("over plain HTTP to loopback, a Secure cookie would never be sent back")
	}
	if code := api(c); code != http.StatusOK {
		t.Errorf("with a session: status %d", code)
	}
	if strings.Contains(rec.Body.String(), c.Value) {
		t.Error("the session ID is readable by the page")
	}
	r := onAddress("127.0.0.1", http.MethodGet, "/v1/session", "")
	r.AddCookie(c)
	var view sessionView
	got := do(h, r)
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view != (sessionView{Method: "password", SignedIn: true, Username: "admin"}) {
		t.Errorf("session %+v", view)
	}
	if got.Header().Get("Cache-Control") != "no-store" {
		t.Error("who's signed in shouldn't be cached")
	}
	r = onAddress("127.0.0.1", http.MethodDelete, "/v1/session", "")
	r.AddCookie(c)
	rec = do(h, r)
	if rec.Code != http.StatusNoContent || sessionCookieOf(t, rec).MaxAge >= 0 {
		t.Errorf("sign out: status %d", rec.Code)
	}
	if code := api(c); code != http.StatusUnauthorized {
		t.Errorf("the same cookie, after signing out: status %d", code)
	}
}

func TestWhatASignedOutRequestCanLearn(t *testing.T) {
	h := sessionServer(t, "").Handler()
	var view sessionView
	if err := json.Unmarshal(do(h, onAddress("127.0.0.1", http.MethodGet, "/v1/session", "")).Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view != (sessionView{Method: "password"}) {
		t.Errorf("session %+v", view)
	}
	wrong := do(h, onAddress("127.0.0.1", http.MethodPost, "/v1/session", `{"username":"admin","password":"wrong"}`))
	unknown := do(h, onAddress("127.0.0.1", http.MethodPost, "/v1/session", `{"username":"nobody","password":"wrong"}`))
	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized || wrong.Body.String() != unknown.Body.String() {
		t.Errorf("these should look the same: %d %s, and %d %s", wrong.Code, wrong.Body, unknown.Code, unknown.Body)
	}
	if len(wrong.Result().Cookies()) != 0 {
		t.Error("a failure set a cookie")
	}
}

func TestSessionMethodSaysWhatsNeeded(t *testing.T) {
	m, err := NewManager(&target.Target{FS: targettest.New(), Root: "/nvmet"}, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		token, want string
		signedIn    bool
	}{{"", "none", true}, {"secret", "token", false}}
	for _, c := range cases {
		var view sessionView
		if err := json.Unmarshal(do(NewServer(m, c.token).Handler(), onAddress("127.0.0.1", http.MethodGet, "/v1/session", "")).Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.Method != c.want || view.SignedIn != c.signedIn {
			t.Errorf("token %q: %+v", c.token, view)
		}
	}
	rec := do(NewServer(m, "").Handler(), onAddress("127.0.0.1", http.MethodPost, "/v1/session", goodLogin))
	if rec.Code != http.StatusNotFound {
		t.Errorf("signing in with no users file: status %d", rec.Code)
	}
}

func TestPasswordsNeedAProtectedConnection(t *testing.T) {
	const network = "192.0.2.10"
	cases := []struct {
		name  string
		ip    string
		tls   bool
		allow bool
		host  string
		want  int
	}{
		{"loopback", "127.0.0.1", false, false, "", http.StatusOK},
		{"IPv6 loopback", "::1", false, false, "", http.StatusOK},
		{"over the network", network, false, false, "", http.StatusForbidden},
		// The Host header is the sender's own to set, so it can't count.
		{"over the network, claiming to be loopback", network, false, false, "127.0.0.1:8420", http.StatusForbidden},
		{"over the network, with TLS", network, true, false, "", http.StatusOK},
		{"over the network, allowed by the admin", network, false, true, "", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := sessionServer(t, "")
			s.AllowInsecureLogin = c.allow
			r := onAddress(c.ip, http.MethodPost, "/v1/session", goodLogin)
			if c.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if c.host != "" {
				r.Host = c.host
			}
			rec := do(s.Handler(), r)
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
			if c.want == http.StatusForbidden {
				if !strings.Contains(rec.Body.String(), "NVWEAVER_ALLOW_INSECURE_LOGIN") {
					t.Errorf("it should say what to do: %s", rec.Body)
				}
				if len(rec.Result().Cookies()) != 0 {
					t.Error("a refusal set a cookie")
				}
			} else if got := sessionCookieOf(t, rec); got.Secure != c.tls {
				t.Errorf("Secure is %v, with TLS %v", got.Secure, c.tls)
			}
		})
	}
}

func TestAConnectionWithNoKnownAddressIsntTrusted(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/session", strings.NewReader(goodLogin))
	if connectionIsProtected(r) {
		t.Error("it should need to know")
	}
}

func TestTooManyGuessesSaysHowLongToWait(t *testing.T) {
	h := sessionServer(t, "").Handler()
	var rec *httptest.ResponseRecorder
	for range 11 {
		rec = do(h, onAddress("127.0.0.1", http.MethodPost, "/v1/session", `{"username":"admin","password":"guess"}`))
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d", rec.Code)
	}
	if after := rec.Header().Get("Retry-After"); after == "" || after == "0" {
		t.Errorf("Retry-After %q", after)
	}
}

func TestTheTokenStillWorksForPrograms(t *testing.T) {
	h := sessionServer(t, "secret").Handler()
	r := onAddress("127.0.0.1", http.MethodGet, "/v1/system", "")
	r.Header.Set("Authorization", "Bearer secret")
	if rec := do(h, r); rec.Code != http.StatusOK {
		t.Errorf("status %d", rec.Code)
	}
	r = onAddress("127.0.0.1", http.MethodGet, "/v1/system", "")
	r.Header.Set("Authorization", "Bearer wrong")
	if rec := do(h, r); rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong token: status %d", rec.Code)
	}
	r = onAddress("127.0.0.1", http.MethodGet, "/v1/system", "")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "made-up"})
	if rec := do(h, r); rec.Code != http.StatusUnauthorized {
		t.Errorf("a made-up session: status %d", rec.Code)
	}
}

func TestWithUsersAndNoTokenAnyNameWorks(t *testing.T) {
	h := sessionServer(t, "").Handler()
	r := onAddress("192.0.2.10", http.MethodGet, "/ui/", "")
	r.Host = "storage.example:8420"
	if rec := do(h, r); rec.Code != http.StatusOK {
		t.Errorf("status %d: it can be on a network with only a users file", rec.Code)
	}
	r = onAddress("192.0.2.10", http.MethodGet, "/v1/system", "")
	r.Host = "storage.example:8420"
	if rec := do(h, r); rec.Code != http.StatusUnauthorized {
		t.Errorf("status %d: the API still needs signing in", rec.Code)
	}
}

func TestAWebPageCantSignInOrUseASession(t *testing.T) {
	s := sessionServer(t, "")
	h := s.Handler()
	c := sessionCookieOf(t, do(h, onAddress("127.0.0.1", http.MethodPost, "/v1/session", goodLogin)))
	r := onAddress("127.0.0.1", http.MethodPost, "/v1/session", goodLogin)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := do(h, r); rec.Code != http.StatusForbidden {
		t.Errorf("signing in from another site: status %d", rec.Code)
	}
	r = onAddress("127.0.0.1", http.MethodPost, "/v1/hosts", `{"nqn":"nqn.2026-11.net.example:evil"}`)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.AddCookie(c)
	if rec := do(h, r); rec.Code != http.StatusForbidden {
		t.Errorf("using a session from another site: status %d", rec.Code)
	}
}
