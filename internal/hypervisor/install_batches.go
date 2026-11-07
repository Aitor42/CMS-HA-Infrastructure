package hypervisor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

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
func installBatchSSH(ctx context.Context, l *libvirt.Client, s ssh.Executor, names, ips []string, budget time.Duration) error {
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
func waitForInstalledNode(ctx context.Context, l *libvirt.Client, s ssh.Executor, name, ip string) error {
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

// installNode is one guest of the batched provisioning pass.
type installNode struct {
	name         string
	ip           string
	ramInstallMB int
	ramFinalMB   int
}

// installBatch is a set of guests whose installers run at the same time.
type installBatch struct {
	name  string
	nodes []installNode
}

// planInstallBatches decides which client guests install together.
//
// The documented order is dependency order: the router and the management
// nodes first, then the Kubernetes nodes, then the frontends and workstations.
// Without a configured width that order is kept as three semantic batches.
//
// `deploy.install_batch_size` re-splits the same order into chunks of at most
// that many guests, and exists because the width is what decides the wall
// time: every installer saturates about one vCPU, so on a 4-vCPU host a group
// of five costs two install rounds (~8 minutes each) while the same guests in
// groups of four cost three rounds in total. It is also a memory setting:
// four 3 GiB installers plus the jumpstart fit a 15 GiB host, five do not.
func planInstallBatches(cfg *config.Config) []installBatch {
	network := []installNode{
		{cfg.Nodes.Router.Name, cfg.Nodes.Router.IP, cfg.Nodes.Router.RAMInstallMB, cfg.Nodes.Router.RAMFinalMB},
		{cfg.Nodes.Monitor.Name, cfg.Nodes.Monitor.IP, cfg.Nodes.Monitor.RAMInstallMB, cfg.Nodes.Monitor.RAMFinalMB},
		{cfg.Nodes.Storage.Name, cfg.Nodes.Storage.IP, cfg.Nodes.Storage.RAMInstallMB, cfg.Nodes.Storage.RAMFinalMB},
	}

	var kubernetes []installNode
	for _, m := range cfg.Nodes.Masters {
		kubernetes = append(kubernetes, installNode{m.Name, m.IP, m.RAMInstallMB, m.RAMFinalMB})
	}
	for _, w := range cfg.Nodes.Workers {
		kubernetes = append(kubernetes, installNode{w.Name, w.IP, w.RAMInstallMB, w.RAMFinalMB})
	}

	var fronts []installNode
	for _, lb := range cfg.Nodes.LBs {
		fronts = append(fronts, installNode{lb.Name, lb.IP, lb.RAMInstallMB, lb.RAMFinalMB})
	}
	for _, c := range cfg.Nodes.CMSFrontends {
		fronts = append(fronts, installNode{c.Name, c.IP, c.RAMInstallMB, c.RAMFinalMB})
	}
	for _, hd := range cfg.HotdeskSpecs() {
		fronts = append(fronts, installNode{hd.Name, hd.IP, hd.RAMInstallMB, hd.RAMFinalMB})
	}

	if width := cfg.Deploy.InstallBatchSize; width > 0 {
		ordered := make([]installNode, 0, len(network)+len(kubernetes)+len(fronts))
		ordered = append(ordered, network...)
		ordered = append(ordered, kubernetes...)
		ordered = append(ordered, fronts...)
		return chunkInstallNodes(ordered, width)
	}

	return []installBatch{
		{name: "1: Network/Management", nodes: network},
		{name: "2: Kubernetes & DB", nodes: kubernetes},
		{name: "3: CMS Frontends & Workstations", nodes: fronts},
	}
}

// chunkInstallNodes splits an ordered guest list into consecutive chunks of at
// most width guests. Consecutive chunks are also the minimum number of rounds:
// a chunk never mixes guests that could have installed earlier.
func chunkInstallNodes(ordered []installNode, width int) []installBatch {
	if width <= 0 || width > len(ordered) {
		width = len(ordered)
	}
	if width == 0 {
		return nil
	}
	rounds := (len(ordered) + width - 1) / width
	batches := make([]installBatch, 0, rounds)
	for i := 0; i < len(ordered); i += width {
		end := i + width
		if end > len(ordered) {
			end = len(ordered)
		}
		batches = append(batches, installBatch{
			name:  fmt.Sprintf("%d/%d", i/width+1, rounds),
			nodes: ordered[i:end],
		})
	}
	return batches
}

// installNodeNames returns the guest names of a batch for the phase log.
func installNodeNames(nodes []installNode) string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.name
	}
	return strings.Join(names, ", ")
}

// batchBackend is the subset of the libvirt client used by the batched
// installer. Keeping it as an interface lets the orchestration (create/start,
// wait, shut down, resize, final boot) be tested without a live hypervisor;
// the production adapter below is a thin pass-through.
type batchBackend interface {
	CreateVM(ctx context.Context, name string) error
	DomainStates(ctx context.Context) (map[string]string, error)
	Start(ctx context.Context, name string) error
	Shutdown(ctx context.Context, name string) error
	ForceStop(ctx context.Context, name string) error
	SetMaxMemory(ctx context.Context, name string, kib int64) error
	SetMemory(ctx context.Context, name string, kib int64) error
	FixBootOrder(ctx context.Context, names []string) error
	WaitForSSH(ctx context.Context, s ssh.Executor, names, ips []string, budget time.Duration) error
}

