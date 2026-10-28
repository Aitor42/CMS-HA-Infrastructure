package ufw

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

const testRouterIP = "192.168.10.1"

func testConfig() *config.Config {
	return &config.Config{
		Network: config.NetworkConfig{
			Internal: config.NetworkDetail{CIDR: "192.168.10.0/24", Gateway: "192.168.10.1"},
			Main:     config.NetworkDetail{CIDR: "192.168.20.0/24", Gateway: "192.168.20.1", VIP: "192.168.20.100"},
		},
		Nodes: config.NodesConfig{
			Router: config.NodeDetail{
				Name:   "ufw-router",
				IP:     testRouterIP,
				MACWAN: "52:54:00:10:00:02",
			},
			Jumpstart: config.NodeDetail{
				Name:   "jumpstart",
				IP:     "192.168.10.10",
				IPMain: "192.168.20.10",
			},
			Masters: []config.NodeDetail{
				{Name: "internal-master1", IP: "192.168.10.11"},
				{Name: "internal-master2", IP: "192.168.10.12"},
				{Name: "internal-master3", IP: "192.168.10.16"},
			},
			Workers: []config.NodeDetail{
				{Name: "internal-worker1", IP: "192.168.10.13"},
			},
			Storage: config.NodeDetail{Name: "internal-storage", IP: "192.168.10.15"},
			Monitor: config.NodeDetail{Name: "internal-monitor", IP: "192.168.10.20"},
			LBs: []config.NodeDetail{
				{Name: "main-lb1", IP: "192.168.20.103"},
			},
			CMSFrontends: []config.NodeDetail{
				{Name: "main-cms1", IP: "192.168.20.101"},
			},
		},
	}
}

const testRouterPolicy = `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), deny (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
Anywhere on enp3s0         ALLOW       Anywhere                   # Main -> Internet
Anywhere on enp3s0         ALLOW       Anywhere                   # Internal -> Internet
192.168.20.0/24            ALLOW       192.168.10.0/24            # Internal -> Main
192.168.10.0/24            ALLOW       192.168.20.0/24            # Main -> Internal
192.168.20.100 80/tcp      ALLOW       Anywhere                   # DNAT HTTP -> LB
192.168.20.100 443/tcp     ALLOW       Anywhere                   # DNAT HTTPS -> LB
`

const testNATTable = `-P PREROUTING ACCEPT
-A PREROUTING -i enp3s0 -p tcp -m tcp --dport 80 -j DNAT --to-destination 192.168.20.100:80
-A PREROUTING -i enp3s0 -p tcp -m tcp --dport 443 -j DNAT --to-destination 192.168.20.100:443
-P POSTROUTING ACCEPT
-A POSTROUTING -s 192.168.10.0/24 -o enp3s0 -j MASQUERADE
-A POSTROUTING -s 192.168.20.0/24 -o enp3s0 -j MASQUERADE
`

// healthyRun returns the output the UFW phase expects from a converged router.
// Tests override individual commands to model a failure.
func healthyRun(_ context.Context, _ string, cmd string) (string, string, int, error) {
	switch {
	case strings.Contains(cmd, "ip -o link"):
		return "enp3s0\n", "", 0, nil
	case strings.Contains(cmd, "cp /etc/ufw/before.rules"):
		return "", "", 0, nil
	case cmd == "ufw reload":
		return "", "", 0, nil
	case cmd == ufwActiveCheck:
		return "Status: active\n", "", 0, nil
	case cmd == "ufw status verbose":
		return testRouterPolicy, "", 0, nil
	case cmd == ipForwardCheck:
		return "1\n", "", 0, nil
	case cmd == natTableDump:
		return testNATTable, "", 0, nil
	default:
		return "", "", 0, nil
	}
}

func healthyPhaseFake(t *testing.T) *testutil.FakeExecutor {
	t.Helper()
	return &testutil.FakeExecutor{RunFunc: healthyRun}
}

func TestPhaseRunChecksEveryNodeIncludingJumpstart(t *testing.T) {
	cfg := testConfig()
	fake := healthyPhaseFake(t)
	p := NewPhase(cfg, fake).(*Phase)

	if err := p.Run(t.Context()); err != nil {
		t.Fatalf("Run() = %v, want success", err)
	}

	var checked []string
	for _, c := range fake.Commands {
		if c.Cmd == ufwActiveCheck {
			checked = append(checked, c.Host)
		}
	}
	for _, want := range []string{"192.168.10.10", "192.168.10.11", "192.168.10.20", "192.168.20.101", "192.168.20.103"} {
		if !contains(checked, want) {
			t.Errorf("UFW active check did not run on %s; checked hosts: %v", want, checked)
		}
	}
	if !contains(checked, testRouterIP) {
		t.Errorf("UFW active check did not run on the router; checked hosts: %v", checked)
	}
	if !hasCommand(fake.Commands, natTableDump) {
		t.Error("phase did not inspect the live NAT table")
	}
	if !hasCommand(fake.Commands, ipForwardCheck) {
		t.Error("phase did not verify IP forwarding")
	}

	natRules := string(fake.Files[testRouterIP][natRulesPath])
	if strings.Contains(natRules, "${") {
		t.Errorf("uploaded NAT rules still contain an unresolved placeholder:\n%s", natRules)
	}
	for _, want := range []string{"192.168.20.100:80", "-s 192.168.10.0/24", "-s 192.168.20.0/24"} {
		if !strings.Contains(natRules, want) {
			t.Errorf("uploaded NAT rules are missing %q:\n%s", want, natRules)
		}
	}
}

