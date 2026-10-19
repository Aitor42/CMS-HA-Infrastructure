package utils

import "testing"

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
