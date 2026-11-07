package hypervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
)

func TestRestrictDiskHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := RestrictDisk(ctx, filepath.Join(t.TempDir(), "missing.qcow2"))
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("RestrictDisk() = %v, want the context cancellation", err)
	}
}

func TestDynamicOwnershipEnabled(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "default when absent", in: "", want: true},
		{name: "commented default", in: "#dynamic_ownership = 0\n", want: true},
		{name: "explicit enabled", in: "dynamic_ownership = 1\n", want: true},
		{name: "explicit disabled", in: "dynamic_ownership = 0\n", want: false},
		{name: "quoted disabled", in: "dynamic_ownership = \"0\"\n", want: false},
		{name: "unrelated key", in: "user = \"libvirt-qemu\"\ndynamic_ownership = 0\n", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dynamicOwnershipEnabled([]byte(tt.in)); got != tt.want {
				t.Errorf("dynamicOwnershipEnabled(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestQEMUUserFromConfig(t *testing.T) {
	old := qemuConfigPath
	t.Cleanup(func() { qemuConfigPath = old })
	qemuConfigPath = filepath.Join(t.TempDir(), "qemu.conf")
	if err := os.WriteFile(qemuConfigPath, []byte("#user = \"root\"\nuser = \"lab-qemu\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := qemuUser(t.Context()); got != "lab-qemu" {
		t.Fatalf("qemuUser(t.Context()) = %q, want lab-qemu", got)
	}
}

func TestCheckDiskAccessRejectsDisabledDynamicOwnership(t *testing.T) {
	old := qemuConfigPath
	t.Cleanup(func() { qemuConfigPath = old })
	qemuConfigPath = filepath.Join(t.TempDir(), "qemu.conf")
	if err := os.WriteFile(qemuConfigPath, []byte("dynamic_ownership = 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDiskAccess(t.Context(), &libvirt.Client{URI: "qemu:///system"}); err == nil {
		t.Fatal("expected an error when dynamic_ownership is disabled")
	}
	if err := CheckDiskAccess(t.Context(), &libvirt.Client{URI: "qemu:///session"}); err != nil {
		t.Fatalf("session connections should not require system dynamic ownership: %v", err)
	}
}
