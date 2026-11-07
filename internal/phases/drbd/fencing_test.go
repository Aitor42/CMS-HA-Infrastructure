package drbd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func fenceTestKey(t *testing.T) ([]byte, cryptossh.PublicKey) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := cryptossh.MarshalPrivateKey(key, "test")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := cryptossh.NewPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block), pub
}

func fenceTestWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func fenceFixture(t *testing.T) (*Phase, string, string, []byte, cryptossh.PublicKey) {
	t.Helper()
	root := t.TempDir()
	home, hostDir := filepath.Join(root, "home"), filepath.Join(root, "sshd")
	for _, path := range []string{home, filepath.Join(home, ".ssh"), hostDir} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	private, public := fenceTestKey(t)
	fenceTestWrite(t, filepath.Join(home, ".ssh/id_ed25519_fence"), private, 0600)
	_, hostKey := fenceTestKey(t)
	fenceTestWrite(t, filepath.Join(hostDir, "ssh_host_ed25519_key.pub"), cryptossh.MarshalAuthorizedKey(hostKey), 0644)
	cfg := &config.Config{
		DRBD:    config.DRBDConfig{Fencing: true},
		Network: config.NetworkConfig{Internal: config.NetworkDetail{CIDR: "192.168.10.0/24"}},
		Nodes:   config.NodesConfig{Masters: []config.NodeDetail{{Name: "master1", IP: "192.168.10.11"}, {Name: "master2", IP: "192.168.10.12"}}},
	}
	return &Phase{cfg: cfg, pool: &testutil.FakeExecutor{}}, home, hostDir, private, public
}

func TestFenceAuthorizedKeysMigratesIdentityNotComment(t *testing.T) {
	_, key := fenceTestKey(t)
	_, other := fenceTestKey(t)
	pub := strings.TrimSpace(string(cryptossh.MarshalAuthorizedKey(key)))
	otherLine := "command=\"echo keep me\" " + strings.TrimSpace(string(cryptossh.MarshalAuthorizedKey(other))) + " cms-ha-fence\n"
	existing := "# operator keys\n" + otherLine + pub + " old-comment\n" +
		"no-pty,command=\"echo old command\" " + pub + " changed-comment\n" +
		"restrict " + pub + " duplicate\n# keep final comment"
	helper := "/home/operator space/it's/.ssh/cms-ha-fence/command"
	got := fenceAuthorizedKeys([]byte(existing), key, helper)
	if !bytes.Contains(got, []byte(otherLine)) || !bytes.Contains(got, []byte("# keep final comment\n")) {
		t.Fatalf("lost unrelated entries: %s", got)
	}
	if strings.Count(string(got), pub) != 1 {
		t.Fatalf("old identities survive: %s", got)
	}
	parsed, _, options, _, err := cryptossh.ParseAuthorizedKey(bytes.SplitAfter(got, []byte("\n"))[3])
	if err != nil || !bytes.Equal(parsed.Marshal(), key.Marshal()) {
		t.Fatalf("invalid forced entry: %v: %s", err, got)
	}
	joined := strings.Join(options, ",")
	for _, want := range []string{"restrict", "no-agent-forwarding", "no-port-forwarding", "no-X11-forwarding", "no-pty", "no-user-rc", "command=\"exec "} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q: %s", want, joined)
		}
	}
	if twice := fenceAuthorizedKeys(got, key, helper); !bytes.Equal(got, twice) {
		t.Fatal("migration is not idempotent")
	}
}

func TestFenceForcedCommandQuotesControlledPath(t *testing.T) {
	_, key := fenceTestKey(t)
	dir := filepath.Join(t.TempDir(), "operator 'quoted\" space")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "command")
	fenceTestWrite(t, helper, []byte("#!/bin/sh\nprintf 'restricted-helper\\n'\n"), 0700)
	_, _, options, _, err := cryptossh.ParseAuthorizedKey(fenceAuthorizedKeys(nil, key, helper))
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range options {
		if value, ok := strings.CutPrefix(option, "command="); ok {
			command, err := strconv.Unquote(value)
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("/bin/sh", "-c", command).CombinedOutput()
			if err != nil || string(out) != "restricted-helper\n" {
				t.Fatalf("forced command quoting: %v %s", err, out)
			}
			return
		}
	}
	t.Fatal("missing forced command")
}

