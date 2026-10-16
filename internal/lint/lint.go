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
	All     bool
	Go      bool
	Shell   bool
	YAML    bool
	Puppet  bool
	K8s     bool
	Ansible bool
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
	root, err := FindModuleRoot()
	if err != nil {
		root = "."
	}

	codeFiles := FindFilesMulti(root, "*.sh", "*.pp")
	shellScripts := codeFiles["*.sh"]
	puppetFiles := codeFiles["*.pp"]
	k8sDir := filepath.Join(root, "kubernetes")
	monDir := filepath.Join(root, "templates", "monitoring")
	ansibleDir := filepath.Join(root, "ansible")

	yamlTargets := []string{}
	if dirExists(k8sDir) {
		yamlTargets = append(yamlTargets, k8sDir)
	}
	if dirExists(monDir) {
		yamlTargets = append(yamlTargets, monDir)
	}

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
			args:    append([]string{"-c", filepath.Join(root, ".yamllint")}, yamlTargets...),
			enabled: (opts.All || opts.YAML) && len(yamlTargets) > 0,
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
			args:    []string{"-strict", "-summary", "-skip", "Kustomization", "-ignore-filename-pattern", `\.patch\.yaml$`, "-kubernetes-version", "1.29.0", k8sDir},
			enabled: (opts.All || opts.K8s) && dirExists(k8sDir),
		},
		{
			name:    "ansible-lint",
			cmd:     "ansible-lint",
			args:    []string{ansibleDir},
			enabled: (opts.All || opts.Ansible) && dirExists(ansibleDir),
		},
		{
			name: "ansible-syntax-check",
			cmd:  "ansible-playbook",
			args: []string{
				"--syntax-check",
				"-i", filepath.Join(ansibleDir, "inventory", "hosts.yml"),
				filepath.Join(ansibleDir, "playbooks", "provision.yml"),
			},
			enabled: (opts.All || opts.Ansible) && dirExists(ansibleDir),
		},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	var ranCount, skippedCount int

	for _, l := range linters {
		if !l.enabled {
			continue
		}
		wg.Add(1)
		go func(lint linterDef) {
			defer wg.Done()

			if _, err := exec.LookPath(lint.cmd); err != nil {
				logging.Info("%s not installed — skipping", lint.name)
				mu.Lock()
				skippedCount++
				mu.Unlock()
				return
			}

			cmd := exec.CommandContext(ctx, lint.cmd, lint.args...) // #nosec G204 -- executes predefined static linters
			cmd.Dir = root
			out, err := cmd.CombinedOutput()

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if ctx.Err() != nil {
					// A cancelled context is not a linter failure.
					return
				}
				failures = append(failures, fmt.Sprintf("[%s] %s", lint.name, tail(out)))
				logging.Error("%s: FAILED", lint.name)
				return
			}
			ranCount++
			logging.Success("%s: passed", lint.name)
		}(l)
	}

	wg.Wait()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(failures) > 0 {
		return fmt.Errorf("lint failures:\n%s", strings.Join(failures, "\n"))
	}
	if ranCount == 0 {
		// If zero checks passed, the run either ran no tool or none was
		// installed; not reporting this leaks silent green runs.
		logging.Warn("no linters were executed (check that the tools are installed or directories exist)")
		if skippedCount > 0 {
			logging.Warn("skipped due to missing tools: %d of %d", skippedCount, len(linters))
		}
	}
	return nil
}

// tail returns the end of linter output, capped so huge failures do not
// flood logs or memory-held error strings.
func tail(out []byte) string {
	s := strings.TrimSpace(string(out))
	const maxLintOutput = 8 * 1024
	if len(s) > maxLintOutput {
		s = "… " + s[len(s)-maxLintOutput:]
	}
	return s
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// FindModuleRoot finds the Go module root by walking up from cwd.
func FindModuleRoot() (string, error) {
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

// skipLintDir reports directories that must never be scanned.
func skipLintDir(name string) bool {
	switch name {
	case ".git", "vendor", "node_modules", "testdata", ".terraform", ".vagrant":
		return true
	}
	return false
}

// FindFiles recursively finds all files matching the pattern.
func FindFiles(root, pattern string) []string {
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Report but continue: unreadable directories should not hide
			// the files that could be checked; they will linter-fail alone.
			logging.Warn("skipping unreadable path during lint scan: %s (%v)", path, err)
			return nil
		}
		if d.IsDir() {
			if skipLintDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		matched, err := filepath.Match(pattern, d.Name())
		if err != nil {
			return err
		}
		if matched {
			files = append(files, path)
		}
		return nil
	})
	return files
}

// FindFilesMulti walks root a single time and buckets matching files by the
// pattern that matched them, avoiding one full tree walk per extension.
func FindFilesMulti(root string, patterns ...string) map[string][]string {
	out := make(map[string][]string, len(patterns))
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			logging.Warn("skipping unreadable path during lint scan: %s (%v)", path, err)
			return nil
		}
		if d.IsDir() {
			if skipLintDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		for _, pattern := range patterns {
			if matched, merr := filepath.Match(pattern, base); merr == nil && matched {
				out[pattern] = append(out[pattern], path)
				break
			}
		}
		return nil
	})
	return out
}
