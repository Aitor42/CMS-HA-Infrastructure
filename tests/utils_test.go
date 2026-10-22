package tests

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
)

func newTestPool(t *testing.T) *ssh.Pool {
	t.Helper()
	s, err := ssh.NewPool(filepath.Join(t.TempDir(), "id_ed25519"), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("new ssh pool: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestUtils_NewFailoverTester(t *testing.T) {
	cfg := &config.Config{
		Network: config.NetworkConfig{
			Main: config.NetworkDetail{CIDR: "192.168.20.0/24", VIP: "192.168.20.100"},
		},
		Nodes: config.NodesConfig{
			LBs: []config.NodeDetail{{IP: "192.168.20.103"}, {IP: "192.168.20.104"}},
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
			// Not present in the libvirt test driver: the safe
			// implementation must skip it without touching storage.
			Masters:      []config.NodeDetail{{Name: "master1", IP: "192.168.10.11"}},
			CMSFrontends: []config.NodeDetail{{Name: "cms1", IP: "192.168.20.101"}},
		},
	}
	l := libvirt.NewClient("test:///default")
	// With the destructive pre-fix behavior this would have attempted to
	// destroy/qemu-img-recreate the missing domains and could not have
	// failed cleanly; it must now pass while touching nothing.
	if err := utils.RecreateFailedVMs(t.Context(), cfg, l); err != nil {
		t.Errorf("expected RecreateFailedVMs to skip missing VMs without error, got: %v", err)
	}
}

func TestUtils_CheckSSH_Concurrent(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{Name: "vm1", IP: "127.0.0.1"},
			Router:    config.NodeDetail{Name: "vm2", IP: "127.0.0.2"},
		},
	}
	s := newTestPool(t)
	l := libvirt.NewClient("test:///default")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// CheckSSH fans out across nodes; the race detector covers the shared pool.
	_ = utils.CheckSSH(ctx, cfg, s, l)
}

func TestUtils_VerifyAll_Concurrent(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{Name: "jumpstart", IP: "127.0.0.1"},
			LBs:       []config.NodeDetail{{Name: "lb", IP: "127.0.0.2"}},
			Monitor:   config.NodeDetail{Name: "monitor", IP: "127.0.0.3"},
			Router:    config.NodeDetail{Name: "router", IP: "127.0.0.4"},
			Masters:   []config.NodeDetail{{Name: "master1", IP: "127.0.0.5"}},
		},
	}
	s := newTestPool(t)
	l := libvirt.NewClient("test:///default")

	v := utils.NewVerifier(cfg, s, l)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// VerifyAll runs every check concurrently. The dummy IPs make it fail, but
	// it must complete without deadlocks, panics or data races.
	if verr := v.VerifyAll(ctx); verr == nil {
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
	s := newTestPool(t)
	l := libvirt.NewClient("test:///default")

	ft := utils.NewFailoverTester(cfg, s, l)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if rerr := ft.Run(ctx, utils.FailoverOpts{SkipRestore: true}); rerr != nil {
		t.Errorf("expected Run to succeed with skipped scenarios, got: %v", rerr)
	}
}

func TestUtils_StopAllVMs_NoRunningVMs(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Router:    config.NodeDetail{Name: "router-nonexistent-123"},
			Jumpstart: config.NodeDetail{Name: "jumpstart-nonexistent-123"},
		},
	}
	l := libvirt.NewClient("test:///default")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := utils.StopAllVMs(ctx, cfg, l, utils.StopOptions{Timeout: 1 * time.Second})
	if err != nil {
		t.Errorf("expected StopAllVMs to succeed when no VMs are running, got: %v", err)
	}
}
