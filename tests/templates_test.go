package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/templates"
	"gopkg.in/yaml.v3"
)

func TestTemplates_Renderer(t *testing.T) {
	mockFS := fstest.MapFS{
		"templates/test.txt": &fstest.MapFile{
			Data: []byte("Hello {{ .Name }}! Value: {{ default \"none\" .Value }}"),
		},
	}

	r := templates.NewRenderer(mockFS)
	data := map[string]string{
		"Name": "CMS",
	}

	rendered, err := r.Render("templates/test.txt", data)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	expected := "Hello CMS! Value: none"
	if string(rendered) != expected {
		t.Errorf("expected %q, got %q", expected, string(rendered))
	}
}

func TestTemplates_RenderToFile(t *testing.T) {
	mockFS := fstest.MapFS{
		"templates/out.conf": &fstest.MapFile{
			Data: []byte("server_name {{ .Domain }};"),
		},
	}

	r := templates.NewRenderer(mockFS)
	tmpDir := t.TempDir()
	nestedFile := filepath.Join(tmpDir, "nested", "subdir", "out.conf")

	err := r.RenderToFile("templates/out.conf", map[string]string{"Domain": "example.com"}, nestedFile, 0640)
	if err != nil {
		t.Fatalf("RenderToFile failed: %v", err)
	}

	content, err := os.ReadFile(nestedFile)
	if err != nil {
		t.Fatalf("failed to read rendered file: %v", err)
	}

	if string(content) != "server_name example.com;" {
		t.Errorf("unexpected content: %s", string(content))
	}
}

func TestTemplates_EmbeddedNetworkXML(t *testing.T) {
	nets := []struct {
		file   string
		bridge string
		domain string
	}{
		{"templates/libvirt/internal-net.xml", "virbr-int", "internal.local"},
		{"templates/libvirt/main-net.xml", "virbr-main", "main.local"},
	}

	for _, n := range nets {
		t.Run(n.file, func(t *testing.T) {
			data, err := cms.TemplatesFS.ReadFile(n.file)
			if err != nil {
				t.Fatalf("failed to read embedded template %s: %v", n.file, err)
			}
			content := string(data)
			if !strings.Contains(content, "stp='on'") {
				t.Errorf("template %s must enable STP (stp='on') for loop prevention", n.file)
			}
			if !strings.Contains(content, fmt.Sprintf("bridge name='%s'", n.bridge)) {
				t.Errorf("template %s missing expected bridge %s", n.file, n.bridge)
			}
			if !strings.Contains(content, fmt.Sprintf("domain name='%s'", n.domain)) {
				t.Errorf("template %s missing expected domain %s", n.file, n.domain)
			}
		})
	}
}

func TestTemplates_EmbeddedCobblerAutoinstall(t *testing.T) {
	data, err := cms.TemplatesFS.ReadFile("templates/cobbler/ubuntu-24.04-autoinstall.yaml")
	if err != nil {
		t.Fatalf("failed to read embedded autoinstall template: %v", err)
	}

	content := string(data)
	if !strings.HasPrefix(content, "#cloud-config") {
		t.Errorf("autoinstall template must start with #cloud-config header")
	}
	expectedSections := []string{
		"autoinstall:",
		"identity:",
		"keyboard:",
		"late-commands:",
		"cblr/pub/authorized_keys",
		"nopxe",
	}
	for _, sec := range expectedSections {
		if !strings.Contains(content, sec) {
			t.Errorf("autoinstall template missing required directive: %s", sec)
		}
	}
}

func TestTemplates_EmbeddedMonitoringConfigs(t *testing.T) {
	t.Run("prometheus.yml", func(t *testing.T) {
		data, err := cms.TemplatesFS.ReadFile("templates/monitoring/prometheus.yml")
		if err != nil {
			t.Fatalf("failed to read prometheus.yml: %v", err)
		}
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("prometheus.yml is not valid YAML: %v", err)
		}
		if _, ok := parsed["scrape_configs"]; !ok {
			t.Error("prometheus.yml missing scrape_configs")
		}
	})

	t.Run("alert-rules.yml", func(t *testing.T) {
		data, err := cms.TemplatesFS.ReadFile("templates/monitoring/alert-rules.yml")
		if err != nil {
			t.Fatalf("failed to read alert-rules.yml: %v", err)
		}
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("alert-rules.yml is not valid YAML: %v", err)
		}
		if _, ok := parsed["groups"]; !ok {
			t.Error("alert-rules.yml missing groups")
		}
	})
}

func TestTemplates_EmbeddedNginxConfig(t *testing.T) {
	data, err := cms.TemplatesFS.ReadFile("templates/nginx/cms-lb.conf")
	if err != nil {
		t.Fatalf("failed to read cms-lb.conf: %v", err)
	}
	content := string(data)
	expectedDirectives := []string{
		"upstream cms_backend",
		"proxy_pass",
		"listen 80",
		"listen 443 ssl",
	}
	for _, dir := range expectedDirectives {
		if !strings.Contains(content, dir) {
			t.Errorf("cms-lb.conf missing expected directive: %s", dir)
		}
	}
}
