package utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

// sshRunFunc is the signature of testutil.FakeExecutor.RunFunc.
type sshRunFunc func(ctx context.Context, host, cmd string) (stdout, stderr string, exitCode int, err error)

// fakeDomainController is an in-memory libvirt.DomainController. It models just
// the state transitions the failover scenarios depend on and records every
// call, so a test can assert which node was actually taken down.
type fakeDomainController struct {
	states       map[string]string
	calls        []string
	startErr     error
	destroyErr   error
	destroyState string
}

func newFakeDomain(names ...string) *fakeDomainController {
	states := make(map[string]string, len(names))
	for _, name := range names {
		states[name] = "running"
	}
	return &fakeDomainController{states: states}
}

func (d *fakeDomainController) DomainState(_ context.Context, name string) (string, error) {
	state, ok := d.states[name]
	if !ok {
		return "", fmt.Errorf("domain %s not found", name)
	}
	return state, nil
}

func (d *fakeDomainController) Shutdown(_ context.Context, name string) error {
	d.calls = append(d.calls, "shutdown:"+name)
	d.states[name] = "shut off"
	return nil
}

func (d *fakeDomainController) Destroy(_ context.Context, name string) error {
	d.calls = append(d.calls, "destroy:"+name)
	if d.destroyState != "" {
		d.states[name] = d.destroyState
	} else {
		d.states[name] = "shut off"
	}
	return d.destroyErr
}

func (d *fakeDomainController) Start(_ context.Context, name string) error {
	d.calls = append(d.calls, "start:"+name)
	if d.startErr != nil {
		return d.startErr
	}
	d.states[name] = "running"
	return nil
}

func (d *fakeDomainController) Resume(_ context.Context, name string) error {
	d.calls = append(d.calls, "resume:"+name)
	d.states[name] = "running"
	return nil
}

func (d *fakeDomainController) hasCall(call string) bool {
	for _, recorded := range d.calls {
		if recorded == call {
			return true
		}
	}
	return false
}

func (d *fakeDomainController) callIndex(prefix string) int {
	for i, recorded := range d.calls {
		if strings.HasPrefix(recorded, prefix) {
			return i
		}
	}
	return -1
}

func failoverConfig() *config.Config {
	return &config.Config{
		Network: config.NetworkConfig{
			Main: config.NetworkDetail{CIDR: "192.168.20.0/24", VIP: "192.168.20.100"},
		},
		Nodes: config.NodesConfig{
			Masters: []config.NodeDetail{
				{Name: "internal-master1", IP: "192.168.10.11", FQDN: "internal-master1.internal.local"},
				{Name: "internal-master2", IP: "192.168.10.12", FQDN: "internal-master2.internal.local"},
			},
			Workers: []config.NodeDetail{
				{Name: "internal-worker1", IP: "192.168.10.13"},
				{Name: "internal-worker2", IP: "192.168.10.14"},
			},
			CMSFrontends: []config.NodeDetail{
				{Name: "internal-cms1", IP: "192.168.20.11"},
				{Name: "internal-cms2", IP: "192.168.20.12"},
			},
			LBs: []config.NodeDetail{
				{Name: "main-lb1", IP: "192.168.20.103"},
				{Name: "main-lb2", IP: "192.168.20.104"},
			},
		},
	}
}

func newTester(cfg *config.Config, domain *fakeDomainController, run sshRunFunc) *FailoverTester {
	return &FailoverTester{
		cfg:            cfg,
		ssh:            &testutil.FakeExecutor{RunFunc: run},
		libvirt:        domain,
		reachable:      func(context.Context, string, string) bool { return true },
		reachableHTTPS: func(context.Context, string, string) bool { return true },
	}
}

func TestShutdownForTestRejectsAlreadyOffDomain(t *testing.T) {
	for _, state := range []string{"shut off", "crashed"} {
		t.Run(state, func(t *testing.T) {
			domain := newFakeDomain("internal-master1")
			domain.states["internal-master1"] = state
			ft := newTester(failoverConfig(), domain, func(context.Context, string, string) (string, string, int, error) {
				return "", "", 0, nil
			})

			err := ft.shutdownForTest(t.Context(), "internal-master1")
			if err == nil {
				t.Fatalf("shutdownForTest(%q) = nil, want an error: the scenario cannot prove a failover from a stopped node", state)
			}
			if domain.hasCall("shutdown:internal-master1") {
				t.Fatal("shutdownForTest sent a shutdown to a domain that was already down")
			}
		})
	}
}

