package initvms

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"gopkg.in/yaml.v3"
)

func TestJumpstartImageMode(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		config  string
		want    string
		wantErr bool
	}{
		{name: "default is iso", want: "iso"},
		{name: "config cloud", config: "cloud", want: "cloud"},
		{name: "flag wins", flag: "iso", config: "cloud", want: "iso"},
		{name: "normalises case and spaces", flag: " Cloud ", want: "cloud"},
		{name: "rejects unknown values", flag: "pxe", config: "iso", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Phase{
				cfg:  &config.Config{VM: config.VMConfig{JumpstartImage: tc.config}},
				opts: Options{JumpstartImage: tc.flag},
			}
			got, err := p.jumpstartImageMode()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("jumpstartImageMode() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("jumpstartImageMode(): %v", err)
			}
			if got != tc.want {
				t.Errorf("jumpstartImageMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

// sparseImage creates a file of the given size without allocating it, so the
// >100 MB cloud-image check can be exercised cheaply.
func sparseImage(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatalf("truncate %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func TestFindCloudImage(t *testing.T) {
	t.Run("missing storage image explains how to get it", func(t *testing.T) {
		_, err := findCloudImage(t.TempDir(), "")
		if err == nil {
			t.Fatal("expected an error when no cloud image exists")
		}
		if !strings.Contains(err.Error(), "cloud-images.ubuntu.com") {
			t.Errorf("error %q should point at the download URL", err)
		}
	})

	t.Run("finds the storage candidate", func(t *testing.T) {
		dir := t.TempDir()
		want := filepath.Join(dir, "ubuntu-24.04-server-cloudimg-amd64.img")
		sparseImage(t, want, 101<<20)
		got, err := findCloudImage(dir, "")
		if err != nil {
			t.Fatalf("findCloudImage: %v", err)
		}
		if got != want {
			t.Errorf("findCloudImage() = %q, want %q", got, want)
		}
	})

	t.Run("configured path wins", func(t *testing.T) {
		dir := t.TempDir()
		configured := filepath.Join(t.TempDir(), "custom-cloud.img")
		sparseImage(t, configured, 101<<20)
		// A differently named file in storage_dir must not shadow the config.
		sparseImage(t, filepath.Join(dir, "ubuntu-24.04-server-cloudimg-amd64.img"), 101<<20)
		got, err := findCloudImage(dir, configured)
		if err != nil {
			t.Fatalf("findCloudImage: %v", err)
		}
		if got != configured {
			t.Errorf("findCloudImage() = %q, want %q", got, configured)
		}
	})

	t.Run("configured path must exist", func(t *testing.T) {
		_, err := findCloudImage(t.TempDir(), filepath.Join(t.TempDir(), "nope.img"))
		if err == nil {
			t.Fatal("expected an error for a missing configured image")
		}
	})
}

func TestRenderCloudUserData(t *testing.T) {
	data := autoinstallData{
		Hostname:            "jumpstart",
		PasswordHash:        "$6$rounds=4096$salt$hash",
		PubKey:              "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test@host",
		MACInternal:         "52:54:00:10:00:01",
		IPInternal:          "192.168.10.10",
		GatewayInternal:     "192.168.10.1",
		MACMain:             "52:54:00:10:02:0a",
		IPMain:              "192.168.20.10",
		WANIfaceConfigCloud: "\n    net2:\n      match:\n        macaddress: \"52:54:00:10:00:09\"\n      dhcp4: true",
	}

	out, err := renderTemplate("templates/autoinstall/cloud-user-data.tmpl", data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered cloud user-data is not valid YAML: %v\n%s", err, out)
	}

	for _, want := range []string{
		"#cloud-config",
		"hostname: jumpstart",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test@host",
		"macaddress: \"52:54:00:10:00:01\"",
		"macaddress: \"52:54:00:10:02:0a\"",
		"net2:",
		"192.168.20.10/24",
		"ALL=(ALL) NOPASSWD:ALL",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendered user-data is missing %q", want)
		}
	}
	if strings.Contains(string(out), "autoinstall:") {
		t.Error("cloud image user-data must not contain the Subiquity autoinstall block")
	}
	if doc["hostname"] != "jumpstart" {
		t.Errorf("parsed hostname = %v, want jumpstart", doc["hostname"])
	}
}
