package libvirt

import (
	"context"
	"testing"
)

func TestNewClient(t *testing.T) {
	c := NewClient("qemu:///system")
	if c == nil {
		t.Fatal("expected non-nil Client")
	}
	if c.URI != "qemu:///system" {
		t.Errorf("unexpected URI: %s", c.URI)
	}
}

func TestVirtInstallOptsFormatting(t *testing.T) {
	opts := VirtInstallOpts{
		Name:      "test-vm",
		RAM:       2048,
		VCPUs:     2,
		OSVariant: "ubuntu24.04",
		Disks: []DiskOpt{
			{Path: "/var/lib/libvirt/images/test.qcow2", SizeGB: 10},
		},
		Networks: []NetworkOpt{
			{Network: "internal", Mac: "52:54:00:10:01:02"},
		},
		BootOrder:    []string{"hd", "network"},
		PXE:          true,
		GraphicsNone: true,
	}

	if opts.Name != "test-vm" {
		t.Errorf("expected test-vm, got %s", opts.Name)
	}
	if len(opts.BootOrder) != 2 {
		t.Errorf("expected 2 boot devices, got %d", len(opts.BootOrder))
	}
}

func TestMockDomainOperations(t *testing.T) {
	c := &Client{
		URI:     "test:///default",
		UseSudo: false,
	}

	ctx := context.Background()
	_ = c.DomainExists(ctx, "nonexistent-vm-12345")
	_ = c.NetExists(ctx, "nonexistent-net-12345")
}
