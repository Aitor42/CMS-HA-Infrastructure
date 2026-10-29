package utils

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/httpcheck"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// FailoverOpts options for failover testing.
type FailoverOpts struct {
	SkipRestore bool
}

// shutdownGracePeriod is how long a guest is given to honour the ACPI power
// request before it is powered off the hard way. Node-loss scenarios care that
// the node is gone, so a guest that wedges its own shutdown must not abort the
// test (and must not be reported as a failover failure) — see shutdownForTest.
const shutdownGracePeriod = 90 * time.Second

// restoreSSHTimeout is how long a restarted node has to accept SSH. A node that
// never comes back must fail the scenario: otherwise a test could report PASS
// after leaving the "restored" node down. restoreRoleTimeout covers the DRBD
// reconnect and the after-sb-1pri resync after a power cut.
const (
	restoreSSHTimeout  = 180 * time.Second
	restoreRoleTimeout = 180 * time.Second
)

// errDRBDPairUnavailable means the two-node DRBD pair cannot be inspected
// because at least one master did not answer. The power-cut scenario treats
// this as an expected SKIP under --skip-restore (the previous scenario
// intentionally left a node down) but as a failure otherwise.
var errDRBDPairUnavailable = errors.New("DRBD pair is not fully reachable")

// DomainController is the libvirt subset the failover scenarios use.
// *libvirt.Client implements it; tests use a fake so the orchestration can be
// exercised without a hypervisor.
type DomainController interface {
	Shutdown(ctx context.Context, name string) error
	Destroy(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
	Resume(ctx context.Context, name string) error
	DomainState(ctx context.Context, name string) (string, error)
}

// The production client must keep satisfying the interface the scenarios use.
var _ DomainController = (*libvirt.Client)(nil)

// FailoverTester struct contains dependencies for failover tests.
type FailoverTester struct {
	cfg            *config.Config
	ssh            ssh.Executor
	libvirt        DomainController
	reachable      func(ctx context.Context, host, vhost string) bool
	reachableHTTPS func(ctx context.Context, host, vhost string) bool
}

// NewFailoverTester initializes a new FailoverTester.
func NewFailoverTester(cfg *config.Config, s ssh.Executor, l DomainController) *FailoverTester {
	return &FailoverTester{
		cfg:            cfg,
		ssh:            s,
		libvirt:        l,
		reachable:      httpcheck.LBReachable,
		reachableHTTPS: httpcheck.LBReachableHTTPS,
	}
}

// lbReachable asks whether the VIP serves the CMS over HTTPS or HTTP. The
// function field keeps the failover orchestration testable without a live LB.
func (f *FailoverTester) lbReachable(ctx context.Context, host string) bool {
	if f.reachable == nil {
		return httpcheck.LBReachable(ctx, host, f.cfg.TrafficVHost())
	}
	return f.reachable(ctx, host, f.cfg.TrafficVHost())
}

// lbReachableHTTPS is the strict probe used by the VIP scenario, whose claim is
// that the surviving balancer serves HTTPS, not merely the HTTP->HTTPS redirect.
func (f *FailoverTester) lbReachableHTTPS(ctx context.Context, host string) bool {
	if f.reachableHTTPS == nil {
		return httpcheck.LBReachableHTTPS(ctx, host, f.cfg.TrafficVHost())
	}
	return f.reachableHTTPS(ctx, host, f.cfg.TrafficVHost())
}

// Run executes the failover scenarios.
func (f *FailoverTester) Run(ctx context.Context, opts FailoverOpts) error {
	timer := logging.PhaseStart("Failover Chaos Engineering Tests")
	defer timer.End()

	// Every scenario's verdict is propagated: a scenario that reports FAIL must
	// fail this command, otherwise the chaos gate is decorative and a broken
	// cluster still exits zero. A scenario skipped for a small topology returns
	// true (see printSkipped), so it is never counted as a failure.
	failed := 0
	if !f.testDRBDFailover(ctx, opts) {
		failed++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !f.testDRBDPowerCutFailover(ctx, opts) {
		failed++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !f.testCMSFailover(ctx, opts) {
		failed++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !f.testK3sWorkerFailover(ctx, opts) {
		failed++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !f.testLBFailover(ctx, opts) {
		failed++
	}
	// Distinguish "all scenarios ran" from "interrupted halfway through" so
	// callers never mistake a cancelled run for a successful one.
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d of 5 failover scenarios failed", failed)
	}
	return nil
}

// shutdownForTest shuts a VM down and verifies it actually reached "shut off".
// A failed Shutdown must never be reported as PASS: without this check the
// scenario would "verify" a failover that never happened. A guest that ignores
// the ACPI request past the grace period is powered off the hard way: a power
// cut is still a valid node-loss scenario, and aborting because the guest
// wedged its own shutdown would report a healthy cluster as broken.
//
// The domain is inspected first. A paused guest ignores ACPI, so it is resumed
// before the request; a guest that is already off or crashed is rejected,
// because shutting down a node that was never running would let the scenario
// report a failover that the test did not cause.
func (f *FailoverTester) shutdownForTest(ctx context.Context, name string) error {
	state, err := f.libvirt.DomainState(ctx, name)
	if err != nil {
		return fmt.Errorf("query state of %s before shutdown: %w", name, err)
	}
	switch state {
	case "shut off", "crashed":
		return fmt.Errorf("%s is already %q: the scenario cannot prove a failover from a running node", name, state)
	case "paused", "pmsuspended":
		if err := f.libvirt.Resume(ctx, name); err != nil {
			return fmt.Errorf("resume paused domain %s: %w", name, err)
		}
	}

	if err := f.libvirt.Shutdown(ctx, name); err != nil {
		return fmt.Errorf("shutdown %s: %w", name, err)
	}
	deadline := time.Now().Add(shutdownGracePeriod)
	for {
		state, err := f.libvirt.DomainState(ctx, name)
		if err != nil {
			return fmt.Errorf("query state of %s after shutdown: %w", name, err)
		}
		if state == "shut off" {
			return nil
		}
		if time.Now().After(deadline) {
			logging.Warn("%s is still %s after %s; forcing a power-off so the node-loss test can proceed",
				name, state, shutdownGracePeriod)
			if err := f.libvirt.Destroy(ctx, name); err != nil {
				return fmt.Errorf("force power-off %s: %w", name, err)
			}
			if !retry.Wait(ctx, 30*time.Second, 2*time.Second, func() bool {
				st, derr := f.libvirt.DomainState(ctx, name)
				return derr == nil && st == "shut off"
			}) {
				return fmt.Errorf("%s did not reach \"shut off\" even after a forced power-off (test aborted so it cannot be reported as PASS)", name)
			}
			return nil
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return fmt.Errorf("context cancelled waiting for %s to shut off", name)
		}
	}
}

func (f *FailoverTester) testDRBDFailover(ctx context.Context, opts FailoverOpts) bool {
	logging.Info("Scenario 1: DRBD Master Failover")
	start := time.Now()
	pass := true

	if len(f.cfg.Nodes.Masters) < 2 {
		logging.Warn("Skipping DRBD failover test: requires at least 2 master nodes")
		f.printSkipped("DRBD Master Failover")
		return true
	}

	// Fail over the node that actually holds the Primary role, not Masters[0].
	// Shutting down a Secondary leaves the writer untouched: the cluster keeps
	// serving and the scenario would report PASS without exercising failover.
	primary, standby, err := f.drbdPrimary(ctx)
	if err != nil {
		if errors.Is(err, errDRBDPairUnavailable) && opts.SkipRestore {
			logging.Warn("Skipping DRBD failover test: --skip-restore left a master down (%v)", err)
			f.printSkipped("DRBD Master Failover")
			return true
		}
		logging.Error("cannot determine the DRBD Primary before the test: %v", err)
		f.logFailoverDiagnostics(ctx, "DRBD role could not be established before the failover")
		f.printSummary("DRBD Master Failover", false, time.Since(start))
		return false
	}

	// The watchdog on the survivor is what promotes it, and MariaDB must be
	// healthy on the writer before the test starts: otherwise a failed
	// promotion cannot be attributed to the node loss.
	if err := f.requireDRBDPreflight(ctx, primary, standby); err != nil {
		logging.Error("%v", err)
		f.logFailoverDiagnostics(ctx, "DRBD preflight failed before the failover")
		f.printSummary("DRBD Master Failover", false, time.Since(start))
		return false
	}

	logging.Info("Shutting down %s, the current DRBD Primary", primary.Name)
	if err := f.shutdownForTest(ctx, primary.Name); err != nil {
		f.printSummary("DRBD Master Failover", false, time.Since(start))
		logging.Error("%v", err)
		return false
	}
	// Wait until standby has promoted DRBD and MariaDB is Running and Ready
	// *on standby*. Matching the pod by name only would also match the pod
	// object left behind on the dead node and would pass without any failover
	// at all, so the node it is scheduled on is part of the assertion. The
	// window has to cover the DRBD ping timeout and its strikes, the fence
	// action, the promote, the node label, the forced deletion of the pod
	// stranded on the dead node, the pod reschedule and the container start
	// (which may have to pull the MariaDB image on a master that never ran it).
	// It is generous on purpose: a slow-but-correct failover must not be
	// recorded as a broken one.
	mariadbRunning := retry.Wait(ctx, 480*time.Second, 5*time.Second, func() bool {
		return f.mariadbRunningOn(ctx, standby.IP, standby)
	})
	if !mariadbRunning {
		pass = false
		f.logFailoverDiagnostics(ctx, fmt.Sprintf("MariaDB pod did not come back Running and Ready on %s", standby.Name))
	}

	// Only then check the CMS: every WordPress request needs the database, so
	// the VIP is expected to serve again as soon as MariaDB is back.
	if !retry.Wait(ctx, 60*time.Second, 2*time.Second, func() bool {
		return f.lbReachable(ctx, f.cfg.LBVIP())
	}) {
		pass = false
		logging.Warn("CMS frontend not reachable on Load Balancer VIP after the database recovered")
		f.logFailoverDiagnostics(ctx, "CMS frontend not reachable after the DRBD failover")
	}

	if !opts.SkipRestore {
		if err := f.restoreNode(ctx, primary); err != nil {
			pass = false
			logging.Error("%v", err)
			f.logFailoverDiagnostics(ctx, "the DRBD Primary did not come back after the failover")
		} else if !retry.Wait(ctx, restoreRoleTimeout, 5*time.Second, func() bool {
			// The return path is part of the scenario: a restarted master must
			// come back as the Secondary. A node that keeps the Primary role it
			// had before the restart is a split brain — the pod runs on standby
			// while the restarted node also believes it owns the volume.
			out, _, _, err := f.ssh.RunCommand(ctx, standby.IP, "drbdadm role cms_data")
			return err == nil && strings.TrimSpace(out) == "Primary/Secondary"
		}) {
			pass = false
			logging.Error("%s did not return as Secondary (%s is not Primary/Secondary)", primary.Name, standby.Name)
			f.logFailoverDiagnostics(ctx, "restarted master did not come back as Secondary")
		}
	}

	duration := time.Since(start)
	f.printSummary("DRBD Master Failover", pass, duration)
	return pass
}

// drbdPrimary reports which master currently holds the Primary role and which
// one is the standby. Both masters are queried and each answer is read as
// "<local>/<peer>": relying on Masters[0] alone breaks as soon as that node is
// the one that is down (for example on a --skip-restore re-run) and, worse,
// could pick a Secondary as the node to fail over.
func (f *FailoverTester) drbdPrimary(ctx context.Context) (primary, standby config.NodeDetail, err error) {
	masters := f.cfg.Nodes.Masters[:2]
	roles := make(map[string]string, len(masters))
	reachable := 0
	for _, master := range masters {
		out, _, code, runErr := f.ssh.RunCommand(ctx, master.IP, "drbdadm role cms_data")
		if runErr != nil || code != 0 {
			continue
		}
		role := strings.TrimSpace(out)
		local, _, _ := strings.Cut(role, "/")
		if local == "" {
			continue
		}
		roles[master.Name] = local
		reachable++
	}
	if reachable != len(masters) {
		return primary, standby, errDRBDPairUnavailable
	}

	var primaries, secondaries []config.NodeDetail
	for _, master := range masters {
		switch roles[master.Name] {
		case "Primary":
			primaries = append(primaries, master)
		case "Secondary":
			secondaries = append(secondaries, master)
		}
	}
	if len(primaries) != 1 || len(secondaries) != 1 {
		return primary, standby, fmt.Errorf("expected one Primary and one Secondary, got roles %v (the volume is not a healthy Primary/Secondary pair)", roles)
	}
	return primaries[0], secondaries[0], nil
}

// requireDRBDPreflight asserts the state the failover depends on: the watchdog
// on the survivor is active and can promote, and the database is healthy on the
// writer. Without this a broken watchdog or an already-down MariaDB is reported
// as a failed failover even though the node loss was not the cause.
func (f *FailoverTester) requireDRBDPreflight(ctx context.Context, primary, standby config.NodeDetail) error {
	if err := f.requireWatchdog(ctx, standby); err != nil {
		return err
	}
	if !f.mariadbRunningOn(ctx, primary.IP, primary) {
		return fmt.Errorf("mariadb-0 is not Running and Ready on %s before the failover", primary.Name)
	}
	return nil
}

// requireWatchdog fails unless drbd-failover-watchdog is active on node.
func (f *FailoverTester) requireWatchdog(ctx context.Context, node config.NodeDetail) error {
	_, stderr, code, err := f.ssh.RunCommand(ctx, node.IP, "systemctl is-active --quiet drbd-failover-watchdog")
	if err != nil || code != 0 {
		return fmt.Errorf("drbd-failover-watchdog is not active on %s (exit %d: %s): the survivor could not promote the volume",
			node.Name, code, strings.TrimSpace(stderr))
	}
	return nil
}

// mariadbPodQuery asks kubectl for the MariaDB pod's node, phase and Ready
// condition in one call. The Ready condition matters: a pod that is Running but
// not Ready (still starting, or Terminating) cannot serve the CMS.
const mariadbPodQuery = "kubectl --request-timeout=10s get pod mariadb-0 -n cms " +
	"-o jsonpath='{.spec.nodeName} {.status.phase} {.status.conditions[?(@.type==\"Ready\")].status}'"

// mariadbPod parses the node, phase and readiness of mariadb-0 as seen from
// host. kubectl is run on the host whose cluster view is being used, so a
// failure here means the pod (or the node) is not available to that host.
func (f *FailoverTester) mariadbPod(ctx context.Context, host string) (node, phase string, ready bool, err error) {
	out, stderr, code, runErr := f.ssh.RunCommand(ctx, host, mariadbPodQuery)
	if runErr != nil {
		return "", "", false, fmt.Errorf("query mariadb-0 on %s: %w", host, runErr)
	}
	if code != 0 {
		return "", "", false, fmt.Errorf("query mariadb-0 on %s exited %d: %s", host, code, strings.TrimSpace(stderr))
	}
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return "", "", false, fmt.Errorf("unexpected mariadb-0 output on %s: %q", host, strings.TrimSpace(out))
	}
	ready = len(fields) >= 3 && fields[2] == "True"
	return fields[0], fields[1], ready, nil
}

// mariadbRunningOn reports whether mariadb-0 is Running and Ready on node,
// using host's kubectl view. The node is compared exactly (by configured name
// or FQDN): a substring match would let "internal-master2" match
// "internal-master20".
func (f *FailoverTester) mariadbRunningOn(ctx context.Context, host string, node config.NodeDetail) bool {
	podNode, phase, ready, err := f.mariadbPod(ctx, host)
	if err != nil {
		return false
	}
	return nodeMatches(podNode, node) && phase == "Running" && ready
}

// nodeMatches reports whether a Kubernetes node identity (the configured name
// or its FQDN, both accepted by the readiness check) refers to node.
func nodeMatches(identity string, node config.NodeDetail) bool {
	return identity == node.Name || (node.FQDN != "" && identity == node.FQDN)
}

// testDRBDPowerCutFailover loses the master that currently holds the Primary
// role the hard way: no ACPI shutdown, so DRBD never gets to demote itself and
// the node keeps "Primary" in its metadata. This is the sequence that used to
// leave two writers — the peer takes over while the dead node is down, and the
// dead node comes back thinking it is still primary. drbd-cms-boot.service must
// stand it down before k3s starts; this scenario asserts exactly that.
func (f *FailoverTester) testDRBDPowerCutFailover(ctx context.Context, opts FailoverOpts) bool {
	logging.Info("Scenario 2: DRBD Master Power Cut")
	start := time.Now()
	pass := true

	if len(f.cfg.Nodes.Masters) < 2 {
		logging.Warn("Skipping DRBD power-cut test: requires at least 2 master nodes")
		f.printSkipped("DRBD Master Power Cut")
		return true
	}

	primary, standby, err := f.drbdPrimary(ctx)
	if err != nil {
		if errors.Is(err, errDRBDPairUnavailable) && opts.SkipRestore {
			logging.Warn("Skipping DRBD power-cut test: --skip-restore left a master down (%v)", err)
			f.printSkipped("DRBD Master Power Cut")
			return true
		}
		logging.Error("%v", err)
		f.printSummary("DRBD Master Power Cut", false, time.Since(start))
		return false
	}

	if err := f.requireDRBDPreflight(ctx, primary, standby); err != nil {
		logging.Error("%v", err)
		f.logFailoverDiagnostics(ctx, "DRBD preflight failed before the power cut")
		f.printSummary("DRBD Master Power Cut", false, time.Since(start))
		return false
	}

	if err := f.libvirt.Destroy(ctx, primary.Name); err != nil {
		logging.Error("failed to power off %s: %v", primary.Name, err)
		f.printSummary("DRBD Master Power Cut", false, time.Since(start))
		return false
	}
	if !retry.Wait(ctx, 30*time.Second, 2*time.Second, func() bool {
		state, derr := f.libvirt.DomainState(ctx, primary.Name)
		return derr == nil && state == "shut off"
	}) {
		logging.Error("%s did not reach \"shut off\" after the power cut", primary.Name)
		f.printSummary("DRBD Master Power Cut", false, time.Since(start))
		return false
	}

	// The survivor has to fence the dead domain (the fence helper exits 0 when
	// it is already off) and promote itself. Same generous budget as scenario 1:
	// a slow-but-correct failover must not be recorded as a broken one.
	mariadbRunning := retry.Wait(ctx, 480*time.Second, 5*time.Second, func() bool {
		return f.mariadbRunningOn(ctx, standby.IP, standby)
	})
	if !mariadbRunning {
		pass = false
		f.logFailoverDiagnostics(ctx, fmt.Sprintf("MariaDB pod did not come back Running and Ready on %s after the power cut", standby.Name))
	}
	if !retry.Wait(ctx, 60*time.Second, 2*time.Second, func() bool {
		return f.lbReachable(ctx, f.cfg.LBVIP())
	}) {
		pass = false
		logging.Warn("CMS frontend not reachable on Load Balancer VIP after the power-cut failover")
		f.logFailoverDiagnostics(ctx, "CMS frontend not reachable after the power-cut failover")
	}

	if !opts.SkipRestore {
		if err := f.restoreNode(ctx, primary); err != nil {
			pass = false
			logging.Error("%v", err)
			f.logFailoverDiagnostics(ctx, "the power-cut master did not come back")
		} else if !retry.Wait(ctx, restoreRoleTimeout, 5*time.Second, func() bool {
			// The point of the scenario: the node that comes back with stale
			// Primary metadata must end up as the Secondary, with the survivor
			// as the only writer. The budget covers the boot guard, the
			// reconnect and the resync after-sb-1pri triggers.
			out, _, _, err := f.ssh.RunCommand(ctx, standby.IP, "drbdadm role cms_data")
			return err == nil && strings.TrimSpace(out) == "Primary/Secondary"
		}) {
			pass = false
			logging.Error("%s did not come back as Secondary (%s is not Primary/Secondary): stale Primary metadata can split the volume",
				primary.Name, standby.Name)
			f.logFailoverDiagnostics(ctx, "power-cut master did not come back as Secondary")
		}
	}

	duration := time.Since(start)
	f.printSummary("DRBD Master Power Cut", pass, duration)
	return pass
}

func (f *FailoverTester) testCMSFailover(ctx context.Context, opts FailoverOpts) bool {
	logging.Info("Scenario 3: CMS Frontend Failover")
	start := time.Now()
	pass := true

	if len(f.cfg.Nodes.CMSFrontends) < 2 {
		logging.Warn("Skipping CMS failover test: requires at least 2 frontend nodes")
		f.printSkipped("CMS Frontend Failover")
		return true
	}

	cms1 := f.cfg.Nodes.CMSFrontends[0]

	if err := f.shutdownForTest(ctx, cms1.Name); err != nil {
		f.printSummary("CMS Frontend Failover", false, time.Since(start))
		logging.Error("%v", err)
		return false
	}
	// Verify HTTP / HTTPS reachability (routes to cms2, via the VIP). The window
	// covers nginx noticing the dead backend and the surviving frontend serving
	// the request on its own.
	if !retry.Wait(ctx, 60*time.Second, 2*time.Second, func() bool {
		return f.lbReachable(ctx, f.cfg.LBVIP())
	}) {
		pass = false
		logging.Warn("CMS frontend not returning HTTP 200/301/302 after cms1 shutdown")
		f.logFailoverDiagnostics(ctx, "CMS frontend not reachable after the frontend failover")
	}

	if !opts.SkipRestore {
		if err := f.restoreNode(ctx, cms1); err != nil {
			pass = false
			logging.Error("%v", err)
			f.logFailoverDiagnostics(ctx, "the CMS frontend did not come back after the failover")
		}
	}

	duration := time.Since(start)
	f.printSummary("CMS Frontend Failover", pass, duration)
	return pass
}

func (f *FailoverTester) testK3sWorkerFailover(ctx context.Context, opts FailoverOpts) bool {
	logging.Info("Scenario 4: K3s Worker Failover")
	start := time.Now()
	pass := true

	if len(f.cfg.Nodes.Workers) < 2 || len(f.cfg.Nodes.Masters) < 2 {
		logging.Warn("Skipping Worker failover test: requires at least 2 workers and 2 masters")
		f.printSkipped("K3s Worker Failover")
		return true
	}

	worker1 := f.cfg.Nodes.Workers[0]

	// Any master can answer kubectl: Masters[1] is not guaranteed to be up (a
	// previous scenario may have left it down), so pick the first that answers.
	master, err := f.reachableMaster(ctx)
	if err != nil {
		logging.Error("cannot run the worker failover test: %v", err)
		f.printSummary("K3s Worker Failover", false, time.Since(start))
		return false
	}

	if err := f.shutdownForTest(ctx, worker1.Name); err != nil {
		f.printSummary("K3s Worker Failover", false, time.Since(start))
		logging.Error("%v", err)
		return false
	}

	// Nothing is expected to *migrate*: the only stateful workload (mariadb-0)
	// is pinned to the DRBD primary by node affinity and its volume is a
	// hostPath, so the workers host no long-running pod that could move. What a
	// worker loss must not do is take the database or the CMS down, so assert
	// exactly that instead of a reschedule this design never performs.
	if !retry.Wait(ctx, 120*time.Second, 5*time.Second, func() bool {
		// Query mariadb-0 by name: `grep mariadb` also matches the backup and
		// restore CronJob pods, so a running backup could hide a database that
		// is actually down. It must stay Running and Ready on a master; a pod
		// that lands on a worker has lost its hostPath volume.
		podNode, phase, ready, err := f.mariadbPod(ctx, master.IP)
		return err == nil && phase == "Running" && ready && f.nodeIsMaster(podNode)
	}) {
		pass = false
		logging.Warn("MariaDB pod is not Running and Ready on a master after worker1 was shut down")
		f.logFailoverDiagnostics(ctx, "MariaDB pod is not Running and Ready after the worker failover")
	}
	if !retry.Wait(ctx, 60*time.Second, 2*time.Second, func() bool {
		return f.lbReachable(ctx, f.cfg.LBVIP())
	}) {
		pass = false
		logging.Warn("CMS frontend not reachable on the Load Balancer VIP after worker1 was shut down")
		f.logFailoverDiagnostics(ctx, "CMS frontend not reachable after the worker failover")
	}

	if !opts.SkipRestore {
		if err := f.restoreNode(ctx, worker1); err != nil {
			pass = false
			logging.Error("%v", err)
			f.logFailoverDiagnostics(ctx, "the worker did not come back after the failover")
		}
	}

	duration := time.Since(start)
	f.printSummary("K3s Worker Failover", pass, duration)
	return pass
}

// testLBFailover stops the balancer that currently owns the keepalived VIP and
// asserts that the survivor takes it over. The earlier scenarios only prove
// that both balancers answer individually; none of them proves the floating
// address moves, which is the whole point of having two of them.
func (f *FailoverTester) testLBFailover(ctx context.Context, opts FailoverOpts) bool {
	logging.Info("Scenario 5: Load Balancer VIP Failover")
	start := time.Now()
	pass := true

	if len(f.cfg.Nodes.LBs) < 2 {
		logging.Warn("Skipping LB failover test: requires at least 2 balancer nodes")
		f.printSkipped("Load Balancer VIP Failover")
		return true
	}

	vip := f.cfg.LBVIP()
	if vip == "" {
		logging.Warn("Skipping LB failover test: network.main.vip is not configured")
		f.printSkipped("Load Balancer VIP Failover")
		return true
	}

	holders, reachable, total := f.vipOwnership(ctx, vip)
	if holders != 1 || reachable != total {
		// Zero holders means the front door is already down; two holders is a
		// VRRP split; an unreachable balancer means the state cannot be
		// established. None is a failover the test caused, so report it instead
		// of shutting a balancer down on top of a broken VIP.
		logging.Warn("Expected exactly one balancer to hold the VIP %s before the test, found %d (reachable %d/%d)",
			vip, holders, reachable, total)
		f.logLBState(ctx, "VIP ownership is not 1-of-2 before the LB failover")
		f.printSummary("Load Balancer VIP Failover", false, time.Since(start))
		return false
	}
	var holder *config.NodeDetail
	for i := range f.cfg.Nodes.LBs {
		lb := &f.cfg.Nodes.LBs[i]
		if lb.IP != "" && f.lbHoldsVIP(ctx, lb.IP, vip) {
			holder = lb
			break
		}
	}
	if holder == nil {
		logging.Warn("Skipping LB failover test: the VIP holder could not be identified")
		f.printSkipped("Load Balancer VIP Failover")
		return true
	}
	var survivor *config.NodeDetail
	for i := range f.cfg.Nodes.LBs {
		lb := &f.cfg.Nodes.LBs[i]
		if lb.IP != "" && lb.Name != holder.Name {
			survivor = lb
			break
		}
	}
	if survivor == nil {
		logging.Warn("Skipping LB failover test: no second balancer with an address")
		f.printSkipped("Load Balancer VIP Failover")
		return true
	}
	// Never stop the VIP holder if the survivor is not ready to take over: the
	// test would turn a degraded cluster into a full outage.
	if _, _, code, err := f.ssh.RunCommand(ctx, survivor.IP, "systemctl is-active --quiet keepalived"); err != nil || code != 0 {
		logging.Warn("%s cannot take over the VIP (keepalived is not active or the host is unreachable)", survivor.Name)
		f.logLBState(ctx, "the surviving balancer is not ready before the LB failover")
		f.printSummary("Load Balancer VIP Failover", false, time.Since(start))
		return false
	}

	logging.Info("Stopping %s, which currently owns %s", holder.Name, vip)
	if err := f.shutdownForTest(ctx, holder.Name); err != nil {
		f.printSummary("Load Balancer VIP Failover", false, time.Since(start))
		logging.Error("%v", err)
		return false
	}

	// The survivor must both acquire the address and serve the CMS through it
	// over HTTPS. Waiting on the address alone would pass while nginx is still
	// down; accepting plain HTTP would pass on the redirect alone, without
	// proving that the survivor terminates TLS.
	if !retry.Wait(ctx, 90*time.Second, 2*time.Second, func() bool {
		return f.lbHoldsVIP(ctx, survivor.IP, vip) &&
			f.lbReachableHTTPS(ctx, vip)
	}) {
		pass = false
		logging.Warn("%s did not take over the VIP %s after %s was shut down", survivor.Name, vip, holder.Name)
		f.logLBState(ctx, "VIP did not move to the surviving balancer")
	}

	restored := true
	if !opts.SkipRestore {
		if err := f.restoreNode(ctx, *holder); err != nil {
			pass = false
			restored = false
			logging.Error("%v", err)
			f.logFailoverDiagnostics(ctx, "the balancer did not come back after the failover")
		}
	}

	// After the reboot exactly one balancer must own the VIP again, and both
	// must be reachable: counting holders only would report "1-of-2" while the
	// restored node is still down. keepalived preempts back to the
	// higher-priority node, so give it time to settle. Under --skip-restore the
	// node is intentionally left down, so the post-restore state is not checked.
	if !opts.SkipRestore && restored {
		if !retry.Wait(ctx, 120*time.Second, 5*time.Second, func() bool {
			h, r, t := f.vipOwnership(ctx, vip)
			return h == 1 && r == t && f.lbReachableHTTPS(ctx, vip)
		}) {
			pass = false
			logging.Warn("VIP %s is not owned by exactly one balancer after %s recovered", vip, holder.Name)
			f.logLBState(ctx, "VIP ownership did not settle after the balancer recovered")
		}
	}

	duration := time.Since(start)
	f.printSummary("Load Balancer VIP Failover", pass, duration)
	return pass
}

// lbHoldsVIP reports whether host currently has the floating address.
func (f *FailoverTester) lbHoldsVIP(ctx context.Context, host, vip string) bool {
	out, _, code, err := f.ssh.RunCommand(ctx, host, "ip -4 -o addr show")
	if err != nil || code != 0 {
		return false
	}
	return addressListHasVIP(out, vip)
}

// addressListHasVIP looks for the VIP (with its prefix length) in `ip -4 -o
// addr show` output.
func addressListHasVIP(out, vip string) bool {
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, vip+"/") {
			return true
		}
	}
	return false
}

// vipOwnership counts how many balancers hold the VIP and how many answered at
// all. The caller needs both: one holder out of one reachable balancer is not
// the same as one holder out of two, and an unreachable node must not silently
// disappear from the denominator.
func (f *FailoverTester) vipOwnership(ctx context.Context, vip string) (holders, reachable, total int) {
	for _, lb := range f.cfg.Nodes.LBs {
		if lb.IP == "" {
			continue
		}
		total++
		out, _, code, err := f.ssh.RunCommand(ctx, lb.IP, "ip -4 -o addr show")
		if err != nil || code != 0 {
			continue
		}
		reachable++
		if addressListHasVIP(out, vip) {
			holders++
		}
	}
	return holders, reachable, total
}

// reachableMaster returns the first master that accepts a command. Scenarios
// that only need a cluster view must not assume a specific master is up: an
// earlier scenario may have shut one down and left it that way.
func (f *FailoverTester) reachableMaster(ctx context.Context) (config.NodeDetail, error) {
	for _, master := range f.cfg.Nodes.Masters {
		if master.IP == "" {
			continue
		}
		if _, _, code, err := f.ssh.RunCommand(ctx, master.IP, "true"); err == nil && code == 0 {
			return master, nil
		}
	}
	return config.NodeDetail{}, errors.New("no master node accepted a command")
}

// nodeIsMaster reports whether name (a Kubernetes node identity) is one of the
// configured masters.
func (f *FailoverTester) nodeIsMaster(name string) bool {
	for _, master := range f.cfg.Nodes.Masters {
		if nodeMatches(name, master) {
			return true
		}
	}
	return false
}

// logLBState dumps the balancers' addresses and keepalived state when a VIP
// assertion fails; otherwise the log only says that a wait expired.
func (f *FailoverTester) logLBState(ctx context.Context, reason string) {
	const cmd = "ip -4 -o addr show; " +
		"systemctl is-active nginx keepalived 2>&1; " +
		"journalctl -u keepalived --no-pager -n 20 2>&1"
	for _, lb := range f.cfg.Nodes.LBs {
		if lb.IP == "" {
			continue
		}
		out, _, _, err := f.ssh.RunCommand(ctx, lb.IP, cmd)
		if err != nil {
			continue
		}
		logging.Warn("%s — %s\n%s", reason, lb.Name, strings.TrimSpace(out))
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// logFailoverDiagnostics dumps the surviving cluster's view into the log when a
// scenario fails. Without it a failure only reports that a wait expired, which
// says nothing about why: a pod stuck Terminating on the dead node, a node that
// never became Ready, DRBD not promoted, or the replicated volume not mounted
// all look identical from the assertion. The first master that answers is used
// because during scenario 1 the primary master is the one that was shut down.
func (f *FailoverTester) logFailoverDiagnostics(ctx context.Context, reason string) {
	const cmd = "kubectl --request-timeout=10s get nodes 2>&1; " +
		"kubectl --request-timeout=10s get pods -n cms -o wide 2>&1; " +
		"echo -n 'mariadb-0: '; kubectl --request-timeout=10s get pod mariadb-0 -n cms " +
		"-o jsonpath='{.metadata.deletionTimestamp} node={.spec.nodeName} phase={.status.phase}' 2>&1; echo; " +
		"drbdadm status cms_data 2>&1; " +
		"findmnt -n -o TARGET,SOURCE,FSTYPE /mnt/data/mariadb 2>&1"
	for _, master := range f.cfg.Nodes.Masters {
		out, _, _, err := f.ssh.RunCommand(ctx, master.IP, cmd)
		if err != nil {
			continue
		}
		logging.Warn("%s\n%s", reason, strings.TrimSpace(out))
		return
	}
	logging.Warn("%s (diagnostics unavailable: no master accepted a command)", reason)
}

func (f *FailoverTester) printSummary(name string, pass bool, duration time.Duration) {
	status := "FAIL"
	if pass {
		status = "PASS"
	}
	msg := fmt.Sprintf("Scenario: %-25s | Status: %-4s | Duration: %s", name, status, duration.Round(time.Second))
	if pass {
		logging.Success("%s", msg)
	} else {
		logging.Error("%s", msg)
	}
}

// printSkipped reports a scenario that did not run. It must never be counted
// as FAIL: that would pollute the summary with failures that did not occur.
func (f *FailoverTester) printSkipped(name string) {
	logging.Warn("Scenario: %-25s | Status: %-4s | Duration: %s", name, "SKIP", "0s")
}

// restoreNode powers a node that a scenario shut down back on and waits until
// it accepts SSH. The wait is part of the verdict: reporting PASS while the
// node that was supposed to be restored is still unreachable hides a failed
// restore and leaves the lab degraded for the next scenario.
func (f *FailoverTester) restoreNode(ctx context.Context, node config.NodeDetail) error {
	if err := f.libvirt.Start(ctx, node.Name); err != nil {
		return fmt.Errorf("restore %s: %w", node.Name, err)
	}
	if !f.waitForSSH(ctx, node.IP, restoreSSHTimeout) {
		return fmt.Errorf("%s did not accept SSH within %s after the restart", node.Name, restoreSSHTimeout)
	}
	return nil
}

// waitForSSH blocks until host accepts a trivial SSH command or the timeout
// elapses. It returns whether the host answered, so a restore can be treated as
// failed instead of silently proceeding.
func (f *FailoverTester) waitForSSH(ctx context.Context, host string, timeout time.Duration) bool {
	return retry.Wait(ctx, timeout, 5*time.Second, func() bool {
		_, _, code, err := f.ssh.RunCommand(ctx, host, "true")
		return err == nil && code == 0
	})
}
