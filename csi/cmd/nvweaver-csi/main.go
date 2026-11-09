// Command nvweaver-csi is nvweaver's CSI driver: the controller, the node plugin, or both.
//
// The API's address, token and CA come from NVWEAVER_API_URL, NVWEAVER_API_TOKEN
// (or NVWEAVER_API_TOKEN_FILE) and NVWEAVER_CA, as for the command line utility.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"nvweaver/csi/driver"
	"nvweaver/internal/client"
)

var version = "dev"

func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "the gRPC socket to serve CSI on")
	mode := flag.String("mode", "controller", "controller, node, or all")
	name := flag.String("driver-name", driver.DefaultName, "the driver's name in Kubernetes")
	cluster := flag.String("cluster", "k8s", "a name for this cluster, which starts every volume, snapshot and subsystem name, so that clusters can share one nvweaver")
	prefix := flag.String("nqn-prefix", "nqn.2026-11.io.github.xphyr.nvweaver:csi", "how the NQN of each subsystem the driver makes starts")
	port := flag.Int("port", 1, "the nvweaver port that volumes are offered on, unless a StorageClass sets port")
	hostNQN := flag.String("hostnqn-file", "/etc/nvme/hostnqn", "the node's NVMe host NQN, which is its node ID")
	lossTmo := flag.Int("ctrl-loss-tmo", -1, "seconds a node keeps reconnecting to nvweaver before giving its volumes up; -1 is for ever, as there's no failover")
	maxVols := flag.Int64("max-volumes-per-node", driver.MaxNamespaces, "how many volumes a node tells Kubernetes it can take")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*endpoint, *mode, *hostNQN, driver.Config{
		Name: *name, Version: version, Cluster: *cluster, NQNPrefix: *prefix, Port: *port,
		CtrlLossTmo: *lossTmo, MaxVolumesPerNode: *maxVols, Log: log,
	}, log); err != nil {
		log.Error("stopped", "err", err)
		os.Exit(1)
	}
}

func run(endpoint, mode, hostNQNFile string, cfg driver.Config, log *slog.Logger) error {
	if err := cfg.Check(); err != nil {
		return err
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(logCalls(log)))
	var api driver.API
	if mode == "controller" || mode == "all" {
		token := os.Getenv("NVWEAVER_API_TOKEN")
		if f := os.Getenv("NVWEAVER_API_TOKEN_FILE"); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return fmt.Errorf("the API token: %w", err)
			}
			token = strings.TrimSpace(string(b))
		}
		c, err := client.New(client.Options{URL: os.Getenv("NVWEAVER_API_URL"), Token: token, CAFile: os.Getenv("NVWEAVER_CA")})
		if err != nil {
			return err
		}
		api = c
		csi.RegisterControllerServer(srv, driver.NewController(cfg, api))
	}
	if mode == "node" || mode == "all" {
		n, err := driver.NewNode(cfg, hostNQNFile)
		if err != nil {
			return err
		}
		csi.RegisterNodeServer(srv, n)
	}
	if mode != "controller" && mode != "node" && mode != "all" {
		return fmt.Errorf("mode %q: it's controller, node or all", mode)
	}
	csi.RegisterIdentityServer(srv, driver.NewIdentity(cfg, api))

	lis, err := listen(endpoint)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	log.Info("serving", "driver", cfg.Name, "version", cfg.Version, "mode", mode, "cluster", cfg.Cluster, "endpoint", endpoint)
	return srv.Serve(lis)
}

func listen(endpoint string) (net.Listener, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" {
		return nil, fmt.Errorf("endpoint %q: it should look like unix:///csi/csi.sock", endpoint)
	}
	path := u.Path
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return net.Listen("unix", path)
}

// logCalls logs each call that changes something, and every failure, with how long it took. Secrets aren't in CSI's requests here.
func logCalls(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := h(ctx, req)
		quiet := strings.HasSuffix(info.FullMethod, "Probe") || strings.Contains(info.FullMethod, "Get")
		if err != nil {
			log.Warn("call failed", "method", info.FullMethod, "code", status.Code(err), "err", err, "took", time.Since(start).Round(time.Millisecond))
		} else if !quiet {
			log.Info("call", "method", info.FullMethod, "took", time.Since(start).Round(time.Millisecond))
		}
		return resp, err
	}
}
