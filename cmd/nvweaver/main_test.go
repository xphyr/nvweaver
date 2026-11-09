package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"nvweaver/internal/api"
	"nvweaver/internal/auth"
	"nvweaver/internal/target"
	"nvweaver/internal/target/targettest"
)

// result is what one run of the command did.
type result struct {
	code           int
	stdout, stderr string
}

type tester struct {
	t   *testing.T
	url string
	env map[string]string
}

func (x *tester) run(args ...string) result {
	x.t.Helper()
	var stdout, stderr bytes.Buffer
	getenv := func(name string) string {
		if name == "NVWEAVER_API_URL" {
			return x.url
		}
		return x.env[name]
	}
	code := run(context.Background(), args, &stdout, &stderr, getenv)
	return result{code, stdout.String(), stderr.String()}
}

func (x *tester) ok(args ...string) string {
	x.t.Helper()
	r := x.run(args...)
	if r.code != exitOK {
		x.t.Fatalf("nvweaver %s: status %d: %s", strings.Join(args, " "), r.code, r.stderr)
	}
	return r.stdout
}

// realAPI runs the command against the real handlers, with a fake configfs.
func realAPI(t *testing.T, token string) *tester {
	t.Helper()
	m, err := api.NewManager(&target.Target{FS: targettest.New(), Root: "/nvmet"}, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewServer(m, token).Handler())
	t.Cleanup(srv.Close)
	return &tester{t: t, url: srv.URL, env: map[string]string{}}
}

// recordingAPI captures what's sent, for commands that need real storage to run against the real handlers. test/e2e.sh runs those for real.
func recordingAPI(t *testing.T, reply string) (*tester, *[]string) {
	t.Helper()
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+string(body)))
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return &tester{t: t, url: srv.URL, env: map[string]string{}}, &requests
}