func TestShutdownForTestResumesPausedDomain(t *testing.T) {
	domain := newFakeDomain("internal-master1")
	domain.states["internal-master1"] = "paused"
	ft := newTester(failoverConfig(), domain, func(context.Context, string, string) (string, string, int, error) {
		return "", "", 0, nil
	})

	if err := ft.shutdownForTest(t.Context(), "internal-master1"); err != nil {
		t.Fatalf("shutdownForTest: %v", err)
	}
	resume := domain.callIndex("resume:")
	shutdown := domain.callIndex("shutdown:")
	if resume < 0 || shutdown < 0 || resume > shutdown {
		t.Fatalf("calls = %v, want resume before shutdown", domain.calls)
	}
}

func TestDRBDPrimarySelectsActualPrimary(t *testing.T) {
	cfg := failoverConfig()
	m1, m2 := cfg.Nodes.Masters[0], cfg.Nodes.Masters[1]

	ft := newTester(cfg, newFakeDomain(m1.Name, m2.Name), func(_ context.Context, host, cmd string) (string, string, int, error) {
		if cmd != "drbdadm role cms_data" {
			return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
		}
		switch host {
		case m1.IP:
			// A disconnected peer is reported as Unknown, not Secondary.
			return "Primary/Unknown\n", "", 0, nil
		case m2.IP:
			return "Secondary/Primary\n", "", 0, nil
		}
		return "", "", 1, fmt.Errorf("unexpected host %s", host)
	})

	primary, standby, err := ft.drbdPrimary(t.Context())
	if err != nil {
		t.Fatalf("drbdPrimary: %v", err)
	}
	if primary.Name != m1.Name || standby.Name != m2.Name {
		t.Fatalf("drbdPrimary = %s/%s, want %s/%s", primary.Name, standby.Name, m1.Name, m2.Name)
	}
}

func TestDRBDPrimaryReportsUnavailablePair(t *testing.T) {
	cfg := failoverConfig()
	m1, m2 := cfg.Nodes.Masters[0], cfg.Nodes.Masters[1]

	ft := newTester(cfg, newFakeDomain(m1.Name, m2.Name), func(_ context.Context, host, cmd string) (string, string, int, error) {
		if cmd != "drbdadm role cms_data" {
			return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
		}
		if host == m1.IP {
			return "", "ssh: connect to host: Connection refused", 255, errors.New("ssh failed")
		}
		return "Secondary/Primary\n", "", 0, nil
	})

	if _, _, err := ft.drbdPrimary(t.Context()); !errors.Is(err, errDRBDPairUnavailable) {
		t.Fatalf("drbdPrimary error = %v, want errDRBDPairUnavailable", err)
	}
}

func TestDRBDPrimaryRejectsTwoPrimaries(t *testing.T) {
	cfg := failoverConfig()
	m1, m2 := cfg.Nodes.Masters[0], cfg.Nodes.Masters[1]

	ft := newTester(cfg, newFakeDomain(m1.Name, m2.Name), func(_ context.Context, _ string, cmd string) (string, string, int, error) {
		if cmd != "drbdadm role cms_data" {
			return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
		}
		return "Primary/Secondary\n", "", 0, nil
	})

	_, _, err := ft.drbdPrimary(t.Context())
	if err == nil || errors.Is(err, errDRBDPairUnavailable) {
		t.Fatalf("drbdPrimary error = %v, want a split-brain role error", err)
	}
}

