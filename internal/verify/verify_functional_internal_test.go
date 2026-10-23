package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/dashboards"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/lint"
)

func TestFirstAssetPath(t *testing.T) {
	tests := []struct {
		name string
		page string
		want string
	}{
		{
			name: "root relative asset",
			page: `<html><head><link rel="stylesheet" href="/wp-includes/css/dist/block-library/style.min.css?ver=6.4" /></head></html>`,
			want: "/wp-includes/css/dist/block-library/style.min.css?ver=6.4",
		},
		{
			name: "absolute asset url",
			page: `<link rel='stylesheet' href="https://cms.fake-enterprise.com/wp-content/themes/twentytwentyfive/style.css" />`,
			want: "/wp-content/themes/twentytwentyfive/style.css",
		},
		{
			name: "script src",
			page: `<script src="https://cms.fake-enterprise.com/wp-includes/js/wp-emoji-release.min.js?ver=6.4"></script>`,
			want: "/wp-includes/js/wp-emoji-release.min.js?ver=6.4",
		},
		{
			name: "first match wins",
			page: `<a href="/wp-content/uploads/2026/01/photo.jpg">x</a><link href="/wp-content/themes/t/style.css">`,
			want: "/wp-content/uploads/2026/01/photo.jpg",
		},
		{
			name: "no wordpress asset",
			page: `<html><body><p>nothing here</p></body></html>`,
			want: "",
		},
		{
			name: "external asset is ignored",
			page: `<link href="https://fonts.example.com/wp-content.css">`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstAssetPath(tt.page); got != tt.want {
				t.Errorf("firstAssetPath(%q) = %q, want %q", tt.page, got, tt.want)
			}
		})
	}
}

func TestLoginFormPresent(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "wordpress login form",
			body: `<form name="loginform" id="loginform" action="https://cms.fake-enterprise.com/wp-login.php" method="post"><input name="log" id="user_login" /><input name="pwd" id="user_pass" /></form>`,
			want: true,
		},
		{
			name: "another page",
			body: `<html><body><h1>Fake Enterprise CMS</h1></body></html>`,
			want: false,
		},
		{
			name: "username field only",
			body: `<input name="log" />`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := loginFormPresent(tt.body); got != tt.want {
				t.Errorf("loginFormPresent(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestPermalinkPath(t *testing.T) {
	tests := []struct {
		name string
		link string
		want string
	}{
		{name: "pretty permalink", link: "https://cms.fake-enterprise.com/hello-world/", want: "/hello-world/"},
		{name: "plain permalink", link: "https://cms.fake-enterprise.com/?p=1", want: "/?p=1"},
		{name: "site root is not a permalink", link: "https://cms.fake-enterprise.com/", want: ""},
		{name: "empty link", link: "", want: ""},
		{name: "path with a fragment", link: "https://cms.fake-enterprise.com/hello-world/#respond", want: "/hello-world/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := permalinkPath(tt.link); got != tt.want {
				t.Errorf("permalinkPath(%q) = %q, want %q", tt.link, got, tt.want)
			}
		})
	}
}

func TestParseMariadbRoundTrip(t *testing.T) {
	t.Run("three counts", func(t *testing.T) {
		writes, posts, options, err := parseMariadbRoundTrip("1\n5\n187\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if writes != 1 || posts != 5 || options != 187 {
			t.Errorf("got writes=%d posts=%d options=%d, want 1/5/187", writes, posts, options)
		}
	})

	t.Run("too few counts", func(t *testing.T) {
		if _, _, _, err := parseMariadbRoundTrip("1\n5\n"); err == nil {
			t.Error("expected an error for a truncated result")
		}
	})

	t.Run("unexpected line", func(t *testing.T) {
		if _, _, _, err := parseMariadbRoundTrip("1\nERROR 1146 (42S02): table does not exist\n5\n"); err == nil {
			t.Error("expected an error for a non-numeric line")
		}
	})
}

