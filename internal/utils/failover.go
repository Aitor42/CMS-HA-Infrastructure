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
	ssh     *ssh.Pool
	libvirt *libvirt.Client
}

// NewFailoverTester initializes a new FailoverTester.
func NewFailoverTester(cfg *config.Config, s *ssh.Pool, l *libvirt.Client) *FailoverTester {
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
	if !f.testCMSFailover(ctx, opts) {
		failed++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !f.testK3sWorkerFailover(ctx, opts) {
		failed++
	}
	// Distinguish "all scenarios ran" from "interrupted halfway through" so
	// callers never mistake a cancelled run for a successful one.
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d of 3 failover scenarios failed", failed)
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

	// Shutdown master1
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
	}

	duration := time.Since(start)
	f.printSummary("DRBD Master Failover", pass, duration)
	return pass
}

func (f *FailoverTester) testCMSFailover(ctx context.Context, opts FailoverOpts) bool {
	logging.Info("Scenario 2: CMS Frontend Failover")
	start := time.Now()
	pass := true

	if len(f.cfg.Nodes.CMSFrontends) < 2 {
		logging.Warn("Skipping CMS failover test: requires at least 2 frontend nodes")
		f.printSkipped("CMS Frontend Failover")
		return true
	}

	cms1 := f.cfg.Nodes.CMSFrontends[0]

	// Shutdown cms1
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
	logging.Info("Scenario 3: K3s Worker Failover")
	start := time.Now()
	pass := true

	if len(f.cfg.Nodes.Workers) < 2 || len(f.cfg.Nodes.Masters) < 2 {
		logging.Warn("Skipping Worker failover test: requires at least 2 workers and 2 masters")
		f.printSkipped("K3s Worker Failover")
		return true
	}

	worker1 := f.cfg.Nodes.Workers[0]
	master2 := f.cfg.Nodes.Masters[1]

	// Shutdown worker1
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
