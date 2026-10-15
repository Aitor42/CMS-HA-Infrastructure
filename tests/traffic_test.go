package tests

import (
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

// The k6 invocation internals (resolveBaseURL/buildArgs) are white-box and stay
// in internal/phases/traffic; the exported config accessors are covered here.
func TestTraffic_VHostDefault(t *testing.T) {
	cfg := &config.Config{}
	if got := cfg.TrafficVHost(); got != "cms.fake-enterprise.com" {
		t.Fatalf("TrafficVHost() default = %q", got)
	}
	cfg.Traffic.VHost = "  cms.lab  "
	if got := cfg.TrafficVHost(); got != "cms.lab" {
		t.Fatalf("TrafficVHost() override = %q, want cms.lab", got)
	}
}

func TestTraffic_K6ImageDefault(t *testing.T) {
	cfg := &config.Config{}
	if got := cfg.K6Image(); got != "grafana/k6:0.55.0" {
		t.Fatalf("K6Image() default = %q", got)
	}
	cfg.Traffic.K6Version = "0.60.0"
	if got := cfg.K6Image(); got != "grafana/k6:0.60.0" {
		t.Fatalf("K6Image() override = %q", got)
	}
}
