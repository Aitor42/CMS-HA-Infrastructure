package traffic

import (
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func TestNewTraffic(t *testing.T) {
	cfg := &config.Config{}
	opts := Options{
		Mode:        "internal",
		TargetIP:    "192.168.20.100",
		Duration:    1,
		Concurrency: 2,
		WithDB:      false,
		Verbose:     false,
	}

	tr := New(cfg, opts)
	if tr == nil {
		t.Fatal("expected non-nil Generator")
	}
}
