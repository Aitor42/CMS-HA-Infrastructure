package utils

import (
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/dashboards"
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

func TestParsePHPPatchProbe(t *testing.T) {
	t.Run("patched frontend", func(t *testing.T) {
		out := "php_package=8.3.6-0ubuntu0.24.04.13\nphp_patched=yes\nphp_mod_php=yes\n" +
			"php_mitigation=upload_progress_off\nphp_mitigation=memory_limit_unlimited\n"
		state, err := parsePHPPatchProbe(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.packageVersion != "8.3.6-0ubuntu0.24.04.13" {
			t.Errorf("got version %q, want 8.3.6-0ubuntu0.24.04.13", state.packageVersion)
		}
		if !state.patched {
			t.Error("the frontend must be reported as patched")
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

	t.Run("vulnerable frontend", func(t *testing.T) {
		out := "php_package=8.3.6-0ubuntu0.24.04.9\nphp_patched=no\nphp_mod_php=yes\n" +
			"php_mitigation=upload_progress_off\nphp_mitigation=memory_limit_unlimited\n"
		state, err := parsePHPPatchProbe(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.patched {
			t.Error("an unpatched frontend must not be reported as patched")
		}
		if state.packageVersion != "8.3.6-0ubuntu0.24.04.9" {
			t.Errorf("got version %q, want the installed one", state.packageVersion)
		}
	})

	t.Run("missing mitigation", func(t *testing.T) {
		out := "php_package=8.3.6-0ubuntu0.24.04.13\nphp_patched=yes\nphp_mod_php=yes\n" +
			"php_mitigation=upload_progress_off\nphp_mitigation=memory_limit_missing\n"
		state, err := parsePHPPatchProbe(out)
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
		out := "PHP Deprecated: something in /var/www/html/wp-includes/x.php\n" +
			"php_package=8.3.6-0ubuntu0.24.04.13\nphp_patched=yes\nphp_mod_php=no\n"
		state, err := parsePHPPatchProbe(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.modPHP {
			t.Error("mod_php must not be reported as loaded")
		}
	})

	t.Run("no package version", func(t *testing.T) {
		if _, err := parsePHPPatchProbe("php_patched=yes\nphp_mod_php=yes\n"); err == nil {
			t.Error("expected an error without a package version")
		}
	})

	t.Run("no patched answer", func(t *testing.T) {
		out := "php_package=8.3.6-0ubuntu0.24.04.13\nphp_mod_php=yes\n"
		if _, err := parsePHPPatchProbe(out); err == nil {
			t.Error("expected an error without the patched answer")
		}
	})

	t.Run("no mod_php answer", func(t *testing.T) {
		out := "php_package=8.3.6-0ubuntu0.24.04.13\nphp_patched=yes\n"
		if _, err := parsePHPPatchProbe(out); err == nil {
			t.Error("expected an error without the mod_php answer")
		}
	})
}

func TestPHPPatchProbeComparesAgainstThePatchedRevision(t *testing.T) {
	// The probe asks the node itself: dpkg knows Debian's version rules, and
	// the floor the project pins is the revision that carries the fix.
	if !strings.Contains(phpPatchProbe, phpPatchedPackage) {
		t.Errorf("the probe must compare the installed revision against %q", phpPatchedPackage)
	}
	if !strings.Contains(phpPatchProbe, "dpkg --compare-versions") {
		t.Error("the probe must let dpkg compare the versions")
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

func TestPrometheusValues(t *testing.T) {
	t.Run("all series", func(t *testing.T) {
		payload := `{"status":"success","data":{"result":[{"value":[1700000000,"1"]},{"value":[1700000000,"1"]},{"value":[1700000000,"0"]}]}}`
		values, err := prometheusValues(payload)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(values) != 3 || values[0] != 1 || values[2] != 0 {
			t.Errorf("got %v, want [1 1 0]", values)
		}
	})

	t.Run("empty result", func(t *testing.T) {
		values, err := prometheusValues(`{"status":"success","data":{"result":[]}}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(values) != 0 {
			t.Errorf("got %v, want no values", values)
		}
	})

	t.Run("error status", func(t *testing.T) {
		if _, err := prometheusValues(`{"status":"error","errorType":"bad_data"}`); err == nil {
			t.Error("expected an error for a failed query status")
		}
	})

	t.Run("sample without a value", func(t *testing.T) {
		if _, err := prometheusValues(`{"status":"success","data":{"result":[{"value":[1700000000]}]}}`); err == nil {
			t.Error("expected an error for a sample without a value")
		}
	})
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "plain", want: "'plain'"},
		{in: "pa'ss", want: `'pa'\''ss'`},
		{in: "", want: "''"},
	}
	for _, tt := range tests {
		if got := shellQuote(tt.in); got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
		}
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
