// Package auth signs people in to the web interface with a username and password, from a file that an admin keeps. Programs use the API's
// token instead, as they can't type a password. Everyone who can sign in can do everything, for now.
package auth

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	// MaxPasswordBytes is as much of a password as bcrypt looks at. Longer ones are refused, rather than quietly cut short.
	MaxPasswordBytes = 72
	// IdleTimeout ends a session that nothing has used for this long.
	IdleTimeout = 8 * time.Hour
	// SessionLifetime ends a session this long after it began, however much it's used.
	SessionLifetime = 24 * time.Hour

	maxSessions = 10000
	// One client gets this many failures in a window. It isn't per username, so that someone guessing can't lock out an admin.
	maxFailures   = 10
	failureWindow = 5 * time.Minute
	maxClients    = 10000
	// bcrypt is slow on purpose, so too many at once would be a way to tie up the machine.
	maxConcurrentChecks = 4
)

// ErrWrongCredentials says nothing about which part was wrong, so that it doesn't reveal which usernames exist.
var ErrWrongCredentials = errors.New("wrong username or password")

// TooManyError means this client should wait, either from too many failures or too many logins at once.
type TooManyError struct{ RetryAfter time.Duration }

func (e *TooManyError) Error() string { return "too many attempts: try again shortly" }

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// Parse reads a users file, of "name:hash" lines, as htpasswd -B writes them. Blank lines and # comments are ignored. Anything
// that it can't use is an error, as silently skipping a line would look like a user with a forgotten password.
func Parse(data []byte) (map[string][]byte, error) {
	users := map[string][]byte{}
	lines := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; lines.Scan(); n++ {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, hash, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: should be name:hash", n)
		}
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("line %d: %q isn't a usable name", n, name)
		}
		if _, dup := users[name]; dup {
			return nil, fmt.Errorf("line %d: %s is there twice", n, name)
		}
		// Only name it by its line and user, as the hash is worth keeping out of logs.
		if !strings.HasPrefix(hash, "$2") {
			return nil, fmt.Errorf("line %d: the hash for %s isn't bcrypt: make one with \"nvweaver passwd\" or \"htpasswd -B\"", n, name)
		}
		if _, err := bcrypt.Cost([]byte(hash)); err != nil {
			return nil, fmt.Errorf("line %d: the hash for %s isn't usable: %w", n, name, err)
		}
		users[name] = []byte(hash)
	}
	if err := lines.Err(); err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, errors.New("it has no users")
	}
	return users, nil
}

