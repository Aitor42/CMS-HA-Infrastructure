package monitoring

import (
	"context"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func TestMonitoringVerifyWaitsForEveryListener(t *testing.T) {
	for _, want := range []string{
		"systemctl is-active prometheus grafana-server prometheus-alertmanager",
		"http://127.0.0.1:3000/api/health",
		"http://127.0.0.1:9090/-/ready",
		"http://127.0.0.1:9093/-/ready",
	} {
		if !strings.Contains(monitoringVerifyCommand, want) {
			t.Fatalf("monitoring readiness check is missing %q: %q", want, monitoringVerifyCommand)
		}
	}
}

func TestGrafanaAdminPasswordPrefersTheConfiguredValue(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	password, generated, err := grafanaAdminPassword(t.Context(), fake, "192.168.10.20", "operator-value")
	if err != nil {
		t.Fatalf("grafanaAdminPassword: %v", err)
	}
	if password != "operator-value" || generated {
		t.Fatalf("grafanaAdminPassword() = %q, generated=%v, want the configured value", password, generated)
	}
}

func TestGrafanaAdminPasswordReusesTheExistingFile(t *testing.T) {
	fake := &testutil.FakeExecutor{ReadFiles: map[string]map[string][]byte{
		"192.168.10.20": {grafanaAdminEnvPath: []byte("GF_SECURITY_ADMIN_PASSWORD=kept-secret\n")},
	}}
	password, generated, err := grafanaAdminPassword(t.Context(), fake, "192.168.10.20", "")
	if err != nil {
		t.Fatalf("grafanaAdminPassword: %v", err)
	}
	if password != "kept-secret" || generated {
		t.Fatalf("grafanaAdminPassword() = %q, generated=%v, want the existing password reused", password, generated)
	}
}

func TestGrafanaAdminPasswordGeneratesOnlyWhenMissing(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	password, generated, err := grafanaAdminPassword(t.Context(), fake, "192.168.10.20", "")
	if err != nil {
		t.Fatalf("grafanaAdminPassword: %v", err)
	}
	if !generated || len(password) < 32 {
		t.Fatalf("grafanaAdminPassword() = %q, generated=%v, want a generated password", password, generated)
	}
}

func TestParseGrafanaAdminPassword(t *testing.T) {
	if got := parseGrafanaAdminPassword("OTHER=1\nGF_SECURITY_ADMIN_PASSWORD=abc-123\n"); got != "abc-123" {
		t.Errorf("parseGrafanaAdminPassword() = %q, want abc-123", got)
	}
	if got := parseGrafanaAdminPassword("OTHER=1\n"); got != "" {
		t.Errorf("parseGrafanaAdminPassword() = %q, want an empty value", got)
	}
}

func TestDeployAlertRulesValidatesBeforeReloading(t *testing.T) {
	var commands []string
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			commands = append(commands, cmd)
			return "", "", 0, nil
		},
	}
	if err := deployAlertRules(t.Context(), fake, "192.168.10.20"); err != nil {
		t.Fatalf("deployAlertRules: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("deployAlertRules ran %d commands, want 1", len(commands))
	}
	check := strings.Index(commands[0], "promtool check rules")
	reload := strings.Index(commands[0], "systemctl reload prometheus")
	if check < 0 || reload < 0 || check > reload {
		t.Fatalf("deployAlertRules command does not validate before reloading: %q", commands[0])
	}
	rules := string(fake.Files["192.168.10.20"]["/etc/prometheus/alert-rules.yml"])
	if !strings.Contains(rules, "groups:") {
		t.Errorf("deployed alert rules = %q, want the embedded rule file", rules)
	}
}

func TestDeployAlertRulesFailsWhenPromtoolRejectsTheFile(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			return "", "error parsing rules", 1, nil
		},
	}
	err := deployAlertRules(t.Context(), fake, "192.168.10.20")
	if err == nil || !strings.Contains(err.Error(), "alert rules") {
		t.Fatalf("deployAlertRules() = %v, want a fatal validation error", err)
	}
}
