package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/monitoring"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func monitoringConfig() *config.Config {
	return &config.Config{
		Nodes: config.NodesConfig{
			Monitor: config.NodeDetail{Name: "internal-monitor", IP: "192.168.10.20"},
			Router:  config.NodeDetail{Name: "ufw-router", IP: "192.168.10.1"},
			Storage: config.NodeDetail{Name: "internal-storage", IP: "192.168.10.15"},
			Masters: []config.NodeDetail{
				{Name: "internal-master1", IP: "192.168.10.11"},
			},
			Workers: []config.NodeDetail{
				{Name: "internal-worker1", IP: "192.168.10.13"},
			},
			LBs: []config.NodeDetail{
				{Name: "main-lb1", IP: "192.168.20.103"},
			},
			CMSFrontends: []config.NodeDetail{
				{Name: "main-cms1", IP: "192.168.20.101"},
			},
		},
	}
}

func monitoringRunFunc(t *testing.T) func(context.Context, string, string) (string, string, int, error) {
	t.Helper()
	return func(_ context.Context, _ string, cmd string) (string, string, int, error) {
		switch {
		case strings.Contains(cmd, "kubectl create token"):
			return "kube-token\n", "", 0, nil
		case strings.Contains(cmd, "api/v1/status/buildinfo"):
			return `{"status":"success","data":{"version":"2.53.0"}}`, "", 0, nil
		default:
			return "", "", 0, nil
		}
	}
}

func TestMonitoringPhaseReusesTheGeneratedPasswordAndValidatesRules(t *testing.T) {
	cfg := monitoringConfig()
	fake := &testutil.FakeExecutor{
		ReadFiles: map[string]map[string][]byte{
			"192.168.10.20": {
				"/etc/grafana/grafana-admin.env": []byte("GF_SECURITY_ADMIN_PASSWORD=kept-secret\n"),
			},
		},
		RunFunc: monitoringRunFunc(t),
	}

	if err := monitoring.NewPhase(cfg, fake).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	env := string(fake.Files["192.168.10.20"]["/etc/grafana/grafana-admin.env"])
	if env != "GF_SECURITY_ADMIN_PASSWORD=kept-secret\n" {
		t.Errorf("Grafana env = %q, want the existing password reused", env)
	}
	if !hasCommand(fake.Commands, "192.168.10.20", "promtool check rules /etc/prometheus/alert-rules.yml") {
		t.Error("the phase did not validate the alert rules with promtool")
	}
	if !hasCommand(fake.Commands, "192.168.10.20", "http://127.0.0.1:9093/-/ready") {
		t.Error("the phase did not wait for the Alertmanager listener")
	}
	// The monitor node runs role::base, so its own exporter must be checked.
	if !hasCommand(fake.Commands, "192.168.10.20", "systemctl is-active prometheus-node-exporter") {
		t.Error("the phase did not verify the monitor node's exporter")
	}
}

func TestMonitoringPhaseFailsWhenTheAlertRulesAreInvalid(t *testing.T) {
	cfg := monitoringConfig()
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "kubectl create token"):
				return "kube-token\n", "", 0, nil
			case strings.Contains(cmd, "api/v1/status/buildinfo"):
				return `{"status":"success","data":{"version":"2.53.0"}}`, "", 0, nil
			case strings.Contains(cmd, "promtool check rules"):
				return "", "error parsing rules", 1, nil
			default:
				return "", "", 0, nil
			}
		},
	}

	err := monitoring.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "alert rules") {
		t.Fatalf("Run() = %v, want invalid alert rules to fail the phase", err)
	}
}
