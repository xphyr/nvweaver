package spdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	startTimeout = 60 * time.Second
	stopTimeout  = 15 * time.Second
)

// Options configures the nvmf_tgt process.
type Options struct {
	Binary string
	// Cores is an SPDK CPU list or mask. Empty picks one CPU this process may
	// use, because every core SPDK is given can spin at 100%.
	Cores string
	// MemoryMB is the memory SPDK reserves for its buffers.
	MemoryMB int
	// Hugepages uses the host's hugepages instead of ordinary memory, which
	// requires them to be reserved on the host first.
	Hugepages bool
	// Polling makes the target poll continuously, for lower latency at the cost
	// of a core. Otherwise it sleeps until there is work.
	Polling bool
	// Durability is target.DurabilityFUA when Binary is nvmf_tgt with nvweaver's patch, which makes FUA writes
	// durable itself; otherwise files get the sync attribute. See decision 58.
	Durability string
}

// Args returns the nvmf_tgt command line for the given RPC socket.
func (o Options) Args(socket, cores string) []string {
	args := []string{
		"-r", socket,
		"-m", cores,
		"-s", strconv.Itoa(o.MemoryMB),
		"--no-pci", // Storage goes through the kernel, never a userspace driver.
		"--wait-for-rpc",
		"--num-trace-entries", "0",
	}
	if !o.Hugepages {
		args = append(args, "--no-huge")
	}
	if !o.Polling {
		args = append(args, "--interrupt-mode")
	}
	return args
}

type process struct {
	cmd    *exec.Cmd
	client *client
	runDir string
}

// Start runs nvmf_tgt and returns a backend once it is ready to configure.
func Start(o Options, log *slog.Logger) (*Backend, error) {
	cores := o.Cores
	if cores == "" {
		c, err := lastAllowedCPU()
		if err != nil {
			return nil, fmt.Errorf("choose a CPU for SPDK: %w", err)
		}
		cores = "[" + strconv.Itoa(c) + "]"
	}
	runDir, err := os.MkdirTemp("", "nvweaver-spdk-")
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(runDir, "spdk.sock")
	args := o.Args(socket, cores)
	cmd := exec.Command(o.Binary, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// nvmf_tgt must not outlive this process, which would strand the target.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	log.Info("starting SPDK", "binary", o.Binary, "args", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		os.RemoveAll(runDir)
		return nil, fmt.Errorf("start %s: %w", o.Binary, err)
	}
	done := make(chan struct{})
	go func() {
		err := cmd.Wait()
		log.Info("SPDK exited", "result", err)
		close(done)
	}()
	p := &process{cmd: cmd, runDir: runDir}
	b := &Backend{proc: p, keyDir: runDir, probe: probeListen, done: done, durability: o.Durability}
	if err := b.initialise(socket, done); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func (b *Backend) initialise(socket string, done <-chan struct{}) error {
	deadline := time.Now().Add(startTimeout)
	for b.rpc == nil {
		select {
		case <-done:
			return errors.New("nvmf_tgt exited during startup; see its messages above " +
				"(it may need more memory: NVWEAVER_SPDK_MEM_MB)")
		default:
		}
		c, err := dial(socket)
		if err == nil {
			b.proc.client = c
			b.rpc = c
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nvmf_tgt did not open %s: %w", socket, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// List only the subsystems on the port the host asked, as the kernel does.
	// SPDK's default would list every subsystem on every port.
	err := b.rpc.Call("nvmf_set_config", map[string]any{
		"discovery_filters": []string{"transport", "address", "svcid"},
	}, nil)
	if err != nil {
		return err
	}
	// SPDK's default of 1,024 stops at 1,023 volumes with one subsystem each, as its discovery subsystem counts too.
	if err := b.rpc.Call("nvmf_set_max_subsystems", map[string]any{"max_subsystems": maxSubsystems + 1}, nil); err != nil {
		return err
	}
	if err := b.rpc.Call("framework_start_init", nil, nil); err != nil {
		return err
	}
	if err := b.rpc.Call("framework_wait_init", nil, nil); err != nil {
		return err
	}
	var reactors struct {
		Reactors []json.RawMessage `json:"reactors"`
	}
	if err := b.rpc.Call("framework_get_reactors", nil, &reactors); err != nil {
		return err
	}
	b.reactors, b.aioLimit = len(reactors.Reactors), readAIOLimit
	return b.rpc.Call("nvmf_create_transport", map[string]any{"trtype": "TCP"}, nil)
}

// Close implements target.Backend.
func (b *Backend) Close() error {
	if b.proc == nil {
		return nil
	}
	defer os.RemoveAll(b.proc.runDir)
	if b.proc.client != nil {
		b.proc.client.close()
	}
	_ = b.proc.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-b.done:
		return nil
	case <-time.After(stopTimeout):
		_ = b.proc.cmd.Process.Kill()
		<-b.done
		return errors.New("nvmf_tgt did not stop in time and was killed")
	}
}

// lastAllowedCPU picks the last CPU in this process's affinity mask: the first
// is usually busiest with interrupts, and it may not be CPU 0 under a cpuset.
func lastAllowedCPU() (int, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			return parseLastCPU(v)
		}
	}
	return 0, errors.New("no Cpus_allowed_list in /proc/self/status")
}

// parseLastCPU returns the highest CPU in a list such as "0-3,8".
func parseLastCPU(list string) (int, error) {
	parts := strings.Split(strings.TrimSpace(list), ",")
	last := parts[len(parts)-1]
	if _, hi, ok := strings.Cut(last, "-"); ok {
		last = hi
	}
	return strconv.Atoi(last)
}