func TestFenceProvisionPersistsPinAndDeploysIt(t *testing.T) {
	p, home, hostDir, private, public := fenceFixture(t)
	_, other := fenceTestKey(t)
	auth := filepath.Join(home, ".ssh/authorized_keys")
	otherLine := string(cryptossh.MarshalAuthorizedKey(other))
	fenceTestWrite(t, auth, []byte(string(cryptossh.MarshalAuthorizedKey(public))+otherLine), 0644)
	// A mismatched .pub must not change which identity is migrated.
	fenceTestWrite(t, filepath.Join(home, ".ssh/id_ed25519_fence.pub"), cryptossh.MarshalAuthorizedKey(other), 0644)
	key, pins, err := p.provisionFenceKey(t.Context(), home, hostDir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, private) || !strings.HasPrefix(string(pins), "192.168.10.254 ssh-ed25519 ") {
		t.Fatal("wrong key or pin")
	}
	before, err := os.ReadFile(auth)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(before, []byte(otherLine)) || !bytes.Contains(before, []byte("restrict,")) {
		t.Fatal("bad key migration")
	}
	for path, want := range map[string]os.FileMode{
		auth: 0600, filepath.Join(home, ".ssh/cms-ha-fence/command"): 0700, filepath.Join(home, ".ssh/cms-ha-fence/known_hosts"): 0600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("permissions on %s: %v %v", path, info, err)
		}
	}
	checkHost, err := knownhosts.New(filepath.Join(home, ".ssh/cms-ha-fence/known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	hostData, err := os.ReadFile(filepath.Join(hostDir, "ssh_host_ed25519_key.pub"))
	if err != nil {
		t.Fatal(err)
	}
	hostPublic, _, _, _, err := cryptossh.ParseAuthorizedKey(hostData)
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("192.168.10.254"), Port: 22}
	if err := checkHost("192.168.10.254:22", addr, hostPublic); err != nil {
		t.Fatalf("pin rejects real local host key: %v", err)
	}
	if err := checkHost("192.168.10.254:22", addr, other); err == nil {
		t.Fatal("pin accepts a substituted host key")
	}
	_, again, err := p.provisionFenceKey(t.Context(), home, hostDir)
	if err != nil || !bytes.Equal(pins, again) {
		t.Fatalf("repeat provisioning: %v", err)
	}
	script, tmpl, err := p.loadFenceTemplates()
	if err != nil {
		t.Fatal(err)
	}
	off, state, err := p.cfg.FenceCommandsChecked()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.deployFenceArtifacts(t.Context(), p.cfg.Nodes.Masters, script, tmpl, key, pins, "1", off, state); err != nil {
		t.Fatal(err)
	}
	fake := p.pool.(*testutil.FakeExecutor)
	for _, node := range p.cfg.Nodes.Masters {
		if !bytes.Equal(fake.Files[node.IP]["/etc/drbd-fence/known_hosts"], pins) {
			t.Fatal("pin not distributed")
		}
		if !bytes.Equal(fake.Files[node.IP]["/etc/drbd-fence/fence_key"], key) {
			t.Fatal("key not distributed")
		}
	}
	_, newHost := fenceTestKey(t)
	fenceTestWrite(t, filepath.Join(hostDir, "ssh_host_ed25519_key.pub"), cryptossh.MarshalAuthorizedKey(newHost), 0644)
	if _, _, err := p.provisionFenceKey(t.Context(), home, hostDir); err == nil || !strings.Contains(err.Error(), "differ from persistent") {
		t.Fatalf("rotation accepted: %v", err)
	}
	after, _ := os.ReadFile(auth)
	if !bytes.Equal(before, after) {
		t.Fatal("pin mismatch changed authorized_keys")
	}
	persisted, _ := os.ReadFile(filepath.Join(home, ".ssh/cms-ha-fence/known_hosts"))
	if !bytes.Equal(persisted, pins) {
		t.Fatal("pin silently replaced")
	}
}

