package api

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// Metrics, in Prometheus's text format, written with the standard library: see decision 47. OpenTelemetry users scrape
// this with the Collector's prometheus receiver. It's authenticated like the rest of the API, so a scoped token that owns
// nothing makes a read-only token for a monitoring system.

// latencyBuckets are the upper bounds, in seconds, of the request duration histogram. A clone can take minutes.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}

type requestKey struct{ method, route, code string }
type routeKey struct{ method, route string }

type histogram struct {
	counts []uint64 // one per bucket, then the total count
	sum    float64
}

// requestMetrics counts the API's requests by route and status, and times them.
type requestMetrics struct {
	mu        sync.Mutex
	total     map[requestKey]uint64
	durations map[routeKey]*histogram
}

func newRequestMetrics() *requestMetrics {
	return &requestMetrics{total: map[requestKey]uint64{}, durations: map[routeKey]*histogram{}}
}

func (m *requestMetrics) observe(method, route string, code int, took time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total[requestKey{method, route, fmt.Sprint(code)}]++
	k := routeKey{method, route}
	h := m.durations[k]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(latencyBuckets)+1)}
		m.durations[k] = h
	}
	s := took.Seconds()
	for i, le := range latencyBuckets {
		if s <= le {
			h.counts[i]++
		}
	}
	h.counts[len(latencyBuckets)]++
	h.sum += s
}

// codeRecorder notes the status a handler writes.
type codeRecorder struct {
	http.ResponseWriter
	code int
}

func (r *codeRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *codeRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// instrument times a route's requests. route is its pattern, such as "/v1/volumes/{volume}", so names don't make new series.
func (s *Server) instrument(pattern string, h http.HandlerFunc) http.HandlerFunc {
	method, route, _ := strings.Cut(pattern, " ")
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &codeRecorder{ResponseWriter: w}
		h(rec, r)
		if rec.code == 0 {
			rec.code = http.StatusOK
		}
		s.requests.observe(method, route, rec.code, time.Since(start))
	}
}

// promWriter writes Prometheus's text format, with each metric's help and type once.
type promWriter struct {
	w    io.Writer
	seen map[string]bool
}

