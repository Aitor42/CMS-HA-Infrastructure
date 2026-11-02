package monitoring

import (
	"context"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
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

func TestGrafanaAdminResetCommandStreamsThePasswordOnStdin(t *testing.T) {
	cmd := grafanaAdminResetCommand(grafanaAdminEnvPath)
	// The password must come from the EnvironmentFile on stdin. Passing it as
	// an argv word made the CLI parse a leading "-" as a flag.
	const tail = "admin reset-admin-password --password-from-stdin && systemctl restart grafana-server"
	if !strings.HasSuffix(cmd, tail) {
		t.Fatalf("grafanaAdminResetCommand() does not end with %q: %q", tail, cmd)
	}
	read := "sed -n 's/^GF_SECURITY_ADMIN_PASSWORD=//p' " + grafanaAdminEnvPath + " | "
	if !strings.Contains(cmd, read) {
		t.Fatalf("grafanaAdminResetCommand() does not stream the password from %s: %q", grafanaAdminEnvPath, cmd)
	}
	if strings.Contains(cmd, "reset-admin-password '") {
		t.Fatalf("grafanaAdminResetCommand() still passes the password as an argument: %q", cmd)
	}
}

func TestConfigureGrafanaAdminKeepsThePasswordOutOfArgv(t *testing.T) {
	const password = "-leading-dash-secret-0123456789"
	var commands []string
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			commands = append(commands, cmd)
			return "", "", 0, nil
		},
	}
	phase := &Phase{
		cfg:  &config.Config{Monitoring: config.MonitoringConfig{GrafanaAdminPassword: password}},
		pool: fake,
	}
	if err := phase.configureGrafanaAdmin(t.Context(), "192.168.10.20"); err != nil {
		t.Fatalf("configureGrafanaAdmin: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("configureGrafanaAdmin ran %d commands, want 1", len(commands))
	}
	if strings.Contains(commands[0], password) {
		t.Fatalf("the admin password leaked into argv: %q", commands[0])
	}
	want := "GF_SECURITY_ADMIN_PASSWORD=" + password + "\n"
	if got := string(fake.Files["192.168.10.20"][grafanaAdminEnvPath]); got != want {
		t.Fatalf("EnvironmentFile = %q, want %q", got, want)
	}
}

func TestConfigureGrafanaAdminSurfacesCLIStdout(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, _ string) (string, string, int, error) {
			// Grafana prints CLI errors to stdout and exits 1 with an empty
			// stderr, which hid the original failure.
			return "Error: ✗ flag provided but not defined: -secret", "", 1, nil
		},
	}
	phase := &Phase{
		cfg:  &config.Config{Monitoring: config.MonitoringConfig{GrafanaAdminPassword: "operator-value"}},
		pool: fake,
	}
	err := phase.configureGrafanaAdmin(t.Context(), "192.168.10.20")
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("configureGrafanaAdmin() = %v, want the CLI stdout in the error", err)
	}
	if !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("configureGrafanaAdmin() = %v, want the exit code in the error", err)
	}
}

func TestConfigureGrafanaAdminRetriesATransientDatabaseLock(t *testing.T) {
	oldDelay := grafanaAdminRetryDelay
	grafanaAdminRetryDelay = 0
	t.Cleanup(func() { grafanaAdminRetryDelay = oldDelay })

	calls := 0
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, _ string) (string, string, int, error) {
			calls++
			if calls == 1 {
				return "database is locked", "", 1, nil
			}
			return "", "", 0, nil
		},
	}
	phase := &Phase{
		cfg:  &config.Config{Monitoring: config.MonitoringConfig{GrafanaAdminPassword: "operator-value"}},
		pool: fake,
	}
	if err := phase.configureGrafanaAdmin(t.Context(), "192.168.10.20"); err != nil {
		t.Fatalf("configureGrafanaAdmin: %v", err)
	}
	if calls != 2 {
		t.Fatalf("configureGrafanaAdmin ran the reset %d times, want a single retry", calls)
	}
}

func TestConfigureGrafanaAdminStopsRetryingAfterTheLimit(t *testing.T) {
	oldDelay := grafanaAdminRetryDelay
	grafanaAdminRetryDelay = 0
	t.Cleanup(func() { grafanaAdminRetryDelay = oldDelay })

	calls := 0
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, _ string) (string, string, int, error) {
			calls++
			return "database is locked", "", 1, nil
		},
	}
	phase := &Phase{
		cfg:  &config.Config{Monitoring: config.MonitoringConfig{GrafanaAdminPassword: "operator-value"}},
		pool: fake,
	}
	err := phase.configureGrafanaAdmin(t.Context(), "192.168.10.20")
	if err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("configureGrafanaAdmin() = %v, want the last failure surfaced", err)
	}
	if calls != grafanaAdminAttempts {
		t.Fatalf("configureGrafanaAdmin ran the reset %d times, want %d", calls, grafanaAdminAttempts)
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
