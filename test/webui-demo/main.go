// Command webui-demo runs the real API and Web UI against a fake target, with sample data, so that the interface can be seen and tested
// with no root, no kernel modules and no VM. Nothing it does reaches a real target or disk: its files are in a temporary directory.
//
//	go run ./test/webui-demo [-listen 127.0.0.1:8421] [-token TOKEN] [-users FILE] [-volumes N]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"nvweaver/internal/api"
	"nvweaver/internal/auth"
	"nvweaver/internal/client"
	"nvweaver/internal/target"
	"nvweaver/internal/target/targettest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8421", "address to listen on")
	token := flag.String("token", "", "require this API token")
	volumes := flag.Int("volumes", 12, "how many volumes to make, to see how it copes with many")
	users := flag.String("users", "", "a users file, for signing in with a password")
	dir := flag.String("dir", "", "where to keep its files, which needs reflink for pools. Defaults to a temporary directory here")
	flag.Parse()
	if err := run(*listen, *token, *users, *volumes, *dir); err != nil {
		fmt.Fprintln(os.Stderr, "webui-demo:", err)
		os.Exit(1)
	}
}

func run(listen, token, users string, volumes int, dir string) error {
	if dir == "" {
		// Here, rather than in /tmp, which is often tmpfs, where reflink pools don't work.
		d, err := os.MkdirTemp(".", ".webui-demo-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(d) }()
		dir = d
	}
	// The API refuses a relative path for a pool.
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	data, pool, archive := dir+"/data", dir+"/pool", dir+"/archive"
	for _, d := range []string{data, pool, archive} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m, err := api.NewManager(&target.Target{FS: targettest.New(), Root: "/nvmet"}, data, log)
	if err != nil {
		return err
	}
	srv := api.NewServer(m, token)
	srv.Version = "demo"
	if srv.Auth, err = auth.New(users, log); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: srv.Handler()}
	errs := make(chan error, 1)
	go func() { errs <- server.Serve(listener) }()

	// With only a users file, the sample data can't be made through the API, so that needs a token too.
	c, err := client.New(client.Options{URL: "http://" + listen, Token: token})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := seed(ctx, c, pool, archive, volumes); err != nil {
		return fmt.Errorf("sample data: %w", err)
	}
	fmt.Printf("Demo running, with a fake target: http://%s/\n", listen)
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return server.Close()
	}
}

func seed(ctx context.Context, c *client.Client, poolDir, archiveDir string, volumes int) error {
	const prefix = "nqn.2026-11.net.example:"
	first, second := 1, 2
	if _, err := c.CreatePort(ctx, client.PortCreate{ID: &first, Address: "192.0.2.10", Service: 4420}); err != nil {
		return err
	}
	if _, err := c.CreatePort(ctx, client.PortCreate{ID: &second, Address: "198.51.100.10", Service: 4420}); err != nil {
		return err
	}
	hosts := []string{prefix + "node-a", prefix + "node-b", prefix + "node-c"}
	for _, h := range hosts {
		if _, err := c.CreateHost(ctx, client.HostCreate{NQN: h}); err != nil {
			return err
		}
	}
	// A label with markup in it, so that the browser test can check that it stays text.
	if _, err := c.CreatePool(ctx, client.PoolCreate{Name: "fast", Type: "reflink", Path: poolDir, Labels: map[string]string{
		"tier": "nvme", "note": "<img src=x onerror=window.pwned=true>",
	}}); err != nil {
		return err
	}
	for i := 1; i <= volumes; i++ {
		name := fmt.Sprintf("vol-%04d", i)
		vol, err := c.CreateVolume(ctx, client.VolumeCreate{Name: name, Pool: "fast", SizeBytes: int64(i%8+1) << 30, Labels: map[string]string{"app": "demo"}})
		if err != nil {
			return err
		}
		if i <= 4 {
			// Some real data, so that they don't all show as empty.
			if err := writeSome(vol.DevicePath, int64(i)*32<<20); err != nil {
				return err
			}
			nqn := prefix + name
			if _, err := c.CreateSubsystem(ctx, client.SubsystemCreate{NQN: nqn, AllowedHosts: hosts[:i%len(hosts)+1], Ports: []int{1, 2}[:i%2+1]}); err != nil {
				return err
			}
			if _, err := c.CreateNamespace(ctx, nqn, client.NamespaceCreate{Volume: name}); err != nil {
				return err
			}
		}
	}
	if volumes >= 2 {
		if _, err := c.CreateSnapshot(ctx, client.SnapshotCreate{Name: "vol-0001-nightly", Volume: "vol-0001"}); err != nil {
			return err
		}
		if _, err := c.CreateSnapshot(ctx, client.SnapshotCreate{Name: "vol-0002-before-upgrade", Volume: "vol-0002"}); err != nil {
			return err
		}
		if _, err := c.CreateVolume(ctx, client.VolumeCreate{Name: "vol-0001-copy", Source: &client.VolumeSource{Snapshot: "vol-0001-nightly"}}); err != nil {
			return err
		}
	}
	if _, err := c.CreateSubsystem(ctx, client.SubsystemCreate{NQN: prefix + "shared", AllowAnyHost: true, Ports: []int{1}}); err != nil {
		return err
	}
	return seedArchive(ctx, c, archiveDir, prefix, hosts[0])
}

// seedArchive makes a second pool. With more than one, a volume has to say which it's in. Both are on the same disk here, so they report the same space.
func seedArchive(ctx context.Context, c *client.Client, dir, prefix, host string) error {
	if _, err := c.CreatePool(ctx, client.PoolCreate{Name: "archive", Type: "reflink", Path: dir, Labels: map[string]string{"tier": "sata"}}); err != nil {
		return err
	}
	for i, size := range []int64{20, 50, 100} {
		name := fmt.Sprintf("backup-%02d", i+1)
		vol, err := c.CreateVolume(ctx, client.VolumeCreate{Name: name, Pool: "archive", SizeBytes: size << 30, Labels: map[string]string{"app": "backups"}})
		if err != nil {
			return err
		}
		if err := writeSome(vol.DevicePath, int64(i+1)*16<<20); err != nil {
			return err
		}
	}
	if _, err := c.CreateSnapshot(ctx, client.SnapshotCreate{Name: "backup-01-monthly", Volume: "backup-01"}); err != nil {
		return err
	}
	nqn := prefix + "backups"
	if _, err := c.CreateSubsystem(ctx, client.SubsystemCreate{NQN: nqn, AllowedHosts: []string{host}, Ports: []int{2}}); err != nil {
		return err
	}
	_, err := c.CreateNamespace(ctx, nqn, client.NamespaceCreate{Volume: "backup-01"})
	return err
}

func writeSome(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(make([]byte, size), 0); err != nil {
		_ = f.Close()
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
