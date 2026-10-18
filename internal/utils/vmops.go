package utils

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
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
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

			xml, err := l.DumpXML(ctx, vmName)
			if err != nil {
				recordError(fmt.Errorf("dump XML for %s: %w", vmName, err))
				return
			}

			if strings.Contains(xml, "<boot dev='network'/>") && strings.Contains(xml, "<boot dev='hd'/>") {
				xml = strings.Replace(xml, "<boot dev='network'/>", "", 1)
				xml = strings.Replace(xml, "<boot dev='hd'/>", "<boot dev='hd'/>\n    <boot dev='network'/>", 1)

				tmpFile, err := os.CreateTemp("", fmt.Sprintf("%s-boot-*.xml", vmName))
				if err != nil {
					recordError(fmt.Errorf("create temporary XML for %s: %w", vmName, err))
					return
				}
				tmpPath := tmpFile.Name()
				defer os.Remove(tmpPath)

				if _, err := tmpFile.Write([]byte(xml)); err != nil {
					_ = tmpFile.Close()
					recordError(fmt.Errorf("write updated XML for %s: %w", vmName, err))
					return
				}
				if err := tmpFile.Close(); err != nil {
					recordError(fmt.Errorf("close updated XML for %s: %w", vmName, err))
					return
				}

				if err := l.Define(ctx, tmpPath); err != nil {
					recordError(fmt.Errorf("define updated XML for %s: %w", vmName, err))
				} else {
					logging.Success("Fixed boot order for %s (hd before network)", vmName)
				}
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// InstallByBatches provisions VMs in batches using PXE.
func InstallByBatches(ctx context.Context, cfg *config.Config, l *libvirt.Client, s *ssh.Pool) error {
	timer := logging.PhaseStart("Install By Batches")
	defer timer.End()

	type nodeInfo struct {
		name         string
		ip           string
		ramInstallMB int
		ramFinalMB   int
	}

	// Batch 1: Network & Management
	batch1 := []nodeInfo{
		{cfg.Nodes.Router.Name, cfg.Nodes.Router.IP, cfg.Nodes.Router.RAMInstallMB, cfg.Nodes.Router.RAMFinalMB},
		{cfg.Nodes.Monitor.Name, cfg.Nodes.Monitor.IP, cfg.Nodes.Monitor.RAMInstallMB, cfg.Nodes.Monitor.RAMFinalMB},
		{cfg.Nodes.Storage.Name, cfg.Nodes.Storage.IP, cfg.Nodes.Storage.RAMInstallMB, cfg.Nodes.Storage.RAMFinalMB},
	}

	// Batch 2: K8s Masters & Workers
	var batch2 []nodeInfo
	for _, m := range cfg.Nodes.Masters {
		batch2 = append(batch2, nodeInfo{m.Name, m.IP, m.RAMInstallMB, m.RAMFinalMB})
	}
	for _, w := range cfg.Nodes.Workers {
		batch2 = append(batch2, nodeInfo{w.Name, w.IP, w.RAMInstallMB, w.RAMFinalMB})
	}

	// Batch 3: Load Balancers, CMS Frontends & Hotdesks
	var batch3 []nodeInfo
	for _, lb := range cfg.Nodes.LBs {
		batch3 = append(batch3, nodeInfo{lb.Name, lb.IP, lb.RAMInstallMB, lb.RAMFinalMB})
	}
	for _, c := range cfg.Nodes.CMSFrontends {
		batch3 = append(batch3, nodeInfo{c.Name, c.IP, c.RAMInstallMB, c.RAMFinalMB})
	}
	for _, hd := range cfg.HotdeskSpecs() {
		batch3 = append(batch3, nodeInfo{hd.Name, hd.IP, hd.RAMInstallMB, hd.RAMFinalMB})
	}

	batches := [][]nodeInfo{batch1, batch2, batch3}
	batchNames := []string{"1: Network/Management", "2: Kubernetes & DB", "3: CMS Frontends & Workstations"}

	for i, batch := range batches {
		logging.Info("Starting Batch %s...", batchNames[i])

		// 1. Boot all VMs in this batch
		var ips []string
		states, err := l.DomainStates(ctx)
		if err != nil {
			return fmt.Errorf("list domains for batch %d: %w", i+1, err)
		}
		for _, node := range batch {
			if node.name == "" || node.ip == "" {
				return fmt.Errorf("batch %d has a VM with an empty name or IP", i+1)
			}
			state, exists := states[node.name]
			if !exists {
				return fmt.Errorf("VM %s is not defined; run init-vms before installing batches", node.name)
			}
			if state != "running" {
				if err := l.Start(ctx, node.name); err != nil {
					return fmt.Errorf("start VM %s: %w", node.name, err)
				}
			}
			ips = append(ips, node.ip)
		}

		// 2. Wait for SSH across the batch
		logging.Info("Waiting for SSH on all VMs in Batch %d...", i+1)
		if err := s.WaitForAllSSH(ctx, ips, 15*time.Minute); err != nil {
			return fmt.Errorf("wait for SSH in batch %d: %w", i+1, err)
		}

		// 3. Fix boot order in parallel and signal shutdown
		var batchNamesList []string
		for _, node := range batch {
			batchNamesList = append(batchNamesList, node.name)
		}
		if err := FixBootOrder(ctx, l, batchNamesList); err != nil {
			logging.Warn("Could not fix boot order in batch %d: %v", i+1, err)
		}

		shutdownStates, err := l.DomainStates(ctx)
		if err != nil {
			return fmt.Errorf("list domains before shutdown in batch %d: %w", i+1, err)
		}
		for _, node := range batch {
			if state := shutdownStates[node.name]; state != "shut off" {
				if err := l.Shutdown(ctx, node.name); err != nil {
					return fmt.Errorf("shutdown VM %s: %w", node.name, err)
				}
			}
		}

		// Wait for all VMs in the batch to shut down, polling a single
		// `virsh list --all` snapshot per tick instead of one per VM.
		pending := make([]string, 0, len(batch))
		for _, node := range batch {
			pending = append(pending, node.name)
		}
		shutdownDeadline := time.Now().Add(30 * time.Second)
		for len(pending) > 0 && time.Now().Before(shutdownDeadline) {
			states, err := l.DomainStates(ctx)
			if err != nil {
				return fmt.Errorf("list domains while shutting down batch %d: %w", i+1, err)
			}
			var still []string
			for _, name := range pending {
				if state := states[name]; state != "shut off" {
					still = append(still, name)
				}
			}
			pending = still
			if len(pending) == 0 {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		var errMsgs []string
		for _, name := range pending {
			logging.Warn("VM %s did not shut down gracefully; forcing stop", name)
			if err := l.Destroy(ctx, name); err != nil {
				errMsgs = append(errMsgs, fmt.Sprintf("force stop %s: %v", name, err))
			}
		}
		if len(errMsgs) > 0 {
			return fmt.Errorf("failed to shut down batch %d: %s", i+1, strings.Join(errMsgs, "; "))
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		// Resize and restart all VMs in the batch with final RAM (in parallel).
		var resizeWg sync.WaitGroup
		var resizeMu sync.Mutex
		var resizeErrs []string
		for _, node := range batch {
			n := node
			resizeWg.Add(1)
			go func() {
				defer resizeWg.Done()
				kib := int64(n.ramFinalMB * 1024)
				if err := l.SetMaxMemory(ctx, n.name, kib); err != nil {
					resizeMu.Lock()
					resizeErrs = append(resizeErrs, fmt.Sprintf("set maximum memory for %s: %v", n.name, err))
					resizeMu.Unlock()
					return
				}
				if err := l.SetMemory(ctx, n.name, kib); err != nil {
					resizeMu.Lock()
					resizeErrs = append(resizeErrs, fmt.Sprintf("set memory for %s: %v", n.name, err))
					resizeMu.Unlock()
					return
				}
				if err := l.Start(ctx, n.name); err != nil {
					resizeMu.Lock()
					resizeErrs = append(resizeErrs, fmt.Sprintf("restart VM %s: %v", n.name, err))
					resizeMu.Unlock()
					return
				}
				logging.Success("Batch %d: %s resized to %d MB and running", i+1, n.name, n.ramFinalMB)
			}()
		}
		resizeWg.Wait()
		if len(resizeErrs) > 0 {
			return fmt.Errorf("failed to resize batch %d: %s", i+1, strings.Join(resizeErrs, "; "))
		}
	}

	logging.Success("All batches provisioned and resized")
	return nil
}

// RecreateFailedVMs cleans up client VMs left in a failed state (destroy,
// undefine, remove disks); creating them again is deferred to init-vms.
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
