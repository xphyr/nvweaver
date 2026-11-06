package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Made by a real htpasswd, as an admin would: htpasswd -nbB -C 5 admin "correct horse". It's a low cost, so the tests are quick.
const realHtpasswd = "admin:$2y$05$S59rbe.HBNjlebyPt896r.18ioUcnHVsPZfgr3rL.nNQujixCO3by\n"

type fixture struct {
	*Authenticator
	path  string
	clock time.Time
}

func newFixture(t *testing.T, content string) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{Authenticator: a, path: path, clock: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	a.now = func() time.Time { return f.clock }
	return f
}

// replace swaps the file with a rename, as an editor or tool does.
func (f *fixture) replace(t *testing.T, content string, mode os.FileMode) {
	t.Helper()
	tmp := f.path + ".new"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		t.Fatal(err)
	}
}

func TestARealHtpasswdHashWorks(t *testing.T) {
	f := newFixture(t, realHtpasswd)
	id, expires, err := f.Login("c", "admin", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if len(id) < 40 || !expires.After(f.clock) {
		t.Errorf("session %q, expiring %v", id, expires)
	}
	if user, ok := f.Check(id); !ok || user != "admin" {
		t.Errorf("Check: %q, %v", user, ok)
	}
}

func TestWhatGoesInAUsersFileWorks(t *testing.T) {
	hash, err := Hash("a good password")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, "ops:"+hash+"\n")
	if _, _, err := f.Login("c", "ops", "a good password"); err != nil {
		t.Error(err)
	}
	if _, err := Hash(""); err == nil {
		t.Error("an empty password was accepted")
	}
	if _, err := Hash(strings.Repeat("x", MaxPasswordBytes+1)); err == nil {
		t.Error("a password past bcrypt's limit was accepted, which would quietly ignore the rest")
	}
}

func TestWrongCredentialsAllLookTheSame(t *testing.T) {
	f := newFixture(t, realHtpasswd)
	cases := map[string][2]string{
		"wrong password":     {"admin", "wrong"},
		"unknown user":       {"nobody", "correct horse"},
		"empty":              {"", ""},
		"too long":           {"admin", strings.Repeat("x", 100)},
		"correct but longer": {"admin", "correct horse" + strings.Repeat("x", 100)},
	}
	for name, c := range cases {
		if _, _, err := f.Login(name, c[0], c[1]); !errors.Is(err, ErrWrongCredentials) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// With no check against a real hash, an unknown name would answer far faster, which would reveal who exists.
func TestAnUnknownUserTakesAsLongAsAKnownOne(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), 8)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, "admin:"+string(hash)+"\n")
	// The decoy has to cost the same, which is the part that can be checked exactly.
	if got, err := bcrypt.Cost(f.dummy); err != nil || got != 8 {
		t.Errorf("the decoy's cost is %d, %v, not the file's 8", got, err)
	}
	// And it has to really be checked. A busy machine only makes things slower, so the fastest of several is the fair measure.
	fastest := func(user string) time.Duration {
		best := time.Hour
		for i := range 5 {
			start := time.Now()
			_, _, _ = f.Login(fmt.Sprintf("timing-%s-%d", user, i), user, "wrong")
			best = min(best, time.Since(start))
		}
		return best
	}
	known, unknown := fastest("admin"), fastest("nobody")
	if unknown < known/2 {
		t.Errorf("an unknown user took %v, and a known one %v", unknown, known)
	}
}

func TestBadFilesAreRefused(t *testing.T) {
	good := "$2y$05$S59rbe.HBNjlebyPt896r.18ioUcnHVsPZfgr3rL.nNQujixCO3by"
	cases := map[string]string{
		"empty":                "",
		"only comments":        "# nobody\n",
		"no hash":              "admin\n",
		"a shadow-style hash":  "admin:$6$salt$hash\n",
		"yescrypt":             "admin:$y$j9T$salt$hash\n",
		"a plain password":     "admin:hunter2\n",
		"a broken bcrypt hash": "admin:$2y$05$short\n",
		"a bad name":           "ad min:" + good + "\n",
		"twice":                "a:" + good + "\na:" + good + "\n",
	}
	for name, content := range cases {
		if _, err := Parse([]byte(content)); err == nil {
			t.Errorf("%s was accepted", name)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error repeats a secret: %v", name, err)
		}
	}
	users, err := Parse([]byte("# a comment\n\n  admin:" + good + "  \n"))
	if err != nil || len(users) != 1 {
		t.Errorf("comments and spaces: %v, %v", users, err)
	}
}