func TestMariadbPodParsesNodePhaseAndReady(t *testing.T) {
	tests := []struct {
		name      string
		stdout    string
		code      int
		wantNode  string
		wantPhase string
		wantReady bool
		wantErr   bool
	}{
		{name: "ready", stdout: "internal-master1 Running True\n", wantNode: "internal-master1", wantPhase: "Running", wantReady: true},
		{name: "running but not ready", stdout: "internal-master1 Running False\n", wantNode: "internal-master1", wantPhase: "Running", wantReady: false},
		{name: "pending", stdout: "internal-master1 Pending False\n", wantNode: "internal-master1", wantPhase: "Pending", wantReady: false},
		{name: "kubectl failure", code: 1, wantErr: true},
		{name: "truncated output", stdout: "internal-master1\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ft := newTester(failoverConfig(), newFakeDomain(), func(_ context.Context, _, cmd string) (string, string, int, error) {
				if cmd != mariadbPodQuery {
					return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
				}
				return tt.stdout, "", tt.code, nil
			})

			node, phase, ready, err := ft.mariadbPod(t.Context(), "192.168.10.11")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("mariadbPod = %q/%q/%v, want an error", node, phase, ready)
				}
				return
			}
			if err != nil {
				t.Fatalf("mariadbPod: %v", err)
			}
			if node != tt.wantNode || phase != tt.wantPhase || ready != tt.wantReady {
				t.Fatalf("mariadbPod = %q/%q/%v, want %q/%q/%v", node, phase, ready, tt.wantNode, tt.wantPhase, tt.wantReady)
			}
		})
	}
}

func TestMariadbRunningOnAcceptsNameOrFQDN(t *testing.T) {
	cfg := failoverConfig()
	node := cfg.Nodes.Masters[0]

	for _, identity := range []string{node.Name, node.FQDN} {
		t.Run(identity, func(t *testing.T) {
			ft := newTester(cfg, newFakeDomain(), func(_ context.Context, _, cmd string) (string, string, int, error) {
				if cmd != mariadbPodQuery {
					return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
				}
				return identity + " Running True\n", "", 0, nil
			})
			if !ft.mariadbRunningOn(t.Context(), node.IP, node) {
				t.Fatalf("mariadbRunningOn(%q) = false, want true", identity)
			}
			if !ft.nodeIsMaster(identity) {
				t.Fatalf("nodeIsMaster(%q) = false, want true", identity)
			}
		})
	}

	t.Run("substring is not a match", func(t *testing.T) {
		ft := newTester(cfg, newFakeDomain(), func(_ context.Context, _, cmd string) (string, string, int, error) {
			if cmd != mariadbPodQuery {
				return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
			}
			return node.Name + "0 Running True\n", "", 0, nil
		})
		if ft.mariadbRunningOn(t.Context(), node.IP, node) {
			t.Fatal("mariadbRunningOn matched a node whose name merely contains the configured name")
		}
	})
}

func TestDRBDFailoverShutsDownCurrentPrimary(t *testing.T) {
	cfg := failoverConfig()
	m1, m2 := cfg.Nodes.Masters[0], cfg.Nodes.Masters[1]
	domain := newFakeDomain(m1.Name, m2.Name)

	m1RoleCalls := 0
	run := func(_ context.Context, host, cmd string) (string, string, int, error) {
		switch {
		case cmd == "drbdadm role cms_data" && host == m1.IP:
			// Before the test m1 is the Secondary; after m2 is restored it must
			// be the Primary again and see its peer as Secondary.
			m1RoleCalls++
			if m1RoleCalls == 1 {
				return "Secondary/Primary\n", "", 0, nil
			}
			return "Primary/Secondary\n", "", 0, nil
		case cmd == "drbdadm role cms_data" && host == m2.IP:
			return "Primary/Secondary\n", "", 0, nil
		case cmd == "systemctl is-active --quiet drbd-failover-watchdog":
			return "", "", 0, nil
		case cmd == mariadbPodQuery && host == m2.IP:
			return m2.Name + " Running True\n", "", 0, nil
		case cmd == mariadbPodQuery && host == m1.IP:
			return m1.Name + " Running True\n", "", 0, nil
		case cmd == "true":
			return "", "", 0, nil
		}
		return "", "", 1, fmt.Errorf("unexpected command %q on %s", cmd, host)
	}
	ft := newTester(cfg, domain, run)

	if result := ft.testDRBDFailover(t.Context(), FailoverOpts{}); result.Status != "PASS" {
		t.Fatalf("testDRBDFailover = %+v, want PASS", result)
	}
	if !domain.hasCall("shutdown:" + m2.Name) {
		t.Fatalf("calls = %v, want the current Primary %s to be shut down", domain.calls, m2.Name)
	}
	if domain.hasCall("shutdown:" + m1.Name) {
		t.Fatalf("calls = %v, the Secondary %s must not be shut down", domain.calls, m1.Name)
	}
	if !domain.hasCall("start:" + m2.Name) {
		t.Fatalf("calls = %v, want the Primary to be restored", domain.calls)
	}
}

