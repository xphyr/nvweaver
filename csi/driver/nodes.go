package driver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"nvweaver/internal/client"
)

// The labels the driver gives each node's host in nvweaver, so that people can tell which node of which cluster it is.
const labelNode = "csi-node"

// NodesAPI is what the node janitor uses of nvweaver's API.
type NodesAPI interface {
	ListSubsystems(ctx context.Context) ([]client.Subsystem, error)
	DeleteSubsystem(ctx context.Context, nqn string) error
	GetHost(ctx context.Context, nqn string) (*client.Host, error)
	UpdateHost(ctx context.Context, nqn string, in client.HostUpdate) (*client.Host, error)
	DeleteHost(ctx context.Context, nqn string) error
}

// NodeLister returns the cluster's nodes that have the driver, by their node ID, the host NQN, to their name.
type NodeLister func(ctx context.Context) (map[string]string, error)

// Janitor keeps nvweaver's record of the cluster's nodes current: it labels each node's host with its cluster and
// Kubernetes node, and forgets a node that has left, once nothing is exported to it. See decision 54.
type Janitor struct {
	cfg   Config
	api   NodesAPI
	nodes NodeLister
	gone  map[string]int // node subsystems missed in a row, by name
}

// NewJanitor returns a janitor for this cluster's nodes.
func NewJanitor(cfg Config, api NodesAPI, nodes NodeLister) *Janitor {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Janitor{cfg: cfg, api: api, nodes: nodes, gone: map[string]int{}}
}

// Run checks every interval until ctx is done.
func (j *Janitor) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		j.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// missesBeforeForgetting is how many checks in a row a node must be missing from, so that a node being re-registered,
// which loses its CSINode for a moment, isn't taken for one that has left.
const missesBeforeForgetting = 2

// Check does one pass. It does nothing if it can't tell which nodes there are: an empty answer is more likely a broken
// one than a cluster with no nodes.
func (j *Janitor) Check(ctx context.Context) {
	log := j.cfg.Log
	nodes, err := j.nodes(ctx)
	if err != nil {
		log.Warn("node janitor: can't list the cluster's nodes, so nothing is changed", "error", err)
		return
	}
	if len(nodes) == 0 {
		log.Warn("node janitor: no node has the driver, which is more likely a fault than true, so nothing is changed")
		return
	}
	subs, err := j.api.ListSubsystems(ctx)
	if err != nil {
		log.Warn("node janitor: can't list nvweaver's subsystems", "error", err)
		return
	}
	here := map[string]string{} // node subsystem to its host NQN, for the nodes there are
	for nqn := range nodes {
		here[j.cfg.nodeSubsystem(nqn)] = nqn
	}
	prefix := j.cfg.NQNPrefix + ":" + j.cfg.Cluster + ":node-"
	seen := map[string]bool{}
	for _, sub := range subs {
		if !strings.HasPrefix(sub.NQN, prefix) {
			continue
		}
		seen[sub.NQN] = true
		if host, ok := here[sub.NQN]; ok {
			delete(j.gone, sub.NQN)
			j.label(ctx, host, nodes[host])
			continue
		}
		j.gone[sub.NQN]++
		if j.gone[sub.NQN] < missesBeforeForgetting {
			continue
		}
		if len(sub.Namespaces) > 0 {
			log.Warn("node janitor: a node that has left still has volumes exported to it, so it's kept until Kubernetes unpublishes them",
				"subsystem", sub.NQN, "volumes", len(sub.Namespaces))
			continue
		}
		j.forget(ctx, sub)
	}
	for nqn := range j.gone {
		if !seen[nqn] {
			delete(j.gone, nqn)
		}
	}
}

