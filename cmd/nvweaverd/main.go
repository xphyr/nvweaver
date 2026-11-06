// Command nvweaverd runs an NVMe over TCP target. The kernel's nvmet handles the
// protocol and all I/O where it is available, with SPDK as a fallback where it
// isn't; this process manages the configuration of whichever is in use.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"nvweaver/internal/api"
	"nvweaver/internal/sdnotify"
	"nvweaver/internal/spdk"
	"nvweaver/internal/target"
)

const configfsMount = "/sys/kernel/config"

// version is set at build time, with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("nvweaverd " + version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func env(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}

func run(log *slog.Logger) error {
	listen := env("NVWEAVER_LISTEN", "127.0.0.1:8420")
	token := env("NVWEAVER_API_TOKEN", "")
	dataDir := env("NVWEAVER_DATA_DIR", "/var/lib/nvweaver")
	keepOnExit := env("NVWEAVER_KEEP_ON_EXIT", "false") == "true"

	if err := checkExposure(listen, token); err != nil {
		return err
	}
	backend, err := selectBackend(log)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := backend.Close(); cerr != nil {
			// Not lost data: hosts' writes are finished before they're acknowledged. At thousands of volumes, SPDK can
			// take longer than this allows to clean up what's about to go with its process anyway.
			log.Warn("backend did not stop cleanly", "error", cerr)
		}
	}()
	if keepOnExit && !backend.Capabilities().SurvivesServiceExit {
		return fmt.Errorf("NVWEAVER_KEEP_ON_EXIT cannot work with the %s backend, "+
			"whose data path stops with the container", backend.Name())
	}
	log.Info("target backend ready", "backend", backend.Name(), "version", version)
	mgr, err := api.NewManager(backend, dataDir, log)
	if err != nil {
		return err
	}
	mgr.Recover()

	apiServer := api.NewServer(mgr, token)
	apiServer.Version = version
	srv := &http.Server{
		Addr:              listen,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Listen before telling systemd we're ready, so that anything ordered after
	// this service finds both the target and the API usable.
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("API: %w", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	log.Info("API listening", "address", listen, "authenticated", token != "")
	notify(log, "READY=1\nSTATUS=Serving with the "+backend.Name()+" backend")

	select {
	case err = <-serveErr:
	case <-backend.Done():
		// Exiting lets the container's restart policy recover: the next start
		// restores the saved configuration, and hosts reconnect by themselves.
		return fmt.Errorf("the %s target stopped unexpectedly", backend.Name())
	case <-ctx.Done():
		notify(log, "STOPPING=1")
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = srv.Shutdown(shutdown)
	}

	// The kernel target outlives this process. By default stopping the
	// container stops the service; keeping it allows upgrades without
	// dropping hosts, because the next instance adopts the running target.
	switch {
	case keepOnExit:
		log.Info("leaving kernel target running", "reason", "NVWEAVER_KEEP_ON_EXIT=true")
	case !backend.Capabilities().SurvivesServiceExit:
		// Its configuration goes with its process. Taking it apart first took minutes at 5,000 volumes.
		log.Info("target stops with the service", "backend", backend.Name())
	default:
		if terr := mgr.Teardown(); terr != nil {
			log.Error("teardown incomplete", "error", terr)
			err = errors.Join(err, terr)
		} else {
			log.Info("target configuration removed")
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func notify(log *slog.Logger, state string) {
	if err := sdnotify.Send(state); err != nil {
		log.Warn("could not notify systemd", "error", err)
	}
}

// checkExposure refuses an unauthenticated API off the local machine: it can
// export any block device on the host to the network.
func checkExposure(listen, token string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("NVWEAVER_LISTEN %q: %w", listen, err)
	}
	ip := net.ParseIP(host)
	if token == "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("refusing to serve the API on %s without NVWEAVER_API_TOKEN set", listen)
	}
	return nil
}

// selectBackend honours NVWEAVER_BACKEND. With auto, the kernel wins wherever it
// works, because it uses no CPU while idle; SPDK is only for hosts without it.
func selectBackend(log *slog.Logger) (target.Backend, error) {
	kernel := func() (target.Backend, error) {
		if err := prepareKernel(log); err != nil {
			return nil, err
		}
		return &target.Target{FS: target.OSConfigFS{}, Root: filepath.Join(configfsMount, "nvmet")}, nil
	}
	switch choice := env("NVWEAVER_BACKEND", "auto"); choice {
	case "nvmet":
		return kernel()
	case "spdk":
		return startSPDK(log)
	case "auto":
		b, err := kernel()
		if err == nil {
			return b, nil
		}
		if _, lookErr := exec.LookPath(spdkBinary); lookErr != nil {
			return nil, err
		}
		log.Warn("kernel target unavailable, falling back to SPDK", "reason", err)
		return startSPDK(log)
	default:
		return nil, fmt.Errorf("NVWEAVER_BACKEND %q must be auto, nvmet or spdk", choice)
	}
}

const spdkBinary = "nvmf_tgt"

func startSPDK(log *slog.Logger) (target.Backend, error) {
	bin, err := exec.LookPath(spdkBinary)
	if err != nil {
		return nil, fmt.Errorf("the SPDK backend needs %s, which is in the image built from Containerfile.spdk: %w", spdkBinary, err)
	}
	mem, err := strconv.Atoi(env("NVWEAVER_SPDK_MEM_MB", "1024"))
	if err != nil || mem <= 0 {
		return nil, fmt.Errorf("NVWEAVER_SPDK_MEM_MB must be a positive number of megabytes")
	}
	return spdk.Start(spdk.Options{
		Binary:    bin,
		Cores:     env("NVWEAVER_SPDK_CORES", ""),
		MemoryMB:  mem,
		Hugepages: env("NVWEAVER_SPDK_HUGEPAGES", "false") == "true",
		Polling:   env("NVWEAVER_SPDK_POLLING", "false") == "true",
	}, log)
}

// prepareKernel makes sure nvmet-tcp is loaded and configfs is mounted, with
// errors that say what to do about it.
func prepareKernel(log *slog.Logger) error {
	if _, err := os.Stat("/sys/module/nvmet_tcp"); err != nil {
		out, err := exec.Command("modprobe", "nvmet-tcp").CombinedOutput()
		if err != nil {
			return fmt.Errorf("could not load nvmet-tcp: %v: %s\n"+
				"This service needs the kernel's NVMe target, which is a separate module from the\n"+
				"nvme-tcp initiator. Run the container privileged with /lib/modules bind-mounted,\n"+
				"or run 'modprobe nvmet-tcp' on the host. If the host has no such module (see\n"+
				"'modinfo nvmet-tcp'), its kernel cannot run this service",
				err, strings.TrimSpace(string(out)))
		}
		log.Info("loaded kernel module", "module", "nvmet-tcp")
	}
	nvmetDir := filepath.Join(configfsMount, "nvmet")
	if _, err := os.Stat(nvmetDir); err == nil {
		return nil
	}
	if err := syscall.Mount("configfs", configfsMount, "configfs", 0, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("mount configfs on %s: %w (the container must run privileged)", configfsMount, err)
	}
	if _, err := os.Stat(nvmetDir); err != nil {
		return fmt.Errorf("nvmet is loaded but %s is missing: %w", nvmetDir, err)
	}
	return nil
}
