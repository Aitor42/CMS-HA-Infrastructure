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

	f.testDRBDFailover(ctx, opts)
	if err := ctx.Err(); err != nil {
		return err
	}
	f.testCMSFailover(ctx, opts)
	if err := ctx.Err(); err != nil {
		return err
	}
	f.testK3sWorkerFailover(ctx, opts)
	// Distinguish "all scenarios ran" from "interrupted halfway through" so
	// callers never mistake a cancelled run for a successful one.
	return ctx.Err()
}

// shutdownForTest shuts a VM down and verifies it actually reached "shut off".
// A failed Shutdown must never be reported as PASS: without this check the
// scenario would "verify" a failover that never happened.
func (f *FailoverTester) shutdownForTest(ctx context.Context, name string) error {
	if err := f.libvirt.Shutdown(ctx, name); err != nil {
		return fmt.Errorf("shutdown %s: %w", name, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		state, err := f.libvirt.DomainState(ctx, name)
		if err != nil {
			return fmt.Errorf("query state of %s after shutdown: %w", name, err)
		}
		if state == "shut off" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is still %s after 30s (test aborted so it cannot be reported as PASS)", name, state)
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return fmt.Errorf("context cancelled waiting for %s to shut off", name)
		}
	}
}

func (f *FailoverTester) testDRBDFailover(ctx context.Context, opts FailoverOpts) {
	logging.Info("Scenario 1: DRBD Master Failover")
	start := time.Now()
	pass := true

	if len(f.cfg.Nodes.Masters) < 2 {
		logging.Warn("Skipping DRBD failover test: requires at least 2 master nodes")
		f.printSkipped("DRBD Master Failover")
		return
	}

	master1 := f.cfg.Nodes.Masters[0]
	master2 := f.cfg.Nodes.Masters[1]

	// Shutdown master1
	if err := f.shutdownForTest(ctx, master1.Name); err != nil {
		f.printSummary("DRBD Master Failover", false, time.Since(start))
		logging.Error("%v", err)
		return
	}
	// Wait until master2 has promoted DRBD and the MariaDB pod is running there
	// (poll until ready instead of sleeping a fixed 30s).
	mariadbRunning := waitFor(ctx, 90*time.Second, 3*time.Second, func() bool {
		out, _, _, err := f.ssh.RunCommand(ctx, master2.IP, "kubectl get pods -n cms | grep mariadb")
		return err == nil && strings.Contains(out, "Running")
	})
	if !mariadbRunning {
		pass = false
		logging.Warn("MariaDB pod not running on master2")
	}

	// Verify HTTP / HTTPS reachability (via the keepalived VIP)
	if !waitFor(ctx, 30*time.Second, 2*time.Second, func() bool {
		return checkLBReachability(ctx, f.cfg.LBVIP(), f.cfg.TrafficVHost())
	}) {
		pass = false
		logging.Warn("CMS frontend not reachable on Load Balancer VIP (HTTP 200/301/302)")
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
}

func (f *FailoverTester) testCMSFailover(ctx context.Context, opts FailoverOpts) {
	logging.Info("Scenario 2: CMS Frontend Failover")
	start := time.Now()
	pass := true

	if len(f.cfg.Nodes.CMSFrontends) < 2 {
		logging.Warn("Skipping CMS failover test: requires at least 2 frontend nodes")
		f.printSkipped("CMS Frontend Failover")
		return
	}

	cms1 := f.cfg.Nodes.CMSFrontends[0]

	// Shutdown cms1
	if err := f.shutdownForTest(ctx, cms1.Name); err != nil {
		f.printSummary("CMS Frontend Failover", false, time.Since(start))
		logging.Error("%v", err)
		return
	}
	// Verify HTTP / HTTPS reachability (routes to cms2, via the VIP)
	if !waitFor(ctx, 30*time.Second, 2*time.Second, func() bool {
		return checkLBReachability(ctx, f.cfg.LBVIP(), f.cfg.TrafficVHost())
	}) {
		pass = false
		logging.Warn("CMS frontend not returning HTTP 200/301/302 after cms1 shutdown")
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
}

func (f *FailoverTester) testK3sWorkerFailover(ctx context.Context, opts FailoverOpts) {
	logging.Info("Scenario 3: K3s Worker Failover")
	start := time.Now()
	pass := true

	if len(f.cfg.Nodes.Workers) < 2 || len(f.cfg.Nodes.Masters) < 2 {
		logging.Warn("Skipping Worker failover test: requires at least 2 workers and 2 masters")
		f.printSkipped("K3s Worker Failover")
		return
	}

	worker1 := f.cfg.Nodes.Workers[0]
	worker2 := f.cfg.Nodes.Workers[1]
	master2 := f.cfg.Nodes.Masters[1]

	// Shutdown worker1
	if err := f.shutdownForTest(ctx, worker1.Name); err != nil {
		f.printSummary("K3s Worker Failover", false, time.Since(start))
		logging.Error("%v", err)
		return
	}
	// Verify pods reschedule to worker2 (poll until rescheduled, up to 90s)
	rescheduled := waitFor(ctx, 90*time.Second, 5*time.Second, func() bool {
		out, _, _, err := f.ssh.RunCommand(ctx, master2.IP, "kubectl get pods -o wide -A | grep -v Terminating")
		return err == nil && strings.Contains(out, worker2.Name)
	})
	if !rescheduled {
		pass = false
		logging.Warn("Pods not rescheduled to worker2")
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
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
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
