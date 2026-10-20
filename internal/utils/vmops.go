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
// reports whether the order had to change. virt-install 4.1 emits the network
// first whenever --pxe is used, and a guest keeps the order it booted with
// across reboots, so such a guest would reinstall itself instead of booting
// the system the installer left on the disk.
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

// InstallByBatches provisions VMs in batches using PXE.
// Batch install waits. The live installer a node PXE-boots also listens on
// port 22, but by design it does not trust the deployment key: only the
// installed system does. So "port open, key refused" means the installer is
// still working, and only a successful login means the node reached its disk.
const (
	// nodePollInterval is how often a node is probed while installing.
	nodePollInterval = 5 * time.Second
	// installerGoneGrace is how long a node may stay silent, after its live
	// installer was answering, before the batch treats the installer's
	// shutdown as wedged. A node that reboots normally reopens port 22 in well
	// under this, so the restart only ever happens on a stuck one.
	installerGoneGrace = 2 * time.Minute
	// maxNodeRestarts bounds the restarts per node: a node that needs more has
	// a real problem (no bootable disk, wrong boot order) and must fail the
	// batch instead of looping.
	maxNodeRestarts = 3
)

// installBatchSSH waits for every node in a batch to answer SSH as its
// installed system.
//
// A node that stops listening right after its installer was answering is stuck
// in the installer's shutdown instead of rebooting: the live media is an NFS
// mount (nfsroot=...) whose squashfs stays loop-mounted, so /cdrom cannot be
// unmounted and the guest can wedge before it ever boots the installed system.
// No guest-side action can recover from that, but a restart can: the
// definition already boots disk-first (see FixBootOrder), so the node comes
// back straight into the installed system.
func installBatchSSH(ctx context.Context, l *libvirt.Client, s *ssh.Pool, names, ips []string, budget time.Duration) error {
	if len(names) != len(ips) {
		return fmt.Errorf("batch has %d node names and %d IPs", len(names), len(ips))
	}

	waitCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	errs := make([]error, len(ips))
	var wg sync.WaitGroup
	for i := range ips {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = waitForInstalledNode(waitCtx, l, s, names[i], ips[i])
		}(i)
	}
	wg.Wait()

	var failures []string
	for i, err := range errs {
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s): %v", names[i], ips[i], err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

// waitForInstalledNode waits for one node to reach its installed system,
// restarting it when the installer's shutdown wedges (see installBatchSSH).
func waitForInstalledNode(ctx context.Context, l *libvirt.Client, s *ssh.Pool, name, ip string) error {
	logging.Info("Waiting for %s (%s) to boot its installed system...", name, ip)

	var installerSeen time.Time
	restarts := 0

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("timed out after %d restart(s): %w", restarts, err)
		}

		if s.CanLogin(ctx, ip) {
			logging.Success("Node %s is up on %s", name, ip)
			return nil
		}

		if ssh.IsListening(ctx, ip, nodePollInterval) {
			// The live installer is answering: the install is still running.
			installerSeen = time.Now()
		} else if !installerSeen.IsZero() && time.Since(installerSeen) > installerGoneGrace {
			if restarts >= maxNodeRestarts {
				return fmt.Errorf("node went silent after its installer finished and %d restart(s) did not bring it back", restarts)
			}
			restarts++
			logging.Warn("Node %s stopped answering after its installer went away: the installer's shutdown is wedged. Restarting it to boot the installed disk (%d/%d)", name, restarts, maxNodeRestarts)
			// The installed system may present a new host key; the pinned one
			// must not make the next login fail closed.
			s.ForgetHost(ip)
			if err := restartNode(ctx, l, name); err != nil {
				logging.Warn("Could not restart %s: %v", name, err)
			}
			// A fresh boot, including its own brief silence, gets a new grace
			// window before another restart is considered.
			installerSeen = time.Now()
		}

		select {
		case <-ctx.Done():
		case <-time.After(nodePollInterval):
		}
	}
}

// restartNode hard-stops and starts a VM. A guest wedged in its own shutdown
// cannot be asked to reboot, so the host resets it instead; nothing is lost
// because the installed system is already on its disk.
func restartNode(ctx context.Context, l *libvirt.Client, name string) error {
	if err := forceStop(ctx, l, name); err != nil {
		logging.Warn("Could not force stop %s: %v", name, err)
	}
	if err := l.Start(ctx, name); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}

// forceStopGrace bounds how long forceStop waits for a domain to end up shut
// off after the force stop was issued.
const forceStopGrace = 30 * time.Second

// forceStop makes sure a domain ends up shut off, forcing it if needed.
//
// `virsh destroy` is not idempotent: it fails on a domain that is already off
// and, on some libvirt versions, on one that is still finishing an ACPI
// shutdown ("domain is being shut down"). Neither is a real failure, so after
// an error the state is re-checked rather than trusted.
func forceStop(ctx context.Context, l *libvirt.Client, name string) error {
	deadline := time.Now().Add(forceStopGrace)
	var lastErr error

	for {
		if err := l.Destroy(ctx, name); err != nil {
			lastErr = err
		} else {
			return nil
		}

		if states, err := l.DomainStates(ctx); err == nil && states[name] == "shut off" {
			return nil
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("force stop %s: %w", name, lastErr)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("force stop %s: %w", name, lastErr)
		case <-time.After(2 * time.Second):
		}
	}
}

