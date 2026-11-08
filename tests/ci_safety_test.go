package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type ciSafetyStep struct {
	If   string            `yaml:"if"`
	Name string            `yaml:"name"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
}

type ciSafetyJob struct {
	With  map[string]any    `yaml:"with"`
	Uses  string            `yaml:"uses"`
	Needs []string          `yaml:"needs"`
	If    string            `yaml:"if"`
	Env   map[string]string `yaml:"env"`
	Steps []ciSafetyStep    `yaml:"steps"`
}

type ciSafetyWorkflow struct {
	On          map[string]any         `yaml:"on"`
	Jobs        map[string]ciSafetyJob `yaml:"jobs"`
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
}

func ciSafetyRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func ciSafetyLoad(t *testing.T, name string) ciSafetyWorkflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ciSafetyRoot(t), ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var workflow ciSafetyWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func ciSafetyRun(t *testing.T, dir string, env []string, command string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s timed out: %s", command, out)
	}
	return out, err
}

func TestCISafetySummaryRequiresEveryJobSuccess(t *testing.T) {
	workflow := ciSafetyLoad(t, "ci.yml")
	summary := workflow.Jobs["ci-pass"]
	if summary.If != "always()" || len(summary.Steps) != 1 {
		t.Fatal("CI Summary must always run its result check")
	}
	for name := range workflow.Jobs {
		if name != "ci-pass" && !strings.Contains(" "+strings.Join(summary.Needs, " ")+" ", " "+name+" ") {
			t.Fatalf("CI Summary does not require %s", name)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	check := func(t *testing.T, failing, status string, wantSuccess bool) {
		t.Helper()
		script := summary.Steps[0].Run
		for _, name := range summary.Needs {
			result := "success"
			if name == failing {
				result = status
			}
			script = strings.ReplaceAll(script, "${{ needs."+name+".result }}", result)
		}
		if strings.Contains(script, "${{") {
			t.Fatal("unresolved workflow expression in summary test")
		}
		out, err := ciSafetyRun(t, t.TempDir(), nil, bash, "-eu", "-c", script)
		if (err == nil) != wantSuccess {
			t.Fatalf("summary success = %v, want %v: %s", err == nil, wantSuccess, out)
		}
	}
	t.Run("all successful", func(t *testing.T) { check(t, "", "", true) })
	for _, name := range summary.Needs {
		for _, status := range []string{"failure", "cancelled", "skipped", ""} {
			t.Run(name+"/"+status, func(t *testing.T) { check(t, name, status, false) })
		}
	}
}

func TestCISafetyReleaseRequiresTaggedCI(t *testing.T) {
	ci := ciSafetyLoad(t, "ci.yml")
	if _, ok := ci.On["workflow_call"]; !ok {
		t.Fatal("CI must be callable for tags without previous branch CI")
	}
	release := ciSafetyLoad(t, "release.yml")
	if release.Jobs["ci"].Uses != "./.github/workflows/ci.yml" {
		t.Fatal("release must run local CI against the triggering commit")
	}
	publish := release.Jobs["release"]
	if !reflect.DeepEqual(publish.Needs, []string{"ci", "integration-pxe"}) || publish.If != "" {
		t.Fatal("publication must require successful CI and PXE without an always() bypass")
	}
	if len(publish.Steps) == 0 || publish.Steps[0].With["ref"] != "${{ github.sha }}" {
		t.Fatal("release checkout must use the validated SHA, not a mutable tag")
	}
	build := ci.Jobs["build-go"]
	if build.Env["CGO_ENABLED"] != "0" {
		t.Fatal("production builds must remain static")
	}
	foundRace, foundK8s := false, false
	for _, step := range build.Steps {
		if step.Run == "make test" {
			foundRace = step.Env["CGO_ENABLED"] == "1"
		}
	}
	for _, step := range ci.Jobs["validate-k8s"].Steps {
		if step.Run == "make lint-k8s" {
			foundK8s = true
		}
	}
	if !foundRace || !foundK8s {
		t.Fatal("CI must enable cgo for race tests and use the tested rendered-resource gate")
	}
}

func TestCISafetyPXEGateBeforePublication(t *testing.T) {
	release := ciSafetyLoad(t, "release.yml")
	gate := release.Jobs["integration-pxe"]
	if gate.Uses != "./.github/workflows/integration-hosted.yml" ||
		!reflect.DeepEqual(gate.Needs, []string{"ci"}) || gate.If != "" {
		t.Fatal("PXE must call the local hosted workflow only after CI succeeds")
	}
	want := map[string]any{
		"stage": "full", "skip_chaos": false, "jumpstart_image": "iso",
		"fast_provisioning": false, "teardown": true,
	}
	if !reflect.DeepEqual(gate.With, want) {
		t.Fatalf("PXE inputs = %#v, want %#v", gate.With, want)
	}

	hosted := ciSafetyLoad(t, "integration-hosted.yml")
	if _, ok := hosted.On["release"]; ok {
		t.Fatal("release.published must not trigger a duplicate, post-publication PXE run")
	}
	for _, event := range []string{"workflow_dispatch", "push", "schedule"} {
		if _, ok := hosted.On[event]; !ok {
			t.Errorf("hosted workflow lost its %s trigger", event)
		}
	}
	if !strings.HasPrefix(hosted.Concurrency.Group, "integration-hosted-") ||
		!strings.Contains(hosted.Concurrency.Group, "${{ github.workflow }}") ||
		!strings.Contains(hosted.Concurrency.Group, "${{ github.ref }}") ||
		hosted.Concurrency.CancelInProgress || hosted.Concurrency.Group == release.Concurrency.Group {
		t.Fatal("hosted concurrency must be namespaced, non-cancelling and distinct from the caller lock")
	}

	steps := hosted.Jobs["integration"].Steps
	if len(steps) == 0 || steps[0].With["ref"] != "${{ github.sha }}" {
		t.Fatal("hosted checkout must validate the exact triggering SHA")
	}
	foundArtifacts, foundTeardown, foundFull := false, false, false
	for _, step := range steps {
		switch step.Name {
		case "Upload artifacts":
			foundArtifacts = step.If == "always()" &&
				(step.With["retention-days"] == "30" || step.With["retention-days"] == "90")
		case "Tear the lab down":
			foundTeardown = step.If == "${{ inputs.teardown && always() }}" &&
				strings.Contains(step.Run, "cleanup --yes")
		case "Run the integration test (deploy + verify + traffic + drills + failover)":
			foundFull = step.If == "${{ inputs.stage != 'provision' }}" &&
				step.Env["SKIP_CHAOS"] == "${{ inputs.skip_chaos }}"
		}
	}
	if !foundArtifacts || !foundTeardown || !foundFull {
		t.Fatal("PXE must retain evidence, run full integration/chaos and always attempt requested teardown")
	}
}

func TestCISafetyHostedInputContract(t *testing.T) {
	hosted := ciSafetyLoad(t, "integration-hosted.yml")
	type input struct {
		Type        string `yaml:"type"`
		Default     any    `yaml:"default"`
		Description string `yaml:"description"`
	}
	type trigger struct {
		Inputs   map[string]input `yaml:"inputs"`
		Branches []string         `yaml:"branches"`
	}
	decode := func(event string) trigger {
		t.Helper()
		raw, err := yaml.Marshal(hosted.On[event])
		if err != nil {
			t.Fatal(err)
		}
		var result trigger
		if err := yaml.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	call, dispatch := decode("workflow_call"), decode("workflow_dispatch")
	defaults := map[string]any{
		"stage": "full", "skip_chaos": false, "teardown": false,
		"jumpstart_image": "cloud", "fast_provisioning": true,
	}
	if len(call.Inputs) != len(defaults) || len(dispatch.Inputs) != len(defaults) {
		t.Fatal("call and dispatch must expose the same five inputs")
	}
	for name, wantDefault := range defaults {
		manual, reusable := dispatch.Inputs[name], call.Inputs[name]
		wantType := "boolean"
		if name == "stage" || name == "jumpstart_image" {
			wantType = "string"
			if manual.Type != "choice" {
				t.Errorf("dispatch input %s must retain its choice UI", name)
			}
		}
		if reusable.Type != wantType || !reflect.DeepEqual(reusable.Default, wantDefault) ||
			!reflect.DeepEqual(manual.Default, wantDefault) || reusable.Description != manual.Description ||
			(wantType == "boolean" && manual.Type != "boolean") {
			t.Errorf("input %s: call=%+v dispatch=%+v", name, reusable, manual)
		}
	}
	if !reflect.DeepEqual(decode("push").Branches, []string{"main"}) {
		t.Fatal("standalone pushes must remain main-only; tags enter via the release caller")
	}
}

func TestCISafetyHostedProfileResolution(t *testing.T) {
	hosted := ciSafetyLoad(t, "integration-hosted.yml")
	var resolver ciSafetyStep
	for _, step := range hosted.Jobs["integration"].Steps {
		if step.Name == "Resolve integration profile" {
			resolver = step
		}
	}
	wantEnv := map[string]string{
		"STAGE_INPUT": "${{ inputs.stage }}", "IMAGE_INPUT": "${{ inputs.jumpstart_image }}",
		"FAST_INPUT": "${{ inputs.fast_provisioning }}",
	}
	if resolver.Run == "" || !reflect.DeepEqual(resolver.Env, wantEnv) || strings.Contains(resolver.Run, "${{") {
		t.Fatal("resolve typed inputs through env, never infer workflow_call from github.event_name")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	tests := []struct {
		name, event, ref, stage, image, fast string
		want                                 string
		wantError                            bool
	}{
		{name: "nightly cloud", event: "schedule", ref: "refs/heads/main", want: "JUMPSTART_IMAGE=cloud\nFAST_PROVISIONING=true\n"},
		{name: "main push cloud", event: "push", ref: "refs/heads/main", want: "JUMPSTART_IMAGE=cloud\nFAST_PROVISIONING=true\n"},
		{name: "manual defaults", event: "workflow_dispatch", stage: "full", image: "cloud", fast: "true", want: "JUMPSTART_IMAGE=cloud\nFAST_PROVISIONING=true\n"},
		{name: "manual PXE", event: "workflow_dispatch", stage: "full", image: "iso", fast: "false", want: "JUMPSTART_IMAGE=iso\nFAST_PROVISIONING=false\n"},
		{name: "reusable retains caller push tag", event: "push", ref: "refs/tags/v1.2.3", stage: "full", image: "iso", fast: "false", want: "JUMPSTART_IMAGE=iso\nFAST_PROVISIONING=false\n"},
		{name: "manual provision cloud", event: "workflow_dispatch", stage: "provision", image: "cloud", fast: "true", want: "JUMPSTART_IMAGE=cloud\nFAST_PROVISIONING=true\n"},
		{name: "invalid called stage", stage: "typo", wantError: true},
		{name: "invalid called image", image: "typo", wantError: true},
		{name: "invalid fast value", fast: "typo", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			envFile := filepath.Join(dir, "github-env")
			env := []string{
				"GITHUB_ENV=" + envFile, "GITHUB_EVENT_NAME=" + tt.event, "GITHUB_REF=" + tt.ref,
				"STAGE_INPUT=" + tt.stage, "IMAGE_INPUT=" + tt.image, "FAST_INPUT=" + tt.fast,
			}
			out, err := ciSafetyRun(t, dir, env, bash, "-eu", "-c", resolver.Run)
			if tt.wantError {
				if err == nil {
					t.Fatalf("invalid input passed: %s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve profile: %v: %s", err, out)
			}
			got, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("profile = %q, want %q", got, tt.want)
			}
		})
	}
}

// Run Makefile recipes in an isolated directory with fake tools: no scanner,
// network access, infrastructure command or Go build is needed by these tests.
func ciSafetyMake(t *testing.T, tools map[string]string, target string, extra ...string) ([]byte, error) {
	t.Helper()
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, script := range tools {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+script+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Only expose explicitly requested utilities, never real installed scanners.
	for _, name := range []string{"bash", "mktemp", "rm", "cat", "python3"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s not available", name)
		}
		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"--no-print-directory", "-f", filepath.Join(ciSafetyRoot(t), "Makefile"), target,
		"VERSION=test", "COMMIT=test", "BUILD_DATE=test"}
	args = append(args, extra...)
	return ciSafetyRun(t, dir, []string{"PATH=" + bin, "MAKEFLAGS=", "MAKELEVEL=0", "CGO_ENABLED=0"}, makePath, args...)
}

func TestCISafetyMakeRace(t *testing.T) {
	out, err := ciSafetyMake(t, map[string]string{"go": `test "$CGO_ENABLED" = 1 && test "$*" = 'test -race ./...'`}, "test")
	if err != nil {
		t.Fatalf("make test must override inherited CGO_ENABLED=0: %v: %s", err, out)
	}
}

func TestCISafetySecurity(t *testing.T) {
	for _, failing := range []string{"", "gosec", "govulncheck", "gitleaks", "trivy"} {
		t.Run("scanner/"+failing, func(t *testing.T) {
			tools := map[string]string{"gosec": "exit 0", "govulncheck": "exit 0", "gitleaks": "exit 0", "trivy": "exit 0"}
			if failing != "" {
				tools[failing] = "exit 23"
				if failing == "trivy" {
					// Its later report-only invocation must not mask vulnerabilities.
					tools[failing] = `[[ "$*" != *vuln,secret* ]] || exit 23`
				}
			}
			out, err := ciSafetyMake(t, tools, "security", "SECURITY_STRICT=1")
			if (err == nil) != (failing == "") {
				t.Fatalf("unexpected scanner result: %v: %s", err, out)
			}
		})
	}
	for _, missing := range []string{"gosec", "govulncheck", "gitleaks", "trivy"} {
		t.Run("missing/"+missing, func(t *testing.T) {
			tools := map[string]string{"gosec": "exit 0", "govulncheck": "exit 0", "gitleaks": "exit 0", "trivy": "exit 0"}
			delete(tools, missing)
			out, err := ciSafetyMake(t, tools, "security")
			if err == nil || !strings.Contains(string(out), "Required security scanner missing: "+missing) {
				t.Fatalf("missing scanner must fail by default: %v: %s", err, out)
			}
		})
	}
	out, err := ciSafetyMake(t, nil, "security", "SECURITY_STRICT=0")
	if err != nil {
		t.Fatalf("explicit best-effort mode should allow missing tools: %v: %s", err, out)
	}
}

func TestCISafetyRenderedKubernetes(t *testing.T) {
	t.Run("missing tools", func(t *testing.T) {
		out, err := ciSafetyMake(t, nil, "lint-k8s")
		if err == nil || !strings.Contains(string(out), "Required tool missing: kustomize") {
			t.Fatalf("missing renderer must fail: %v: %s", err, out)
		}
	})
	for _, scenario := range []string{"valid", "empty", "invalid", "errors", "skipped", "malformed summary", "render failure", "validator failure"} {
		t.Run(scenario, func(t *testing.T) {
			render := "printf 'apiVersion: v1\\nkind: Namespace\\nmetadata:\\n  name: test\\n'"
			valid, invalid, errors, skipped := "1", "0", "0", "0"
			switch scenario {
			case "empty":
				render, valid = "exit 0", "0"
			case "invalid":
				invalid = "1"
			case "errors":
				errors = "1"
			case "skipped":
				skipped = "1"
			case "render failure":
				render = "exit 23"
			}
			validate := `[[ "$*" == '-strict -summary -output json -kubernetes-version 1.37.2 '* ]] || exit 24
file="${@: -1}"
[[ "$file" == */rendered.yaml && -f "$file" ]] || exit 25
while IFS= read -r line; do [[ "$line" != 'kind: Namespace' ]] || found=1; done < "$file"
[[ "${found:-0}" == 1 ]] || exit 26
printf '%s\n' '{"summary":{"valid":` + valid + `,"invalid":` + invalid + `,"errors":` + errors + `,"skipped":` + skipped + `}}'`
			if scenario == "empty" {
				validate = `printf '%s\n' '{"summary":{"valid":0,"invalid":0,"errors":0,"skipped":0}}'`
			}
			if scenario == "malformed summary" {
				validate = `printf '%s\n' '{"summary":{}}'`
			}
			if scenario == "validator failure" {
				validate = "exit 23"
			}
			out, err := ciSafetyMake(t, map[string]string{"kustomize": render, "kubeconform": validate}, "lint-k8s")
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("rendered-resource validation: %v: %s", err, out)
			}
			if scenario == "valid" {
				for _, root := range []string{"overlays/lab", "backups", "jobs", "monitoring"} {
					if !strings.Contains(string(out), "/kubernetes/"+root) {
						t.Errorf("root %s not validated: %s", root, out)
					}
				}
			}
		})
	}
}
