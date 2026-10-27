package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

// StartAllVMs boots VMs in dependency order with stagger delay.
func StartAllVMs(ctx context.Context, cfg *config.Config, l *libvirt.Client, stagger time.Duration) error {
	timer := logging.PhaseStart("Start All VMs")
	defer timer.End()

	var order []string
	order = append(order, cfg.Nodes.Router.Name)
	order = append(order, cfg.Nodes.Jumpstart.Name)
	order = append(order, cfg.Nodes.Monitor.Name)
	for _, n := range cfg.Nodes.Masters {
		order = append(order, n.Name)
	}
	for _, n := range cfg.Nodes.Workers {
		order = append(order, n.Name)
	}
	order = append(order, cfg.Nodes.Storage.Name)
	for _, lb := range cfg.Nodes.LBs {
		order = append(order, lb.Name)
	}
	for _, n := range cfg.Nodes.CMSFrontends {
		order = append(order, n.Name)
	}
	for _, hd := range cfg.HotdeskSpecs() {
		order = append(order, hd.Name)
	}

	states, err := l.DomainStates(ctx)
	if err != nil {
		return fmt.Errorf("list domains: %w", err)
	}

	for _, vmName := range order {
		if vmName == "" {
			continue
		}
		state, exists := states[vmName]
		if !exists {
			logging.Warn("VM %s does not exist, skipping", vmName)
			continue
		}
		if state != "running" {
			logging.Info("Starting VM: %s", vmName)
			if err := l.Start(ctx, vmName); err != nil {
				return fmt.Errorf("start VM %s: %w", vmName, err)
			} else {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(stagger):
				}
			}
		}
	}
	return nil
}

// StopOptions contains options for shutting down cluster VMs.
type StopOptions struct {
	Force   bool          // If true, forces destruction (kill) if graceful ACPI shutdown fails
	Timeout time.Duration // Graceful shutdown wait timeout (defaults to 30s)
}

// StopAllVMs shuts down all cluster VMs gracefully in reverse dependency order.
func StopAllVMs(ctx context.Context, cfg *config.Config, l *libvirt.Client, opts StopOptions) error {
	timer := logging.PhaseStart("Stop All VMs")
	defer timer.End()

	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}

	var order []string
	for _, hd := range cfg.HotdeskSpecs() {
		order = append(order, hd.Name)
	}
	for i := len(cfg.Nodes.CMSFrontends) - 1; i >= 0; i-- {
		order = append(order, cfg.Nodes.CMSFrontends[i].Name)
	}
	for _, lb := range cfg.Nodes.LBs {
		order = append(order, lb.Name)
	}
	order = append(order, cfg.Nodes.Storage.Name)
	for i := len(cfg.Nodes.Workers) - 1; i >= 0; i-- {
		order = append(order, cfg.Nodes.Workers[i].Name)
	}
	for i := len(cfg.Nodes.Masters) - 1; i >= 0; i-- {
		order = append(order, cfg.Nodes.Masters[i].Name)
	}
	order = append(order, cfg.Nodes.Monitor.Name)
	order = append(order, cfg.Nodes.Jumpstart.Name)
	order = append(order, cfg.Nodes.Router.Name)

	var runningVMs []string
	pausedVMs := make(map[string]bool)
	states, err := l.DomainStates(ctx)
	if err != nil {
		return fmt.Errorf("list domains: %w", err)
	}
	for _, vmName := range order {
		if vmName == "" {
			continue
		}
		state, ok := states[vmName]
		if !ok {
			continue
		}
		if state == "running" || state == "paused" {
			runningVMs = append(runningVMs, vmName)
			if state == "paused" {
				pausedVMs[vmName] = true
			}
		}
	}

	if len(runningVMs) == 0 {
		logging.Success("No running VMs to stop")
		return nil
	}

	logging.Info("Sending graceful ACPI shutdown signals to %d running VMs...", len(runningVMs))
	for _, vmName := range runningVMs {
		// A paused domain cannot process an ACPI event, so resume it first.
		if pausedVMs[vmName] {
			logging.Info("Resuming paused VM before shutdown: %s", vmName)
			if err := l.Resume(ctx, vmName); err != nil {
				logging.Warn("Could not resume %s: %v", vmName, err)
			}
		}
		logging.Info("Shutting down VM: %s", vmName)
		if err := l.Shutdown(ctx, vmName); err != nil {
			logging.Warn("Could not send ACPI shutdown to %s: %v", vmName, err)
		}
	}

	// Wait for VMs to reach "shut off" state, polling until the deadline.
	logging.Info("Waiting up to %v for VMs to shut down...", opts.Timeout)
	deadline := time.Now().Add(opts.Timeout)
	var remaining []string

	for time.Now().Before(deadline) {
		remaining = remaining[:0]
		states, err := l.DomainStates(ctx)
		if err != nil {
			logging.Warn("could not list domain states: %v", err)
		}
		for _, vmName := range runningVMs {
			state, ok := states[vmName]
			if !ok || (state != "shut off" && state != "crashed") {
				remaining = append(remaining, vmName)
			}
		}
		if len(remaining) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}

	if len(remaining) > 0 {
		if opts.Force {
			logging.Warn("VMs did not shut down within timeout; forcing power-off (--force): %s", strings.Join(remaining, ", "))
			for _, vmName := range remaining {
				if err := l.Destroy(ctx, vmName); err != nil {
					logging.Warn("Could not force destroy %s: %v", vmName, err)
				} else {
					logging.Success("Force stopped: %s", vmName)
				}
			}
		} else {
			return fmt.Errorf("some VMs did not shut down gracefully within %v (use --force to kill): %s", opts.Timeout, strings.Join(remaining, ", "))
		}
	}

	logging.Success("All VMs stopped successfully")
	return nil
}

