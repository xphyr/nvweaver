package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func (e *env) metrics() string {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.http.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET /metrics = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		e.t.Errorf("content type %q", ct)
	}
	return rec.Body.String()
}

// A line of Prometheus's text format: a comment, or a name, optional labels, and a number.
var sampleRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\.)*",?)*\})? [-+0-9.eE]+$|^[-+]Inf$`)

func TestMetricsDescribeTheService(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v1", "size_bytes": 8192}, 201)
	e.do("POST", "/v1/volumes", obj{"name": "v2", "size_bytes": 8192}, 201)
	e.do("POST", "/v1/snapshots", obj{"name": "s1", "volume": "v1"}, 201)
	e.do("GET", "/v1/volumes/nothing", nil, 404)
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"volume": "v1"}, 201)

	out := e.metrics()
	for _, want := range []string{
		`nvweaver_info{version="",backend="nvmet"} 1`,
		`nvweaver_objects{kind="volumes"} 2`,
		`nvweaver_objects{kind="snapshots"} 1`,
		`nvweaver_objects{kind="subsystems"} 1`,
		`nvweaver_objects{kind="namespaces"} 1`,
		`nvweaver_objects{kind="pools"} 1`,
		`nvweaver_pool_up{pool="p",type="reflink"} 1`,
		`nvweaver_pool_volumes{pool="p"} 2`,
		`nvweaver_pool_snapshots{pool="p"} 1`,
		`nvweaver_pool_warning_level_ratio{pool="p"} 0.8`,
		`nvweaver_intents_pending 0`,
		`nvweaver_http_requests_total{method="POST",route="/v1/volumes",code="201"} 2`,
		`nvweaver_http_requests_total{method="GET",route="/v1/volumes/{volume}",code="404"} 1`,
		`nvweaver_http_request_duration_seconds_count{method="POST",route="/v1/volumes"} 2`,
		`nvweaver_http_request_duration_seconds_bucket{method="POST",route="/v1/volumes",le="+Inf"} 2`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	for _, name := range []string{"nvweaver_pool_size_bytes", "nvweaver_pool_used_bytes", "nvweaver_pool_used_ratio", "nvweaver_target_ready", "nvweaver_warnings"} {
		if !strings.Contains(out, "\n"+name) {
			t.Errorf("missing %s", name)
		}
	}
	if strings.Contains(out, "nvweaver_pool_metadata_used_ratio") {
		t.Error("a reflink pool has no metadata to report")
	}
	// Every sample's metric is declared first, and every line parses.
	declared := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) >= 4 && f[0] == "#" && f[1] == "TYPE" {
			declared[f[2]] = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !sampleRE.MatchString(line) {
			t.Errorf("not Prometheus text: %q", line)
		}
		name := regexp.MustCompile(`^[a-z_]+`).FindString(line)
		base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
		if !declared[name] && !declared[base] {
			t.Errorf("%s comes before its TYPE", name)
		}
	}
}

// Metrics need a token, as the rest of the API does. A scoped token can read them, so one that owns nothing is a read-only token.
func TestMetricsNeedAToken(t *testing.T) {
	e := newScopedEnv(t)
	e.as("", "GET", "/metrics", nil, http.StatusUnauthorized)
	e.as(e.other, "GET", "/metrics", nil, http.StatusOK)
	e.as(e.admin, "GET", "/metrics", nil, http.StatusOK)
}

func TestMetricLabelsAreEscaped(t *testing.T) {
	if got := labelString([]string{"pool", "a\"b\\c\nd", "x", "1"}); got != `{pool="a\"b\\c\nd",x="1"}` {
		t.Errorf("%s", got)
	}
	if labelString(nil) != "" {
		t.Error("no labels make no braces")
	}
}
