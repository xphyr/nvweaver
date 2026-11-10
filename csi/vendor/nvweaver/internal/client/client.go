// Package client talks to the nvweaver API, for the command line utility and the CSI driver.
//
// It uses the standard library only, like the rest of this project. Types are written by hand, rather than
// generated, and tests in internal/api check every operation and field against api/openapi.yaml, and the real handlers.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is where the service listens by default.
const DefaultURL = "http://127.0.0.1:8420"

const (
	defaultTimeout   = 2 * time.Minute
	defaultBusyRetry = time.Minute
	longestBusyWait  = 30 * time.Second
	// errorBodyLimit bounds what's kept of an error from something that isn't the API, such as a proxy's page.
	errorBodyLimit = 4096
)

// Options configures a Client.
type Options struct {
	// URL is the API's address, which defaults to DefaultURL.
	URL string
	// Token is the bearer token, if the API needs one.
	Token string
	// CAFile is a PEM file of certificates to trust for HTTPS, in place of the system's.
	CAFile string
	// Timeout limits each attempt at a request. It defaults to 2 minutes, as a clone can be slow.
	Timeout time.Duration
	// BusyRetry is how long to keep retrying when the API says it's busy, which it does with 503 and Retry-After, having
	// changed nothing. It defaults to a minute. A negative value turns retrying off.
	BusyRetry time.Duration
}

// Client calls the API. It's safe to use from several goroutines.
type Client struct {
	base      *url.URL
	token     string
	http      *http.Client
	busyRetry time.Duration
	sleep     func(context.Context, time.Duration) error
}

// New returns a Client.
func New(o Options) (*Client, error) {
	raw := o.URL
	if raw == "" {
		raw = DefaultURL
	}
	base, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("API address %q: %w", raw, err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("API address %q: it should look like https://host:8420", raw)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s: no certificates in it", o.CAFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	busyRetry := o.BusyRetry
	if busyRetry == 0 {
		busyRetry = defaultBusyRetry
	}
	return &Client{
		base:  base,
		token: o.Token,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// The API doesn't redirect, so something else is answering. Don't follow it anywhere with a request.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		busyRetry: busyRetry,
		sleep:     sleepContext,
	}, nil
}

// Error is an error response from the API.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return e.Message
}

func hasStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// IsNotFound reports that the object doesn't exist.
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// IsConflict reports that the request clashes with something that exists.
func IsConflict(err error) bool { return hasStatus(err, http.StatusConflict) }

// IsInvalid reports that the request was refused as it stands.
func IsInvalid(err error) bool { return hasStatus(err, http.StatusUnprocessableEntity) }

// IsUnauthorized reports that the token was missing or wrong.
func IsUnauthorized(err error) bool { return hasStatus(err, http.StatusUnauthorized) }

// IsBusy reports that the API stayed busy for as long as this was willing to retry.
func IsBusy(err error) bool { return hasStatus(err, http.StatusServiceUnavailable) }

// IsOutOfSpace reports that a pool has no room.
func IsOutOfSpace(err error) bool { return hasStatus(err, http.StatusInsufficientStorage) }

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryDelay reads Retry-After, in seconds, within reason. The API only sends whole seconds.
func retryDelay(resp *http.Response) time.Duration {
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || seconds < 1 {
		return time.Second
	}
	return min(time.Duration(seconds)*time.Second, longestBusyWait)
}

// request sends one call, retrying while the API says it's busy, and decodes a successful response into out, if it's given.
func (c *Client) request(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	target := c.base.String() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	deadline := time.Now().Add(c.busyRetry)
	for {
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			// Not retried: unlike a 503, this doesn't say that nothing happened.
			return err
		}
		if resp.StatusCode == http.StatusServiceUnavailable {
			wait := retryDelay(resp)
			if time.Now().Add(wait).Before(deadline) {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if err := c.sleep(ctx, wait); err != nil {
					return err
				}
				continue
			}
		}
		return finish(resp, out)
	}
}

func finish(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(text, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(text))
		}
		return &Error{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("reading the API's response: %w", err)
		}
		*raw = b
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("reading the API's response: %w", err)
	}
	return nil
}

