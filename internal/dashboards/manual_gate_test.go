package dashboards

import (
	"context"
	"os"
	"testing"
)

func TestManualGate(t *testing.T) {
	base := os.Getenv("CMS_GATE_PROM")
	if base == "" {
		t.Skip("CMS_GATE_PROM not set")
	}
	report, err := CheckPrometheus(context.Background(), base)
	if err != nil {
		t.Fatalf("CheckPrometheus: %v", err)
	}
	if !report.OK() {
		t.Fatalf("gate failed: %s", report.Error())
	}
	t.Logf("OK: %d/%d panel queries return data", report.Queries, report.Queries)
}