type libvirtBatchBackend struct {
	cfg *config.Config
	l   *libvirt.Client
}

func (b libvirtBatchBackend) CreateVM(ctx context.Context, name string) error {
	return CreateClientVM(ctx, b.cfg, b.l, name)
}

func (b libvirtBatchBackend) DomainStates(ctx context.Context) (map[string]string, error) {
	return b.l.DomainStates(ctx)
}

func (b libvirtBatchBackend) Start(ctx context.Context, name string) error {
	return b.l.Start(ctx, name)
}

func (b libvirtBatchBackend) Shutdown(ctx context.Context, name string) error {
	return b.l.Shutdown(ctx, name)
}

func (b libvirtBatchBackend) ForceStop(ctx context.Context, name string) error {
	return forceStop(ctx, b.l, name)
}

func (b libvirtBatchBackend) SetMaxMemory(ctx context.Context, name string, kib int64) error {
	return b.l.SetMaxMemory(ctx, name, kib)
}

func (b libvirtBatchBackend) SetMemory(ctx context.Context, name string, kib int64) error {
	return b.l.SetMemory(ctx, name, kib)
}

func (b libvirtBatchBackend) FixBootOrder(ctx context.Context, names []string) error {
	return FixBootOrder(ctx, b.l, names)
}

func (b libvirtBatchBackend) WaitForSSH(ctx context.Context, s ssh.Executor, names, ips []string, budget time.Duration) error {
	return installBatchSSH(ctx, b.l, s, names, ips, budget)
}

// InstallByBatches provisions VMs in batches using PXE. The live installer a
// node PXE-boots also listens on port 22 but does not trust the deployment
// key, so "port open, key refused" means the installer is still working;
// only a successful login means the node reached its disk.
func InstallByBatches(ctx context.Context, cfg *config.Config, l *libvirt.Client, s ssh.Executor) error {
	return installByBatches(ctx, cfg, libvirtBatchBackend{cfg: cfg, l: l}, s)
}

func installByBatches(ctx context.Context, cfg *config.Config, b batchBackend, s ssh.Executor) error {
	timer := logging.PhaseStart("Install By Batches")
	defer timer.End()

	// Every node in a batch installs at the same time and they all share the
	// host's vCPUs, so the budget for a batch to finish and answer SSH grows
	// with slower hosts; 15 minutes is what the reference lab needs.
	sshBudget := cfg.Deploy.NodeInstallTimeout
	if sshBudget <= 0 {
		sshBudget = 15 * time.Minute
	}

	batches := planInstallBatches(cfg)
	for i, batch := range batches {
		if err := runInstallBatch(ctx, b, s, batch, i, sshBudget); err != nil {
			return err
		}
	}

	if err := startProvisionedTopology(ctx, b, batches); err != nil {
		return err
	}

	logging.Success("All batches provisioned and resized; the whole topology is booting")
	return nil
}

// installBatch runs the full lifecycle of one batch: make its guests run, let
// the installers finish, then shut them down and resize them to their final
// memory before the next batch starts.
func runInstallBatch(ctx context.Context, b batchBackend, s ssh.Executor, batch installBatch, index int, sshBudget time.Duration) error {
	nodes := batch.nodes
	logging.Info("Starting Batch %s (%d VMs: %s)...", batch.name, len(nodes), installNodeNames(nodes))

	names, ips, err := startBatchVMs(ctx, b, nodes, index)
	if err != nil {
		return err
	}

	// Pin disk-first booting now, minutes before the installers finish: a PXE
	// guest left booting from the network would re-enter PXE and reinstall the
	// node at the reboot the autoinstall ends with.
	if err := b.FixBootOrder(ctx, names); err != nil {
		logging.Warn("Could not fix boot order in batch %d: %v", index+1, err)
	}

	// Wait for each installed system to answer SSH. The live installer answers
	// on port 22 too but refuses our key, and a node whose installer wedges in
	// its shutdown is restarted from here.
	logging.Info("Waiting for SSH on all VMs in Batch %d (budget %s)...", index+1, sshBudget)
	if err := b.WaitForSSH(ctx, s, names, ips, sshBudget); err != nil {
		return fmt.Errorf("wait for SSH in batch %d: %w", index+1, err)
	}

	// Re-assert the boot order (idempotent) and signal shutdown, so the batch
	// releases its RAM before the next one starts its installer. It is not
	// restarted here: an idle batch still holds its final RAM, and with four
	// or five installers in the next batch that idle footprint is what pushes
	// a memory-tight host into swapping. The whole topology boots together
	// once every batch is installed.
	if err := b.FixBootOrder(ctx, names); err != nil {
		logging.Warn("Could not fix boot order in batch %d: %v", index+1, err)
	}
	if err := shutdownBatch(ctx, b, nodes, index); err != nil {
		return err
	}
	if err := resizeBatch(ctx, b, nodes, index); err != nil {
		return err
	}
	return nil
}

