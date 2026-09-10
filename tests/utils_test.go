package tests

import (
	"context"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
)

func TestUtils_NewFailoverTester(t *testing.T) {
	cfg := &config.Config{
		Network: config.NetworkConfig{
			Main: config.NetworkDetail{CIDR: "192.168.20.0/24"},
		},
		Nodes: config.NodesConfig{
			LB: config.NodeDetail{IP: "192.168.20.100"},
		},
	}
	s := &ssh.Pool{}
	l := libvirt.NewClient("qemu:///system")

	ft := utils.NewFailoverTester(cfg, s, l)
	if ft == nil {
		t.Fatalf("expected non-nil FailoverTester")
	}
}

func TestUtils_NewVerifier(t *testing.T) {
	cfg := &config.Config{}
	s := &ssh.Pool{}
	l := libvirt.NewClient("qemu:///system")

	v := utils.NewVerifier(cfg, s, l)
	if v == nil {
		t.Fatalf("expected non-nil Verifier")
	}
}

func TestUtils_FixBootOrderSkippingNonExistent(t *testing.T) {
	l := libvirt.NewClient("test:///default")
	err := utils.FixBootOrder(t.Context(), l, []string{"nonexistent-vm-9999"})
	if err != nil {
		t.Errorf("expected FixBootOrder to safely skip nonexistent VMs, got: %v", err)
	}
}

func TestUtils_RecreateFailedVMsSafe(t *testing.T) {
	cfg := &config.Config{
		VM: config.VMConfig{StorageDir: t.TempDir()},
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{Name: "jumpstart"},
		},
	}
	l := libvirt.NewClient("test:///default")
	err := utils.RecreateFailedVMs(t.Context(), cfg, l)
	if err != nil {
		t.Errorf("expected RecreateFailedVMs to succeed, got: %v", err)
	}
}

func TestUtils_CheckSSH_Concurrent(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{Name: "vm1", IP: "127.0.0.1"},
			Router:    config.NodeDetail{Name: "vm2", IP: "127.0.0.2"},
		},
	}
	s, _ := ssh.NewPool("/tmp/dummy_key", 100*time.Millisecond)
	defer s.Close()
	l := libvirt.NewClient("test:///default")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Runs concurrently across nodes and should not data race
	_ = utils.CheckSSH(ctx, cfg, s, l)
}

func TestUtils_VerifyAll_Concurrent(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{Name: "jumpstart", IP: "127.0.0.1"},
			LB:        config.NodeDetail{Name: "lb", IP: "127.0.0.2"},
			Monitor:   config.NodeDetail{Name: "monitor", IP: "127.0.0.3"},
			Router:    config.NodeDetail{Name: "router", IP: "127.0.0.4"},
			Masters:   []config.NodeDetail{{Name: "master1", IP: "127.0.0.5"}},
		},
	}
	s, _ := ssh.NewPool("/tmp/dummy_key", 100*time.Millisecond)
	defer s.Close()
	l := libvirt.NewClient("test:///default")

	v := utils.NewVerifier(cfg, s, l)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// VerifyAll runs all 8 phases concurrently. It will fail because dummy IPs are unreachable,
	// but it must execute all phases without deadlocks, panics, or data races.
	err := v.VerifyAll(ctx)
	if err == nil {
		t.Errorf("expected verification to fail with unreachable dummy nodes")
	}
}

func TestUtils_FailoverTester_SkipConditions(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Masters:      []config.NodeDetail{{Name: "master1"}}, // < 2
			CMSFrontends: []config.NodeDetail{{Name: "cms1"}},    // < 2
			Workers:      []config.NodeDetail{{Name: "worker1"}}, // < 2
		},
	}
	s, _ := ssh.NewPool("/tmp/dummy_key", 100*time.Millisecond)
	defer s.Close()
	l := libvirt.NewClient("test:///default")

	ft := utils.NewFailoverTester(cfg, s, l)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := ft.Run(ctx, utils.FailoverOpts{SkipRestore: true})
	if err != nil {
		t.Errorf("expected Run to succeed with skipped scenarios, got: %v", err)
	}
}
