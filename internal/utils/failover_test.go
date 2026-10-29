package utils

import (
	"context"
	"errors"
	"fmt"
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
	states   map[string]string
	calls    []string
	startErr error
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
	d.states[name] = "shut off"
	return nil
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

	if !ft.testDRBDFailover(t.Context(), FailoverOpts{}) {
		t.Fatal("testDRBDFailover = false, want true")
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

	if !ft.testDRBDPowerCutFailover(t.Context(), FailoverOpts{SkipRestore: true}) {
		t.Fatal("testDRBDPowerCutFailover = false, want true (SKIP) under --skip-restore")
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

	if ft.testDRBDPowerCutFailover(t.Context(), FailoverOpts{}) {
		t.Fatal("testDRBDPowerCutFailover = true, want false when a master is unreachable")
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

	if ft.testCMSFailover(t.Context(), FailoverOpts{}) {
		t.Fatal("testCMSFailover = true, want false when the frontend cannot be restored")
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

	if !ft.testK3sWorkerFailover(t.Context(), FailoverOpts{SkipRestore: true}) {
		t.Fatal("testK3sWorkerFailover = false, want true")
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
		if !ft.testLBFailover(t.Context(), FailoverOpts{SkipRestore: true}) {
			t.Fatal("testLBFailover = false, want true when the survivor serves HTTPS")
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
		if ft.testLBFailover(ctx, FailoverOpts{SkipRestore: true}) {
			t.Fatal("testLBFailover = true, want false when only plain HTTP answers")
		}
	})
}
