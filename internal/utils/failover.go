package utils

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
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

// FailoverTester struct contains dependencies for failover tests.
type FailoverTester struct {
	cfg     *config.Config
	ssh     ssh.Executor
	libvirt *libvirt.Client
}

// NewFailoverTester initializes a new FailoverTester.
func NewFailoverTester(cfg *config.Config, s ssh.Executor, l *libvirt.Client) *FailoverTester {
	return &FailoverTester{cfg: cfg, ssh: s, libvirt: l}
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
func (f *FailoverTester) shutdownForTest(ctx context.Context, name string) error {
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
			if !waitFor(ctx, 30*time.Second, 2*time.Second, func() bool {
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

	master1 := f.cfg.Nodes.Masters[0]
	master2 := f.cfg.Nodes.Masters[1]

	if err := f.shutdownForTest(ctx, master1.Name); err != nil {
		f.printSummary("DRBD Master Failover", false, time.Since(start))
		logging.Error("%v", err)
		return false
	}
	// Wait until master2 has promoted DRBD and MariaDB is running *on master2*.
	// Matching the pod by name only would also match the pod object left behind
	// on the dead node and would pass without any failover at all, so the node
	// it is scheduled on is part of the assertion. The window has to cover the
	// DRBD ping timeout and its strikes, the fence action, the promote, the node
	// label, the forced deletion of the pod stranded on the dead node, the pod
	// reschedule and the container start (which may have to pull the MariaDB
	// image on a master that never ran it). It is generous on purpose: a
	// slow-but-correct failover must not be recorded as a broken one.
	mariadbRunning := waitFor(ctx, 480*time.Second, 5*time.Second, func() bool {
		out, _, _, err := f.ssh.RunCommand(ctx, master2.IP,
			"kubectl --request-timeout=10s get pod mariadb-0 -n cms -o jsonpath='{.spec.nodeName} {.status.phase}'")
		if err != nil {
			return false
		}
		return strings.Contains(out, master2.Name) && strings.Contains(out, "Running")
	})
	if !mariadbRunning {
		pass = false
		f.logFailoverDiagnostics(ctx, fmt.Sprintf("MariaDB pod did not come back Running on %s", master2.Name))
	}

	// Only then check the CMS: every WordPress request needs the database, so
	// the VIP is expected to serve again as soon as MariaDB is back.
	if !waitFor(ctx, 60*time.Second, 2*time.Second, func() bool {
		return checkLBReachability(ctx, f.cfg.LBVIP(), f.cfg.TrafficVHost())
	}) {
		pass = false
		logging.Warn("CMS frontend not reachable on Load Balancer VIP after the database recovered")
		f.logFailoverDiagnostics(ctx, "CMS frontend not reachable after the DRBD failover")
	}

	if !opts.SkipRestore {
		if err := f.libvirt.Start(ctx, master1.Name); err != nil {
			pass = false
			logging.Error("failed to restore %s: %v", master1.Name, err)
		}
		f.waitForSSH(ctx, master1.IP, 120*time.Second)

		// The return path is part of the scenario: a restarted master must come
		// back as the Secondary. A node that keeps the Primary role it had before
		// the restart is a split brain — the pod runs on master2 while master1
		// also believes it owns the volume.
		if !waitFor(ctx, 120*time.Second, 5*time.Second, func() bool {
			out, _, _, err := f.ssh.RunCommand(ctx, master2.IP, "drbdadm role cms_data")
			return err == nil && strings.TrimSpace(out) == "Primary/Secondary"
		}) {
			pass = false
			logging.Error("%s did not return as Secondary (%s is not Primary/Secondary)", master1.Name, master2.Name)
			f.logFailoverDiagnostics(ctx, "restarted master did not come back as Secondary")
		}
	}

	duration := time.Since(start)
	f.printSummary("DRBD Master Failover", pass, duration)
	return pass
}

// drbdPrimary reports which master currently holds the Primary role, as seen
// from master1's DRBD ("<local>/<peer>").
func (f *FailoverTester) drbdPrimary(ctx context.Context) (primary, standby config.NodeDetail, err error) {
	master1, master2 := f.cfg.Nodes.Masters[0], f.cfg.Nodes.Masters[1]
	out, _, _, runErr := f.ssh.RunCommand(ctx, master1.IP, "drbdadm role cms_data")
	if runErr != nil {
		return primary, standby, fmt.Errorf("querying DRBD role on %s: %w", master1.IP, runErr)
	}
	switch role := strings.TrimSpace(out); role {
	case "Primary/Secondary":
		return master1, master2, nil
	case "Secondary/Primary":
		return master2, master1, nil
	default:
		return primary, standby, fmt.Errorf("unexpected DRBD role on %s: %q (the volume is not a healthy Primary/Secondary pair)", master1.IP, role)
	}
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
		logging.Error("%v", err)
		f.printSummary("DRBD Master Power Cut", false, time.Since(start))
		return false
	}

	if err := f.libvirt.Destroy(ctx, primary.Name); err != nil {
		logging.Error("failed to power off %s: %v", primary.Name, err)
		f.printSummary("DRBD Master Power Cut", false, time.Since(start))
		return false
	}
	if !waitFor(ctx, 30*time.Second, 2*time.Second, func() bool {
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
	mariadbRunning := waitFor(ctx, 480*time.Second, 5*time.Second, func() bool {
		out, _, _, err := f.ssh.RunCommand(ctx, standby.IP,
			"kubectl --request-timeout=10s get pod mariadb-0 -n cms -o jsonpath='{.spec.nodeName} {.status.phase}'")
		return err == nil && strings.Contains(out, standby.Name) && strings.Contains(out, "Running")
	})
	if !mariadbRunning {
		pass = false
		f.logFailoverDiagnostics(ctx, fmt.Sprintf("MariaDB pod did not come back Running on %s after the power cut", standby.Name))
	}
	if !waitFor(ctx, 60*time.Second, 2*time.Second, func() bool {
		return checkLBReachability(ctx, f.cfg.LBVIP(), f.cfg.TrafficVHost())
	}) {
		pass = false
		logging.Warn("CMS frontend not reachable on Load Balancer VIP after the power-cut failover")
		f.logFailoverDiagnostics(ctx, "CMS frontend not reachable after the power-cut failover")
	}

	if !opts.SkipRestore {
		if err := f.libvirt.Start(ctx, primary.Name); err != nil {
			pass = false
			logging.Error("failed to restore %s: %v", primary.Name, err)
		}
		f.waitForSSH(ctx, primary.IP, 120*time.Second)

		// The point of the scenario: the node that comes back with stale Primary
		// metadata must end up as the Secondary, with the survivor as the only
		// writer. The budget covers the boot guard, the reconnect and the resync
		// after-sb-1pri triggers.
		if !waitFor(ctx, 180*time.Second, 5*time.Second, func() bool {
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
	if !waitFor(ctx, 60*time.Second, 2*time.Second, func() bool {
		return checkLBReachability(ctx, f.cfg.LBVIP(), f.cfg.TrafficVHost())
	}) {
		pass = false
		logging.Warn("CMS frontend not returning HTTP 200/301/302 after cms1 shutdown")
		f.logFailoverDiagnostics(ctx, "CMS frontend not reachable after the frontend failover")
	}

	if !opts.SkipRestore {
		if err := f.libvirt.Start(ctx, cms1.Name); err != nil {
			pass = false
			logging.Error("failed to restore %s: %v", cms1.Name, err)
		}
		f.waitForSSH(ctx, cms1.IP, 120*time.Second)
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
	master2 := f.cfg.Nodes.Masters[1]

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
	if !waitFor(ctx, 120*time.Second, 5*time.Second, func() bool {
		out, _, _, err := f.ssh.RunCommand(ctx, master2.IP, "kubectl --request-timeout=10s get pods -n cms | grep mariadb")
		return err == nil && strings.Contains(out, "Running")
	}) {
		pass = false
		logging.Warn("MariaDB pod is not Running after worker1 was shut down")
		f.logFailoverDiagnostics(ctx, "MariaDB pod is not Running after the worker failover")
	}
	if !waitFor(ctx, 60*time.Second, 2*time.Second, func() bool {
		return checkLBReachability(ctx, f.cfg.LBVIP(), f.cfg.TrafficVHost())
	}) {
		pass = false
		logging.Warn("CMS frontend not reachable on the Load Balancer VIP after worker1 was shut down")
		f.logFailoverDiagnostics(ctx, "CMS frontend not reachable after the worker failover")
	}

	if !opts.SkipRestore {
		if err := f.libvirt.Start(ctx, worker1.Name); err != nil {
			pass = false
			logging.Error("failed to restore %s: %v", worker1.Name, err)
		}
		f.waitForSSH(ctx, worker1.IP, 120*time.Second)
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

	var holders []*config.NodeDetail
	for i := range f.cfg.Nodes.LBs {
		lb := &f.cfg.Nodes.LBs[i]
		if lb.IP != "" && f.lbHoldsVIP(ctx, lb.IP, vip) {
			holders = append(holders, lb)
		}
	}
	if len(holders) != 1 {
		// Zero holders means the front door is already down; two holders is a
		// VRRP split. Neither is a failover the test caused, so report it
		// instead of shutting a balancer down on top of a broken VIP.
		logging.Warn("Expected exactly one balancer to hold the VIP %s before the test, found %d", vip, len(holders))
		f.logLBState(ctx, "VIP ownership is not 1-of-2 before the LB failover")
		f.printSummary("Load Balancer VIP Failover", false, time.Since(start))
		return false
	}
	holder := holders[0]
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

	// The survivor must both acquire the address and serve the CMS through it.
	// Waiting on the address alone would pass while nginx is still down, and
	// waiting on HTTPS alone could be answered by a stale connection.
	if !waitFor(ctx, 90*time.Second, 2*time.Second, func() bool {
		return f.lbHoldsVIP(ctx, survivor.IP, vip) &&
			checkLBReachability(ctx, vip, f.cfg.TrafficVHost())
	}) {
		pass = false
		logging.Warn("%s did not take over the VIP %s after %s was shut down", survivor.Name, vip, holder.Name)
		f.logLBState(ctx, "VIP did not move to the surviving balancer")
	}

	if !opts.SkipRestore {
		if err := f.libvirt.Start(ctx, holder.Name); err != nil {
			pass = false
			logging.Error("failed to restore %s: %v", holder.Name, err)
		}
		f.waitForSSH(ctx, holder.IP, 120*time.Second)
	}

	// After the reboot exactly one balancer must own the VIP again. keepalived
	// preempts back to the higher-priority node, so give it time to settle.
	if !waitFor(ctx, 120*time.Second, 5*time.Second, func() bool {
		return f.countVIPHolders(ctx, vip) == 1 &&
			checkLBReachability(ctx, vip, f.cfg.TrafficVHost())
	}) {
		pass = false
		logging.Warn("VIP %s is not owned by exactly one balancer after %s recovered", vip, holder.Name)
		f.logLBState(ctx, "VIP ownership did not settle after the balancer recovered")
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
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, vip+"/") {
			return true
		}
	}
	return false
}

// countVIPHolders returns how many balancers currently own the VIP.
func (f *FailoverTester) countVIPHolders(ctx context.Context, vip string) int {
	holders := 0
	for _, lb := range f.cfg.Nodes.LBs {
		if lb.IP != "" && f.lbHoldsVIP(ctx, lb.IP, vip) {
			holders++
		}
	}
	return holders
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

// waitFor polls fn until it returns true, the context is cancelled or the
// timeout elapses. It replaces blind fixed sleeps so a failover that completes
// early is detected immediately instead of after an arbitrary delay.
func waitFor(ctx context.Context, timeout, interval time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		if !sleepCtx(ctx, interval) {
			return false
		}
	}
}

// waitForSSH blocks until host accepts a trivial SSH command or the timeout
// elapses. Used after restoring a VM so the next scenario starts as soon as the
// node is back, rather than after a fixed delay.
func (f *FailoverTester) waitForSSH(ctx context.Context, host string, timeout time.Duration) {
	waitFor(ctx, timeout, 5*time.Second, func() bool {
		_, _, code, err := f.ssh.RunCommand(ctx, host, "true")
		return err == nil && code == 0
	})
}

// lbHTTPClient is shared across reachability probes so its connection pool is
// reused instead of being rebuilt on every call.
var lbHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- test failover on lab LB
	},
}

func checkLBReachability(ctx context.Context, lbIP, vhost string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://%s", lbIP), nil)
	if err == nil {
		req.Host = vhost
		resp, err := lbHTTPClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			return resp.StatusCode == 200 || resp.StatusCode == 301 || resp.StatusCode == 302
		}
	}
	reqHTTP, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s", lbIP), nil)
	if err == nil {
		reqHTTP.Host = vhost
		resp, err := lbHTTPClient.Do(reqHTTP)
		if err == nil {
			defer resp.Body.Close()
			return resp.StatusCode == 200 || resp.StatusCode == 301 || resp.StatusCode == 302
		}
	}
	return false
}