func TestAWholeExportThroughTheRealAPI(t *testing.T) {
	x := realAPI(t, "")
	const sub, host = "nqn.2026-11.net.example:sub", "nqn.2026-11.net.example:host"
	if out := x.ok("system"); !strings.Contains(out, "Backend:") || !strings.Contains(out, "nvmet") {
		t.Errorf("system: %s", out)
	}
	if out := x.ok("health"); !strings.Contains(out, "ok") {
		t.Errorf("health: %s", out)
	}
	if out := x.ok("port", "create", "--id", "3", "--address", "127.0.0.1", "--service", "4420"); !strings.Contains(out, "127.0.0.1") {
		t.Errorf("port create: %s", out)
	}
	x.ok("host", "create", host)
	// Flags after, before and between arguments all work.
	if out := x.ok("subsystem", "create", sub, "--allow-host", host, "--port", "3"); !strings.Contains(out, host) {
		t.Errorf("subsystem create: %s", out)
	}
	if out := x.ok("namespace", "create", "--file", "a.img", sub, "--size", "1M"); !strings.Contains(out, "1M") {
		t.Errorf("namespace create: %s", out)
	}
	if out := x.ok("namespace", "update", sub, "1", "--size", "2M"); !strings.Contains(out, "2M") {
		t.Errorf("namespace update: %s", out)
	}
	var got struct {
		NQN          string   `json:"nqn"`
		AllowedHosts []string `json:"allowed_hosts"`
		Ports        []int    `json:"ports"`
		Namespaces   []struct {
			NSID int `json:"nsid"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal([]byte(x.ok("subsystem", "get", sub, "-o", "json")), &got); err != nil {
		t.Fatalf("JSON output didn't parse: %v", err)
	}
	if got.NQN != sub || len(got.AllowedHosts) != 1 || len(got.Ports) != 1 || len(got.Namespaces) != 1 {
		t.Errorf("subsystem came back as %+v", got)
	}
	x.ok("subsystem", "deny", sub, host)
	x.ok("subsystem", "allow", sub, host)
	x.ok("subsystem", "detach", sub, "3")
	x.ok("subsystem", "attach", sub, "3")
	// The API refuses this alongside an allow list, and that should come through.
	if r := x.run("subsystem", "update", sub, "--allow-any-host=true"); r.code != exitError || !strings.Contains(r.stderr, "allowed_hosts") {
		t.Errorf("with an allow list: %+v", r)
	}
	x.ok("subsystem", "deny", sub, host)
	if out := x.ok("subsystem", "update", sub, "--allow-any-host=true"); !strings.Contains(out, "true") {
		t.Errorf("subsystem update: %s", out)
	}
	if out := x.ok("namespace", "delete", sub, "1", "--delete-file", "-o", "json"); out != "" {
		t.Errorf("JSON output should stay empty with nothing to show, but got %q", out)
	}
	x.ok("subsystem", "delete", sub)
	x.ok("host", "delete", host)
	x.ok("port", "delete", "3")
	if out := x.ok("port", "list", "-o", "json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("an empty list should be [], for scripts, but got %q", out)
	}
}

func TestExitStatusSaysWhatKindOfFailure(t *testing.T) {
	x := realAPI(t, "")
	cases := []struct {
		name string
		want int
		args []string
	}{
		{"nothing to do", exitUsage, nil},
		{"unknown command", exitUsage, []string{"bogus"}},
		{"unknown action", exitUsage, []string{"port", "frob"}},
		{"no action", exitUsage, []string{"port"}},
		{"missing argument", exitUsage, []string{"port", "get"}},
		{"too many arguments", exitUsage, []string{"port", "get", "1", "2"}},
		{"not a number", exitUsage, []string{"port", "get", "one"}},
		{"unknown flag", exitUsage, []string{"port", "list", "--frob"}},
		{"bad output format", exitUsage, []string{"port", "list", "-o", "xml"}},
		{"missing required flag", exitUsage, []string{"port", "create"}},
		{"bad size", exitUsage, []string{"volume", "create", "v", "--size", "lots"}},
		{"no size", exitUsage, []string{"volume", "create", "v"}},
		{"two things to export", exitUsage, []string{"namespace", "create", "nqn", "--volume", "v", "--device", "/dev/x"}},
		{"nothing to export", exitUsage, []string{"namespace", "create", "nqn"}},
		{"a token on the command line", exitUsage, []string{"port", "list", "--token", "secret"}},
		{"doesn't exist", exitError, []string{"port", "get", "7"}},
		{"refused", exitError, []string{"host", "create", "not-an-nqn"}},
		{"help", exitOK, []string{"help"}},
		{"help for an action", exitOK, []string{"volume", "create", "--help"}},
		{"version", exitOK, []string{"version"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := x.run(c.args...)
			if r.code != c.want {
				t.Errorf("status %d, want %d: %s", r.code, c.want, r.stderr)
			}
			if c.want != exitOK && r.stderr == "" {
				t.Error("it failed without saying why")
			}
		})
	}
}

func TestUnreachableIsAFailedRequest(t *testing.T) {
	x := &tester{t: t, url: "http://127.0.0.1:1", env: map[string]string{}}
	if r := x.run("port", "list"); r.code != exitError {
		t.Errorf("status %d", r.code)
	}
}

func TestTokenComesFromTheEnvironmentOrAFile(t *testing.T) {
	x := realAPI(t, "right")
	if r := x.run("port", "list"); r.code != exitError || !strings.Contains(r.stderr, "token") {
		t.Errorf("without one: %+v", r)
	}
	x.env["NVWEAVER_API_TOKEN"] = "wrong"
	if r := x.run("port", "list"); r.code != exitError {
		t.Errorf("with the wrong one: %+v", r)
	}
	x.env["NVWEAVER_API_TOKEN"] = "right"
	x.ok("port", "list")
	x.env["NVWEAVER_API_TOKEN"] = "wrong"
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("right\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	x.ok("port", "list", "--token-file", file)
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := x.run("port", "list", "--token-file", file); r.code != exitError {
		t.Errorf("an empty token file should fail, rather than send no token: %+v", r)
	}
}

func TestClearTextTokenIsWarnedAbout(t *testing.T) {
	cases := map[string]bool{
		"http://192.0.2.1:8420": true, "http://storage.example:8420": true, "https://192.0.2.1:8420": false,
		"http://127.0.0.1:8420": false, "http://localhost:8420": false, "http://[::1]:8420": false, "": false,
	}
	for url, want := range cases {
		if got := clearTextOverNetwork(url); got != want {
			t.Errorf("%q: got %v", url, got)
		}
	}
}

func TestKeysComeFromFilesAndAreSentOnce(t *testing.T) {
	x, requests := recordingAPI(t, `{"nqn":"n"}`)
	dir := t.TempDir()
	file := filepath.Join(dir, "key")
	if err := os.WriteFile(file, []byte("DHHC-1:00:secret:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := x.ok("host", "create", "n", "--dhchap-key-file", file)
	if strings.Contains(out, "secret") {
		t.Errorf("the key was printed: %s", out)
	}
	if !strings.Contains((*requests)[0], `"dhchap_key":"DHHC-1:00:secret:"`) {
		t.Errorf("sent %s", (*requests)[0])
	}
	if r := x.run("host", "create", "n", "--dhchap-key", "secret"); r.code != exitUsage {
		t.Errorf("a key on the command line should be refused, but got %+v", r)
	}
}

func TestRequestsSent(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"reflink pool", []string{"pool", "create", "p", "--type", "reflink", "--path", "/srv/p", "--label", "a=b"},
			`POST /v1/pools {"name":"p","type":"reflink","path":"/srv/p","labels":{"a":"b"}}`},
		{"LVM pool", []string{"pool", "create", "l", "--type", "lvm-thin", "--volume-group", "vg", "--thin-pool", "t", "--warning-percent", "0"},
			`POST /v1/pools {"name":"l","type":"lvm-thin","volume_group":"vg","thin_pool":"t","warning_percent":0}`},
		{"clear pool labels", []string{"pool", "update", "p", "--clear-labels"}, `PATCH /v1/pools/p {"labels":{}}`},
		{"only what's given", []string{"pool", "update", "p", "--warning-percent", "90"}, `PATCH /v1/pools/p {"warning_percent":90}`},
		{"volume", []string{"volume", "create", "v", "--size", "10G"}, `POST /v1/volumes {"name":"v","size_bytes":10737418240}`},
		{"clone of a snapshot", []string{"volume", "create", "c", "--from-snapshot", "s"}, `POST /v1/volumes {"name":"c","source":{"snapshot":"s"}}`},
		{"larger clone of a volume", []string{"volume", "create", "c", "--from-volume", "v", "--size", "1G"},
			`POST /v1/volumes {"name":"c","size_bytes":1073741824,"source":{"volume":"v"}}`},
		{"grow", []string{"volume", "update", "v", "--size", "20G"}, `PATCH /v1/volumes/v {"size_bytes":21474836480}`},
		{"snapshot", []string{"snapshot", "create", "s", "--volume", "v"}, `POST /v1/snapshots {"name":"s","volume":"v"}`},
		{"delete keeps its file", []string{"namespace", "delete", "n", "1"}, `DELETE /v1/subsystems/n/namespaces/1`},
		{"delete with its file", []string{"namespace", "delete", "n", "1", "--delete-file"}, `DELETE /v1/subsystems/n/namespaces/1?delete_file=true`},
		{"port 0 is a real ID", []string{"port", "create", "--id", "0", "--address", "::1"}, `POST /v1/ports {"id":0,"address":"::1"}`},
		{"disable", []string{"namespace", "update", "n", "1", "--enabled=false"}, `PATCH /v1/subsystems/n/namespaces/1 {"enabled":false}`},
		{"created disabled", []string{"namespace", "create", "n", "--volume", "v", "--disabled"}, `POST /v1/subsystems/n/namespaces {"volume":"v","enabled":false}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x, requests := recordingAPI(t, `{}`)
			x.ok(c.args...)
			if len(*requests) != 1 || (*requests)[0] != c.want {
				t.Errorf("sent %q, want %q", *requests, c.want)
			}
		})
	}
}

