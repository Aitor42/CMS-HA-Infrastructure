package tests

import (
	"os"
	"path/filepath"
	"strings"
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

// fakeVirtInstall puts a recording `virt-install` first on PATH and returns the
// file its arguments are written to. VirtInstall invokes the binary through
// exec.Command, so this exercises the argument construction without a
// hypervisor.
func fakeVirtInstall(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsFile + "\"\n"
	binary := filepath.Join(dir, "virt-install")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake virt-install: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func TestLibvirt_VirtInstallBuildsExpectedArgs(t *testing.T) {
	argsFile := fakeVirtInstall(t)
	c := libvirt.NewClient("test:///default")

	err := c.VirtInstall(t.Context(), libvirt.VirtInstallOpts{
		Name:      "test-vm",
		RAM:       2048,
		VCPUs:     2,
		OSVariant: "ubuntu24.04",
		Disks: []libvirt.DiskOpt{
			{Path: "/var/lib/libvirt/images/test.qcow2", SizeGB: 10},
			{Path: "/var/lib/libvirt/images/seed.iso"},
		},
		Networks: []libvirt.NetworkOpt{
			{Network: "internal", Mac: "52:54:00:10:01:02"},
		},
		BootOrder:     []string{"hd", "network"},
		PXE:           true,
		GraphicsNone:  true,
		NoAutoConsole: true,
		Wait:          30,
	})
	if err != nil {
		t.Fatalf("VirtInstall: %v", err)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{
		"--connect", "test:///default",
		"--name", "test-vm",
		"--memory", "2048",
		"--vcpus", "2",
		"--os-variant", "ubuntu24.04",
		"--disk", "path=/var/lib/libvirt/images/test.qcow2,size=10,format=qcow2,bus=virtio",
		"--disk", "path=/var/lib/libvirt/images/seed.iso,bus=sata,device=cdrom",
		"--network", "network=internal,model=virtio,mac=52:54:00:10:01:02",
		"--pxe",
		"--boot", "hd,network",
		"--wait", "30",
		"--noautoconsole",
		"--graphics", "none",
	}
	if len(got) != len(want) {
		t.Fatalf("virt-install argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("virt-install argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestLibvirt_VirtInstallISOLocationArgs pins the ISO jumpstart path: the
// combined --location "<iso>,kernel=...,initrd=..." form must pass validation
// and reach virt-install verbatim.
func TestLibvirt_VirtInstallISOLocationArgs(t *testing.T) {
	argsFile := fakeVirtInstall(t)
	c := libvirt.NewClient("test:///default")

	err := c.VirtInstall(t.Context(), libvirt.VirtInstallOpts{
		Name:      "iso-jumpstart",
		RAM:       3072,
		VCPUs:     2,
		OSVariant: "ubuntu24.04",
		Disks: []libvirt.DiskOpt{
			{Path: "/var/lib/libvirt/images/jumpstart.qcow2", SizeGB: 30},
			{Path: "/var/lib/libvirt/images/ubuntu-24.04.5-live-server-amd64.iso"},
			{Path: "/var/lib/libvirt/images/seed.iso"},
		},
		Networks: []libvirt.NetworkOpt{
			{Network: "internal"},
		},
		Location:      "/var/lib/libvirt/images/ubuntu-24.04.5-live-server-amd64.iso,kernel=casper/vmlinuz,initrd=casper/initrd",
		ExtraArgs:     "autoinstall ds=nocloud console=ttyS0,115200n8",
		GraphicsNone:  true,
		NoAutoConsole: true,
		NoReboot:      true,
		Wait:          -1,
	})
	if err != nil {
		t.Fatalf("VirtInstall: %v", err)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	for _, want := range []string{
		"--location", "/var/lib/libvirt/images/ubuntu-24.04.5-live-server-amd64.iso,kernel=casper/vmlinuz,initrd=casper/initrd",
		"--extra-args", "autoinstall ds=nocloud console=ttyS0,115200n8",
	} {
		found := false
		for _, arg := range got {
			if arg == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("virt-install argv missing %q (got %v)", want, got)
		}
	}
}

func TestLibvirt_VirtInstallISOLocationRejectsUnsafeOptions(t *testing.T) {
	c := libvirt.NewClient("test:///default")
	err := c.VirtInstall(t.Context(), libvirt.VirtInstallOpts{
		Name:     "iso-jumpstart",
		RAM:      3072,
		VCPUs:    2,
		Location: "/var/lib/libvirt/images/ubuntu.iso,kernel=casper/vmlinuz,evil=$(reboot)",
	})
	if err == nil || !strings.Contains(err.Error(), "not a supported key=value pair") {
		t.Fatalf("VirtInstall with an unsafe location option = %v, want a key=value pair error", err)
	}
}

func TestLibvirt_VirtInstallImportArgs(t *testing.T) {
	argsFile := fakeVirtInstall(t)
	c := libvirt.NewClient("test:///default")

	err := c.VirtInstall(t.Context(), libvirt.VirtInstallOpts{
		Name:      "cloud-jumpstart",
		RAM:       3072,
		VCPUs:     2,
		OSVariant: "ubuntu24.04",
		Disks: []libvirt.DiskOpt{
			{Path: "/var/lib/libvirt/images/jumpstart.qcow2"},
			{Path: "/var/lib/libvirt/images/seed.iso"},
		},
		Networks: []libvirt.NetworkOpt{
			{Network: "internal", Mac: "52:54:00:10:00:01"},
		},
		Import:        true,
		BootOrder:     []string{"hd"},
		NoWait:        true,
		GraphicsNone:  true,
		NoAutoConsole: true,
	})
	if err != nil {
		t.Fatalf("VirtInstall: %v", err)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{
		"--connect", "test:///default",
		"--name", "cloud-jumpstart",
		"--memory", "3072",
		"--vcpus", "2",
		"--os-variant", "ubuntu24.04",
		"--disk", "path=/var/lib/libvirt/images/jumpstart.qcow2,format=qcow2,bus=virtio",
		"--disk", "path=/var/lib/libvirt/images/seed.iso,bus=sata,device=cdrom",
		"--network", "network=internal,model=virtio,mac=52:54:00:10:00:01",
		"--import",
		"--boot", "hd",
		"--noautoconsole",
		"--graphics", "none",
	}
	if len(got) != len(want) {
		t.Fatalf("virt-install argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("virt-install argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, arg := range got {
		if arg == "--wait" || arg == "--location" {
			t.Errorf("imported guest must not use %s; argv = %v", arg, got)
		}
	}
}

func fakeQemuImg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsFile + "\"\n"
	binary := filepath.Join(dir, "qemu-img")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qemu-img: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func TestLibvirt_CreateOverlayDisk(t *testing.T) {
	argsFile := fakeQemuImg(t)
	base := filepath.Join(t.TempDir(), "ubuntu-cloud.img")
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil {
		t.Fatalf("write base image: %v", err)
	}
	overlay := filepath.Join(t.TempDir(), "jumpstart.qcow2")

	if err := libvirt.CreateOverlayDisk(t.Context(), overlay, base, 30); err != nil {
		t.Fatalf("CreateOverlayDisk: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{"create", "-f", "qcow2", "-F", "qcow2", "-b", base, overlay, "30G"}
	if len(got) != len(want) {
		t.Fatalf("qemu-img argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("qemu-img argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if err := libvirt.CreateOverlayDisk(t.Context(), overlay, base, 0); err == nil {
		t.Fatal("expected a non-positive overlay size to be rejected")
	}
}

func TestLibvirt_VirtInstallRejectsCommaInDiskAndNetwork(t *testing.T) {
	argsFile := fakeVirtInstall(t)
	c := libvirt.NewClient("test:///default")

	tests := []struct {
		name string
		opts libvirt.VirtInstallOpts
	}{
		{
			name: "disk path",
			opts: libvirt.VirtInstallOpts{
				Name:  "test-vm",
				Disks: []libvirt.DiskOpt{{Path: "/tmp/a,b.qcow2"}},
			},
		},
		{
			name: "network mac",
			opts: libvirt.VirtInstallOpts{
				Name:     "test-vm",
				Networks: []libvirt.NetworkOpt{{Network: "internal", Mac: "52:54:00:00:00:01,x"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.VirtInstall(t.Context(), tt.opts)
			if err == nil {
				t.Fatal("expected a comma in a virt-install field to be rejected")
			}
			if !strings.Contains(err.Error(), "must not contain commas") {
				t.Errorf("error = %q, want the comma validation message", err)
			}
			if _, statErr := os.Stat(argsFile); statErr == nil {
				t.Error("virt-install must not run when an argument is invalid")
			}
		})
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