// label gives a node's host its cluster and node, if it doesn't have them, and if the driver may: a host made before
// hosts had owners belongs to no token, and only an admin can give it to this cluster's.
func (j *Janitor) label(ctx context.Context, nqn, node string) {
	h, err := j.api.GetHost(ctx, nqn)
	if err != nil {
		if !client.IsNotFound(err) {
			j.cfg.Log.Warn("node janitor: reading a node's host", "host", nqn, "error", err)
		}
		return
	}
	want := maps.Clone(h.Labels)
	if want == nil {
		want = map[string]string{}
	}
	want[labelCluster], want[labelNode] = j.cfg.Cluster, node
	if maps.Equal(want, h.Labels) {
		return
	}
	if _, err := j.api.UpdateHost(ctx, nqn, client.HostUpdate{Labels: &want}); err != nil {
		j.cfg.Log.Warn("node janitor: labelling a node's host; one made before hosts had owners needs an admin to give it to this cluster's token",
			"host", nqn, "node", node, "error", err)
	}
}

// forget deletes a departed node's subsystem, and then its host, which can only go once no subsystem allows it.
func (j *Janitor) forget(ctx context.Context, sub client.Subsystem) {
	log := j.cfg.Log
	if err := j.api.DeleteSubsystem(ctx, sub.NQN); err != nil && !client.IsNotFound(err) {
		log.Warn("node janitor: deleting a departed node's subsystem", "subsystem", sub.NQN, "error", err)
		return
	}
	delete(j.gone, sub.NQN)
	log.Info("node janitor: forgot a node that has left the cluster", "subsystem", sub.NQN, "hosts", sub.AllowedHosts)
	for _, host := range sub.AllowedHosts {
		if err := j.api.DeleteHost(ctx, host); err != nil && !client.IsNotFound(err) {
			// Another subsystem may allow it still, such as a shared volume's, or it may belong to no token.
			log.Warn("node janitor: deleting a departed node's host", "host", host, "error", err)
		}
	}
}

// csiNodeDriver is a driver kubelet has registered on a node, in its CSINode.
type csiNodeDriver struct {
	Name   string `json:"name"`
	NodeID string `json:"nodeID"`
}

// InClusterNodes lists the nodes that have this driver, from their CSINodes, with the pod's service account, as kubelet
// registers each node's driver and its node ID there. It uses only the standard library: see decision 40.
func InClusterNodes(driverName string) (NodeLister, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not in a Kubernetes pod: KUBERNETES_SERVICE_HOST and _PORT aren't set")
	}
	const sa = "/var/run/secrets/kubernetes.io/serviceaccount/"
	ca, err := os.ReadFile(sa + "ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificates in %sca.crt", sa)
	}
	hc := &http.Client{Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	base := "https://" + net.JoinHostPort(host, port) + "/apis/storage.k8s.io/v1/csinodes"
	return func(ctx context.Context) (map[string]string, error) {
		// Read each time, as the kubelet rotates the token.
		token, err := os.ReadFile(sa + "token")
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		next := ""
		for {
			q := url.Values{"limit": {"500"}}
			if next != "" {
				q.Set("continue", next)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
			resp, err := hc.Do(req)
			if err != nil {
				return nil, err
			}
			var list struct {
				Metadata struct {
					Continue string `json:"continue"`
				} `json:"metadata"`
				Items []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
					Spec struct {
						Drivers []csiNodeDriver `json:"drivers"`
					} `json:"spec"`
				} `json:"items"`
			}
			err = json.NewDecoder(resp.Body).Decode(&list)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("listing CSINodes: %s", resp.Status)
			}
			if err != nil {
				return nil, fmt.Errorf("listing CSINodes: %w", err)
			}
			for _, n := range list.Items {
				if i := slices.IndexFunc(n.Spec.Drivers, func(d csiNodeDriver) bool { return d.Name == driverName }); i >= 0 {
					out[n.Spec.Drivers[i].NodeID] = n.Metadata.Name
				}
			}
			if next = list.Metadata.Continue; next == "" {
				return out, nil
			}
		}
	}, nil
}
