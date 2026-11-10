// Command scale drives the CSI driver's controller, the code Kubernetes calls, against a real nvweaver, at production-like
// counts: it creates volumes, publishes them across many simulated nodes, lists, snapshots, unpublishes and deletes them,
// timing every call. Nodes are simulated, as the controller only needs their host NQNs; no node connects. See
// docs/testing.md, "The CSI driver at scale".
//
// It reaches nvweaver as the driver does, from NVWEAVER_API_URL, NVWEAVER_API_TOKEN_FILE and NVWEAVER_CA. Use a scoped
// token of its own, named as -cluster, so that it can't touch anything else. Results go to stdout, as tab-separated lines.
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"nvweaver/csi/driver"
	"nvweaver/internal/client"
)

func main() {
	volumes := flag.Int("volumes", 5000, "how many volumes")
	nodes := flag.Int("nodes", 100, "how many simulated nodes to publish them to")
	snapshots := flag.Int("snapshots", 500, "how many of them to snapshot")
	workers := flag.Int("workers", 16, "calls at once, as the CSI sidecars' workers make them")
	cluster := flag.String("cluster", "scale", "the cluster name, which must be the scoped token's")
	size := flag.Int64("size", 1<<30, "each volume's size, which thin storage doesn't allocate")
	window := flag.Int("window", 500, "report every this many calls")
	pool := flag.String("pool", "", "the nvweaver pool, if there's more than one")
	keep := flag.Bool("keep", false, "leave everything, rather than deleting it at the end")
	flag.Parse()

	token, err := os.ReadFile(os.Getenv("NVWEAVER_API_TOKEN_FILE"))
	if err != nil {
		fail("the API token: %v", err)
	}
	c, err := client.New(client.Options{URL: os.Getenv("NVWEAVER_API_URL"), Token: strings.TrimSpace(string(token)), CAFile: os.Getenv("NVWEAVER_CA")})
	if err != nil {
		fail("%v", err)
	}
	cfg := driver.Config{Name: driver.DefaultName, Version: "scale", Cluster: *cluster, NQNPrefix: "nqn.2026-11.io.github.xphyr.nvweaver:csi",
		Port: 1, CtrlLossTmo: -1, ReconnectDelay: 2, MaxVolumesPerNode: driver.MaxNamespaces, Log: slog.New(slog.DiscardHandler)}
	if err := cfg.Check(); err != nil {
		fail("%v", err)
	}
	ctl := driver.NewController(cfg, c)
	ctx := context.Background()

	cap := &csi.VolumeCapability{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}}
	params := map[string]string{}
	if *pool != "" {
		params["pool"] = *pool
	}
	name := func(i int) string { return fmt.Sprintf("pvc-%05d", i) }
	id := func(i int) string { return *cluster + "-" + name(i) }
	node := func(i int) string {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s-node-%d", *cluster, i%*nodes)))
		return fmt.Sprintf("nqn.2014-08.org.nvmexpress:uuid:%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	}

	fmt.Printf("# %s: %d volumes, %d nodes, %d snapshots, %d workers, cluster %s\n", time.Now().Format(time.RFC3339), *volumes, *nodes, *snapshots, *workers, *cluster)
	fmt.Println("phase\tdone\tseconds\tper_second\tp50_ms\tp95_ms\tmax_ms\terrors\tfirst_error")
	total := time.Now()

	run("create", *volumes, *workers, *window, func(i int) error {
		_, err := ctl.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: name(i), CapacityRange: &csi.CapacityRange{RequiredBytes: *size},
			VolumeCapabilities: []*csi.VolumeCapability{cap}, Parameters: params})
		return err
	})
	run("publish", *volumes, *workers, *window, func(i int) error {
		_, err := ctl.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: id(i), NodeId: node(i), VolumeCapability: cap,
			VolumeContext: map[string]string{"port": "1"}})
		return err
	})
	run("list", 1, 1, 1, func(int) error {
		n, token := 0, ""
		for {
			r, err := ctl.ListVolumes(ctx, &csi.ListVolumesRequest{MaxEntries: 500, StartingToken: token})
			if err != nil {
				return err
			}
			n += len(r.Entries)
			if token = r.NextToken; token == "" {
				break
			}
		}
		if n != *volumes {
			return fmt.Errorf("listed %d volumes, not %d", n, *volumes)
		}
		return nil
	})
	run("snapshot", *snapshots, *workers, *window, func(i int) error {
		_, err := ctl.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{Name: "snap-" + name(i), SourceVolumeId: id(i)})
		return err
	})
	if *keep {
		fmt.Printf("# kept everything, after %.0f s\n", time.Since(total).Seconds())
		return
	}
	run("delete-snapshot", *snapshots, *workers, *window, func(i int) error {
		_, err := ctl.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: *cluster + "-snap-" + name(i)})
		return err
	})
	run("unpublish", *volumes, *workers, *window, func(i int) error {
		_, err := ctl.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: id(i), NodeId: node(i)})
		return err
	})
	run("delete", *volumes, *workers, *window, func(i int) error {
		_, err := ctl.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id(i)})
		return err
	})
	// The driver keeps each node's subsystem and host for its next volume, so remove them as an operator would when the
	// nodes leave. Only this cluster's subsystems match the prefix.
	subs, err := c.ListSubsystems(ctx)
	if err != nil {
		fail("listing subsystems: %v", err)
	}
	prefix := cfg.NQNPrefix + ":" + *cluster + ":node-"
	subs = slices.DeleteFunc(subs, func(s client.Subsystem) bool { return !strings.HasPrefix(s.NQN, prefix) })
	run("forget-subsystems", len(subs), *workers, *window, func(i int) error { return c.DeleteSubsystem(ctx, subs[i].NQN) })
	// Hosts aren't owned, so only an admin may delete them: see TODO.md.
	f := os.Getenv("NVWEAVER_ADMIN_TOKEN_FILE")
	if f == "" {
		fmt.Printf("# left the %d simulated hosts, as NVWEAVER_ADMIN_TOKEN_FILE isn't set\n", *nodes)
		fmt.Printf("# all phases in %.0f s\n", time.Since(total).Seconds())
		return
	}
	token, err = os.ReadFile(f)
	if err != nil {
		fail("the admin token: %v", err)
	}
	admin, err := client.New(client.Options{URL: os.Getenv("NVWEAVER_API_URL"), Token: strings.TrimSpace(string(token)), CAFile: os.Getenv("NVWEAVER_CA")})
	if err != nil {
		fail("%v", err)
	}
	run("forget-hosts", *nodes, *workers, *window, func(i int) error {
		if err := admin.DeleteHost(ctx, node(i)); err != nil && !client.IsNotFound(err) {
			return err
		}
		return nil
	})
	fmt.Printf("# all phases in %.0f s\n", time.Since(total).Seconds())
}