// Hash makes what goes in a users file for a password.
func Hash(password string) (string, error) {
	if password == "" {
		return "", errors.New("the password is empty")
	}
	if len(password) > MaxPasswordBytes {
		return "", fmt.Errorf("the password is longer than bcrypt's limit of %d bytes", MaxPasswordBytes)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(hash), err
}

type session struct {
	user            string
	began, lastSeen time.Time
}

type failures struct {
	count int
	since time.Time
}

// Authenticator holds the users, and who's signed in. Sessions are only in memory, so a restart signs everyone out, and hosts and data aren't affected.
type Authenticator struct {
	path string
	log  *slog.Logger
	now  func() time.Time

	mu       sync.Mutex
	users    map[string][]byte
	stamp    string
	failed   string
	dummy    []byte
	sessions map[string]*session
	clients  map[string]*failures

	checks chan struct{}
}

// New reads the users file, or returns nil if no path is given. A file that others can read, or that can't be used, is an error, before the target starts.
func New(path string, log *slog.Logger) (*Authenticator, error) {
	if path == "" {
		return nil, nil
	}
	a := &Authenticator{
		path: path, log: log, now: time.Now,
		sessions: map[string]*session{}, clients: map[string]*failures{},
		checks: make(chan struct{}, maxConcurrentChecks),
	}
	stamp, err := fileStamp(path)
	if err != nil {
		return nil, fmt.Errorf("users file: %w", err)
	}
	if err := a.load(stamp); err != nil {
		return nil, fmt.Errorf("users file %s: %w", path, err)
	}
	return a, nil
}

func fileStamp(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%o", info.Size(), info.ModTime().UnixNano(), info.Mode().Perm()), nil
}

func (a *Authenticator) load(stamp string) error {
	info, err := os.Stat(a.path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("others can read it (mode %04o): run chmod 600 on it", perm)
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		return err
	}
	users, err := Parse(data)
	if err != nil {
		return err
	}
	// Checking an unknown name against this takes as long as checking a real one, so timing doesn't reveal who exists.
	cost := bcrypt.MinCost
	for _, hash := range users {
		if c, err := bcrypt.Cost(hash); err == nil && c > cost {
			cost = c
		}
	}
	dummy, err := bcrypt.GenerateFromPassword(randomBytes(16), cost)
	if err != nil {
		return err
	}
	a.users, a.dummy, a.stamp, a.failed = users, dummy, stamp, ""
	return nil
}

// refresh reads the file again if it's changed, so that changing a password needs no restart. A change that can't be used keeps the
// users as they were, and is logged once. Removing someone also ends their sessions, as those are checked against this.
func (a *Authenticator) refresh() {
	stamp, err := fileStamp(a.path)
	if err != nil {
		stamp = "unreadable: " + err.Error()
	}
	if stamp == a.stamp || stamp == a.failed {
		return
	}
	if err == nil {
		err = a.load(stamp)
	}
	if err != nil {
		a.failed = stamp
		a.log.Error("keeping the current users, as the users file can't be used", "path", a.path, "error", err)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // Go's reader doesn't fail on any supported system.
	}
	return b
}

// Login checks a password, and starts a session. client is whatever identifies the caller, for limiting guesses.
func (a *Authenticator) Login(client, user, password string) (id string, expires time.Time, err error) {
	a.mu.Lock()
	if f := a.clients[client]; f != nil {
		switch elapsed := a.now().Sub(f.since); {
		case elapsed >= failureWindow:
			delete(a.clients, client)
		case f.count >= maxFailures:
			a.mu.Unlock()
			return "", time.Time{}, &TooManyError{RetryAfter: failureWindow - elapsed}
		}
	}
	a.refresh()
	hash, known := a.users[user]
	if !known || len(password) > MaxPasswordBytes {
		hash, known = a.dummy, false
	}
	a.mu.Unlock()

	select {
	case a.checks <- struct{}{}:
	default:
		return "", time.Time{}, &TooManyError{RetryAfter: time.Second}
	}
	// Slow on purpose, so outside the lock.
	checkErr := bcrypt.CompareHashAndPassword(hash, []byte(password))
	<-a.checks

	a.mu.Lock()
	defer a.mu.Unlock()
	if checkErr != nil || !known {
		a.noteFailure(client)
		a.log.Warn("failed sign-in", "user", user, "client", client)
		return "", time.Time{}, ErrWrongCredentials
	}
	delete(a.clients, client)
	a.prune()
	now := a.now()
	id = base64.RawURLEncoding.EncodeToString(randomBytes(32))
	a.sessions[id] = &session{user: user, began: now, lastSeen: now}
	a.log.Info("signed in", "user", user, "client", client)
	return id, now.Add(SessionLifetime), nil
}

func (a *Authenticator) noteFailure(client string) {
	f := a.clients[client]
	if f == nil {
		if len(a.clients) >= maxClients {
			a.clients = map[string]*failures{} // Better to forget some counts than to grow without limit.
		}
		f = &failures{since: a.now()}
		a.clients[client] = f
	}
	f.count++
}

// prune drops what's expired, and if there are still too many, the oldest, so that sign-ins can't grow this without limit.
func (a *Authenticator) prune() {
	now := a.now()
	for id, s := range a.sessions {
		if a.expired(s, now) {
			delete(a.sessions, id)
		}
	}
	for len(a.sessions) >= maxSessions {
		var oldest string
		var when time.Time
		for id, s := range a.sessions {
			if oldest == "" || s.began.Before(when) {
				oldest, when = id, s.began
			}
		}
		delete(a.sessions, oldest)
	}
}

func (a *Authenticator) expired(s *session, now time.Time) bool {
	return now.Sub(s.lastSeen) >= IdleTimeout || now.Sub(s.began) >= SessionLifetime
}

// Check returns who a session belongs to, if it's still good, and notes that it was used.
func (a *Authenticator) Check(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[id]
	if s == nil {
		return "", false
	}
	a.refresh()
	now := a.now()
	if _, stillThere := a.users[s.user]; !stillThere || a.expired(s, now) {
		delete(a.sessions, id)
		return "", false
	}
	s.lastSeen = now
	return s.user, true
}

// Logout ends a session.
func (a *Authenticator) Logout(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, id)
}

// ClientOf identifies a caller for limiting guesses, by address alone.
func ClientOf(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
