package tests

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/cmd/cms-ha/root"
)

func TestCLI_CommandRegistration(t *testing.T) {
	rootCmd := root.GetRootCmd()
	commands := []string{
		"backup",
		"cleanup",
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