func TestFenceProvisionExcludesQuorumOnlyMaster(t *testing.T) {
	p, home, hostDir, _, _ := fenceFixture(t)
	p.cfg.Nodes.Masters = append(p.cfg.Nodes.Masters, config.NodeDetail{Name: "quorum-only-master"})
	if _, _, err := p.provisionFenceKey(t.Context(), home, hostDir); err != nil {
		t.Fatal(err)
	}
	helper, err := os.ReadFile(filepath.Join(home, ".ssh/cms-ha-fence/command"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(helper), "quorum-only-master") {
		t.Fatal("fencing key must not control the third etcd-only master")
	}
}

func TestFenceProvisionRejectsUnsafePaths(t *testing.T) {
	for _, target := range []string{".ssh", ".ssh/authorized_keys", ".ssh/id_ed25519_fence", ".ssh/cms-ha-fence", ".ssh/cms-ha-fence/command", ".ssh/cms-ha-fence/known_hosts"} {
		t.Run(target, func(t *testing.T) {
			p, home, hostDir, _, _ := fenceFixture(t)
			path := filepath.Join(home, target)
			if strings.HasPrefix(target, ".ssh/cms-ha-fence/") {
				if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(t.TempDir(), "victim")
			fenceTestWrite(t, victim, []byte("unchanged"), 0600)
			if err := os.Symlink(victim, path); err != nil {
				t.Fatal(err)
			}
			if _, _, err := p.provisionFenceKey(t.Context(), home, hostDir); err == nil {
				t.Fatal("accepted symlink")
			}
			got, _ := os.ReadFile(victim)
			if string(got) != "unchanged" {
				t.Fatal("modified symlink target")
			}
		})
	}
	t.Run("writable home", func(t *testing.T) {
		p, home, hostDir, _, _ := fenceFixture(t)
		if err := os.Chmod(home, 0777); err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.provisionFenceKey(t.Context(), home, hostDir); err == nil {
			t.Fatal("accepted writable home")
		}
	})
}

func TestFenceProvisionGeneratesDedicatedKeyInTemp(t *testing.T) {
	p, home, hostDir, _, _ := fenceFixture(t)
	keyPath := filepath.Join(home, ".ssh/id_ed25519_fence")
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	key, pins, err := p.provisionFenceKey(t.Context(), home, hostDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cryptossh.ParsePrivateKey(key); err != nil || len(pins) == 0 {
		t.Fatalf("invalid generated key or pins: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private key permissions: %v %v", info, err)
	}
}

func TestFenceDeployRefusesKeyWithoutPin(t *testing.T) {
	p, _, _, key, _ := fenceFixture(t)
	script, tmpl, err := p.loadFenceTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.deployFenceArtifacts(t.Context(), p.cfg.Nodes.Masters, script, tmpl, key, nil, "1", "off", "state"); err == nil {
		t.Fatal("deployed key without pin")
	}
	fake := p.pool.(*testutil.FakeExecutor)
	for _, node := range p.cfg.Nodes.Masters {
		if len(fake.Files[node.IP]["/etc/drbd-fence/fence_key"]) != 0 {
			t.Fatal("copied unpinned key")
		}
	}
}

func TestFenceHostKeysFailClosed(t *testing.T) {
	dir := t.TempDir()
	if _, err := fenceHostKeys("192.168.10.254", dir); err == nil {
		t.Fatal("accepted missing host keys")
	}
	fenceTestWrite(t, filepath.Join(dir, "ssh_host_ed25519_key.pub"), []byte("invalid"), 0644)
	if _, err := fenceHostKeys("192.168.10.254", dir); err == nil {
		t.Fatal("accepted malformed host key")
	}
}

func TestFenceRejectsPartialCustomCommandsBeforeLocalProvision(t *testing.T) {
	p, _, _, _, _ := fenceFixture(t)
	p.cfg.DRBD.FenceCommand = "custom-off {domain}"
	if err := p.setupFencing(t.Context(), p.cfg.Nodes.Masters); err == nil {
		t.Fatal("accepted partial custom commands")
	}
}
