package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"nvweaver/internal/storage"
	"nvweaver/internal/target"
)

const (
	// lvmCheckEvery is how often to look again, as the host's configuration can change with nothing here knowing.
	lvmCheckEvery = 5 * time.Minute
	// lvmCheckGap keeps a burst of changes from running LVM constantly: a change is checked this long after the last check.
	lvmCheckGap = 30 * time.Second
	// lvmWarningNames is how many devices a warning names, as there could be thousands.
	lvmWarningNames = 5
)

// WatchLVM reports, through Warnings, if this machine's LVM can read what's exported. A client's LVM on such a
// device can be activated here at boot, and a volume group with the same name as a pool's takes that pool offline.
// Only this machine's LVM configuration can prevent that, so this reports it, rather than changing that configuration.
func (m *Manager) WatchLVM(ctx context.Context) {
	m.checkLVM()
	timer := time.NewTimer(lvmCheckEvery)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			m.checkLVM()
			timer.Reset(lvmCheckEvery)
		case <-m.lvmChanged:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(lvmCheckGap)
		}
	}
}

// noteExportsChanged asks for another check. It never waits.
func (m *Manager) noteExportsChanged() {
	select {
	case m.lvmChanged <- struct{}{}:
	default:
	}
}

// Warnings lists problems with how this machine is set up, for /v1/system.
func (m *Manager) Warnings() []string {
	if w := m.warnings.Load(); w != nil {
		return *w
	}
	return []string{}
}

func (m *Manager) checkLVM() {
	st := m.state.Load()
	warnings := append(lvmWarnings(st, m.visibleToLVM, m.deviceIDs), toolWarnings(st, m.missingLVMTools)...)
	if !slices.Equal(warnings, m.Warnings()) {
		for _, w := range warnings {
			m.log.Warn(w)
		}
	}
	m.warnings.Store(&warnings)
}

// toolWarnings reports missing tools, only if something saved here needs them. That's a warning, rather than stopping the service,
// as nothing else depends on them. What the chosen backend can't run without stops it at startup.
func toolWarnings(st *target.State, missing func() []string) []string {
	for _, pool := range st.Pools {
		if pool.Type == storage.TypeLVMThin {
			return missing()
		}
	}
	return nil
}

// lvmWarnings compares what's exported with what LVM says it can read. It only asks LVM if there's a device to ask about.
func lvmWarnings(st *target.State, visible func() ([]string, error), ids func(string) []string) []string {
	exported := map[string]string{}
	var controllers []string
	for _, sub := range st.Subsystems {
		if sub.Passthru != "" {
			controllers = append(controllers, sub.Passthru)
		}
		for _, ns := range sub.Namespaces {
			for _, id := range ids(ns.DevicePath) {
				exported[id] = ns.DevicePath
			}
		}
	}
	if len(exported) == 0 && len(controllers) == 0 {
		return []string{}
	}
	devices, err := visible()
	switch {
	case errors.Is(err, storage.ErrNoLVM):
		return []string{} // With no LVM here, nothing can activate a client's.
	case err != nil:
		return []string{fmt.Sprintf(`couldn't check whether LVM on this machine can read exported devices: %v. See "Clients' LVM" in docs/pools.md`, err)}
	}
	var seen []string
	for _, device := range devices {
		matched := slices.ContainsFunc(ids(device), func(id string) bool { _, ok := exported[id]; return ok }) ||
			slices.ContainsFunc(controllers, func(c string) bool { return strings.HasPrefix(device, c+"n") })
		if matched {
			seen = append(seen, device)
		}
	}
	if len(seen) == 0 {
		return []string{}
	}
	slices.Sort(seen)
	names := strings.Join(seen[:min(len(seen), lvmWarningNames)], ", ")
	if more := len(seen) - lvmWarningNames; more > 0 {
		names += fmt.Sprintf(" and %d more", more)
	}
	return []string{fmt.Sprintf(`LVM on this machine can read exported devices: %s. If a client puts LVM on one, this machine can activate it at boot, `+
		`and a volume group with the same name as a pool's takes that pool offline. See "Clients' LVM" in docs/pools.md`, names)}
}

// deviceIDs returns a block device's major:minor, and its whole disk's if it's a partition, since a client can partition what's
// exported to it. Comparing these handles the many names one device can have. Anything else, such as a file, has none.
func deviceIDs(path string) []string {
	info, err := os.Stat(path)
	if err != nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	rdev := uint64(st.Rdev)
	id := fmt.Sprintf("%d:%d", (rdev>>8)&0xfff|(rdev>>32)&^uint64(0xfff), rdev&0xff|(rdev>>12)&^uint64(0xff))
	ids := []string{id}
	if _, err := os.Stat("/sys/dev/block/" + id + "/partition"); err == nil {
		if parent, err := os.ReadFile("/sys/dev/block/" + id + "/../dev"); err == nil {
			ids = append(ids, strings.TrimSpace(string(parent)))
		}
	}
	return ids
}