func TestListsFilterAndFollowPages(t *testing.T) {
	x, requests := recordingAPI(t, `{"items":[]}`)
	x.ok("volume", "list", "--pool", "p")
	x.ok("snapshot", "list", "--volume", "v")
	if (*requests)[0] != "GET /v1/volumes?pool=p" || (*requests)[1] != "GET /v1/snapshots?volume=v" {
		t.Errorf("sent %q", *requests)
	}
}

func TestWarningsAreShown(t *testing.T) {
	x, _ := recordingAPI(t, `{"backend":"nvmet","warnings":["LVM can read this"]}`)
	if out := x.ok("system"); !strings.Contains(out, "Warning: LVM can read this") {
		t.Errorf("system: %s", out)
	}
	if out := x.ok("system", "-o", "json"); strings.Contains(out, "Warning:") || !strings.Contains(out, "LVM can read this") {
		t.Errorf("in JSON, it should only be in the data: %s", out)
	}
	y, _ := recordingAPI(t, `{"name":"p","state":"missing","warnings":["no such VG"]}`)
	if out := y.ok("pool", "get", "p"); !strings.Contains(out, "Warning: pool p: no such VG") || !strings.Contains(out, "missing") {
		t.Errorf("pool get: %s", out)
	}
}

func TestUnhealthyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"status":"target unavailable"}`)
	}))
	t.Cleanup(srv.Close)
	x := &tester{t: t, url: srv.URL, env: map[string]string{}}
	if r := x.run("health"); r.code != exitError {
		t.Errorf("a script checking health should see a failure, but got %+v", r)
	}
}

func TestSizes(t *testing.T) {
	good := map[string]int64{"4096": 4096, "4K": 4096, "1m": 1 << 20, "10G": 10 << 30, "10GiB": 10 << 30, "10GB": 10 << 30, "2T": 2 << 40, "1P": 1 << 50}
	for in, want := range good {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("%q: got %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "G", "-1G", "0", "1.5G", "lots", "99999999P"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("%q was accepted, as %d", in, got)
		}
	}
	shown := map[int64]string{4096: "4K", 1 << 30: "1G", 1536 << 20: "1.5G", 512: "512", 0: "0"}
	for in, want := range shown {
		if got := humanSize(in); got != want {
			t.Errorf("%d: got %q, want %q", in, got, want)
		}
	}
}

// TestUsageListsEveryAction stops an action being added without anyone being able to find it.
func TestUsageListsEveryAction(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`"(\w+)": a\.[a-z]+[A-Z]\w+`).FindAllSubmatch(source, -1)
	if len(found) < 20 {
		t.Fatalf("found %d actions in main.go: has its layout changed?", len(found))
	}
	for _, m := range found {
		if !strings.Contains(usage, string(m[1])) {
			t.Errorf("usage doesn't mention action %s", m[1])
		}
	}
}

func TestSharedFlagsWorkBeforeOrAfterTheCommand(t *testing.T) {
	x := realAPI(t, "")
	url := x.url
	x.url = "" // So that only the flag can say where the API is.
	for _, args := range [][]string{
		{"--api", url, "port", "list", "-o", "json"},
		{"--api=" + url, "-o", "json", "port", "list"},
		{"port", "list", "--api", url, "--output=json"},
		{"port", "--api", url, "list", "-o", "json"},
	} {
		r := x.run(args...)
		// The flag package only finds what's after the action, so this one's an error, and that's fine as long as it says so.
		if args[1] == "--api" && args[0] == "port" {
			if r.code != exitUsage {
				t.Errorf("%v: status %d", args, r.code)
			}
			continue
		}
		if r.code != exitOK || strings.TrimSpace(r.stdout) != "[]" {
			t.Errorf("%v: status %d, out %q, err %q", args, r.code, r.stdout, r.stderr)
		}
	}
	if r := x.run("--api", url); r.code != exitUsage {
		t.Errorf("only a flag: status %d", r.code)
	}
	if r := x.run("--frob", "port", "list", "--api", url); r.code != exitUsage || !strings.Contains(r.stderr, "frob") {
		t.Errorf("an unknown flag first: %+v", r)
	}
}

func TestPasswdMakesALineTheServiceAccepts(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := passwd([]string{"admin"}, strings.NewReader("a good password\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	line := stdout.String()
	if !strings.HasPrefix(line, "admin:$2") || strings.Contains(line, "a good password") {
		t.Errorf("got %q", line)
	}
	file := filepath.Join(t.TempDir(), "users")
	if err := os.WriteFile(file, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(file, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Login("c", "admin", "a good password"); err != nil {
		t.Errorf("signing in with it: %v", err)
	}
}

func TestPasswdRefusals(t *testing.T) {
	cases := map[string]struct {
		args []string
		in   string
	}{
		"no name":              {nil, "password\n"},
		"a password as a flag": {[]string{"--password", "x"}, ""},
		"a bad name":           {[]string{"ad min"}, "password\n"},
		"no password":          {[]string{"admin"}, ""},
		"an empty password":    {[]string{"admin"}, "\n"},
		"too long":             {[]string{"admin"}, strings.Repeat("x", auth.MaxPasswordBytes+1) + "\n"},
	}
	for name, c := range cases {
		var stdout, stderr bytes.Buffer
		if err := passwd(c.args, strings.NewReader(c.in), &stdout, &stderr); err == nil {
			t.Errorf("%s was accepted: %s", name, stdout.String())
		}
	}
}

func TestTokenNewMakesATokenTheServiceAccepts(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "lab.token")
	var stdout, stderr bytes.Buffer
	if err := tokenNew([]string{"new", "lab", "--token-file", tokenFile}, nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(b))
	if info, _ := os.Stat(tokenFile); info.Mode().Perm() != 0o600 {
		t.Errorf("the token file is mode %o", info.Mode().Perm())
	}
	if strings.Contains(stdout.String(), token) || stderr.Len() != 0 {
		t.Errorf("the token was printed: %q, %q", stdout.String(), stderr.String())
	}
	tokens := filepath.Join(dir, "tokens")
	if err := os.WriteFile(tokens, stdout.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	toks, err := auth.NewTokens(tokens, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := toks.Check(token); !ok || name != "lab" {
		t.Errorf("the service doesn't accept it: %q, %v", name, ok)
	}
	if err := tokenNew([]string{"new", "lab", "--token-file", tokenFile}, nil, &stdout, &stderr); err == nil {
		t.Error("an existing token file was overwritten")
	}

	// With no file, the token goes to stderr, and only the line to stdout.
	stdout.Reset()
	if err := tokenNew([]string{"new", "other"}, nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "nvw_") || strings.Contains(stdout.String(), "nvw_") || !strings.HasPrefix(stdout.String(), "other:sha256:") {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	for _, args := range [][]string{nil, {"new"}, {"make", "lab"}, {"new", "Not OK"}, {"new", "lab", "extra"}} {
		if err := tokenNew(args, nil, &stdout, &stderr); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}