func (p *promWriter) head(name, kind, help string) {
	if !p.seen[name] {
		fmt.Fprintf(p.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
		p.seen[name] = true
	}
}

func (p *promWriter) sample(name string, labels []string, value float64) {
	fmt.Fprintf(p.w, "%s%s %v\n", name, labelString(labels), value)
}

func (p *promWriter) gauge(name, help string, labels []string, value float64) {
	p.head(name, "gauge", help)
	p.sample(name, labels, value)
}

// labelString makes {a="1",b="2"} from name, value pairs, escaping values as the format asks.
func labelString(pairs []string) string {
	if len(pairs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(pairs[i+1])
		fmt.Fprintf(&b, `%s="%s"`, pairs[i], v)
	}
	b.WriteByte('}')
	return b.String()
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (s *Server) getMetrics(w http.ResponseWriter, _ *http.Request) error {
	st := s.m.current()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	p := &promWriter{w: w, seen: map[string]bool{}}

	p.gauge("nvweaver_info", "The service's version and target backend, always 1.", []string{"version", s.Version, "backend", s.m.backend.Name()}, 1)
	p.gauge("nvweaver_target_ready", "1 if the target can serve hosts.", nil, boolValue(s.m.backend.Available()))

	namespaces := 0
	for _, sub := range st.Subsystems {
		namespaces += len(sub.Namespaces)
	}
	for _, c := range []struct {
		kind string
		n    int
	}{
		{"pools", len(st.Pools)}, {"volumes", len(st.Volumes)}, {"snapshots", len(st.Snapshots)},
		{"subsystems", len(st.Subsystems)}, {"namespaces", namespaces}, {"hosts", len(st.Hosts)}, {"ports", len(st.Ports)},
	} {
		p.gauge("nvweaver_objects", "How many of each kind of object there are.", []string{"kind", c.kind}, float64(c.n))
	}

	warnings := len(s.m.Warnings())
	for _, name := range slices.Sorted(maps.Keys(st.Pools)) {
		pool := st.Pools[name]
		v := s.viewPool(st, pool)
		l := []string{"pool", name}
		warnings += len(v.Warnings)
		p.gauge("nvweaver_pool_up", "1 if the pool's storage can be found.", append(l, "type", pool.Type), boolValue(v.State == "online"))
		p.gauge("nvweaver_pool_volumes", "Volumes in the pool.", l, float64(v.Volumes))
		p.gauge("nvweaver_pool_snapshots", "Snapshots in the pool.", l, float64(v.Snapshots))
		p.gauge("nvweaver_pool_warnings", "Warnings about the pool, which GET /v1/pools/{pool} lists.", l, float64(len(v.Warnings)))
		p.gauge("nvweaver_pool_warning_level_ratio", "How full the pool may get before it warns, from 0 to 1. 0 means it doesn't.", l, float64(pool.WarningPercent)/100)
		if c := v.Capacity; c != nil {
			p.gauge("nvweaver_pool_size_bytes", "The pool's size.", l, float64(c.TotalBytes))
			p.gauge("nvweaver_pool_used_bytes", "What's really allocated in the pool. A full thin pool gives hosts I/O errors.", l, float64(c.UsedBytes))
			p.gauge("nvweaver_pool_provisioned_bytes", "The sum of its volumes' sizes, which thin storage can make more than its size.", l, float64(c.ProvisionedBytes))
			p.gauge("nvweaver_pool_used_ratio", "How full the pool is, from 0 to 1.", l, c.UsedPercent/100)
			if pool.Type == "lvm-thin" {
				p.gauge("nvweaver_pool_metadata_used_ratio", "How full an LVM thin pool's metadata is, from 0 to 1. Full metadata stops the pool.", l, c.MetadataUsedPercent/100)
			}
		}
	}
	p.gauge("nvweaver_warnings", "Warnings about the service and its pools, as GET /v1/system and GET /v1/pools list them.", nil, float64(warnings))
	if entries, err := os.ReadDir(s.m.intentDir); err == nil {
		p.gauge("nvweaver_intents_pending", "Storage changes started but not finished, which the next start finishes. More than a moment's worth means something's stuck.", nil, float64(len(entries)))
	}

	s.requests.mu.Lock()
	defer s.requests.mu.Unlock()
	p.head("nvweaver_http_requests_total", "counter", "API requests, by method, route and status.")
	keys := slices.SortedFunc(maps.Keys(s.requests.total), func(a, b requestKey) int {
		return strings.Compare(a.method+a.route+a.code, b.method+b.route+b.code)
	})
	for _, k := range keys {
		p.sample("nvweaver_http_requests_total", []string{"method", k.method, "route", k.route, "code", k.code}, float64(s.requests.total[k]))
	}
	p.head("nvweaver_http_request_duration_seconds", "histogram", "How long API requests took, by method and route.")
	routes := slices.SortedFunc(maps.Keys(s.requests.durations), func(a, b routeKey) int {
		return strings.Compare(a.method+a.route, b.method+b.route)
	})
	for _, k := range routes {
		h := s.requests.durations[k]
		l := []string{"method", k.method, "route", k.route}
		for i, le := range latencyBuckets {
			p.sample("nvweaver_http_request_duration_seconds_bucket", append(slices.Clone(l), "le", fmt.Sprint(le)), float64(h.counts[i]))
		}
		p.sample("nvweaver_http_request_duration_seconds_bucket", append(slices.Clone(l), "le", "+Inf"), float64(h.counts[len(latencyBuckets)]))
		p.sample("nvweaver_http_request_duration_seconds_sum", l, h.sum)
		p.sample("nvweaver_http_request_duration_seconds_count", l, float64(h.counts[len(latencyBuckets)]))
	}
	return nil
}
