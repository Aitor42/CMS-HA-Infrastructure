package templates

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"text/template"
)

// Renderer handles rendering text templates.
type Renderer struct {
	fs fs.FS
}

// NewRenderer creates a new Renderer with the given fs.FS.
func NewRenderer(f fs.FS) *Renderer {
	return &Renderer{fs: f}
}

func getFuncMap() template.FuncMap {
	return template.FuncMap{
		"env": os.Getenv,
		"default": func(def any, val any) any {
			if val == nil {
				return def
			}
			if s, ok := val.(string); ok && s == "" {
				return def
			}
			return val
		},
		"join": strings.Join,
		"quote": func(s string) string {
			return fmt.Sprintf("%q", s)
		},
	}
}

// Render reads a template from the filesystem and renders it with the given data.
func (r *Renderer) Render(templatePath string, data interface{}) ([]byte, error) {
	content, err := fs.ReadFile(r.fs, templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read template %s: %w", templatePath, err)
	}
	
	str, err := r.RenderString(string(content), data)
	if err != nil {
		return nil, err
	}
	return []byte(str), nil
}

// RenderString renders a template string with the given data.
func (r *Renderer) RenderString(tmpl string, data interface{}) (string, error) {
	t, err := template.New("tmpl").Funcs(getFuncMap()).Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}
	return buf.String(), nil
}

// RenderToFile renders a template from the filesystem and writes it to a file.
func (r *Renderer) RenderToFile(templatePath string, data interface{}, outPath string, perm os.FileMode) error {
	rendered, err := r.Render(templatePath, data)
	if err != nil {
		return err
	}

	if err := os.WriteFile(outPath, rendered, perm); err != nil {
		return fmt.Errorf("failed to write rendered template to %s: %w", outPath, err)
	}
	return nil
}