func get[T any](ctx context.Context, c *Client, path string) (*T, error) {
	out := new(T)
	if err := c.request(ctx, http.MethodGet, path, nil, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

func list[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var out []T
	if err := c.request(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func send[T any](ctx context.Context, c *Client, method, path string, in any) (*T, error) {
	out := new(T)
	if err := c.request(ctx, method, path, nil, in, out); err != nil {
		return nil, err
	}
	return out, nil
}

func p(parts ...string) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString("/")
		b.WriteString(url.PathEscape(part))
	}
	return b.String()
}

func portPath(id int) string { return p("v1", "ports", strconv.Itoa(id)) }

func nsPath(nqn string, nsid int) string {
	return p("v1", "subsystems", nqn, "namespaces", strconv.Itoa(nsid))
}

// Health reports whether the service can serve. An unhealthy service still answers, with a status saying so,
// and that isn't an error here. It needs no token.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	out := &Health{}
	// A 503 here is the answer, rather than a reason to wait.
	once := *c
	once.busyRetry = -1
	err := once.request(ctx, http.MethodGet, "/healthz", nil, nil, out)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusServiceUnavailable {
		return &Health{Status: "unavailable"}, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Metrics returns the service's metrics, in Prometheus's text format.
func (c *Client) Metrics(ctx context.Context) (string, error) {
	var b []byte
	if err := c.request(ctx, http.MethodGet, "/metrics", nil, nil, &b); err != nil {
		return "", err
	}
	return string(b), nil
}

// System describes the service and its target, including any warnings about how the machine is set up.
func (c *Client) System(ctx context.Context) (*System, error) {
	return get[System](ctx, c, "/v1/system")
}

// ListPorts returns every port.
// ListConnections lists who's connected, to the ports of one subsystem, or to every port if nqn is empty.
func (c *Client) ListConnections(ctx context.Context, nqn string) ([]Connection, error) {
	path := "/v1/connections"
	if nqn != "" {
		path += "?subsystem=" + url.QueryEscape(nqn)
	}
	return list[Connection](ctx, c, path)
}

func (c *Client) ListPorts(ctx context.Context) ([]Port, error) {
	return list[Port](ctx, c, "/v1/ports")
}

// GetPort returns one port.
func (c *Client) GetPort(ctx context.Context, id int) (*Port, error) {
	return get[Port](ctx, c, portPath(id))
}

// CreatePort creates a port.
func (c *Client) CreatePort(ctx context.Context, in PortCreate) (*Port, error) {
	return send[Port](ctx, c, http.MethodPost, "/v1/ports", in)
}

// UpdatePort changes a port.
func (c *Client) UpdatePort(ctx context.Context, id int, in PortUpdate) (*Port, error) {
	return send[Port](ctx, c, http.MethodPatch, portPath(id), in)
}

// DeletePort removes a port.
func (c *Client) DeletePort(ctx context.Context, id int) error {
	return c.request(ctx, http.MethodDelete, portPath(id), nil, nil, nil)
}

// ListHosts returns every host.
func (c *Client) ListHosts(ctx context.Context) ([]Host, error) {
	return list[Host](ctx, c, "/v1/hosts")
}

// GetHost returns one host.
func (c *Client) GetHost(ctx context.Context, nqn string) (*Host, error) {
	return get[Host](ctx, c, p("v1", "hosts", nqn))
}

// CreateHost creates a host.
func (c *Client) CreateHost(ctx context.Context, in HostCreate) (*Host, error) {
	return send[Host](ctx, c, http.MethodPost, "/v1/hosts", in)
}

// UpdateHost changes a host.
func (c *Client) UpdateHost(ctx context.Context, nqn string, in HostUpdate) (*Host, error) {
	return send[Host](ctx, c, http.MethodPatch, p("v1", "hosts", nqn), in)
}

// DeleteHost removes a host.
func (c *Client) DeleteHost(ctx context.Context, nqn string) error {
	return c.request(ctx, http.MethodDelete, p("v1", "hosts", nqn), nil, nil, nil)
}

// ListSubsystems returns every subsystem.
func (c *Client) ListSubsystems(ctx context.Context) ([]Subsystem, error) {
	return list[Subsystem](ctx, c, "/v1/subsystems")
}

// GetSubsystem returns one subsystem.
func (c *Client) GetSubsystem(ctx context.Context, nqn string) (*Subsystem, error) {
	return get[Subsystem](ctx, c, p("v1", "subsystems", nqn))
}

// CreateSubsystem creates a subsystem.
func (c *Client) CreateSubsystem(ctx context.Context, in SubsystemCreate) (*Subsystem, error) {
	return send[Subsystem](ctx, c, http.MethodPost, "/v1/subsystems", in)
}

// UpdateSubsystem changes a subsystem.
func (c *Client) UpdateSubsystem(ctx context.Context, nqn string, in SubsystemUpdate) (*Subsystem, error) {
	return send[Subsystem](ctx, c, http.MethodPatch, p("v1", "subsystems", nqn), in)
}

// DeleteSubsystem removes a subsystem.
func (c *Client) DeleteSubsystem(ctx context.Context, nqn string) error {
	return c.request(ctx, http.MethodDelete, p("v1", "subsystems", nqn), nil, nil, nil)
}

// AttachPort makes a subsystem available through a port.
func (c *Client) AttachPort(ctx context.Context, nqn string, port int) error {
	return c.request(ctx, http.MethodPut, p("v1", "subsystems", nqn, "ports", strconv.Itoa(port)), nil, nil, nil)
}

// DetachPort stops offering a subsystem through a port.
func (c *Client) DetachPort(ctx context.Context, nqn string, port int) error {
	return c.request(ctx, http.MethodDelete, p("v1", "subsystems", nqn, "ports", strconv.Itoa(port)), nil, nil, nil)
}

// AllowHost lets a host connect to a subsystem.
func (c *Client) AllowHost(ctx context.Context, nqn, host string) error {
	return c.request(ctx, http.MethodPut, p("v1", "subsystems", nqn, "hosts", host), nil, nil, nil)
}

// DenyHost removes a host's access to a subsystem, which also cuts it off if it's connected.
func (c *Client) DenyHost(ctx context.Context, nqn, host string) error {
	return c.request(ctx, http.MethodDelete, p("v1", "subsystems", nqn, "hosts", host), nil, nil, nil)
}

// ListNamespaces returns a subsystem's namespaces.
func (c *Client) ListNamespaces(ctx context.Context, nqn string) ([]Namespace, error) {
	return list[Namespace](ctx, c, p("v1", "subsystems", nqn, "namespaces"))
}

// GetNamespace returns one namespace.
func (c *Client) GetNamespace(ctx context.Context, nqn string, nsid int) (*Namespace, error) {
	return get[Namespace](ctx, c, nsPath(nqn, nsid))
}

// CreateNamespace exports something in a subsystem.
func (c *Client) CreateNamespace(ctx context.Context, nqn string, in NamespaceCreate) (*Namespace, error) {
	return send[Namespace](ctx, c, http.MethodPost, p("v1", "subsystems", nqn, "namespaces"), in)
}

// UpdateNamespace changes a namespace.
func (c *Client) UpdateNamespace(ctx context.Context, nqn string, nsid int, in NamespaceUpdate) (*Namespace, error) {
	return send[Namespace](ctx, c, http.MethodPatch, nsPath(nqn, nsid), in)
}

// DeleteNamespace stops exporting a namespace. With deleteFile, a file this service created goes too.
func (c *Client) DeleteNamespace(ctx context.Context, nqn string, nsid int, deleteFile bool) error {
	var query url.Values
	if deleteFile {
		query = url.Values{"delete_file": {"true"}}
	}
	return c.request(ctx, http.MethodDelete, nsPath(nqn, nsid), query, nil, nil)
}

// ListPools returns every pool.
func (c *Client) ListPools(ctx context.Context) ([]Pool, error) {
	return list[Pool](ctx, c, "/v1/pools")
}

// GetPool returns one pool.
func (c *Client) GetPool(ctx context.Context, name string) (*Pool, error) {
	return get[Pool](ctx, c, p("v1", "pools", name))
}

// CreatePool adopts a pool. Adopting the same one again succeeds.
func (c *Client) CreatePool(ctx context.Context, in PoolCreate) (*Pool, error) {
	return send[Pool](ctx, c, http.MethodPost, "/v1/pools", in)
}

// UpdatePool changes a pool.
func (c *Client) UpdatePool(ctx context.Context, name string, in PoolUpdate) (*Pool, error) {
	return send[Pool](ctx, c, http.MethodPatch, p("v1", "pools", name), in)
}

// DeletePool forgets a pool, leaving its storage alone.
func (c *Client) DeletePool(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodDelete, p("v1", "pools", name), nil, nil, nil)
}

func pageQuery(filter, value string, limit int, after string) url.Values {
	query := url.Values{}
	if value != "" {
		query.Set(filter, value)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if after != "" {
		query.Set("after", after)
	}
	return query
}

// VolumesPage returns one page of volumes, optionally of one pool. A limit of 0 uses the API's default.
func (c *Client) VolumesPage(ctx context.Context, pool string, limit int, after string) (*VolumePage, error) {
	out := &VolumePage{}
	if err := c.request(ctx, http.MethodGet, "/v1/volumes", pageQuery("pool", pool, limit, after), nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListVolumes returns every volume, optionally of one pool, following pages.
func (c *Client) ListVolumes(ctx context.Context, pool string) ([]Volume, error) {
	var all []Volume
	after := ""
	for {
		page, err := c.VolumesPage(ctx, pool, 0, after)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextAfter == "" {
			return all, nil
		}
		after = page.NextAfter
	}
}

// GetVolume returns one volume.
func (c *Client) GetVolume(ctx context.Context, name string) (*Volume, error) {
	return get[Volume](ctx, c, p("v1", "volumes", name))
}

// GetVolumeUsage asks the storage how much of a volume is really allocated, including what it shares with snapshots and clones.
func (c *Client) GetVolumeUsage(ctx context.Context, name string) (*VolumeUsage, error) {
	return get[VolumeUsage](ctx, c, p("v1", "volumes", name, "usage"))
}

// CreateVolume creates a volume or clone. Asking again with the same settings returns the existing one.
func (c *Client) CreateVolume(ctx context.Context, in VolumeCreate) (*Volume, error) {
	return send[Volume](ctx, c, http.MethodPost, "/v1/volumes", in)
}

// UpdateVolume grows a volume, or changes its labels.
func (c *Client) UpdateVolume(ctx context.Context, name string, in VolumeUpdate) (*Volume, error) {
	return send[Volume](ctx, c, http.MethodPatch, p("v1", "volumes", name), in)
}

// DeleteVolume removes a volume and its data.
func (c *Client) DeleteVolume(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodDelete, p("v1", "volumes", name), nil, nil, nil)
}

// SnapshotsPage returns one page of snapshots, optionally of one volume. A limit of 0 uses the API's default.
func (c *Client) SnapshotsPage(ctx context.Context, volume string, limit int, after string) (*SnapshotPage, error) {
	out := &SnapshotPage{}
	if err := c.request(ctx, http.MethodGet, "/v1/snapshots", pageQuery("volume", volume, limit, after), nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListSnapshots returns every snapshot, optionally of one volume, following pages.
func (c *Client) ListSnapshots(ctx context.Context, volume string) ([]Snapshot, error) {
	var all []Snapshot
	after := ""
	for {
		page, err := c.SnapshotsPage(ctx, volume, 0, after)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextAfter == "" {
			return all, nil
		}
		after = page.NextAfter
	}
}

// GetSnapshot returns one snapshot.
func (c *Client) GetSnapshot(ctx context.Context, name string) (*Snapshot, error) {
	return get[Snapshot](ctx, c, p("v1", "snapshots", name))
}

// SnapshotAllocated returns a page of where a snapshot holds data, from offset; a limit of 0 is the server's default.
func (c *Client) SnapshotAllocated(ctx context.Context, snapshot string, offset int64, limit int) (*BlockRanges, error) {
	return c.blockRanges(ctx, p("v1", "snapshots", snapshot, "allocated"), url.Values{}, offset, limit)
}

// SnapshotChanges returns a page of where snapshot differs from base, an earlier or later snapshot of the same volume.
func (c *Client) SnapshotChanges(ctx context.Context, base, snapshot string, offset int64, limit int) (*BlockRanges, error) {
	return c.blockRanges(ctx, p("v1", "snapshots", snapshot, "changes"), url.Values{"base": {base}}, offset, limit)
}

func (c *Client) blockRanges(ctx context.Context, path string, query url.Values, offset int64, limit int) (*BlockRanges, error) {
	if offset > 0 {
		query.Set("offset", strconv.FormatInt(offset, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	out := &BlockRanges{}
	if err := c.request(ctx, http.MethodGet, path, query, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSnapshot snapshots a volume. Asking again with the same settings returns the existing one.
func (c *Client) CreateSnapshot(ctx context.Context, in SnapshotCreate) (*Snapshot, error) {
	return send[Snapshot](ctx, c, http.MethodPost, "/v1/snapshots", in)
}

// DeleteSnapshot removes a snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodDelete, p("v1", "snapshots", name), nil, nil, nil)
}