// startBatchVMs defines the missing guests and starts the stopped ones.
// Creating the batch here, instead of relying on `init-vms` to have defined
// the whole topology in one go, is what makes batching worth anything on a
// RAM-limited host: each batch is shrunk and idle by the time the next one
// starts its installer.
func startBatchVMs(ctx context.Context, b batchBackend, batch []installNode, index int) ([]string, []string, error) {
	states, err := b.DomainStates(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list domains for batch %d: %w", index+1, err)
	}
	var names, ips []string
	for _, node := range batch {
		if node.name == "" || node.ip == "" {
			return nil, nil, fmt.Errorf("batch %d has a VM with an empty name or IP", index+1)
		}
		names = append(names, node.name)
		state, defined := states[node.name]
		if !defined {
			if err := b.CreateVM(ctx, node.name); err != nil {
				return nil, nil, fmt.Errorf("create VM %s: %w", node.name, err)
			}
		} else if state != "running" {
			if err := b.Start(ctx, node.name); err != nil {
				return nil, nil, fmt.Errorf("start VM %s: %w", node.name, err)
			}
		}
		ips = append(ips, node.ip)
	}
	return names, ips, nil
}

// shutdownBatch asks every guest to shut down and force-stops the ones still
// running after the grace period.
func shutdownBatch(ctx context.Context, b batchBackend, batch []installNode, index int) error {
	if err := requestBatchShutdown(ctx, b, batch, index); err != nil {
		return err
	}
	return waitForBatchShutdown(ctx, b, batch, index)
}

func requestBatchShutdown(ctx context.Context, b batchBackend, batch []installNode, index int) error {
	states, err := b.DomainStates(ctx)
	if err != nil {
		return fmt.Errorf("list domains before shutdown in batch %d: %w", index+1, err)
	}
	for _, node := range batch {
		if state := states[node.name]; state != "shut off" {
			if err := b.Shutdown(ctx, node.name); err != nil {
				return fmt.Errorf("shutdown VM %s: %w", node.name, err)
			}
		}
	}
	return nil
}

// waitForBatchShutdown polls a single `virsh list --all` snapshot per tick
// instead of one per VM, then force-stops whatever is still running.
func waitForBatchShutdown(ctx context.Context, b batchBackend, batch []installNode, index int) error {
	pending := make([]string, 0, len(batch))
	for _, node := range batch {
		pending = append(pending, node.name)
	}
	shutdownDeadline := time.Now().Add(30 * time.Second)
	for len(pending) > 0 && time.Now().Before(shutdownDeadline) {
		states, err := b.DomainStates(ctx)
		if err != nil {
			return fmt.Errorf("list domains while shutting down batch %d: %w", index+1, err)
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
		if err := b.ForceStop(ctx, name); err != nil {
			errMsgs = append(errMsgs, err.Error())
		}
	}
	if len(errMsgs) > 0 {
		return fmt.Errorf("failed to shut down batch %d: %s", index+1, strings.Join(errMsgs, "; "))
	}
	return ctx.Err()
}

// resizeBatch grows every guest to its final RAM in parallel. They stay shut
// off until the last batch is installed.
func resizeBatch(ctx context.Context, b batchBackend, batch []installNode, index int) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	for _, node := range batch {
		n := node
		wg.Add(1)
		go func() {
			defer wg.Done()
			kib := int64(n.ramFinalMB * 1024)
			if err := b.SetMaxMemory(ctx, n.name, kib); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("set maximum memory for %s: %v", n.name, err))
				mu.Unlock()
				return
			}
			if err := b.SetMemory(ctx, n.name, kib); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("set memory for %s: %v", n.name, err))
				mu.Unlock()
				return
			}
			logging.Success("Batch %d: %s resized to %d MB", index+1, n.name, n.ramFinalMB)
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return fmt.Errorf("failed to resize batch %d: %s", index+1, strings.Join(errs, "; "))
	}
	return nil
}

// startProvisionedTopology boots the whole cluster after the last install, so
// no installer competes for RAM with the services that are coming up.
func startProvisionedTopology(ctx context.Context, b batchBackend, batches []installBatch) error {
	states, err := b.DomainStates(ctx)
	if err != nil {
		return fmt.Errorf("list domains before starting the topology: %w", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	for _, batch := range batches {
		for _, n := range batch.nodes {
			name := n.name
			if states[name] == "running" {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := b.Start(ctx, name); err != nil {
					mu.Lock()
					errs = append(errs, fmt.Sprintf("start VM %s: %v", name, err))
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if len(errs) > 0 {
		return fmt.Errorf("failed to start the provisioned topology: %s", strings.Join(errs, "; "))
	}
	return nil
}

// RecreateFailedVMs cleans up client VMs left in a failed state (destroy,
// undefine, remove disks); creating them again is deferred to init-vms.