// run calls fn for 0..n-1 with that many workers, and prints a line of timings for each window of calls, in the order they finish.
func run(phase string, n, workers, window int, fn func(int) error) {
	var next atomic.Int64
	var mu sync.Mutex
	var took []time.Duration
	var errs int
	var firstErr string
	done := 0
	start, windowStart := time.Now(), time.Now()
	flush := func() {
		if len(took) == 0 {
			return
		}
		slices.Sort(took)
		pct := func(p float64) float64 { return float64(took[int(p*float64(len(took)-1))].Microseconds()) / 1000 }
		secs := time.Since(windowStart).Seconds()
		fmt.Printf("%s\t%d\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%d\t%s\n", phase, done, time.Since(start).Seconds(), float64(len(took))/secs,
			pct(0.5), pct(0.95), pct(1), errs, firstErr)
		took, errs, firstErr, windowStart = nil, 0, "", time.Now()
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				t := time.Now()
				err := fn(i)
				d := time.Since(t)
				mu.Lock()
				took = append(took, d)
				done++
				if err != nil {
					errs++
					if firstErr == "" {
						firstErr = strings.ReplaceAll(err.Error(), "\t", " ")
					}
				}
				if len(took) >= window {
					flush()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	flush()
	mu.Unlock()
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "scale: "+format+"\n", a...)
	os.Exit(1)
}