func InstallByBatches(ctx context.Context, cfg *config.Config, l *libvirt.Client, s *ssh.Pool) error {
	timer := logging.PhaseStart("Install By Batches")
	defer timer.End()

	// Every node in a batch installs at the same time and they all share the
	// host's vCPUs, so the budget for a batch to finish and answer SSH grows
	// with slower hosts; 15 minutes is what the reference lab needs.
	sshBudget := cfg.Deploy.NodeInstallTimeout
	if sshBudget <= 0 {
		sshBudget = 15 * time.Minute
	}

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

		// 1. Define (when missing) and boot every VM in this batch. Creating
		// the batch here, instead of relying on `init-vms` to have defined the
		// whole topology in one go, is what makes the batching worth anything
		// on a RAM-limited host: each batch is shrunk and idle by the time the
		// next one starts its installer.
		var ips []string
		var batchNamesList []string
		states, err := l.DomainStates(ctx)
		if err != nil {
			return fmt.Errorf("list domains for batch %d: %w", i+1, err)
		}
		for _, node := range batch {
			if node.name == "" || node.ip == "" {
				return fmt.Errorf("batch %d has a VM with an empty name or IP", i+1)
			}
			batchNamesList = append(batchNamesList, node.name)
			state, defined := states[node.name]
			if !defined {
				if err := CreateClientVM(ctx, cfg, l, node.name); err != nil {
					return fmt.Errorf("create VM %s: %w", node.name, err)
				}
			} else if state != "running" {
				if err := l.Start(ctx, node.name); err != nil {
					return fmt.Errorf("start VM %s: %w", node.name, err)
				}
			}
			ips = append(ips, node.ip)
		}

		// Pin disk-first booting now, minutes before the installers finish:
		// virt-install 4.1 leaves a freshly created PXE guest booting from the
		// network first, and the reboot at the end of the autoinstall would
		// re-enter PXE and reinstall the node if this waited for SSH.
		if err := FixBootOrder(ctx, l, batchNamesList); err != nil {
			logging.Warn("Could not fix boot order in batch %d: %v", i+1, err)
		}

		// 2. Wait for each installed system to answer SSH. The live installer
		// answers on port 22 too but refuses our key, and a node whose
		// installer wedges in its shutdown is restarted from here.
		logging.Info("Waiting for SSH on all VMs in Batch %d (budget %s)...", i+1, sshBudget)
		if err := installBatchSSH(ctx, l, s, batchNamesList, ips, sshBudget); err != nil {
			return fmt.Errorf("wait for SSH in batch %d: %w", i+1, err)
		}

		// 3. Re-assert the boot order (idempotent) and signal shutdown, so the
		// batch releases its RAM before the next one starts its installer. It
		// is not restarted here: an idle batch still holds its final RAM, and
		// with four or five installers in the next batch that idle footprint
		// is what pushes a memory-tight host into swapping. The whole topology
		// boots together once every batch is installed.
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
			if err := forceStop(ctx, l, name); err != nil {
				errMsgs = append(errMsgs, err.Error())
			}
		}
		if len(errMsgs) > 0 {
			return fmt.Errorf("failed to shut down batch %d: %s", i+1, strings.Join(errMsgs, "; "))
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		// Resize all VMs in the batch to their final RAM (in parallel). They
		// stay shut off until the last batch is installed; see step 3 above.
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
				logging.Success("Batch %d: %s resized to %d MB", i+1, n.name, n.ramFinalMB)
			}()
		}
		resizeWg.Wait()
		if len(resizeErrs) > 0 {
			return fmt.Errorf("failed to resize batch %d: %s", i+1, strings.Join(resizeErrs, "; "))
		}
	}

	// Start the whole topology now that no installer is left to compete with
	// it. Booting thirteen guests together is the footprint the deploy phase
	// runs on anyway, so it is a state this host has to hold; the difference
	// is that it happens once, after the last install, instead of adding the
	// idle batches to every later batch's installers.
	states, err := l.DomainStates(ctx)
	if err != nil {
		return fmt.Errorf("list domains before starting the topology: %w", err)
	}
	var startWg sync.WaitGroup
	var startMu sync.Mutex
	var startErrs []string
	for _, batch := range batches {
		for _, node := range batch {
			n := node
			if states[n.name] == "running" {
				continue
			}
			startWg.Add(1)
			go func() {
				defer startWg.Done()
				if err := l.Start(ctx, n.name); err != nil {
					startMu.Lock()
					startErrs = append(startErrs, fmt.Sprintf("start VM %s: %v", n.name, err))
					startMu.Unlock()
				}
			}()
		}
	}
	startWg.Wait()
	if len(startErrs) > 0 {
		return fmt.Errorf("failed to start the provisioned topology: %s", strings.Join(startErrs, "; "))
	}

	logging.Success("All batches provisioned and resized; the whole topology is booting")
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
