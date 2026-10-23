package monitoring

import (
	"strings"
	"testing"
)

func TestMonitoringVerifyWaitsForGrafanaHTTP(t *testing.T) {
	if !strings.Contains(monitoringVerifyCommand, "http://127.0.0.1:3000/api/health") {
		t.Fatalf("monitoring readiness check must wait for Grafana's HTTP listener: %q", monitoringVerifyCommand)
	}
	if !strings.Contains(monitoringVerifyCommand, "systemctl is-active grafana-server") {
		t.Fatalf("monitoring readiness check must still require the Grafana unit: %q", monitoringVerifyCommand)
	}
}
