package webui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestFilesAreServedWithTheirTypes(t *testing.T) {
	for path, want := range map[string]string{
		Path:                           "text/html",
		Path + "app.js":                "javascript",
		Path + "style.css":             "text/css",
		Path + "favicon-italic-32.png": "image/png",
	} {
		rec := get(t, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.Contains(got, want) {
			t.Errorf("%s: type %q, want %q", path, got, want)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty", path)
		}
	}
	if rec := get(t, Path+"nope.js"); rec.Code != http.StatusNotFound {
		t.Errorf("a missing file: status %d", rec.Code)
	}
}

func TestEveryResponseHasItsSecurityHeaders(t *testing.T) {
	for _, path := range []string{Path, Path + "app.js", Path + "nope"} {
		h := get(t, path).Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'", "form-action 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: policy %q lacks %q", path, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe") {
			t.Errorf("%s: policy allows something unsafe: %q", path, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: headers %v", path, h)
		}
	}
}

// go:embed can't reach outside this package, so these are copies of the project's own assets. They mustn't drift.
func TestIconsAreTheProjectsOwn(t *testing.T) {
	for _, name := range []string{"favicon-italic-16.png", "favicon-italic-32.png", "favicon-italic-180.png"} {
		want, err := os.ReadFile("../../images/" + name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("static/%s differs from images/%s: copy it again", name, name)
		}
		if rec := get(t, Path+name); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("%s isn't what's served: status %d", name, rec.Code)
		}
	}
}

// The policy would refuse these anyway, but they'd then simply not work, so catch them here.
func TestThePageUsesNothingThePolicyRefuses(t *testing.T) {
	page, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`<script(?:\s[^>]*)?>[^<\s]`), regexp.MustCompile(`<style`), regexp.MustCompile(`\son\w+=`), regexp.MustCompile(`\sstyle=`), regexp.MustCompile(`https?://`),
	} {
		if m := bad.Find(page); m != nil {
			t.Errorf("index.html has %q", m)
		}
	}
}

// Data from the API must reach the page as text, or a name could become markup.
func TestTheScriptNeverBuildsMarkupFromStrings(t *testing.T) {
	script, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(string(script), bad) {
			t.Errorf("app.js uses %s", bad)
		}
	}
}