// ShrinkVMRAM resizes all VMs to production sizing.
func ShrinkVMRAM(ctx context.Context, cfg *config.Config, l *libvirt.Client) error {
	timer := logging.PhaseStart("Shrink VM RAM")
	defer timer.End()

	vms := make(map[string]int)
	vms[cfg.Nodes.Router.Name] = cfg.Nodes.Router.RAMFinalMB
	vms[cfg.Nodes.Jumpstart.Name] = cfg.Nodes.Jumpstart.RAMFinalMB
	vms[cfg.Nodes.Monitor.Name] = cfg.Nodes.Monitor.RAMFinalMB

	for _, n := range cfg.Nodes.Masters {
		vms[n.Name] = n.RAMFinalMB
	}
	for _, n := range cfg.Nodes.Workers {
		vms[n.Name] = n.RAMFinalMB
	}
	vms[cfg.Nodes.Storage.Name] = cfg.Nodes.Storage.RAMFinalMB
	for _, lb := range cfg.Nodes.LBs {
		vms[lb.Name] = lb.RAMFinalMB
	}
	for _, n := range cfg.Nodes.CMSFrontends {
		vms[n.Name] = n.RAMFinalMB
	}
	for _, hd := range cfg.HotdeskSpecs() {
		vms[hd.Name] = hd.RAMFinalMB
	}

	states, err := l.DomainStates(ctx)
	if err != nil {
		return fmt.Errorf("list domains: %w", err)
	}

	// Sort the target names so the operation is deterministic, and refuse to
	// start if any VM is running: checking every VM first avoids shrinking only
	// some of them before aborting (map iteration order made that random).
	names := make([]string, 0, len(vms))
	for name := range vms {
		if name == "" {
			continue
		}
		if state, ok := states[name]; ok && state == "running" {
			return fmt.Errorf("VM %s is running; shut it down before shrinking RAM", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var errs []error
	for _, vm := range names {
		if _, ok := states[vm]; !ok {
			continue
		}
		kib := int64(vms[vm] * 1024)
		if err := l.SetMaxMemory(ctx, vm, kib); err != nil {
			errs = append(errs, fmt.Errorf("set maximum memory for %s: %w", vm, err))
			continue
		}
		if err := l.SetMemory(ctx, vm, kib); err != nil {
			errs = append(errs, fmt.Errorf("set memory for %s: %w", vm, err))
			continue
		}
		logging.Success("Shrank RAM for %s to %d MB", vm, vms[vm])
	}
	return errors.Join(errs...)
}

// FixBootOrder modifies VM XML in parallel to ensure hd boots before network.
func FixBootOrder(ctx context.Context, l *libvirt.Client, vmNames []string) error {
	timer := logging.PhaseStart("Fix Boot Order")
	defer timer.End()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	recordError := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	for _, vm := range vmNames {
		if vm == "" {
			continue
		}
		vmName := vm
		wg.Add(1)
		go func() {
			defer wg.Done()
			exists, err := l.DomainExistsChecked(ctx, vmName)
			if err != nil {
				recordError(fmt.Errorf("check domain %s: %w", vmName, err))
				return
			}
			if !exists {
				return
			}

			changed, err := normalizeBootOrder(ctx, l, vmName)
			if err != nil {
				recordError(err)
				return
			}
			if changed {
				logging.Success("Fixed boot order for %s (hd before network)", vmName)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// normalizeBootOrder rewrites a domain definition so its disk is tried before
// its NIC and reports whether it had to change anything. It only touches the
// persistent definition: a guest that is already running keeps the order it
// started with, which is why newly created guests are restarted right away
// (see CreateClientVM).
func normalizeBootOrder(ctx context.Context, l *libvirt.Client, vmName string) (bool, error) {
	xml, err := l.DumpXML(ctx, vmName)
	if err != nil {
		return false, fmt.Errorf("dump XML for %s: %w", vmName, err)
	}

	updated, changed := reorderBootElements(xml)
	if !changed {
		return false, nil
	}

	tmpFile, err := os.CreateTemp("", fmt.Sprintf("%s-boot-*.xml", vmName))
	if err != nil {
		return false, fmt.Errorf("create temporary XML for %s: %w", vmName, err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write([]byte(updated)); err != nil {
		_ = tmpFile.Close()
		return false, fmt.Errorf("write updated XML for %s: %w", vmName, err)
	}
	if err := tmpFile.Close(); err != nil {
		return false, fmt.Errorf("close updated XML for %s: %w", vmName, err)
	}

	if err := l.Define(ctx, tmpPath); err != nil {
		return false, fmt.Errorf("define updated XML for %s: %w", vmName, err)
	}
	return true, nil
}

// reorderBootElements moves the network boot entry after the disk one and
// reports whether the order had to change. A guest keeps the boot order it
// booted with, so the network-first order virt-install 4.1 leaves behind
// would make the node reinstall itself (see internal/utils/vmcreate.go).
func reorderBootElements(xml string) (string, bool) {
	const networkBoot = "<boot dev='network'/>"
	const diskBoot = "<boot dev='hd'/>"

	networkAt := strings.Index(xml, networkBoot)
	diskAt := strings.Index(xml, diskBoot)
	if networkAt < 0 || diskAt < 0 || diskAt < networkAt {
		return xml, false
	}

	// Drop the network entry and put it back right after the disk one.
	withoutNetwork := strings.Replace(xml, networkBoot, "", 1)
	return strings.Replace(withoutNetwork, diskBoot, diskBoot+"\n    "+networkBoot, 1), true
}

func RecreateFailedVMs(ctx context.Context, cfg *config.Config, l *libvirt.Client) error {
	timer := logging.PhaseStart("Recreate Failed VMs")
	defer timer.End()

	var errs []error
	states, err := l.DomainStates(ctx)
	if err != nil {
		// A libvirt failure is not evidence that the domain is missing or
		// failed: abort rather than risk destroying VMs and their disks.
		return fmt.Errorf("failed to query domains from libvirt: %w", err)
	}

	for _, node := range cfg.AllNodes() {
		if node.Name == "" || node.Name == cfg.Nodes.Jumpstart.Name {
			continue
		}
		state, exists := states[node.Name]
		if !exists {
			continue
		}
		if state != "shut off" && state != "crashed" {
			continue
		}

		logging.Info("Cleaning failed VM: %s (state: %s)", node.Name, state)
		if err := l.Destroy(ctx, node.Name); err != nil {
			errs = append(errs, fmt.Errorf("destroy %s: %w", node.Name, err))
			continue
		}
		if err := l.Undefine(ctx, node.Name); err != nil {
			errs = append(errs, fmt.Errorf("undefine %s: %w", node.Name, err))
			continue
		}
		for _, disk := range []string{node.Name + ".qcow2", node.Name + "-drbd.qcow2"} {
			diskPath := filepath.Join(cfg.VM.StorageDir, disk)
			if err := os.Remove(diskPath); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove disk %s: %w", diskPath, err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to clean up failed VMs: %w", errors.Join(errs...))
	}

	logging.Success("Failed VMs cleaned up. Run 'cms-ha phase init-vms --nodes-only' to recreate.")
	return nil
}
