package tests

import (
	"context"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/initvms"
)

func TestInitVMs_CleanupMutuallyExclusiveOptions(t *testing.T) {
	l := libvirt.NewClient("test:///default")

	err := initvms.Cleanup(context.Background(), &config.Config{}, l, initvms.CleanupOptions{
		VMsOnly:      true,
		NetworksOnly: true,
	})
	if err == nil {
		t.Fatalf("expected an error when both VMsOnly and NetworksOnly are set")
	}
}
