package tests

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/templates"
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