func TestPhaseRejectsInactiveRouter(t *testing.T) {
	fake := healthyPhaseFake(t)
	fake.RunFunc = func(ctx context.Context, host, cmd string) (string, string, int, error) {
		if host == testRouterIP && cmd == ufwActiveCheck {
			return "Status: inactive\n", "", 1, fmt.Errorf("exit status 1")
		}
		return healthyRun(ctx, host, cmd)
	}

	err := NewPhase(testConfig(), fake).(*Phase).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not active on router") {
		t.Fatalf("Run() = %v, want an inactive-router error", err)
	}
}

func TestPhaseRejectsInactiveJumpstart(t *testing.T) {
	fake := healthyPhaseFake(t)
	fake.RunFunc = func(ctx context.Context, host, cmd string) (string, string, int, error) {
		if host == "192.168.10.10" && cmd == ufwActiveCheck {
			return "Status: inactive\n", "", 1, fmt.Errorf("exit status 1")
		}
		return healthyRun(ctx, host, cmd)
	}

	err := NewPhase(testConfig(), fake).(*Phase).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "192.168.10.10") {
		t.Fatalf("Run() = %v, want jumpstart in the inactive-node error", err)
	}
}

func TestPhaseRejectsFailedReload(t *testing.T) {
	fake := healthyPhaseFake(t)
	fake.RunFunc = func(ctx context.Context, host, cmd string) (string, string, int, error) {
		if host == testRouterIP && cmd == "ufw reload" {
			return "", "invalid before.rules", 1, fmt.Errorf("exit status 1")
		}
		return healthyRun(ctx, host, cmd)
	}

	err := NewPhase(testConfig(), fake).(*Phase).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "reload") || !strings.Contains(err.Error(), "invalid before.rules") {
		t.Fatalf("Run() = %v, want a reload error with stderr", err)
	}
}

func TestPhaseRejectsDisabledForwarding(t *testing.T) {
	fake := healthyPhaseFake(t)
	fake.RunFunc = func(ctx context.Context, host, cmd string) (string, string, int, error) {
		if cmd == ipForwardCheck {
			return "0\n", "", 0, nil
		}
		return healthyRun(ctx, host, cmd)
	}

	err := NewPhase(testConfig(), fake).(*Phase).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not forwarding") {
		t.Fatalf("Run() = %v, want a forwarding error", err)
	}
}

func TestPhaseRejectsMissingLiveNAT(t *testing.T) {
	fake := healthyPhaseFake(t)
	fake.RunFunc = func(ctx context.Context, host, cmd string) (string, string, int, error) {
		if cmd == natTableDump {
			return strings.ReplaceAll(testNATTable, "-A POSTROUTING -s 192.168.20.0/24 -o enp3s0 -j MASQUERADE\n", ""), "", 0, nil
		}
		return healthyRun(ctx, host, cmd)
	}

	err := NewPhase(testConfig(), fake).(*Phase).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "MASQUERADE main") {
		t.Fatalf("Run() = %v, want a missing-NAT error", err)
	}
}

func TestPhaseRejectsMissingRouterPolicy(t *testing.T) {
	fake := healthyPhaseFake(t)
	fake.RunFunc = func(ctx context.Context, host, cmd string) (string, string, int, error) {
		if cmd == "ufw status verbose" {
			return strings.ReplaceAll(testRouterPolicy, "Main -> Internet", "removed"), "", 0, nil
		}
		return healthyRun(ctx, host, cmd)
	}

	err := NewPhase(testConfig(), fake).(*Phase).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Main -> Internet") {
		t.Fatalf("Run() = %v, want a missing-policy error", err)
	}
}

func TestActiveCheckDoesNotMatchInactive(t *testing.T) {
	if strings.Contains(ufwActiveCheck, "grep -i") {
		t.Fatalf("ufwActiveCheck = %q, must not use a case-insensitive substring", ufwActiveCheck)
	}
	if !strings.Contains(ufwActiveCheck, "grep -qx 'Status: active'") {
		t.Fatalf("ufwActiveCheck = %q, want an exact line match", ufwActiveCheck)
	}
}

func TestNodeIPsToVerifySkipsRouterAndDeduplicates(t *testing.T) {
	cfg := testConfig()
	// A duplicate address must not be checked twice.
	cfg.Nodes.Workers = append(cfg.Nodes.Workers, config.NodeDetail{Name: "duplicate", IP: "192.168.10.11"})
	got := (&Phase{cfg: cfg}).nodeIPsToVerify()

	if contains(got, testRouterIP) {
		t.Errorf("nodeIPsToVerify() includes the router: %v", got)
	}
	if !contains(got, "192.168.10.10") {
		t.Errorf("nodeIPsToVerify() excludes jumpstart: %v", got)
	}
	count := 0
	for _, ip := range got {
		if ip == "192.168.10.11" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("nodeIPsToVerify() checked 192.168.10.11 %d times: %v", count, got)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasCommand(commands []testutil.Command, want string) bool {
	for _, c := range commands {
		if strings.Contains(c.Cmd, want) {
			return true
		}
	}
	return false
}
