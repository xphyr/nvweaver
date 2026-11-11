package driver

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Whether a node can be paused, for a snapshot group: nvweaver pauses hosts' I/O with NVMe ANA while it takes one, and
// only a host with native NVMe multipath waits; one without it gets I/O errors. Each node plugin finds out, and records
// the answer in a Lease of its own, in the driver's namespace, which the controller reads before taking a group. A
// Lease, rather than a label on the Node, as patching Nodes would let the node plugin change any node's labels, taints
// and more. See decision 56.

// nvmeMultipath is where the kernel says whether native NVMe multipath is on.
const nvmeMultipath = "/sys/module/nvme_core/parameters/multipath"

// nvmePausable reports whether this node waits out an ANA pause, and if not, why not.
func nvmePausable(path string) (bool, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Sprintf("can't tell whether native NVMe multipath is on: %v", err)
	}
	if v := strings.TrimSpace(string(b)); v != "Y" {
		return false, "native NVMe multipath is off (nvme_core.multipath=" + v + "), so it would get I/O errors while a snapshot group pauses it"
	}
	return true, ""
}

func leaseName(node string) string { return "nvweaver-node-" + node }

// pauseLabel and pauseReason are the Lease's label and annotation, per protocol: only NVMe, so far.
func (c Config) pauseLabel() string  { return c.Name + "/pause-nvme" }
func (c Config) pauseReason() string { return c.Name + "/pause-nvme-reason" }

type lease struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace,omitempty"`
		ResourceVersion string            `json:"resourceVersion,omitempty"`
		Labels          map[string]string `json:"labels,omitempty"`
		Annotations     map[string]string `json:"annotations,omitempty"`
	} `json:"metadata"`
	Spec struct {
		HolderIdentity string `json:"holderIdentity,omitempty"`
		RenewTime      string `json:"renewTime,omitempty"`
	} `json:"spec"`
}

func (k *kube) leasePath(name string) string {
	p := "/apis/coordination.k8s.io/v1/namespaces/" + k.namespace + "/leases"
	if name != "" {
		p += "/" + name
	}
	return p
}

// reportPausable writes this node's Lease, saying whether it can be paused.
func (k *kube) reportPausable(ctx context.Context, cfg Config, node string, ok bool, reason string) error {
	var l lease
	l.APIVersion, l.Kind = "coordination.k8s.io/v1", "Lease"
	l.Metadata.Name, l.Metadata.Namespace = leaseName(node), k.namespace
	// The node's name is in holderIdentity, not a label, whose values can't be as long as node names can.
	l.Metadata.Labels = map[string]string{cfg.pauseLabel(): fmt.Sprint(ok)}
	if reason != "" {
		l.Metadata.Annotations = map[string]string{cfg.pauseReason(): reason}
	}
	l.Spec.HolderIdentity = node
	l.Spec.RenewTime = time.Now().UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	err := k.do(ctx, http.MethodPost, k.leasePath(""), l, nil)
	if !isKubeStatus(err, http.StatusConflict) {
		return err
	}
	var cur lease
	if err := k.do(ctx, http.MethodGet, k.leasePath(l.Metadata.Name), nil, &cur); err != nil {
		return err
	}
	l.Metadata.ResourceVersion = cur.Metadata.ResourceVersion
	return k.do(ctx, http.MethodPut, k.leasePath(l.Metadata.Name), l, nil)
}

// PauseReader says whether a node can be paused, and if not, why not.
type PauseReader func(ctx context.Context, node string) (bool, string, error)

func (k *kube) pausable(cfg Config) PauseReader {
	return func(ctx context.Context, node string) (bool, string, error) {
		var l lease
		err := k.do(ctx, http.MethodGet, k.leasePath(leaseName(node)), nil, &l)
		if isKubeStatus(err, http.StatusNotFound) {
			return false, "its node plugin hasn't said whether it can be paused: is it running this version?", nil
		}
		if err != nil {
			return false, "", err
		}
		if l.Metadata.Labels[cfg.pauseLabel()] == "true" {
			return true, "", nil
		}
		if r := l.Metadata.Annotations[cfg.pauseReason()]; r != "" {
			return false, r, nil
		}
		return false, "its node plugin says it can't be paused", nil
	}
}

// InClusterPausable is the controller's PauseReader.
func InClusterPausable(cfg Config) (PauseReader, error) {
	k, err := inCluster()
	if err != nil {
		return nil, err
	}
	return k.pausable(cfg), nil
}

// ReportPausable runs on a node until ctx is done: it says whether the node can be paused, again every hour in case the
// Lease was removed, and every minute until it has said so once.
func ReportPausable(ctx context.Context, cfg Config, node string) {
	k, err := inCluster()
	if err != nil {
		cfg.Log.Info("not reporting whether this node can be paused for snapshot groups", "error", err)
		return
	}
	ok, reason := nvmePausable(nvmeMultipath)
	if !ok {
		cfg.Log.Warn("snapshot groups of volumes on this node will be refused", "node", node, "reason", reason)
	}
	for {
		wait := time.Hour
		if err := k.reportPausable(ctx, cfg, node, ok, reason); err != nil {
			cfg.Log.Warn("can't say whether this node can be paused for snapshot groups", "node", node, "error", err)
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
