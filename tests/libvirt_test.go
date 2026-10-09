package tests

import (
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

	ctx := t.Context()

	if c.DomainExists(ctx, "nonexistent-vm-12345") {
		t.Error("expected DomainExists=false for a nonexistent VM, got true")
	}
	if c.NetExists(ctx, "nonexistent-net-12345") {
		t.Error("expected NetExists=false for a nonexistent network, got true")
	}

	// The checked variants must return (false, nil) for a cleanly
	// nonexistent domain: RecreateFailedVMs/FixBootOrder rely on that to
	// skip nodes instead of failing or destroying things.
	exists, err := c.DomainExistsChecked(ctx, "nonexistent-vm-12345")
	if err != nil {
		t.Errorf("expected checked lookup of nonexistent VM to not error, got: %v", err)
	}
	if exists {
		t.Error("expected DomainExistsChecked=false for a nonexistent VM, got true")
	}
}
