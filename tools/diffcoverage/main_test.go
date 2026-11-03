package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.out")
	content := "mode: set\n" +
		"github.com/Aitor42/CMS-HA-Infrastructure/internal/config/a.go:10.1,12.2 2 1\n" +
		"github.com/Aitor42/CMS-HA-Infrastructure/internal/config/a.go:20.1,22.2 1 0\n" +
		"example.com/other/b.go:1.1,2.2 1 1\n"
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	content += filepath.Join(cwd, "absolute.go") + ":1.1,2.2 1 1\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	got, err := readProfile(path, "github.com/Aitor42/CMS-HA-Infrastructure")
	if err != nil {
		t.Fatalf("readProfile: %v", err)
	}
	if len(got["absolute.go"]) != 1 {
		t.Fatalf("absolute profile path was not normalized: %+v", got)
	}
	blocks := got["internal/config/a.go"]
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(blocks), blocks)
	}
	if blocks[0] != (block{start: 10, end: 12, count: 1}) {
		t.Fatalf("first block = %+v", blocks[0])
	}
	if blocks[1] != (block{start: 20, end: 22, count: 0}) {
		t.Fatalf("second block = %+v", blocks[1])
	}
}

func TestChangedCoverageMissingProfile(t *testing.T) {
	tests := []struct {
		name, path, source string
		wantError          string
	}{
		{
			name: "function followed by external declaration still needs profile",
			path: "mixed.go", source: "package fixture\nfunc New() int { return 42 }\nfunc External()\n",
			wantError: "missing from coverage profile",
		},
		{
			name: "types constants and variables need no instrumentation",
			path: "internal/config/types.go",
			source: `package fixture

type Config struct { Enabled bool }
type Transform func(int) int
const DefaultEnabled = true
var Default = Config{Enabled: DefaultEnabled}
var Callback Transform
`,
		},
		{
			name:   "embedded filesystem needs no instrumentation",
			path:   "embed.go",
			source: "package fixture\nimport \"embed\"\n//go:embed assets/*\nvar Assets embed.FS\n",
		},
		{
			name: "external function declaration has no body to instrument",
			path: "external.go", source: "package fixture\nfunc External()\n",
		},
		{
			name: "new function without profile fails",
			path: "new.go", source: "package fixture\nfunc New() int { return 42 }\n",
			wantError: "missing from coverage profile",
		},
		{
			name: "empty function still needs instrumentation",
			path: "empty.go", source: "package fixture\nfunc Empty() {}\n",
			wantError: "missing from coverage profile",
		},
		{
			name: "method without profile fails",
			path: "method.go", source: "package fixture\ntype T struct{}\nfunc (T) Method() {}\n",
			wantError: "missing from coverage profile",
		},
		{
			name: "init function without profile fails",
			path: "init.go", source: "package fixture\nfunc init() {}\n",
			wantError: "missing from coverage profile",
		},
		{
			name: "function literal in variable initializer needs instrumentation",
			path: "literal.go", source: "package fixture\nvar Value = func() int { return 42 }()\n",
			wantError: "missing from coverage profile",
		},
		{
			name: "partial AST from malformed declarations must not pass",
			path: "broken.go", source: "package fixture\ntype Valid struct{}\nvar Broken =\n",
			wantError: "parse changed Go file",
		},
		{
			name: "malformed function must not pass",
			path: "broken_func.go", source: "package fixture\nfunc Broken( {\n",
			wantError: "parse changed Go file",
		},
		{
			name: "missing source must fail closed",
			path: "absent.go", wantError: "parse changed Go file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if err := os.Mkdir("assets", 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile("assets/example.txt", []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if tt.source != "" {
				if err := os.MkdirAll(filepath.Dir(tt.path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(tt.path, []byte(tt.source), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Fully covered code elsewhere must not conceal an unprofiled function,
			// nor should uninstrumentable declarations dilute its coverage.
			profile := map[string][]block{"covered.go": {{start: 2, end: 2, count: 1}}}
			changed := map[string]map[int]bool{"covered.go": {2: true}, tt.path: {2: true}}
			total, covered, err := changedCoverage(profile, changed)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) || !strings.Contains(err.Error(), tt.path) {
					t.Fatalf("error = %v, want %q for %s", err, tt.wantError, tt.path)
				}
				return
			}
			if err != nil || total != 1 || covered != 1 {
				t.Fatalf("coverage = %d/%d, error = %v; want 1/1 without error", covered, total, err)
			}
		})
	}
}

func TestChangedCoverage(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, name := range []string{"new.go", "z.go"} {
		if err := os.WriteFile(name, []byte("package fixture\nfunc New() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	profile := map[string][]block{
		"code.go":    {{start: 10, end: 12, count: 0}, {start: 12, end: 14, count: 3}},
		"reverse.go": {{start: 12, end: 14, count: 3}, {start: 10, end: 12, count: 0}},
		"covered.go": {{start: 10, end: 12, count: 1}, {start: 12, end: 14, count: 3}},
	}
	tests := []struct {
		name    string
		changed map[string]map[int]bool
		total   int
		covered int
		missing string
	}{
		{
			name:    "counts each line once, every overlapping block must be covered",
			changed: map[string]map[int]bool{"code.go": {1: true, 10: true, 11: true, 12: true, 14: true, 20: true}},
			total:   4, covered: 1,
		},
		{
			name:    "covered overlap cannot hide a later uncovered block",
			changed: map[string]map[int]bool{"reverse.go": {12: true}},
			total:   1,
		},
		{
			name:    "fully covered overlap is counted once",
			changed: map[string]map[int]bool{"covered.go": {12: true}},
			total:   1, covered: 1,
		},
		{name: "no changes"},
		{name: "no added lines need no source", changed: map[string]map[int]bool{"deleted.go": {}}},
		{name: "non executable changes", changed: map[string]map[int]bool{"code.go": {1: true, 20: true}}},
		{
			name:    "excluded test and coverage tool files need no profile",
			changed: map[string]map[int]bool{"code_test.go": {1: true}, "tools/diffcoverage/main.go": {1: true}},
		},
		{
			name:    "unprofiled files fail even alongside fully covered changes",
			changed: map[string]map[int]bool{"code.go": {14: true}, "z.go": {5: true}, "new.go": {3: true}},
			total:   1, covered: 1, missing: "new.go, z.go",
		},
		{
			name:    "all unprofiled is not a no executable changes pass",
			changed: map[string]map[int]bool{"new.go": {3: true}},
			missing: "new.go",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, covered, err := changedCoverage(profile, tt.changed)
			if total != tt.total || covered != tt.covered {
				t.Errorf("coverage = %d/%d, want %d/%d", covered, total, tt.covered, tt.total)
			}
			if tt.missing == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.missing) {
				t.Errorf("error = %v, want missing files %q", err, tt.missing)
			}
		})
	}
}
