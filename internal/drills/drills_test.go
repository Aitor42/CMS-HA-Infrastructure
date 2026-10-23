package drills

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