func TestDRBDPowerCutSkipsWhenPairUnavailableUnderSkipRestore(t *testing.T) {
	cfg := failoverConfig()
	m1, m2 := cfg.Nodes.Masters[0], cfg.Nodes.Masters[1]
	domain := newFakeDomain(m1.Name, m2.Name)

	ft := newTester(cfg, domain, func(_ context.Context, host, cmd string) (string, string, int, error) {
		if cmd != "drbdadm role cms_data" {
			return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
		}
		if host == m1.IP {
			return "", "connection refused", 255, errors.New("ssh failed")
		}
		return "Primary/Secondary\n", "", 0, nil
	})

	if result := ft.testDRBDPowerCutFailover(t.Context(), FailoverOpts{SkipRestore: true}); result.Status != "SKIP" || result.Reason == "" {
		t.Fatalf("testDRBDPowerCutFailover = %+v, want SKIP with a reason under --skip-restore", result)
	}
	if domain.hasCall("destroy:"+m2.Name) || domain.hasCall("destroy:"+m1.Name) {
		t.Fatalf("calls = %v, want no power cut when the pair cannot be inspected", domain.calls)
	}
}

func TestDRBDPowerCutFailsWhenPairUnavailableWithoutSkipRestore(t *testing.T) {
	cfg := failoverConfig()
	m1, m2 := cfg.Nodes.Masters[0], cfg.Nodes.Masters[1]
	domain := newFakeDomain(m1.Name, m2.Name)

	ft := newTester(cfg, domain, func(_ context.Context, host, cmd string) (string, string, int, error) {
		if cmd != "drbdadm role cms_data" {
			return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
		}
		if host == m1.IP {
			return "", "connection refused", 255, errors.New("ssh failed")
		}
		return "Primary/Secondary\n", "", 0, nil
	})

	if result := ft.testDRBDPowerCutFailover(t.Context(), FailoverOpts{}); result.Status != "FAIL" {
		t.Fatalf("testDRBDPowerCutFailover = %+v, want FAIL when a master is unreachable", result)
	}
}

func TestCMSFailoverFailsWhenRestoreFails(t *testing.T) {
	cfg := failoverConfig()
	cms1 := cfg.Nodes.CMSFrontends[0]
	domain := newFakeDomain(cms1.Name, cfg.Nodes.CMSFrontends[1].Name)
	domain.startErr = errors.New("libvirt start failed")

	ft := newTester(cfg, domain, func(_ context.Context, _, cmd string) (string, string, int, error) {
		if cmd == "true" {
			return "", "", 0, nil
		}
		return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
	})

	if result := ft.testCMSFailover(t.Context(), FailoverOpts{}); result.Status != "FAIL" {
		t.Fatalf("testCMSFailover = %+v, want FAIL when the frontend cannot be restored", result)
	}
	if !domain.hasCall("shutdown:" + cms1.Name) {
		t.Fatalf("calls = %v, want the first frontend to be shut down", domain.calls)
	}
}

