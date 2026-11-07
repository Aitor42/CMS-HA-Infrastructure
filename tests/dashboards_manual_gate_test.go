package tests

import (
	"context"
	"os"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/dashboards"
)

// TestDashboards_ManualGate runs every shipped dashboard query against a live
// Prometheus. It is opt-in because it needs a deployed monitor node:
//
//	CMS_GATE_PROM=http://192.168.10.20:9090 go test ./tests -run ManualGate -v
func TestDashboards_ManualGate(t *testing.T) {
	base := os.Getenv("CMS_GATE_PROM")
	if base == "" {
		t.Skip("CMS_GATE_PROM not set")
	}
	report, err := dashboards.CheckPrometheus(context.Background(), base)
	if err != nil {
		t.Fatalf("CheckPrometheus: %v", err)
	}
	if !report.OK() {
		t.Fatalf("gate failed: %s", report.Error())
	}
	t.Logf("OK: %d/%d panel queries return data", report.Queries, report.Queries)
}
