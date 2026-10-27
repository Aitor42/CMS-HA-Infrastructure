package tests

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/cmd/cms-ha/root"
)

func TestCLI_CommandRegistration(t *testing.T) {
	rootCmd := root.GetRootCmd()
	commands := []string{
		"backup",
		"cleanup",
		"config",
		"deploy",
		"lint",
		"phase",
		"repair",
		"secrets",
		"status",
		"test",
		"traffic",
		"verify",
		"vm",
	}

	for _, cmdName := range commands {
		cmd, _, err := rootCmd.Find([]string{cmdName})
		if err != nil || cmd == nil || cmd.Name() != cmdName {
			t.Errorf("expected command %q to be registered in rootCmd", cmdName)
		}
	}

	// Test alias destroy -> cleanup
	destroyCmd, _, err := rootCmd.Find([]string{"destroy"})
	if err != nil || destroyCmd == nil || destroyCmd.Name() != "cleanup" {
		t.Errorf("expected alias destroy to resolve to cleanup")
	}
}

func TestCLI_SecretsSubcommands(t *testing.T) {
	rootCmd := root.GetRootCmd()
	for _, sub := range []string{"encrypt", "decrypt", "generate-key", "rotate"} {
		cmd, _, err := rootCmd.Find([]string{"secrets", sub})
		if err != nil || cmd == nil || cmd.Name() != sub {
			t.Errorf("expected secrets subcommand %q to be registered", sub)
		}
	}
}

func TestCLI_VersionFlag(t *testing.T) {
	cmd := root.GetRootCmd()
	oldVersion := cmd.Version
	t.Cleanup(func() {
		cmd.Version = oldVersion
		cmd.SetArgs(nil)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	})

	root.SetBuildInfo("v9.9.9", "abc1234", "2026-10-27T00:00:00Z")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--version: %v", err)
	}
	got := out.String()
	for _, want := range []string{"v9.9.9", "abc1234", "2026-10-27T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("--version output %q does not contain %q", got, want)
		}
	}
}

func TestCLI_ConfigValidateJSON(t *testing.T) {
	cmd := root.GetRootCmd()
	t.Cleanup(func() {
		_ = cmd.PersistentFlags().Set("config", "config.yaml")
		cmd.SetArgs(nil)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	})

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"-c", "../config.ci.yaml", "config", "validate", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config validate: %v", err)
	}
	var got struct {
		Valid   bool   `json:"valid"`
		Config  string `json:"config"`
		Nodes   int    `json:"nodes"`
		Masters int    `json:"masters"`
		Workers int    `json:"workers"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON %q: %v", out.String(), err)
	}
	if !got.Valid || got.Nodes != 13 || got.Masters != 3 || got.Workers != 2 {
		t.Fatalf("unexpected validation result: %+v", got)
	}

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("vm: {}\nnodes: {}\n"), 0600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	out.Reset()
	cmd.SetArgs([]string{"-c", bad, "config", "validate", "--json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("config validate accepted an invalid configuration")
	}
	var invalid struct {
		Valid bool   `json:"valid"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &invalid); err != nil {
		t.Fatalf("decode invalid JSON %q: %v", out.String(), err)
	}
	if invalid.Valid || invalid.Error == "" {
		t.Fatalf("unexpected invalid result: %+v", invalid)
	}
}

func TestCLI_PhaseSubcommands(t *testing.T) {
	rootCmd := root.GetRootCmd()
	phaseSubcommands := []string{
		"init-vms",
		"setup-cobbler",
		"register-nodes",
		"repair-ssh",
		"bootstrap-router",
		"setup-puppet",
		"setup-drbd",
		"setup-kubernetes",
		"setup-nginx-wordpress",
		"setup-monitoring",
		"setup-ufw",
		"setup-ca",
	}

	for _, sub := range phaseSubcommands {
		cmd, _, err := rootCmd.Find([]string{"phase", sub})
		if err != nil || cmd == nil || cmd.Name() != sub {
			t.Errorf("expected phase subcommand %q to be registered", sub)
		}
	}

	// Test alias setup-nginx -> setup-nginx-wordpress
	cmd, _, err := rootCmd.Find([]string{"phase", "setup-nginx"})
	if err != nil || cmd == nil || cmd.Name() != "setup-nginx-wordpress" {
		t.Errorf("expected alias setup-nginx to resolve to setup-nginx-wordpress")
	}
}

func TestCLI_VMSubcommands(t *testing.T) {
	rootCmd := root.GetRootCmd()
	vmSubcommands := []string{
		"start",
		"stop",
		"shrink",
		"fix-boot-order",
		"install-batches",
		"recreate",
	}

	for _, sub := range vmSubcommands {
		cmd, _, err := rootCmd.Find([]string{"vm", sub})
		if err != nil || cmd == nil || cmd.Name() != sub {
			t.Errorf("expected vm subcommand %q to be registered", sub)
		}
	}

	aliases := map[string]string{
		"start-all":       "start",
		"stop-all":        "stop",
		"shrink-ram":      "shrink",
		"fix-boot":        "fix-boot-order",
		"recreate-failed": "recreate",
	}
	for alias, canonical := range aliases {
		cmd, _, err := rootCmd.Find([]string{"vm", alias})
		if err != nil || cmd == nil || cmd.Name() != canonical {
			t.Errorf("expected vm alias %q to resolve to %q", alias, canonical)
		}
	}
}

func TestCLI_HelpExecution(t *testing.T) {
	rootCmd := root.GetRootCmd()
	// GetRootCmd returns the process-wide command; restore it so a later test
	// does not inherit the buffer or the `--help` argument set here.
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
	})

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"--help"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("expected --help to succeed, got: %v", err)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("expected help output to be non-empty")
	}
	for _, command := range []string{"deploy", "verify", "traffic", "cleanup", "phase"} {
		if !strings.Contains(out, command) {
			t.Errorf("help output does not list the %q command", command)
		}
	}
}
