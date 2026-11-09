package auth

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
)

// OwnerRE is what a scoped token's name looks like. It's the owner of what the token creates, so it's kept short and plain,
// like a Kubernetes cluster's name. See decision 46.
var OwnerRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

const tokenPrefix = "sha256:"

// ParseTokens reads a tokens file, of "name:sha256:<hex>" lines, where the hex is the SHA-256 of the token. Blank lines and #
// comments are ignored, and anything else that can't be used is an error, as skipping a line would look like a token that
// stopped working for no reason. A hash is enough, as tokens are long and random, unlike passwords.
func ParseTokens(data []byte) (map[[sha256.Size]byte]string, error) {
	out := map[[sha256.Size]byte]string{}
	names := map[string]bool{}
	lines := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; lines.Scan(); n++ {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, hash, ok := strings.Cut(line, ":")
		if !ok || !strings.HasPrefix(hash, tokenPrefix) {
			return nil, fmt.Errorf("line %d: should be name:sha256:<hex>, as \"nvweaver token new\" prints", n)
		}
		if !OwnerRE.MatchString(name) {
			return nil, fmt.Errorf("line %d: %q isn't a usable name: lowercase letters, digits and dashes, up to 64", n, name)
		}
		if names[name] {
			return nil, fmt.Errorf("line %d: %s is there twice", n, name)
		}
		b, err := hex.DecodeString(strings.TrimPrefix(hash, tokenPrefix))
		if err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("line %d: the hash for %s isn't 64 hexadecimal digits", n, name)
		}
		key := [sha256.Size]byte(b)
		if other, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: %s has the same token as %s", n, name, other)
		}
		out[key], names[name] = name, true
	}
	if err := lines.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("it has no tokens")
	}
	return out, nil
}

// NewToken makes a random token for a scoped token's name, and the line that goes in the tokens file for it.
func NewToken(name string) (token, line string, err error) {
	if !OwnerRE.MatchString(name) {
		return "", "", fmt.Errorf("%q isn't a usable name: lowercase letters, digits and dashes, up to 64, as for a Kubernetes cluster", name)
	}
	token = "nvw_" + base64.RawURLEncoding.EncodeToString(randomBytes(32))
	sum := sha256.Sum256([]byte(token))
	return token, name + ":" + tokenPrefix + hex.EncodeToString(sum[:]), nil
}

// Tokens holds the scoped tokens, from a file that's read again when it changes, as the users file is.
type Tokens struct {
	path string
	log  *slog.Logger

	mu     sync.Mutex
	byHash map[[sha256.Size]byte]string
	stamp  string
	failed string
}

// NewTokens reads the tokens file, or returns nil if no path is given. One that others can read, or that can't be used, is an
// error, before the target starts.
func NewTokens(path string, log *slog.Logger) (*Tokens, error) {
	if path == "" {
		return nil, nil
	}
	t := &Tokens{path: path, log: log}
	stamp, err := fileStamp(path)
	if err != nil {
		return nil, fmt.Errorf("tokens file: %w", err)
	}
	if err := t.load(stamp); err != nil {
		return nil, fmt.Errorf("tokens file %s: %w", path, err)
	}
	return t, nil
}

func (t *Tokens) load(stamp string) error {
	info, err := os.Stat(t.path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("others can read it (mode %04o): run chmod 600 on it", perm)
	}
	data, err := os.ReadFile(t.path)
	if err != nil {
		return err
	}
	byHash, err := ParseTokens(data)
	if err != nil {
		return err
	}
	t.byHash, t.stamp, t.failed = byHash, stamp, ""
	return nil
}

// refresh reads the file again if it's changed. A change that can't be used keeps the tokens as they were, and is logged once.
func (t *Tokens) refresh() {
	stamp, err := fileStamp(t.path)
	if err != nil {
		stamp = "unreadable: " + err.Error()
	}
	if stamp == t.stamp || stamp == t.failed {
		return
	}
	if err == nil {
		err = t.load(stamp)
	}
	if err != nil {
		t.failed = stamp
		t.log.Error("keeping the current tokens, as the tokens file can't be used", "path", t.path, "error", err)
	}
}

// Check says whose a token is. Looking it up by its hash reveals nothing by timing that would help guess one.
func (t *Tokens) Check(token string) (string, bool) {
	if t == nil || token == "" {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.refresh()
	name, ok := t.byHash[sha256.Sum256([]byte(token))]
	return name, ok
}
