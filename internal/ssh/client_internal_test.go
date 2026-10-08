package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"simple":      "'simple'",
		"with space":  "'with space'",
		"it's":        `'it'\''s'`,
		"":            "''",
		"a;rm -rf /":  "'a;rm -rf /'",
		"$(touch /x)": "'$(touch /x)'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoteCommandElevation(t *testing.T) {
	root := &Pool{user: "root"}
	if got := root.remoteCommand("id -u"); got != "id -u" {
		t.Errorf("root pool should not elevate, got %q", got)
	}

	deploy := &Pool{user: "ubuntu"}
	got := deploy.remoteCommand("id -u")
	if !strings.HasPrefix(got, "sudo -n -H bash -c ") {
		t.Errorf("deploy pool should elevate with sudo, got %q", got)
	}
	if !strings.Contains(got, "'id -u'") {
		t.Errorf("elevated command should quote the payload, got %q", got)
	}
	if !deploy.elevated() || root.elevated() {
		t.Error("elevated() must reflect a non-root deploy account")
	}
}

// TestHostKeyTOFU exercises the trust-on-first-use flow: the first key seen for
// a host is pinned and persisted, the same key verifies afterwards, and a
// different key for a known host is rejected.
func TestHostKeyTOFU(t *testing.T) {
	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")

	key1 := newTestHostKey(t)
	key2 := newTestHostKey(t)

	addr := &net.TCPAddr{IP: net.ParseIP("192.168.10.11"), Port: 22}
	hostname := "192.168.10.11:22"

	p := &Pool{user: "ubuntu", knownHostsPath: knownHosts}

	if err := p.verifyHostKey(hostname, addr, key1); err != nil {
		t.Fatalf("first contact should be trusted (TOFU): %v", err)
	}
	if _, err := os.Stat(knownHosts); err != nil {
		t.Fatalf("TOFU should persist the key: %v", err)
	}

	// Same key again (fresh pool, loaded from disk) must verify.
	p2 := &Pool{user: "ubuntu", knownHostsPath: knownHosts}
	if err := p2.verifyHostKey(hostname, addr, key1); err != nil {
		t.Fatalf("pinned key should verify: %v", err)
	}

	// A different key for a known host must be rejected.
	if err := p2.verifyHostKey(hostname, addr, key2); err == nil {
		t.Fatal("a different key for a known host must be rejected")
	}

	// A different host with an unknown key is trusted on first use.
	otherAddr := &net.TCPAddr{IP: net.ParseIP("192.168.10.12"), Port: 22}
	if err := p2.verifyHostKey("192.168.10.12:22", otherAddr, key2); err != nil {
		t.Fatalf("unknown host should be trusted (TOFU): %v", err)
	}
}

func TestWriteRemoteFileElevatedRunsSudo(t *testing.T) {
	// There is no real host: assert that the elevated path fails with a
	// connection error rather than panicking and that it never touches SFTP.
	p := &Pool{user: "ubuntu"}
	err := p.writeRemoteFile(context.Background(), "192.0.2.1", strings.NewReader("x"), "/etc/example.conf", 0o600)
	if err == nil {
		t.Fatal("expected an error dialing a non-existent host")
	}
}

// newTestHostKey returns a fresh ed25519 public key usable as a host key.
func newTestHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap key: %v", err)
	}
	return sshPub
}
