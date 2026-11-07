package root

import (
	"strings"
	"testing"
)

func TestDryRunRejectedForNonDeployCommands(t *testing.T) {
	t.Cleanup(func() {
		dryRun = false
		rootCmd.SetArgs(nil)
	})

	rootCmd.SetArgs([]string{"--dry-run", "status", "ssh"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected --dry-run on status ssh to fail")
	}
	if !strings.Contains(err.Error(), "--dry-run is only implemented by 'cms-ha deploy'") {
		t.Fatalf("unexpected error: %v", err)
	}
}
