package tests

import (
	"context"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
)

func TestLibvirt_NewClient(t *testing.T) {
	c := libvirt.NewClient("qemu:///system")
	if c == nil {
		t.Fatal("expected non-nil Client")
	}
	if c.URI != "qemu:///system" {
		t.Errorf("unexpected URI: %s", c.URI)
	}
}

func TestLibvirt_VirtInstallOptsFormatting(t *testing.T) {
	opts := libvirt.VirtInstallOpts{
		Name:      "test-vm",
		RAM:       2048,
		VCPUs:     2,
		OSVariant: "ubuntu24.04",
		Disks: []libvirt.DiskOpt{
			{Path: "/var/lib/libvirt/images/test.qcow2", SizeGB: 10},
		},
		Networks: []libvirt.NetworkOpt{
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

func TestLibvirt_MockDomainOperations(t *testing.T) {
	c := &libvirt.Client{
		URI:     "test:///default",
		UseSudo: false,
	}

	ctx := context.Background()
	_ = c.DomainExists(ctx, "nonexistent-vm-12345")
	_ = c.NetExists(ctx, "nonexistent-net-12345")
}
