package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/repairssh"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func repairSSHConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return &config.Config{
		SSH: config.SSHConfig{
			User:           "ubuntu",
			KnownHostsFile: t.TempDir() + "/cms_known_hosts",
		},
		Nodes: config.NodesConfig{
			Router:    config.NodeDetail{Name: "router", IP: "192.168.10.1", FQDN: "router.internal.local"},
			Jumpstart: config.NodeDetail{Name: "jumpstart", IP: "192.168.10.10", FQDN: "jumpstart.internal.local"},
			Masters: []config.NodeDetail{
				{Name: "master1", IP: "192.168.10.11", FQDN: "master1.internal.local"},
			},
			LBs: []config.NodeDetail{
				{Name: "lb1", IP: "192.168.20.101", FQDN: "lb1.main.local"},
			},
		},
	}
}

func hasCommand(commands []testutil.Command, host, substr string) bool {
	for _, c := range commands {
		if c.Host == host && strings.Contains(c.Cmd, substr) {
			return true
		}
	}
	return false
}

func TestRepairSSHPhaseUsesExecutor(t *testing.T) {
	cfg := repairSSHConfig(t)
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, cmd string) (string, string, int, error) {
			if host == cfg.Nodes.Jumpstart.IP && strings.Contains(cmd, "cat /root/.ssh/id_ed25519.pub") {
				return "ssh-ed25519 AAAATEST jumpstart\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}

	phase := repairssh.NewPhase(cfg, fake)
	if err := phase.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Every node must get a hostname and the generated /etc/hosts.
	for _, n := range cfg.AllNodes() {
		if n.IP == "" {
			continue
		}
		if !hasCommand(fake.Commands, n.IP, "hostnamectl set-hostname "+n.FQDN) {
			t.Errorf("no hostnamectl command for %s", n.IP)
		}
		hosts := fake.Files[n.IP]["/etc/hosts"]
		if !strings.Contains(string(hosts), "127.0.0.1 localhost") {
			t.Errorf("%s: /etc/hosts missing localhost: %q", n.IP, hosts)
		}
	}
	if hosts := fake.Files["192.168.10.11"]["/etc/hosts"]; !strings.Contains(string(hosts), "puppet") {
		t.Errorf("masters' /etc/hosts should alias the jumpstart as puppet: %q", hosts)
	}

	// The jumpstart key is distributed to every other node, never to itself.
	for _, ip := range []string{"192.168.10.1", "192.168.10.11", "192.168.20.101"} {
		if !hasCommand(fake.Commands, ip, "ssh-ed25519 AAAATEST jumpstart") {
			t.Errorf("%s did not receive the jumpstart public key", ip)
		}
	}
	if hasCommand(fake.Commands, "192.168.10.10", "authorized_keys") {
		t.Error("jumpstart should not add its own key to authorized_keys")
	}
}

func TestRepairSSHPhaseFailsWhenWaitFails(t *testing.T) {
	cfg := repairSSHConfig(t)
	fake := &testutil.FakeExecutor{
		WaitErr: map[string]error{"192.168.10.1": errors.New("connection refused")},
	}

	err := repairssh.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "failed waiting for SSH") {
		t.Fatalf("Run() = %v, want a wait failure", err)
	}
	if len(fake.Commands) != 0 {
		t.Errorf("phase ran %d commands after the wait failure, want 0", len(fake.Commands))
	}
}

func TestRepairSSHPhaseRejectsEmptyJumpstartKey(t *testing.T) {
	cfg := repairSSHConfig(t)
	fake := &testutil.FakeExecutor{}

	err := repairssh.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "jumpstart public key is empty") {
		t.Fatalf("Run() = %v, want the empty-key error", err)
	}
}
