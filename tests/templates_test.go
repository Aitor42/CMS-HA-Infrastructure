package tests

import (
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
