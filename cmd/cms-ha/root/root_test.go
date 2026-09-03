package root

import (
	"bytes"
	"testing"
)

func TestRootCmdRegistration(t *testing.T) {
	commands := []string{
		"backup",
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
}

func TestPhaseSubcommands(t *testing.T) {
	phaseSubcommands := []string{
		"init-vms",
		"setup-cobbler",
		"register-nodes",
		"repair-ssh",
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

	// Test alias
	cmd, _, err := rootCmd.Find([]string{"phase", "setup-nginx"})
	if err != nil || cmd == nil || cmd.Name() != "setup-nginx-wordpress" {
		t.Errorf("expected alias setup-nginx to resolve to setup-nginx-wordpress")
	}
}

func TestVMSubcommands(t *testing.T) {
	vmSubcommands := []string{
		"start",
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

	// Test aliases
	aliases := map[string]string{
		"start-all":       "start",
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

func TestRootHelp(t *testing.T) {
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"--help"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("expected --help to succeed, got: %v", err)
	}

	out := buf.String()
	if len(out) == 0 {
		t.Errorf("expected help output to be non-empty")
	}
}
