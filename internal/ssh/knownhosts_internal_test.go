package ssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveHostsFromKnownHosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")

	content := strings.Join([]string{
		"# a comment",
		"192.168.10.11 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIaaa",
		"192.168.10.12,192.168.20.101 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIbbb",
		"10.0.0.5 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIccc",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Remove two of the three hosts (one via a multi-host token).
	if err := RemoveHostsFromKnownHosts(path, []string{"192.168.10.11", "192.168.20.101"}); err != nil {
		t.Fatalf("removeHostsFromKnownHosts: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	out := string(got)

	if strings.Contains(out, "192.168.10.11 ssh") {
		t.Error("expected the 192.168.10.11 entry to be removed")
	}
	if strings.Contains(out, "192.168.20.101") || strings.Contains(out, "192.168.10.12") {
		t.Error("expected the multi-host entry to be removed entirely")
	}
	if !strings.Contains(out, "10.0.0.5 ssh-ed25519") {
		t.Error("expected the unrelated host to be preserved")
	}
	if !strings.Contains(out, "# a comment") {
		t.Error("expected comments to be preserved")
	}
}

func TestRemoveHostsFromKnownHostsMissingFile(t *testing.T) {
	if err := RemoveHostsFromKnownHosts(filepath.Join(t.TempDir(), "does-not-exist"), []string{"192.168.10.11"}); err != nil {
		t.Fatalf("missing file should be a no-op, got: %v", err)
	}
}
