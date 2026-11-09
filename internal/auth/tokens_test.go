package auth

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokensFile(t *testing.T) {
	lab, labLine, err := NewToken("lab")
	if err != nil {
		t.Fatal(err)
	}
	other, otherLine, _ := NewToken("other")
	if !strings.HasPrefix(lab, "nvw_") || len(lab) < 40 || strings.Contains(labLine, lab) {
		t.Errorf("token %q, line %q", lab, labLine)
	}
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("# CSI clusters\n"+labLine+"\n\n"+otherLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	toks, err := NewTokens(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]string{lab: "lab", other: "other", "nvw_wrong": "", "": ""} {
		if got, ok := toks.Check(token); got != want || ok != (want != "") {
			t.Errorf("%q: %q, %v", token, got, ok)
		}
	}

	// Taking a token out of the file stops it, with no restart. A file that can't be used keeps what was there.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(otherLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := toks.Check(lab); ok {
		t.Error("a removed token still works")
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, ok := toks.Check(other); !ok || name != "other" {
		t.Error("a broken file dropped the tokens that were working")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(otherLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTokens(path, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a file others can read: %v", err)
	}
}

func TestTokensFileIsChecked(t *testing.T) {
	_, line, _ := NewToken("lab")
	for name, data := range map[string]string{
		"no hash":       "lab\n",
		"not sha256":    "lab:$2y$10$abc\n",
		"short hash":    "lab:sha256:abcd\n",
		"a bad name":    strings.Replace(line, "lab:", "Lab_1:", 1) + "\n",
		"a name twice":  line + "\n" + line + "\n",
		"a token twice": line + "\n" + strings.Replace(line, "lab:", "lab2:", 1) + "\n",
		"nothing":       "# none yet\n",
	} {
		if _, err := ParseTokens([]byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := NewToken("Not OK"); err == nil {
		t.Error("a bad name made a token")
	}
}
