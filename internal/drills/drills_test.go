package drills

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func TestRunCronJobCreatesAndWaitsForSuccess(t *testing.T) {
	fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, cmd string) (string, string, int, error) {
		if strings.Contains(cmd, "kubectl get job") {
			return "1", "", 0, nil
		}
		return "", "", 0, nil
	}}

	name, err := runCronJob(t.Context(), fake, "10.0.0.1", "mariadb-backup", "cms-backup-drill", time.Second)
	if err != nil {
		t.Fatalf("runCronJob: %v", err)
	}
	if !strings.HasPrefix(name, "cms-backup-drill-") {
		t.Errorf("job name = %q, want the cms-backup-drill prefix", name)
	}
	if len(fake.Commands) != 2 {
		t.Fatalf("commands = %d, want create plus one status probe", len(fake.Commands))
	}
	if !strings.Contains(fake.Commands[0].Cmd, "--from=cronjob/mariadb-backup") {
		t.Errorf("create command = %q, want the source CronJob", fake.Commands[0].Cmd)
	}
	if !strings.Contains(fake.Commands[1].Cmd, name) {
		t.Errorf("status command = %q, want the created job name", fake.Commands[1].Cmd)
	}
}

func TestRunCronJobReportsCreateFailure(t *testing.T) {
	fake := &testutil.FakeExecutor{RunFunc: func(context.Context, string, string) (string, string, int, error) {
		return "boom", "", 1, errors.New("exec failed")
	}}

	if _, err := runCronJob(t.Context(), fake, "10.0.0.1", "mariadb-backup", "cms-backup-drill", time.Second); err == nil {
		t.Fatal("expected an error when the Job cannot be created")
	}
}

func TestRunCronJobReportsTimeoutWithDiagnostics(t *testing.T) {
	fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, cmd string) (string, string, int, error) {
		switch {
		case strings.Contains(cmd, "kubectl get job"):
			return "0", "", 0, nil
		case strings.Contains(cmd, "kubectl describe job"):
			return "job status: failed", "", 0, nil
		default:
			return "", "", 0, nil
		}
	}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := runCronJob(ctx, fake, "10.0.0.1", "mariadb-backup", "cms-backup-drill", time.Hour)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "job status: failed") {
		t.Errorf("error = %v, want the diagnostics output", err)
	}
}

func TestDeleteJobIgnoresMissingJobs(t *testing.T) {
	fake := &testutil.FakeExecutor{}

	deleteJob(t.Context(), fake, "10.0.0.1", "cms-backup-drill-1")

	if len(fake.Commands) != 1 {
		t.Fatalf("commands = %d, want one delete", len(fake.Commands))
	}
	if !strings.Contains(fake.Commands[0].Cmd, "--ignore-not-found") {
		t.Errorf("delete command = %q, want --ignore-not-found", fake.Commands[0].Cmd)
	}
}

func TestRunDrillsSkipsTheBackupDrillWhenBackupsAreDisabled(t *testing.T) {
	// With backup.engine empty the CronJobs do not exist. The drill must not
	// fail trying to create a Job from them; it should fall through to the
	// next drill, which here is the missing-monitor check.
	cfg := &config.Config{}
	cfg.Backup.Engine = ""
	fake := &testutil.FakeExecutor{}

	err := RunDrills(t.Context(), cfg, fake, DrillsOptions{SkipTLS: true})
	if err == nil || !strings.Contains(err.Error(), "no monitor node") {
		t.Fatalf("RunDrills() = %v, want the monitor check (the backup drill must be skipped)", err)
	}
	for _, cmd := range fake.Commands {
		if strings.Contains(cmd.Cmd, "mariadb-backup") || strings.Contains(cmd.Cmd, "mariadb-restore-test") {
			t.Errorf("RunDrills triggered a backup CronJob while backups are disabled: %q", cmd.Cmd)
		}
	}
}

// stubLBProbe replaces the reachability probe with a stub and restores the
// production one when the test finishes.
func stubLBProbe(t *testing.T, stub func(ctx context.Context, lbIP, vhost string) bool) {
	t.Helper()
	previous := lbProbe
	lbProbe = stub
	t.Cleanup(func() { lbProbe = previous })
}

func TestWaitLBAnswersRetriesUntilTheBalancerAnswers(t *testing.T) {
	// The first probe can land inside the nginx reload window right after the
	// renewal (run 42482654972); the drill must poll, not fail on the first
	// unanswered attempt.
	attempts := 0
	stubLBProbe(t, func(context.Context, string, string) bool {
		attempts++
		return attempts >= 3
	})

	if !waitLBAnswers(t.Context(), "192.168.20.104", "cms.fake-enterprise.com", 10*time.Second, time.Millisecond) {
		t.Fatal("waitLBAnswers() = false, want it to succeed once the balancer answers")
	}
	if attempts < 3 {
		t.Errorf("probe attempts = %d, want at least the 3 retries", attempts)
	}
}

func TestWaitLBAnswersReportsUnreachableAfterTheBudget(t *testing.T) {
	stubLBProbe(t, func(context.Context, string, string) bool { return false })

	if waitLBAnswers(t.Context(), "192.168.20.104", "cms.fake-enterprise.com", 50*time.Millisecond, time.Millisecond) {
		t.Fatal("waitLBAnswers() = true, want false when the balancer never answers")
	}
}

func TestRunTLSRenewalDrillRotatesTheCertificateAndWaitsForHTTPS(t *testing.T) {
	cfg := &config.Config{}
	cfg.Nodes.LBs = []config.NodeDetail{{Name: "main-lb2", IP: "192.168.20.104"}}
	serials := 0
	fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, cmd string) (string, string, int, error) {
		switch {
		case strings.Contains(cmd, "openssl x509"):
			serials++
			if serials == 1 {
				return "serial=0aaaaa", "", 0, nil
			}
			return "serial=0bbbbb", "", 0, nil
		case strings.Contains(cmd, "cms-tls-renew.sh"):
			return "cms-tls-renew: certificate renewed and nginx reloaded", "", 0, nil
		default:
			return "", "", 0, nil
		}
	}}
	stubLBProbe(t, func(context.Context, string, string) bool { return true })

	if err := runTLSRenewalDrill(t.Context(), cfg, fake); err != nil {
		t.Fatalf("runTLSRenewalDrill() = %v", err)
	}
	commands := make([]string, 0, len(fake.Commands))
	for _, c := range fake.Commands {
		commands = append(commands, c.Cmd)
	}
	if len(commands) != 4 {
		t.Fatalf("commands = %d (%v), want serial, renew, serial and is-active", len(commands), commands)
	}
	if !strings.Contains(commands[1], "RENEW_FORCE=1") {
		t.Errorf("renewal command = %q, want the forced production script", commands[1])
	}
}
