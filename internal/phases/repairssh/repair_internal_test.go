package repairssh

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func TestBootstrapHostsUsesTheConfiguredJumpstartIdentity(t *testing.T) {
	nodes := []config.NodeSpec{
		{Name: "provisioner", IP: "10.10.0.10", FQDN: "provisioner.lab.internal"},
		{Name: "master1", IP: "10.10.0.11", FQDN: "master1.lab.internal"},
	}
	content := bootstrapHostsContent(nodes, "10.10.0.10", "provisioner")
	if !strings.Contains(content, "10.10.0.10 provisioner.lab.internal provisioner puppet\n") {
		t.Fatalf("renamed jumpstart is missing the puppet alias:\n%s", content)
	}
	if strings.Contains(content, "master1.lab.internal master1 puppet") {
		t.Fatalf("ordinary node was treated as jumpstart:\n%s", content)
	}

	// The IP identifies the jumpstart even if only its name changed upstream.
	content = bootstrapHostsContent(nodes, "10.10.0.10", "")
	if !strings.Contains(content, "10.10.0.10 provisioner.lab.internal provisioner puppet\n") {
		t.Fatalf("jumpstart IP was not recognised:\n%s", content)
	}
}

func TestAuthorizedKeyScriptUsesTheAccountsPrimaryGroup(t *testing.T) {
	script := authorizedKeyScript("deploy", "ssh-ed25519 AAAA test")
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("generated script is not valid shell: %v\n%s", err, out)
	}
	for _, want := range []string{
		"cut -d: -f4",
		`install -d -m 700 -o 'deploy' -g "$deploy_group"`,
		`chown 'deploy':"$deploy_group"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}
	for _, bad := range []string{"-g 'deploy'", "chown 'deploy':'deploy'"} {
		if strings.Contains(script, bad) {
			t.Errorf("script still assumes a same-named group %q:\n%s", bad, script)
		}
	}
}