func TestRestoreNodeFailsWhenSSHNeverReturns(t *testing.T) {
	cfg := failoverConfig()
	node := cfg.Nodes.CMSFrontends[0]
	domain := newFakeDomain(node.Name)
	domain.states[node.Name] = "shut off"

	ft := newTester(cfg, domain, func(_ context.Context, _, cmd string) (string, string, int, error) {
		if cmd == "true" {
			return "", "", 1, errors.New("ssh not ready")
		}
		return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	if err := ft.restoreNode(ctx, node); err == nil {
		t.Fatal("restoreNode = nil, want an error when the node never accepts SSH")
	}
	if !domain.hasCall("start:" + node.Name) {
		t.Fatalf("calls = %v, want a start attempt", domain.calls)
	}
}

func TestRestoreNodeFailsWhenWatchdogNeverStarts(t *testing.T) {
	cfg := failoverConfig()
	node := cfg.Nodes.Masters[0]
	domain := newFakeDomain(node.Name)
	domain.states[node.Name] = "shut off"

	ft := newTester(cfg, domain, func(_ context.Context, _, cmd string) (string, string, int, error) {
		switch cmd {
		case "true":
			return "", "", 0, nil
		case "systemctl is-active --quiet drbd-failover-watchdog":
			return "", "", 3, nil
		}
		return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := ft.restoreNode(ctx, node)
	if err == nil || !strings.Contains(err.Error(), "drbd-failover-watchdog") {
		t.Fatalf("restoreNode = %v, want the watchdog readiness error", err)
	}
}

func TestRestoreNodeDoesNotWaitForWatchdogOffDRBD(t *testing.T) {
	cfg := failoverConfig()
	node := cfg.Nodes.CMSFrontends[0]
	domain := newFakeDomain(node.Name)
	domain.states[node.Name] = "shut off"

	watchdogCalls := 0
	ft := newTester(cfg, domain, func(_ context.Context, _, cmd string) (string, string, int, error) {
		switch cmd {
		case "true":
			return "", "", 0, nil
		case "systemctl is-active --quiet drbd-failover-watchdog":
			watchdogCalls++
			return "", "", 3, nil
		}
		return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
	})

	if err := ft.restoreNode(t.Context(), node); err != nil {
		t.Fatalf("restoreNode = %v, want success: frontends do not run the DRBD watchdog", err)
	}
	if watchdogCalls != 0 {
		t.Fatalf("watchdog was queried %d times on a frontend, want 0", watchdogCalls)
	}
}

func TestForceOffAcceptsAGuestThatFinishedShuttingDown(t *testing.T) {
	cfg := failoverConfig()
	node := cfg.Nodes.Masters[0]
	domain := newFakeDomain(node.Name)
	// The guest finishes its ACPI shutdown between the state read and the
	// destroy call: libvirt reports "domain is not running", but the node is
	// already down, which is exactly what the test wanted.
	domain.destroyErr = errors.New("Requested operation is not valid: domain is not running")
	domain.destroyState = "shut off"

	ft := newTester(cfg, domain, func(_ context.Context, _, cmd string) (string, string, int, error) {
		return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
	})

	if err := ft.forceOff(t.Context(), node.Name); err != nil {
		t.Fatalf("forceOff = %v, want success when the guest already shut down", err)
	}
	if !domain.hasCall("destroy:" + node.Name) {
		t.Fatalf("calls = %v, want a destroy attempt", domain.calls)
	}
}

func TestWorkerFailoverUsesReachableMaster(t *testing.T) {
	cfg := failoverConfig()
	m1, m2 := cfg.Nodes.Masters[0], cfg.Nodes.Masters[1]
	worker1 := cfg.Nodes.Workers[0]
	domain := newFakeDomain(m1.Name, m2.Name, worker1.Name, cfg.Nodes.Workers[1].Name)

	run := func(_ context.Context, host, cmd string) (string, string, int, error) {
		switch {
		case cmd == "true" && host == m1.IP:
			return "", "ssh unreachable", 255, errors.New("ssh failed")
		case cmd == "true" && host == m2.IP:
			return "", "", 0, nil
		case cmd == mariadbPodQuery && host == m2.IP:
			return m2.Name + " Running True\n", "", 0, nil
		}
		return "", "", 1, fmt.Errorf("unexpected command %q on %s", cmd, host)
	}
	ft := newTester(cfg, domain, run)

	if result := ft.testK3sWorkerFailover(t.Context(), FailoverOpts{SkipRestore: true}); result.Status != "PASS" {
		t.Fatalf("testK3sWorkerFailover = %+v, want PASS", result)
	}
	if !domain.hasCall("shutdown:" + worker1.Name) {
		t.Fatalf("calls = %v, want the worker to be shut down", domain.calls)
	}
	fake := ft.ssh.(*testutil.FakeExecutor)
	found := false
	for _, command := range fake.Commands {
		if command.Host == m2.IP && strings.HasPrefix(command.Cmd, "kubectl") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("commands = %v, want kubectl to run on the reachable master %s", fake.Commands, m2.Name)
	}
}

func TestVIPOwnershipCountsUnreachableBalancers(t *testing.T) {
	cfg := failoverConfig()
	lb1, lb2 := cfg.Nodes.LBs[0], cfg.Nodes.LBs[1]
	ft := newTester(cfg, newFakeDomain(), func(_ context.Context, host, cmd string) (string, string, int, error) {
		if cmd != "ip -4 -o addr show" {
			return "", "", 1, fmt.Errorf("unexpected command %q", cmd)
		}
		if host == lb1.IP {
			return "2: eth0    inet 192.168.20.100/24 brd 192.168.20.255 scope global eth0\n", "", 0, nil
		}
		return "", "connection refused", 255, errors.New("ssh failed")
	})

	holders, reachable, total := ft.vipOwnership(t.Context(), cfg.LBVIP())
	if holders != 1 || reachable != 1 || total != 2 {
		t.Fatalf("vipOwnership = %d/%d/%d, want 1 holder, 1 reachable, 2 total", holders, reachable, total)
	}
	if lb2.IP == "" {
		t.Fatal("test fixture lost the second balancer address")
	}
}

func TestLBFailoverRequiresHTTPS(t *testing.T) {
	cfg := failoverConfig()
	lb1, lb2 := cfg.Nodes.LBs[0], cfg.Nodes.LBs[1]

	runFor := func(domain *fakeDomainController) sshRunFunc {
		return func(_ context.Context, host, cmd string) (string, string, int, error) {
			switch {
			case cmd == "systemctl is-active --quiet keepalived" && host == lb2.IP:
				return "", "", 0, nil
			case cmd == "ip -4 -o addr show" && host == lb1.IP:
				return "2: eth0    inet 192.168.20.100/24 brd 192.168.20.255 scope global eth0\n", "", 0, nil
			case cmd == "ip -4 -o addr show" && host == lb2.IP:
				if domain.states[lb1.Name] == "shut off" {
					return "2: eth0    inet 192.168.20.100/24 brd 192.168.20.255 scope global eth0\n", "", 0, nil
				}
				return "2: eth0    inet 192.168.20.104/24 brd 192.168.20.255 scope global eth0\n", "", 0, nil
			}
			return "", "", 1, fmt.Errorf("unexpected command %q on %s", cmd, host)
		}
	}

	t.Run("https passes", func(t *testing.T) {
		domain := newFakeDomain(lb1.Name, lb2.Name)
		ft := newTester(cfg, domain, runFor(domain))
		if result := ft.testLBFailover(t.Context(), FailoverOpts{SkipRestore: true}); result.Status != "PASS" {
			t.Fatalf("testLBFailover = %+v, want PASS when the survivor serves HTTPS", result)
		}
		if !domain.hasCall("shutdown:" + lb1.Name) {
			t.Fatalf("calls = %v, want the VIP holder to be shut down", domain.calls)
		}
	})

	t.Run("plain http fails", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		domain := newFakeDomain(lb1.Name, lb2.Name)
		ft := newTester(cfg, domain, runFor(domain))
		// Cancel as soon as the strict probe runs: the HTTP-only answer must
		// not satisfy the scenario, and cancelling keeps the test fast.
		ft.reachableHTTPS = func(context.Context, string, string) bool {
			cancel()
			return false
		}
		if result := ft.testLBFailover(ctx, FailoverOpts{SkipRestore: true}); result.Status != "FAIL" {
			t.Fatalf("testLBFailover = %+v, want FAIL when only plain HTTP answers", result)
		}
	})
}

func readFailoverReport(t *testing.T, path string) failoverReport {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report failoverReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("invalid JSON %q: %v", data, err)
	}
	return report
}

func TestFailoverRunSkipPolicy(t *testing.T) {
	for _, requireAll := range []bool{false, true} {
		t.Run(fmt.Sprintf("require-all=%t", requireAll), func(t *testing.T) {
			// Reuse the tester to prove results do not leak between invocations.
			ft := newTester(&config.Config{}, newFakeDomain(), func(context.Context, string, string) (string, string, int, error) {
				t.Fatal("an undersized topology must not execute SSH commands")
				return "", "", 1, nil
			})
			for range 2 {
				path := filepath.Join(t.TempDir(), "results.json")
				err := ft.Run(t.Context(), FailoverOpts{RequireAll: requireAll, ResultsFile: path})
				if (err != nil) != requireAll {
					t.Fatalf("Run = %v, require-all=%t", err, requireAll)
				}
				if requireAll && !strings.Contains(err.Error(), "0 of 5 failover scenarios failed, 5 skipped") {
					t.Fatalf("strict verdict must distinguish failures from skips: %v", err)
				}
				report := readFailoverReport(t, path)
				if !report.Complete || report.RequireAll != requireAll || len(report.Scenarios) != 5 || (report.Error != "") != requireAll {
					t.Fatalf("unexpected report: %+v", report)
				}
				for _, result := range report.Scenarios {
					if result.Name == "" || result.Status != "SKIP" || result.Reason == "" || result.DurationSeconds != 0 {
						t.Fatalf("unexpected skipped scenario: %+v", result)
					}
				}
			}
		})
	}
}

func TestFailoverRunMixedVerdicts(t *testing.T) {
	for _, failRestore := range []bool{false, true} {
		for _, requireAll := range []bool{false, true} {
			t.Run(fmt.Sprintf("restore-fails=%t/require-all=%t", failRestore, requireAll), func(t *testing.T) {
				cfg := failoverConfig()
				cfg.Nodes.Masters, cfg.Nodes.Workers, cfg.Nodes.LBs = nil, nil, nil
				domain := newFakeDomain(cfg.Nodes.CMSFrontends[0].Name)
				if failRestore {
					domain.startErr = errors.New("cannot restore frontend")
				}
				ft := newTester(cfg, domain, func(context.Context, string, string) (string, string, int, error) {
					return "", "", 0, nil
				})
				path := filepath.Join(t.TempDir(), "results.json")
				err := ft.Run(t.Context(), FailoverOpts{RequireAll: requireAll, ResultsFile: path})
				if (err != nil) != (failRestore || requireAll) {
					t.Fatalf("Run = %v", err)
				}
				report := readFailoverReport(t, path)
				wantStatus := "PASS"
				if failRestore {
					wantStatus = "FAIL"
				}
				if len(report.Scenarios) != 5 || report.Scenarios[2].Status != wantStatus || !report.Complete {
					t.Fatalf("unexpected report: %+v", report)
				}
				if report.Scenarios[2].DurationSeconds <= 0 || !strings.Contains(report.DurationScope, "not CMS RTO") {
					t.Fatalf("scenario duration must be measured and scoped: %+v", report)
				}
				if !domain.hasCall("start:" + cfg.Nodes.CMSFrontends[0].Name) {
					t.Fatal("strict reporting must not bypass restoration")
				}
			})
		}
	}
}

func TestFailoverRunRequireAllPassesFullTopology(t *testing.T) {
	cfg := failoverConfig()
	var names []string
	for _, nodes := range [][]config.NodeDetail{cfg.Nodes.Masters, cfg.Nodes.Workers, cfg.Nodes.CMSFrontends, cfg.Nodes.LBs} {
		for _, node := range nodes {
			names = append(names, node.Name)
		}
	}
	domain := newFakeDomain(names...)
	primary := 0
	ft := newTester(cfg, domain, func(_ context.Context, host, cmd string) (string, string, int, error) {
		if domain.states[cfg.Nodes.Masters[primary].Name] == "shut off" {
			primary = 1 - primary
		}
		switch cmd {
		case "drbdadm role cms_data":
			if host == cfg.Nodes.Masters[primary].IP {
				return "Primary/Secondary", "", 0, nil
			}
			return "Secondary/Primary", "", 0, nil
		case mariadbPodQuery:
			return cfg.Nodes.Masters[primary].Name + " Running True", "", 0, nil
		case "ip -4 -o addr show":
			holder := cfg.Nodes.LBs[0]
			if domain.states[holder.Name] == "shut off" {
				holder = cfg.Nodes.LBs[1]
			}
			if host == holder.IP {
				return "2: eth0 inet " + cfg.LBVIP() + "/24", "", 0, nil
			}
			return "", "", 0, nil
		case "true", "systemctl is-active --quiet drbd-failover-watchdog", "systemctl is-active --quiet keepalived":
			return "", "", 0, nil
		default:
			t.Fatalf("unexpected SSH command %q on %s", cmd, host)
			return "", "", 1, nil
		}
	})
	path := filepath.Join(t.TempDir(), "results.json")
	if err := ft.Run(t.Context(), FailoverOpts{RequireAll: true, ResultsFile: path}); err != nil {
		t.Fatal(err)
	}
	report := readFailoverReport(t, path)
	if !report.Complete || len(report.Scenarios) != 5 || report.Error != "" || report.SkipRestore {
		t.Fatalf("unexpected report: %+v", report)
	}
	for _, result := range report.Scenarios {
		if result.Status != "PASS" || result.DurationSeconds <= 0 {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	for name, state := range domain.states {
		if state != "running" {
			t.Fatalf("%s left in state %s", name, state)
		}
	}
}

func TestFailoverRunRequireAllRejectsSkipRestoreSkips(t *testing.T) {
	cfg := failoverConfig()
	cfg.Nodes.CMSFrontends, cfg.Nodes.Workers, cfg.Nodes.LBs = nil, nil, nil
	ft := newTester(cfg, newFakeDomain(), func(context.Context, string, string) (string, string, int, error) {
		return "", "unreachable", 255, errors.New("unreachable")
	})
	path := filepath.Join(t.TempDir(), "results.json")
	err := ft.Run(t.Context(), FailoverOpts{SkipRestore: true, RequireAll: true, ResultsFile: path})
	if err == nil || !strings.Contains(err.Error(), "5 skipped") {
		t.Fatalf("Run = %v, want strict skip failure", err)
	}
	report := readFailoverReport(t, path)
	for _, result := range report.Scenarios[:2] {
		if result.Status != "SKIP" || !strings.Contains(result.Reason, "--skip-restore") {
			t.Fatalf("dynamic skip lost its reason: %+v", result)
		}
	}
}

func TestFailoverRunCancellationReport(t *testing.T) {
	for _, cancelBeforeRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("before-run=%t", cancelBeforeRun), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := failoverConfig()
			cfg.Nodes.Masters, cfg.Nodes.Workers, cfg.Nodes.LBs = nil, nil, nil
			domain := newFakeDomain(cfg.Nodes.CMSFrontends[0].Name)
			ft := newTester(cfg, domain, nil)
			ft.reachable = func(context.Context, string, string) bool {
				cancel()
				return false
			}
			if cancelBeforeRun {
				cancel()
			}
			path := filepath.Join(t.TempDir(), "results.json")
			err := ft.Run(ctx, FailoverOpts{SkipRestore: true, RequireAll: true, ResultsFile: path})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run = %v, want context.Canceled", err)
			}
			report := readFailoverReport(t, path)
			wantResults := 3
			if cancelBeforeRun {
				wantResults = 0
				if len(domain.calls) != 0 {
					t.Fatalf("cancelled run mutated domains: %v", domain.calls)
				}
			}
			if report.Complete || report.Error == "" || len(report.Scenarios) != wantResults {
				t.Fatalf("cancelled run must not look complete: %+v", report)
			}
		})
	}
}