func TestParseFrontendProbe(t *testing.T) {
	t.Run("count and filesystem", func(t *testing.T) {
		posts, fstype, err := parseFrontendProbe("posts=1\nuploads_fstype=nfs4\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if posts != 1 || fstype != "nfs4" {
			t.Errorf("got posts=%d fstype=%q, want 1/nfs4", posts, fstype)
		}
	})

	t.Run("php notice on the same stream is ignored", func(t *testing.T) {
		posts, fstype, err := parseFrontendProbe("PHP Deprecated: something in /var/www/html/wp-includes/x.php\nposts=12\nuploads_fstype=nfs\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if posts != 12 || fstype != "nfs" {
			t.Errorf("got posts=%d fstype=%q, want 12/nfs", posts, fstype)
		}
	})

	t.Run("local disk instead of nfs", func(t *testing.T) {
		posts, fstype, err := parseFrontendProbe("posts=3\nuploads_fstype=ext4\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if posts != 3 || fstype != "ext4" {
			t.Errorf("got posts=%d fstype=%q, want 3/ext4", posts, fstype)
		}
	})

	t.Run("unreadable count", func(t *testing.T) {
		if _, _, err := parseFrontendProbe("posts=many\nuploads_fstype=nfs4\n"); err == nil {
			t.Error("expected an error for a non-numeric post count")
		}
	})

	t.Run("no post count", func(t *testing.T) {
		if _, _, err := parseFrontendProbe("uploads_fstype=nfs4\n"); err == nil {
			t.Error("expected an error without a post count")
		}
	})

	t.Run("no filesystem type", func(t *testing.T) {
		if _, _, err := parseFrontendProbe("posts=1\n"); err == nil {
			t.Error("expected an error without a filesystem type")
		}
	})
}

// phpProbeOutput builds the labelled output the shell probe prints for a host.
// Every pinned package gets a version and a patch verdict so a test can vary one
// of them without silently omitting the others.
func phpProbeOutput(versions map[string]string, patched map[string]bool, modPHP string, mitigations ...string) string {
	var b strings.Builder
	for _, pkg := range phpPatchedPackages {
		verdict := "no"
		if patched[pkg] {
			verdict = "yes"
		}
		fmt.Fprintf(&b, "php_package=%s|%s\n", pkg, versions[pkg])
		fmt.Fprintf(&b, "php_patched=%s|%s\n", pkg, verdict)
	}
	fmt.Fprintf(&b, "php_mod_php=%s\n", modPHP)
	for _, m := range mitigations {
		fmt.Fprintf(&b, "php_mitigation=%s\n", m)
	}
	return b.String()
}

// patchedPHPVersions is the happy path: the same revision for every pinned
// binary package, exactly what the Puppet role installs.
func patchedPHPVersions() map[string]string {
	versions := make(map[string]string, len(phpPatchedPackages))
	for _, pkg := range phpPatchedPackages {
		versions[pkg] = phpPatchedPackage
	}
	return versions
}

