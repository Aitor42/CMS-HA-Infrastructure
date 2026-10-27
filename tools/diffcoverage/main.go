// Command diffcoverage enforces coverage on the lines a change actually
// touches. A global percentage rewards untouched packages and hides new,
// untested code; this gate only looks at added or modified Go lines.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type block struct {
	start int
	end   int
	count int
}

func main() {
	base := flag.String("base", "origin/main", "git revision to compare against")
	profile := flag.String("profile", "coverage.out", "Go coverage profile")
	min := flag.Float64("min", 60.0, "minimum changed-line coverage percentage")
	flag.Parse()

	module := modulePath()
	coverage, err := readProfile(*profile, module)
	if err != nil {
		fatalf("%v", err)
	}
	changed, err := changedLines(*base)
	if err != nil {
		fatalf("%v", err)
	}

	total, covered := 0, 0
	for file, lines := range changed {
		if strings.HasSuffix(file, "_test.go") || strings.HasPrefix(file, "tools/diffcoverage/") {
			continue
		}
		blocks := coverage[file]
		if len(blocks) == 0 {
			continue
		}
		for line := range lines {
			hit := false
			executable := false
			for _, b := range blocks {
				if line >= b.start && line <= b.end {
					executable = true
					if b.count > 0 {
						hit = true
						break
					}
				}
			}
			if !executable {
				continue
			}
			total++
			if hit {
				covered++
			}
		}
	}

	if total == 0 {
		fmt.Println("changed-line coverage: no executable Go lines changed")
		return
	}
	pct := float64(covered) / float64(total) * 100
	fmt.Printf("changed-line coverage: %.1f%% (%d/%d executable lines)\n", pct, covered, total)
	if pct < *min {
		fatalf("changed-line coverage %.1f%% is below the %.1f%% minimum", pct, *min)
	}
}

func modulePath() string {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	fatalf("go.mod has no module directive")
	return ""
}

func readProfile(path, module string) (map[string][]block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open coverage profile: %w", err)
	}
	defer f.Close()

	out := make(map[string][]block)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			continue
		}
		file := line[:colon]
		fields := strings.Fields(line[colon+1:])
		if len(fields) != 3 {
			continue
		}
		bounds := strings.Split(fields[0], ",")
		if len(bounds) != 2 {
			continue
		}
		start := lineNumber(strings.Split(bounds[0], ".")[0])
		end := lineNumber(strings.Split(bounds[1], ".")[0])
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		file = strings.TrimPrefix(file, module+"/")
		if filepath.IsAbs(file) {
			if rel, err := filepath.Rel(".", file); err == nil {
				file = rel
			}
		}
		out[file] = append(out[file], block{start: start, end: end, count: count})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read coverage profile: %w", err)
	}
	return out, nil
}

func lineNumber(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func changedLines(base string) (map[string]map[int]bool, error) {
	rev := base
	if out, err := exec.Command("git", "merge-base", base, "HEAD").Output(); err == nil {
		rev = strings.TrimSpace(string(out))
	}
	out, err := exec.Command("git", "diff", "--unified=0", rev, "HEAD", "--", "*.go").Output()
	if err != nil {
		return nil, fmt.Errorf("git diff against %s: %w", base, err)
	}

	changed := make(map[string]map[int]bool)
	current := ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			current = strings.TrimPrefix(line, "+++ b/")
			continue
		}
		if current == "" || !strings.HasPrefix(line, "@@ ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasPrefix(fields[2], "+") {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(fields[2], "+"), ",")
		start := lineNumber(parts[0])
		count := 1
		if len(parts) == 2 {
			count = lineNumber(parts[1])
		}
		if count == 0 {
			continue
		}
		if changed[current] == nil {
			changed[current] = make(map[int]bool)
		}
		for i := 0; i < count; i++ {
			changed[current][start+i] = true
		}
	}
	return changed, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "diffcoverage: "+format+"\n", args...)
	os.Exit(1)
}