func TestFailoverRunResultsFileErrors(t *testing.T) {
	t.Run("unwritable parent prevents disruption", func(t *testing.T) {
		cfg := failoverConfig()
		domain := newFakeDomain()
		ft := newTester(cfg, domain, func(context.Context, string, string) (string, string, int, error) {
			t.Fatal("results preflight must precede SSH")
			return "", "", 1, nil
		})
		path := filepath.Join(t.TempDir(), "missing", "results.json")
		if err := ft.Run(t.Context(), FailoverOpts{ResultsFile: path}); err == nil || !strings.Contains(err.Error(), "create failover results") {
			t.Fatalf("Run = %v, want results preflight error", err)
		}
		if len(domain.calls) != 0 {
			t.Fatalf("results preflight mutated domains: %v", domain.calls)
		}
	})
	t.Run("publish failure preserves verdict and removes temporary file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "directory")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		ft := newTester(&config.Config{}, newFakeDomain(), nil)
		err := ft.Run(t.Context(), FailoverOpts{RequireAll: true, ResultsFile: path})
		if err == nil || !strings.Contains(err.Error(), "5 skipped") || !strings.Contains(err.Error(), "write failover results") {
			t.Fatalf("Run = %v, want both verdict and publish errors", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("temporary results not cleaned up: %v, %v", entries, err)
		}
	})
}
