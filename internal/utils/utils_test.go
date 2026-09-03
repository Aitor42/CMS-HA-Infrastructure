package utils

import (
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

func TestNewFailoverTester(t *testing.T) {
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

	ft := NewFailoverTester(cfg, s, l)
	if ft == nil {
		t.Fatalf("expected non-nil FailoverTester")
	}
}

func TestNewVerifier(t *testing.T) {
	cfg := &config.Config{}
	s := &ssh.Pool{}
	l := libvirt.NewClient("qemu:///system")

	v := NewVerifier(cfg, s, l)
	if v == nil {
		t.Fatalf("expected non-nil Verifier")
	}
}

func TestFixBootOrderSkippingNonExistent(t *testing.T) {
	l := libvirt.NewClient("test:///default")
	err := FixBootOrder(t.Context(), l, []string{"nonexistent-vm-9999"})
	if err != nil {
		t.Errorf("expected FixBootOrder to safely skip nonexistent VMs, got: %v", err)
	}
}

func TestRecreateFailedVMsSafe(t *testing.T) {
	cfg := &config.Config{
		VM: config.VMConfig{StorageDir: t.TempDir()},
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{Name: "jumpstart"},
		},
	}
	l := libvirt.NewClient("test:///default")
	err := RecreateFailedVMs(t.Context(), cfg, l)
	if err != nil {
		t.Errorf("expected RecreateFailedVMs to succeed, got: %v", err)
	}
}
