package tests

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func TestFencingSecurityForcedHelper(t *testing.T) {
	content, err := cms.TemplatesFS.ReadFile("templates/drbd/fence-virsh.sh")
	if err != nil {
		t.Fatal(err)
	}
	// CI lints source templates before rendering; both forms must be shell.
	for _, shell := range []string{"/bin/sh", "bash"} {
		cmd := exec.Command(shell, "-n")
		cmd.Stdin = bytes.NewReader(content)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("raw fencing template is not valid %s: %v %s", shell, err, out)
		}
	}
	tmpl, err := template.New("helper").Parse(string(content))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, []string{"master1", "custom-master.2"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Substitute sudo only in the test fixture: never execute real virsh/sudo.
	sudo := filepath.Join(dir, "sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\nprintf 'locale=%s libvirt=%s\\n' \"$LC_ALL\" \"${LIBVIRT_DEFAULT_URI-unset}\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "helper")
	script := strings.ReplaceAll(buf.String(), "/usr/bin/sudo", sudo)
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, request := range []string{"virsh destroy master1", "virsh domstate master1", "virsh destroy custom-master.2", "virsh domstate custom-master.2"} {
		t.Run(request, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", helper)
			cmd.Env = []string{"SSH_ORIGINAL_COMMAND=" + request, "LIBVIRT_DEFAULT_URI=qemu:///evil", "LC_ALL=C"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			fields := strings.Fields(request)
			want := "-n\n/usr/bin/virsh\n" + fields[1] + "\n" + fields[2] + "\nlocale=C libvirt=unset\n"
			if string(out) != want {
				t.Fatalf("got %q, want %q", out, want)
			}
		})
	}
	for _, request := range []string{"", "sh", "bash -i", "sudo virsh destroy master1", "virsh list", "virsh destroy worker1", "virsh destroy master10", "virsh destroy --all", "virsh destroy master1 --graceful", "virsh destroy master1; id", "virsh domstate master1\nid", "virsh destroy $(id)", "virsh  destroy master1", " virsh destroy master1", "virsh domstate master1 ", "virsh destroy 'master1'", "internal-sftp"} {
		t.Run("deny_"+request, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", helper)
			cmd.Env = []string{"SSH_ORIGINAL_COMMAND=" + request}
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "Denied:") {
				t.Fatalf("request accepted or not denied: %v %s", err, out)
			}
		})
	}
}

func TestFencingSecurityPinnedSSHCommands(t *testing.T) {
	cfg := &config.Config{Network: config.NetworkConfig{Internal: config.NetworkDetail{CIDR: "192.168.10.0/24"}}}
	off, state, err := cfg.FenceCommandsChecked()
	if err != nil {
		t.Fatalf("FenceCommandsChecked() error = %v", err)
	}
	for _, command := range []string{off, state} {
		for _, want := range []string{"StrictHostKeyChecking=yes", "UserKnownHostsFile=/etc/drbd-fence/known_hosts", "GlobalKnownHostsFile=/etc/drbd-fence/known_hosts", "HostKeyAlias=192.168.10.254", "BatchMode=yes", "IdentitiesOnly=yes", "IdentityAgent=none", "ClearAllForwardings=yes", "UpdateHostKeys=no"} {
			if !strings.Contains(command, want) {
				t.Errorf("missing %s: %s", want, command)
			}
		}
		for _, bad := range []string{"accept-new", "/dev/null", "sudo ", "StrictHostKeyChecking=no"} {
			if strings.Contains(command, bad) {
				t.Errorf("insecure or unrestricted command: %s", command)
			}
		}
	}
	if !strings.HasSuffix(off, " virsh destroy {domain}") || !strings.HasSuffix(state, " virsh domstate {domain}") {
		t.Fatal("commands do not match forced helper protocol")
	}
}

func TestFencingSecurityStateMustBeSuccessfulAndExact(t *testing.T) {
	content, err := cms.TemplatesFS.ReadFile("templates/drbd/fence-peer.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, state string
		ok          bool
	}{
		{"off", "printf 'shut off\\n'", true},
		{"crashed", "printf 'crashed\\n'", true},
		{"failed query with off text", "printf 'shut off\\n'; exit 1", false},
		{"error containing off", "printf 'error: not shut off\\n'", false},
		{"running", "printf 'running\\n'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			conf := filepath.Join(dir, "fence.conf")
			// Double-quoted config values contain only these fixed test snippets.
			data := "FENCE_COMMAND='false'\nFENCE_STATE_COMMAND=\"" + tc.state + "\"\n"
			if err := os.WriteFile(conf, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			// Failed confirmation retries must be fast and confined to the fixture.
			if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(dir, "peer")
			if err := os.WriteFile(script, bytes.ReplaceAll(content, []byte("/etc/drbd-fence.conf"), []byte(conf)), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", script, "master1")
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("success=%v, want %v: %s", err == nil, tc.ok, out)
			}
		})
	}
}

func TestFencingSecurityPeerRejectsInjectedDomains(t *testing.T) {
	content, err := cms.TemplatesFS.ReadFile("templates/drbd/fence-peer.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Redirect the config in this fixture so it cannot source a real system file.
	conf := filepath.Join(dir, "fence.conf")
	marker := filepath.Join(dir, "executed")
	if err := os.WriteFile(conf, []byte("FENCE_COMMAND='touch "+marker+"'\nFENCE_STATE_COMMAND='touch "+marker+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "peer")
	if err := os.WriteFile(script, bytes.ReplaceAll(content, []byte("/etc/drbd-fence.conf"), []byte(conf)), 0700); err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"master1;id", "$(id)", "-master1", "master1 extra", "master1\nid"} {
		if out, err := exec.Command("bash", script, domain).CombinedOutput(); err == nil {
			t.Fatalf("accepted %q: %s", domain, out)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("invalid domain executed a fencing command")
	}
}