func TestAFileOthersCanReadStopsItStarting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users")
	if err := os.WriteFile(path, []byte(realHtpasswd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(path, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("got %v", err)
	}
	if _, err := New("/no/such/file", slog.New(slog.DiscardHandler)); err == nil {
		t.Error("a missing file was accepted")
	}
	if a, err := New("", nil); a != nil || err != nil {
		t.Errorf("with no path, got %v, %v", a, err)
	}
}

func TestChangesApplyWithoutARestart(t *testing.T) {
	f := newFixture(t, realHtpasswd)
	id, _, err := f.Login("c", "admin", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	cheap, err := bcrypt.GenerateFromPassword([]byte("new password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	hash := string(cheap)
	f.replace(t, realHtpasswd+"ops:"+hash+"\n", 0o600)
	if _, _, err := f.Login("c", "ops", "new password"); err != nil {
		t.Errorf("a new user: %v", err)
	}
	if _, ok := f.Check(id); !ok {
		t.Error("someone still in the file was signed out")
	}
	// A file that can't be used keeps things as they were, as a mistake shouldn't lock everyone out.
	f.replace(t, "not a users file\n", 0o600)
	if _, _, err := f.Login("c", "ops", "new password"); err != nil {
		t.Errorf("after a bad change: %v", err)
	}
	f.replace(t, realHtpasswd+"ops:"+hash+"\n", 0o644)
	if _, _, err := f.Login("c", "ops", "new password"); err != nil {
		t.Errorf("a file that others can read should keep the current users: %v", err)
	}
	// Removing someone ends their sessions.
	opsID, _, err := f.Login("c", "ops", "new password")
	if err != nil {
		t.Fatal(err)
	}
	f.replace(t, realHtpasswd, 0o600)
	if _, ok := f.Check(opsID); ok {
		t.Error("someone removed from the file is still signed in")
	}
	if _, ok := f.Check(id); !ok {
		t.Error("someone else was signed out")
	}
	if _, _, err := f.Login("c", "ops", "new password"); !errors.Is(err, ErrWrongCredentials) {
		t.Errorf("someone removed can still sign in: %v", err)
	}
}

func TestSessionsEnd(t *testing.T) {
	f := newFixture(t, realHtpasswd)
	login := func() string {
		id, _, err := f.Login("c", "admin", "correct horse")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := login()
	f.Logout(id)
	if _, ok := f.Check(id); ok {
		t.Error("still signed in after signing out")
	}
	id = login()
	f.clock = f.clock.Add(IdleTimeout - time.Minute)
	if _, ok := f.Check(id); !ok {
		t.Error("ended before its idle timeout")
	}
	f.clock = f.clock.Add(IdleTimeout - time.Minute)
	if _, ok := f.Check(id); !ok {
		t.Error("using it should keep it going")
	}
	f.clock = f.clock.Add(IdleTimeout)
	if _, ok := f.Check(id); ok {
		t.Error("still good after being idle")
	}
	id = login()
	for range 4 {
		f.clock = f.clock.Add(SessionLifetime / 4)
		f.Check(id)
	}
	if _, ok := f.Check(id); ok {
		t.Error("still good past its lifetime, however much it's used")
	}
	if _, ok := f.Check(""); ok {
		t.Error("no session was accepted")
	}
	if _, ok := f.Check("made-up"); ok {
		t.Error("a made-up session was accepted")
	}
	a, b := login(), login()
	if a == b {
		t.Error("two sessions have the same ID")
	}
}

func TestSessionsDontGrowWithoutLimit(t *testing.T) {
	f := newFixture(t, realHtpasswd)
	for i := range maxSessions {
		f.sessions[fmt.Sprintf("old-%d", i)] = &session{user: "admin", began: f.clock, lastSeen: f.clock}
	}
	if _, _, err := f.Login("c", "admin", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if len(f.sessions) > maxSessions {
		t.Errorf("%d sessions", len(f.sessions))
	}
}

func TestGuessingIsLimitedPerClientAndCantLockOutAnAdmin(t *testing.T) {
	f := newFixture(t, realHtpasswd)
	for range maxFailures {
		if _, _, err := f.Login("attacker", "admin", "guess"); !errors.Is(err, ErrWrongCredentials) {
			t.Fatalf("got %v", err)
		}
	}
	_, _, err := f.Login("attacker", "admin", "correct horse")
	var many *TooManyError
	if !errors.As(err, &many) || many.RetryAfter <= 0 {
		t.Fatalf("the right password, once limited: got %v", err)
	}
	if _, _, err := f.Login("the admin", "admin", "correct horse"); err != nil {
		t.Errorf("someone else got locked out: %v", err)
	}
	f.clock = f.clock.Add(failureWindow)
	if _, _, err := f.Login("attacker", "admin", "correct horse"); err != nil {
		t.Errorf("after waiting: %v", err)
	}
	for range maxFailures - 1 {
		_, _, _ = f.Login("forgetful", "admin", "guess")
	}
	if _, _, err := f.Login("forgetful", "admin", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Login("forgetful", "admin", "guess"); !errors.Is(err, ErrWrongCredentials) {
		t.Errorf("success should clear the count, but got %v", err)
	}
}

func TestTooManyAtOnceAreTurnedAway(t *testing.T) {
	f := newFixture(t, realHtpasswd)
	for range maxConcurrentChecks {
		f.checks <- struct{}{}
	}
	_, _, err := f.Login("c", "admin", "correct horse")
	var many *TooManyError
	if !errors.As(err, &many) {
		t.Errorf("got %v", err)
	}
	for range maxConcurrentChecks {
		<-f.checks
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Login("c", "admin", "correct horse")
		}()
	}
	wg.Wait()
	if len(f.checks) != 0 {
		t.Errorf("%d checks were never released", len(f.checks))
	}
}

func TestClientIsTheAddressAlone(t *testing.T) {
	if got := ClientOf("192.0.2.1:5555"); got != "192.0.2.1" {
		t.Errorf("got %q", got)
	}
	if got := ClientOf("[2001:db8::1]:5555"); got != "2001:db8::1" {
		t.Errorf("got %q", got)
	}
}
