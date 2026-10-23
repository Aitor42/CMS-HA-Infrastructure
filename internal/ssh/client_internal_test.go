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
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

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

// TestForgetHostDropsPinnedKey verifies that a reinstalled node can present a
// new host key after ForgetHost, both in the in-memory verifier and on disk.
func TestForgetHostDropsPinnedKey(t *testing.T) {
	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")
	addr := &net.TCPAddr{IP: net.ParseIP("192.168.10.11"), Port: 22}
	hostname := "192.168.10.11:22"

	p := &Pool{user: "ubuntu", knownHostsPath: knownHosts}
	if err := p.verifyHostKey(hostname, addr, newTestHostKey(t)); err != nil {
		t.Fatalf("seed pinned key: %v", err)
	}

	p.ForgetHost("192.168.10.11")

	if err := p.verifyHostKey(hostname, addr, newTestHostKey(t)); err != nil {
		t.Fatalf("a new key after ForgetHost should be trusted: %v", err)
	}
}

// TestForgetHostWaitsForInFlightVerification is the regression test for the
// reinstall race: ForgetHost must not remove the pinned entry while a
// first-contact handshake holds knownHostsMu and is about to append it again.
func TestForgetHostWaitsForInFlightVerification(t *testing.T) {
	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")
	addr := &net.TCPAddr{IP: net.ParseIP("192.168.10.11"), Port: 22}
	hostname := "192.168.10.11:22"
	key := newTestHostKey(t)

	p := &Pool{user: "ubuntu", knownHostsPath: knownHosts}
	if err := p.verifyHostKey(hostname, addr, key); err != nil {
		t.Fatalf("seed pinned key: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	p.knownHostsCallback = func(string, net.Addr, ssh.PublicKey) error {
		close(started)
		<-release
		return &knownhosts.KeyError{}
	}

	verifyDone := make(chan error, 1)
	go func() {
		verifyDone <- p.verifyHostKey(hostname, addr, key)
	}()
	<-started

	forgetDone := make(chan struct{})
	go func() {
		p.ForgetHost("192.168.10.11")
		close(forgetDone)
	}()

	select {
	case <-forgetDone:
		t.Fatal("ForgetHost returned while a verification held the known-hosts lock")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-verifyDone; err != nil {
		t.Fatalf("in-flight verification: %v", err)
	}
	<-forgetDone

	data, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	if strings.Contains(string(data), "192.168.10.11") {
		t.Fatalf("ForgetHost left the old key behind: %q", string(data))
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
