package catalog

import (
	"strings"
	"testing"
)

func TestAllSpecsAreCompleteAndUnique(t *testing.T) {
	names := map[string]bool{}
	commands := map[string]bool{}
	for _, spec := range All() {
		if spec.Name == "" || spec.Description == "" || spec.CLI == "" || spec.Short == "" {
			t.Errorf("incomplete spec: %+v", spec)
		}
		if names[spec.Name] {
			t.Errorf("duplicate phase name %q", spec.Name)
		}
		names[spec.Name] = true
		if commands[spec.CLI] {
			t.Errorf("duplicate CLI command %q", spec.CLI)
		}
		commands[spec.CLI] = true
		for _, alias := range spec.Aliases {
			if alias == "" {
				t.Errorf("%s has an empty alias", spec.Name)
			}
			if commands[alias] {
				t.Errorf("alias %q of %s collides with another command", alias, spec.Name)
			}
			commands[alias] = true
		}
	}
}

func TestAllKeepsDeployOrder(t *testing.T) {
	var got []string
	for _, spec := range All() {
		got = append(got, spec.CLI)
	}
	want := strings.Join([]string{
		"init-vms", "setup-cobbler", "register-nodes", "repair-ssh",
		"bootstrap-router", "setup-puppet", "setup-drbd", "setup-backup",
		"setup-kubernetes", "setup-nginx-wordpress", "setup-monitoring",
		"setup-ufw", "setup-ca",
	}, ",")
	if strings.Join(got, ",") != want {
		t.Fatalf("deploy order = %v, want %s", got, want)
	}
}
