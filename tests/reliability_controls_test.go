package tests

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/cmd/cms-ha/root"
	"gopkg.in/yaml.v3"
)

func TestReliabilityFailoverFlags(t *testing.T) {
	cmd, _, err := root.GetRootCmd().Find([]string{"test", "failover"})
	if err != nil || cmd == nil || cmd.Name() != "failover" {
		t.Fatalf("find failover: %v", err)
	}
	for _, tt := range []struct{ name, kind, defaultValue string }{
		{"require-all", "bool", "false"},
		{"results-file", "string", ""},
		{"skip-restore", "bool", "false"},
	} {
		flag := cmd.Flags().Lookup(tt.name)
		if flag == nil || flag.Value.Type() != tt.kind || flag.DefValue != tt.defaultValue {
			t.Fatalf("unexpected --%s flag: %+v", tt.name, flag)
		}
	}
}

func TestReliabilityFailoverCLIHonorsStrictModeAndResultsPath(t *testing.T) {
	rootCmd := root.GetRootCmd()
	cmd, _, err := rootCmd.Find([]string{"test", "failover"})
	if err != nil {
		t.Fatal(err)
	}
	oldConfig := rootCmd.PersistentFlags().Lookup("config").Value.String()
	oldStrict := cmd.Flags().Lookup("require-all").Value.String()
	oldSkipRestore := cmd.Flags().Lookup("skip-restore").Value.String()
	oldResults := cmd.Flags().Lookup("results-file").Value.String()
	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set("config", oldConfig)
		_ = cmd.Flags().Set("require-all", oldStrict)
		_ = cmd.Flags().Set("skip-restore", oldSkipRestore)
		_ = cmd.Flags().Set("results-file", oldResults)
		rootCmd.SetArgs(nil)
		rootCmd.SetContext(context.Background())
	})
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// No nodes and no real SSH key: the real CLI must finish without contacting
	// SSH or libvirt. Keep all configuration and potential key paths temporary.
	configData, err := yaml.Marshal(map[string]any{
		"vm": map[string]string{"storage_dir": filepath.Join(dir, "images")},
		"ssh": map[string]string{
			"private_key":      filepath.Join(dir, "absent-key"),
			"known_hosts_file": filepath.Join(dir, "known_hosts"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "partial.yaml")
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, strict := range []string{"false", "true"} {
		path := filepath.Join(dir, "results-"+strict+".json")
		rootCmd.SetArgs([]string{"-c", configPath, "test", "failover", "--require-all=" + strict, "--skip-restore", "--results-file", path})
		err := root.ExecuteContext(t.Context())
		if (err != nil) != (strict == "true") {
			t.Fatalf("require-all=%s: %v", strict, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report struct {
			RequireAll  bool                      `json:"require_all"`
			SkipRestore bool                      `json:"skip_restore"`
			Complete    bool                      `json:"complete"`
			Scenarios   []struct{ Status string } `json:"scenarios"`
		}
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		if report.RequireAll != (strict == "true") || !report.SkipRestore || !report.Complete || len(report.Scenarios) != 5 {
			t.Fatalf("CLI flags did not reach the runner: %+v", report)
		}
		for _, result := range report.Scenarios {
			if result.Status != "SKIP" {
				t.Fatalf("unexpected CLI result: %+v", result)
			}
		}
	}
}

func TestReliabilityIntegrationRequiresAllScenarios(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("integration", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, want := range []string{
		`test failover --require-all`,
		`--results-file "$ARTIFACTS_DIR/failover.json"`,
		`set -euo pipefail`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("integration acceptance is missing %q", want)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "bash", "-n", filepath.Join("integration", "run.sh")).CombinedOutput(); err != nil {
		t.Fatalf("integration script syntax: %v\n%s", err, out)
	}
}

func reliabilityRestoreScripts(t *testing.T) (fetch, verify string) {
	t.Helper()
	data, err := cms.KubernetesFS.ReadFile("kubernetes/backups/mariadb-restore-test-cronjob.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec struct {
			JobTemplate struct {
				Spec struct {
					Template struct {
						Spec struct {
							InitContainers []struct{ Command []string } `yaml:"initContainers"`
							Containers     []struct{ Command []string } `yaml:"containers"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			} `yaml:"jobTemplate"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	pod := doc.Spec.JobTemplate.Spec.Template.Spec
	if len(pod.InitContainers) != 1 || len(pod.Containers) != 1 || len(pod.InitContainers[0].Command) != 3 || len(pod.Containers[0].Command) != 3 {
		t.Fatal("unexpected restore drill container commands")
	}
	return pod.InitContainers[0].Command[2], pod.Containers[0].Command[2]
}

// Execute both real container scripts with a deterministic clock and fake S3/DB
// commands. Absolute volume paths are relocated; no services or containers run.
func reliabilityRestoreHarness(t *testing.T) (dir string, run func(string) (string, error)) {
	t.Helper()
	dir = t.TempDir()
	for _, sub := range []string{"bin", "staging", "metrics"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write("clock", "100\n")
	for name, body := range map[string]string{
		"date": `if [ "${1:-}" = '+%s' ]; then cat "$TEST_DIR/clock"; else echo 'mock date'; fi`,
		"rclone": `case "$1" in
  lsf) echo wordpress-20261102.sql.gz ;;
  copyto)
    printf 'SELECT 1;\n' > "$3"
    echo $(( $(cat "$TEST_DIR/clock") + 60 )) > "$TEST_DIR/clock"
    ;;
  *) exit 1 ;;
esac`,
		"mariadb-install-db": `echo $(( $(cat "$TEST_DIR/clock") + 20 )) > "$TEST_DIR/clock"`,
		"mariadbd":           `exit 0`,
		"mariadb": `case "$*" in
  *'SELECT 1'*) exit 0 ;;
  *'table_name='*) echo "${TABLE_COUNT:-1}" ;;
  *'SELECT COUNT'*) echo 3 ;;
  *) cat >/dev/null
     echo $(( $(cat "$TEST_DIR/clock") + 20 )) > "$TEST_DIR/clock"
     exit "${SQL_EXIT:-0}" ;;
esac`,
		"mariadb-admin": `exit 0`,
		"zcat":          `cat "$1"`,
	} {
		write(filepath.Join("bin", name), "#!/bin/sh\nset -eu\n"+body+"\n")
	}
	run = func(script string) (string, error) {
		t.Helper()
		script = strings.NewReplacer(
			"/staging", filepath.Join(dir, "staging"),
			"/host-metrics", filepath.Join(dir, "metrics"),
			"/tmp/mysq", filepath.Join(dir, "mysq"),
		).Replace(script)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		shell := "sh"
		if strings.Contains(script, "set -euo pipefail") {
			shell = "bash"
		}
		cmd := exec.CommandContext(ctx, shell, "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"),
			"TEST_DIR="+dir, "S3_ENDPOINT=http://unused.invalid", "S3_ACCESS_KEY=fake",
			"S3_SECRET_KEY=fake", "S3_REGION=test", "S3_BUCKET=backups")
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("restore script did not terminate: %s", out)
		}
		return string(out), err
	}
	return dir, run
}

func TestReliabilityRestoreDurationIncludesFullDownload(t *testing.T) {
	fetch, verify := reliabilityRestoreScripts(t)
	for _, retryFetch := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-attempt", true: "fetch-retry"}[retryFetch], func(t *testing.T) {
			dir, run := reliabilityRestoreHarness(t)
			if out, err := run(fetch); err != nil {
				t.Fatalf("fetch: %v\n%s", err, out)
			}
			wantDuration := "100" // 60 download + 20 initialization + 20 SQL restore.
			if retryFetch {
				if out, err := run(fetch); err != nil {
					t.Fatalf("fetch retry: %v\n%s", err, out)
				}
				wantDuration = "160"
			}
			if out, err := run(verify); err != nil {
				t.Fatalf("verify: %v\n%s", err, out)
			} else if !strings.Contains(out, "not CMS RTO") {
				t.Fatalf("restore scope missing from log: %s", out)
			}
			metric, err := os.ReadFile(filepath.Join(dir, "metrics", "cms_backup_restore.prom"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"cms_backup_restore_duration_seconds " + wantDuration + "\n",
				"including S3 listing, download, DB initialization and validation; not end-to-end CMS RTO",
			} {
				if !strings.Contains(string(metric), want) {
					t.Errorf("metric missing %q:\n%s", want, metric)
				}
			}
		})
	}
}

func TestReliabilityRestoreRejectsInvalidStartAndFailedRestore(t *testing.T) {
	fetch, verify := reliabilityRestoreScripts(t)
	for _, mode := range []string{"missing-start", "invalid-start", "sql-failure", "missing-table"} {
		t.Run(mode, func(t *testing.T) {
			dir, run := reliabilityRestoreHarness(t)
			if out, err := run(fetch); err != nil {
				t.Fatalf("fetch: %v\n%s", err, out)
			}
			start := filepath.Join(dir, "staging", "restore.started")
			script := verify
			switch mode {
			case "missing-start":
				if err := os.Remove(start); err != nil {
					t.Fatal(err)
				}
			case "invalid-start":
				if err := os.WriteFile(start, []byte("not-a-timestamp\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "sql-failure":
				script = "export SQL_EXIT=1\n" + script
			case "missing-table":
				script = "export TABLE_COUNT=0\n" + script
			}
			if out, err := run(script); err == nil {
				t.Fatalf("verify succeeded despite %s:\n%s", mode, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "metrics", "cms_backup_restore.prom")); !os.IsNotExist(err) {
				t.Fatalf("failed restore must not publish success metrics: %v", err)
			}
		})
	}
}
