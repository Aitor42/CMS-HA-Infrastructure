package lint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

// Options controls which linters run.
type Options struct {
	All       bool
	Go        bool
	Shell     bool
	YAML      bool
	Puppet    bool
	K8s       bool
	Terraform bool
}

type linterDef struct {
	name    string
	cmd     string
	args    []string
	enabled bool
}

// RunLints executes all enabled linters in parallel.
func RunLints(ctx context.Context, opts Options) error {
	// Resolve paths relative to the module root
	root, err := findModuleRoot()
	if err != nil {
		root = "."
	}

	shellScripts := findFiles(root, "*.sh")
	puppetFiles := findFiles(root, "*.pp")
	k8sDir := filepath.Join(root, "kubernetes")
	monDir := filepath.Join(root, "templates", "monitoring")
	tfDir := filepath.Join(root, "terraform")

	linters := []linterDef{
		{
			name:    "go-vet",
			cmd:     "go",
			args:    []string{"vet", "./..."},
			enabled: opts.All || opts.Go,
		},
		{
			name:    "staticcheck",
			cmd:     "staticcheck",
			args:    []string{"./..."},
			enabled: opts.All || opts.Go,
		},
		{
			name:    "shellcheck",
			cmd:     "shellcheck",
			args:    append([]string{"-S", "warning"}, shellScripts...),
			enabled: (opts.All || opts.Shell) && len(shellScripts) > 0,
		},
		{
			name:    "yamllint",
			cmd:     "yamllint",
			args:    []string{"-c", filepath.Join(root, ".yamllint"), k8sDir, monDir},
			enabled: opts.All || opts.YAML,
		},
		{
			name:    "puppet-lint",
			cmd:     "puppet-lint",
			args:    append([]string{"--no-class_inherits_from_params_class-check", "--no-documentation-check", "--no-autoloader_layout-check"}, puppetFiles...),
			enabled: (opts.All || opts.Puppet) && len(puppetFiles) > 0,
		},
		{
			name:    "kubeconform",
			cmd:     "kubeconform",
			args:    []string{"-strict", "-summary", "-kubernetes-version", "1.29.0", k8sDir},
			enabled: opts.All || opts.K8s,
		},
		{
			name:    "terraform-fmt",
			cmd:     "terraform",
			args:    []string{"-chdir=" + tfDir, "fmt", "-check", "-recursive"},
			enabled: opts.Terraform,
		},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string

	for _, l := range linters {
		if !l.enabled {
			continue
		}
		wg.Add(1)
		go func(lint linterDef) {
			defer wg.Done()

			if _, err := exec.LookPath(lint.cmd); err != nil {
				logging.Info("%s not installed — skipping", lint.name)
				return
			}

			cmd := exec.CommandContext(ctx, lint.cmd, lint.args...)
			cmd.Dir = root
			out, err := cmd.CombinedOutput()

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, fmt.Sprintf("[%s] %s", lint.name, strings.TrimSpace(string(out))))
				logging.Error("%s: FAILED", lint.name)
			} else {
				logging.Success("%s: passed", lint.name)
			}
		}(l)
	}

	wg.Wait()

	if len(failures) > 0 {
		return fmt.Errorf("lint failures:\n%s", strings.Join(failures, "\n"))
	}
	return nil
}

// findModuleRoot finds the Go module root by walking up from cwd.
func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

// findFiles recursively finds all files matching the pattern.
func findFiles(root, pattern string) []string {
	var files []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		// Skip .git directories
		if strings.Contains(path, string(os.PathSeparator)+".git"+string(os.PathSeparator)) {
			return nil
		}
		matched, _ := filepath.Match(pattern, filepath.Base(path))
		if matched {
			files = append(files, path)
		}
		return nil
	})
	return files
}