func TestParsePHPPatchProbe(t *testing.T) {
	t.Run("patched frontend", func(t *testing.T) {
		patched := map[string]bool{}
		for _, pkg := range phpPatchedPackages {
			patched[pkg] = true
		}
		state, err := parsePHPPatchProbe(phpProbeOutput(patchedPHPVersions(), patched, "yes", requiredPHPMitigations...))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, pkg := range phpPatchedPackages {
			if got := state.packages[pkg].version; got != phpPatchedPackage {
				t.Errorf("%s version = %q, want %q", pkg, got, phpPatchedPackage)
			}
			if !state.packages[pkg].patched {
				t.Errorf("%s must be reported as patched", pkg)
			}
		}
		if !state.modPHP {
			t.Error("Apache must be reported as loading mod_php")
		}
		for _, m := range requiredPHPMitigations {
			if !state.mitigations[m] {
				t.Errorf("mitigation %q must be reported as in place", m)
			}
		}
	})

	t.Run("patched CLI with vulnerable Apache SAPI", func(t *testing.T) {
		// The regression this test exists for: dpkg upgrades are per binary
		// package, so a patched CLI says nothing about the module Apache
		// actually loads. The probe must report the SAPI separately.
		versions := patchedPHPVersions()
		patched := map[string]bool{}
		for _, pkg := range phpPatchedPackages {
			patched[pkg] = true
		}
		const vulnerable = "8.3.6-0ubuntu0.24.04.9"
		versions["libapache2-mod-php8.3"] = vulnerable
		patched["libapache2-mod-php8.3"] = false

		state, err := parsePHPPatchProbe(phpProbeOutput(versions, patched, "yes", requiredPHPMitigations...))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !state.packages["php8.3-cli"].patched {
			t.Error("the CLI was patched; only the SAPI should be reported as vulnerable")
		}
		if state.packages["libapache2-mod-php8.3"].patched {
			t.Error("an unpatched libapache2-mod-php8.3 must not be reported as patched")
		}
		if got := state.packages["libapache2-mod-php8.3"].version; got != vulnerable {
			t.Errorf("SAPI version = %q, want %q", got, vulnerable)
		}
	})

	t.Run("missing mitigation", func(t *testing.T) {
		patched := map[string]bool{}
		for _, pkg := range phpPatchedPackages {
			patched[pkg] = true
		}
		state, err := parsePHPPatchProbe(phpProbeOutput(patchedPHPVersions(), patched, "yes",
			"upload_progress_off", "memory_limit_missing"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.mitigations["memory_limit_unlimited"] {
			t.Error("a missing mitigation must not be reported as in place")
		}
		if !state.mitigations["upload_progress_off"] {
			t.Error("the mitigation that is in place must be reported")
		}
	})

	t.Run("noise on the same stream is ignored", func(t *testing.T) {
		patched := map[string]bool{}
		for _, pkg := range phpPatchedPackages {
			patched[pkg] = true
		}
		out := "PHP Deprecated: something in /var/www/html/wp-includes/x.php\n" +
			phpProbeOutput(patchedPHPVersions(), patched, "no")
		state, err := parsePHPPatchProbe(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.modPHP {
			t.Error("mod_php must not be reported as loaded")
		}
	})

	t.Run("missing package state", func(t *testing.T) {
		out := "php_package=php8.3-cli|" + phpPatchedPackage + "\nphp_patched=php8.3-cli|yes\n" +
			"php_package=libapache2-mod-php8.3|" + phpPatchedPackage + "\nphp_patched=libapache2-mod-php8.3|yes\n" +
			"php_mod_php=yes\n"
		if _, err := parsePHPPatchProbe(out); err == nil {
			t.Error("expected an error when a pinned package is missing from the probe")
		}
	})

	t.Run("no installed version", func(t *testing.T) {
		versions := patchedPHPVersions()
		versions["php8.3-common"] = ""
		patched := map[string]bool{}
		for _, pkg := range phpPatchedPackages {
			patched[pkg] = true
		}
		if _, err := parsePHPPatchProbe(phpProbeOutput(versions, patched, "yes")); err == nil {
			t.Error("expected an error when a pinned package reports no version")
		}
	})

	t.Run("no patched answer", func(t *testing.T) {
		patched := map[string]bool{}
		for _, pkg := range phpPatchedPackages {
			patched[pkg] = true
		}
		out := strings.Replace(phpProbeOutput(patchedPHPVersions(), patched, "yes"),
			"php_patched=php8.3-common|yes\n", "", 1)
		if _, err := parsePHPPatchProbe(out); err == nil {
			t.Error("expected an error when a package has no patch verdict")
		}
	})

	t.Run("no mod_php answer", func(t *testing.T) {
		patched := map[string]bool{}
		for _, pkg := range phpPatchedPackages {
			patched[pkg] = true
		}
		if _, err := parsePHPPatchProbe(phpProbeOutput(patchedPHPVersions(), patched, "")); err == nil {
			t.Error("expected an error without the mod_php answer")
		}
	})
}

func TestPHPPatchProbeChecksEveryPinnedPackage(t *testing.T) {
	// The probe asks the node itself: dpkg knows Debian's version rules, and
	// the floor the project pins is the revision that carries the fix.
	if !strings.Contains(phpPatchProbe, phpPatchedPackage) {
		t.Errorf("the probe must compare the installed revision against %q", phpPatchedPackage)
	}
	if !strings.Contains(phpPatchProbe, "dpkg --compare-versions") {
		t.Error("the probe must let dpkg compare the versions")
	}
	for _, pkg := range phpPatchedPackages {
		if !strings.Contains(phpPatchProbe, pkg) {
			t.Errorf("the probe must report %s, not just the CLI", pkg)
		}
	}
	for _, m := range requiredPHPMitigations {
		if !strings.Contains(phpPatchProbe, m) {
			t.Errorf("the probe must look for the mitigation %q", m)
		}
	}
}

func TestCountAlertRules(t *testing.T) {
	payload := `{"status":"success","data":{"groups":[{"name":"infrastructure","rules":[{"name":"a"},{"name":"b"}]},{"name":"services","rules":[{"name":"c"}]}]}}`
	groups, rules, err := countAlertRules(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if groups != 2 || rules != 3 {
		t.Errorf("got groups=%d rules=%d, want 2/3", groups, rules)
	}

	if _, _, err := countAlertRules(`{"status":"error","error":"bad_data"}`); err == nil {
		t.Error("expected an error for a failed query status")
	}

	if _, _, err := countAlertRules("not json"); err == nil {
		t.Error("expected an error for an unparsable payload")
	}
}

func TestDashboardUIDFromURL(t *testing.T) {
	tests := map[string]string{
		"/d/default-home-dashboard/cms-ha-e28094-drbd-etcd-lb-overview": "default-home-dashboard",
		"/d/cms-ha-overview/ha":                                "cms-ha-overview",
		"http://192.168.10.20:3000/d/default-home-dashboard/x": "default-home-dashboard",
		"/":           "",
		"/dashboards": "",
		"/d/only-uid": "only-uid",
		"":            "",
	}
	for in, want := range tests {
		if got := dashboardUIDFromURL(in); got != want {
			t.Errorf("dashboardUIDFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckHomeDashboard(t *testing.T) {
	shipped, err := dashboards.Load()
	if err != nil {
		t.Fatalf("load dashboards: %v", err)
	}
	home, ok := dashboards.Home(shipped)
	if !ok {
		t.Fatalf("no dashboard with uid %q", dashboards.HomeUID)
	}

	if err := checkHomeDashboard(home, shipped); err != nil {
		t.Errorf("the HA overview must pass the home check: %v", err)
	}

	// Grafana's built-in welcome page: no graphs, so it must be rejected.
	welcome := dashboards.Dashboard{Title: "Home"}
	if err := checkHomeDashboard(welcome, shipped); err == nil {
		t.Error("the welcome dashboard must not pass the home check")
	}

	// The right title with a different query is still the wrong dashboard.
	edited := home
	edited.Panels = append([]dashboards.Panel(nil), home.Panels...)
	edited.Panels[0].Targets = []dashboards.Target{{RefID: "A", Expr: "up"}}
	if err := checkHomeDashboard(edited, shipped); err == nil {
		t.Error("a home dashboard with an edited panel must not pass")
	}
}

// TestPHPPatchFloorIsInSyncWithPuppet keeps the two copies of the patched
// Debian revision together: the Go probe decides whether `cms-ha verify`
// passes, but Puppet is what actually upgrades the package. A bump in one
// place without the other would either fail a correctly patched lab or let a
// vulnerable one pass.
func TestPHPPatchFloorIsInSyncWithPuppet(t *testing.T) {
	root, err := lint.FindModuleRoot()
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "puppet/modules/role/manifests/cms_frontend.pp"))
	if err != nil {
		t.Fatalf("read cms_frontend.pp: %v", err)
	}
	want := "$php_patched_version = '" + phpPatchedPackage + "'"
	if !strings.Contains(string(manifest), want) {
		t.Errorf("cms_frontend.pp does not define %s; the Puppet and verify floors have drifted", want)
	}
}
