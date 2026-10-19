package utils

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests drive the functional checks against local HTTP servers that
// answer the way a converged lab does, so the request paths, the payload
// parsing and the verdicts are exercised without a deployment.

type httpRoute struct {
	path   string
	status int
	body   string
}

// routeHandler answers exactly the registered paths and 404 for anything else,
// so a check that requests a wrong path fails the test instead of matching a
// catch-all.
func routeHandler(routes ...httpRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, rt := range routes {
			if rt.path != r.URL.Path {
				continue
			}
			if rt.status != 0 {
				w.WriteHeader(rt.status)
			}
			_, _ = io.WriteString(w, rt.body)
			return
		}
		http.NotFound(w, r)
	})
}

func newTestServer(t *testing.T, tls bool, handler http.Handler) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(handler)
	} else {
		srv = httptest.NewServer(handler)
	}
	t.Cleanup(srv.Close)
	return srv
}

func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Location", "https://cms.fake-enterprise.com"+r.URL.Path)
	w.WriteHeader(http.StatusMovedPermanently)
}

const (
	wpMarkup      = `<html class="wp-"><body>Fake Enterprise CMS</body></html>`
	wpLoginForm   = `<form name="loginform"><input name="log" id="user_login"><input name="pwd" id="user_pass"></form>`
	wpFeed        = `<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>`
	wpHomeAsset   = `<link rel="stylesheet" href="https://cms.fake-enterprise.com/wp-content/themes/t/style.css?ver=6.4">`
	wpAssetPath   = "/wp-content/themes/t/style.css"
	wpRESTIndex   = `{"namespaces":["wp/v2","oembed/1.0","wp-site-health/v1"]}`
	wpRESTPosts   = `[{"id":1,"link":"https://cms.fake-enterprise.com/hello-world/"}]`
	wpPermalink   = "/hello-world/"
	testVHost     = "cms.fake-enterprise.com"
	probesAllUp   = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"vip"},"value":[1700000000,"1"]},{"metric":{"instance":"cms1"},"value":[1700000000,"1"]}]}}`
	probesOneDown = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"vip"},"value":[1700000000,"1"]},{"metric":{"instance":"cms1"},"value":[1700000000,"0"]}]}}`
	alertRulesDoc = `{"status":"success","data":{"groups":[{"name":"infrastructure","rules":[{"name":"a"},{"name":"b"}]},{"name":"services","rules":[{"name":"c"}]}]}}`
)

func TestCheckCMSPages(t *testing.T) {
	healthyPages := func() http.Handler {
		return routeHandler(
			httpRoute{path: "/wp-login.php", body: wpLoginForm},
			httpRoute{path: "/feed/", body: wpFeed},
			httpRoute{path: "/", body: wpHomeAsset},
			httpRoute{path: wpAssetPath, body: "body{}"},
		)
	}
	siteFor := func(t *testing.T, pages http.Handler, plain http.Handler) cmsSite {
		t.Helper()
		return cmsSite{
			httpsBase: newTestServer(t, true, pages).URL,
			httpBase:  newTestServer(t, false, plain).URL,
			vhost:     testVHost,
		}
	}

	t.Run("healthy site", func(t *testing.T) {
		out := (&Verifier{}).checkCMSPages(context.Background(), siteFor(t, healthyPages(), http.HandlerFunc(redirectToHTTPS)))
		if !out.pass {
			t.Fatalf("expected pass, got %q", out.detail)
		}
		for _, want := range []string{"login form 200", "feed 200", "asset 200", "http->https 301"} {
			if !strings.Contains(out.detail, want) {
				t.Errorf("detail %q does not mention %q", out.detail, want)
			}
		}
	})

	t.Run("login page without the form", func(t *testing.T) {
		pages := routeHandler(
			httpRoute{path: "/wp-login.php", body: `<html>maintenance</html>`},
			httpRoute{path: "/feed/", body: wpFeed},
			httpRoute{path: "/", body: wpHomeAsset},
			httpRoute{path: wpAssetPath, body: "body{}"},
		)
		out := (&Verifier{}).checkCMSPages(context.Background(), siteFor(t, pages, http.HandlerFunc(redirectToHTTPS)))
		if out.pass || !strings.Contains(out.detail, "credential fields") {
			t.Errorf("expected a credential-fields failure, got pass=%v detail=%q", out.pass, out.detail)
		}
	})

	t.Run("home page with no asset reference", func(t *testing.T) {
		pages := routeHandler(
			httpRoute{path: "/wp-login.php", body: wpLoginForm},
			httpRoute{path: "/feed/", body: wpFeed},
			httpRoute{path: "/", body: `<html>plain</html>`},
		)
		out := (&Verifier{}).checkCMSPages(context.Background(), siteFor(t, pages, http.HandlerFunc(redirectToHTTPS)))
		if out.pass || !strings.Contains(out.detail, "references no /wp-content") {
			t.Errorf("expected an asset-reference failure, got pass=%v detail=%q", out.pass, out.detail)
		}
	})

	t.Run("asset is not served", func(t *testing.T) {
		pages := routeHandler(
			httpRoute{path: "/wp-login.php", body: wpLoginForm},
			httpRoute{path: "/feed/", body: wpFeed},
			httpRoute{path: "/", body: wpHomeAsset},
			httpRoute{path: wpAssetPath, status: http.StatusNotFound, body: "not found"},
		)
		out := (&Verifier{}).checkCMSPages(context.Background(), siteFor(t, pages, http.HandlerFunc(redirectToHTTPS)))
		if out.pass || !strings.Contains(out.detail, "returned HTTP 404") {
			t.Errorf("expected an asset failure, got pass=%v detail=%q", out.pass, out.detail)
		}
	})

	t.Run("plain http does not redirect", func(t *testing.T) {
		plain := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, wpMarkup)
		})
		out := (&Verifier{}).checkCMSPages(context.Background(), siteFor(t, healthyPages(), plain))
		if out.pass || !strings.Contains(out.detail, "want a redirect to HTTPS") {
			t.Errorf("expected a redirect failure, got pass=%v detail=%q", out.pass, out.detail)
		}
	})
}

func TestCheckCMSREST(t *testing.T) {
	siteFor := func(t *testing.T, posts string, permalinkStatus int) cmsSite {
		t.Helper()
		srv := newTestServer(t, true, routeHandler(
			httpRoute{path: "/wp-json/", body: wpRESTIndex},
			httpRoute{path: "/wp-json/wp/v2/posts", body: posts},
			httpRoute{path: wpPermalink, status: permalinkStatus, body: wpMarkup},
		))
		return cmsSite{httpsBase: srv.URL, httpBase: srv.URL, vhost: testVHost}
	}

	t.Run("healthy api", func(t *testing.T) {
		out := (&Verifier{}).checkCMSREST(context.Background(), siteFor(t, wpRESTPosts, 0))
		if !out.pass {
			t.Fatalf("expected pass, got %q", out.detail)
		}
		for _, want := range []string{"3 namespace(s)", "1 post(s)", wpPermalink} {
			if !strings.Contains(out.detail, want) {
				t.Errorf("detail %q does not mention %q", out.detail, want)
			}
		}
	})

	t.Run("no application data", func(t *testing.T) {
		out := (&Verifier{}).checkCMSREST(context.Background(), siteFor(t, `[]`, 0))
		if out.pass || !strings.Contains(out.detail, "holds no application data") {
			t.Errorf("expected an empty-collection failure, got pass=%v detail=%q", out.pass, out.detail)
		}
	})

	t.Run("permalink does not render", func(t *testing.T) {
		out := (&Verifier{}).checkCMSREST(context.Background(), siteFor(t, wpRESTPosts, http.StatusNotFound))
		if out.pass || !strings.Contains(out.detail, "returned HTTP 404") {
			t.Errorf("expected a permalink failure, got pass=%v detail=%q", out.pass, out.detail)
		}
	})
}

func TestCheckLoadBalancers(t *testing.T) {
	serve := func(t *testing.T, status int, body string) string {
		t.Helper()
		srv := newTestServer(t, true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if status != 0 {
				w.WriteHeader(status)
			}
			_, _ = io.WriteString(w, body)
		}))
		return strings.TrimPrefix(srv.URL, "https://")
	}

	t.Run("both balancers serve the site", func(t *testing.T) {
		out := (&Verifier{}).checkLoadBalancers(context.Background(), testVHost, []string{
			serve(t, 0, wpMarkup), serve(t, 0, wpMarkup),
		})
		if !out.pass {
			t.Fatalf("expected pass, got %q", out.detail)
		}
		if got := strings.Count(out.detail, "HTTP 200"); got != 2 {
			t.Errorf("detail %q reports %d healthy balancers, want 2", out.detail, got)
		}
	})

	t.Run("backup balancer is broken", func(t *testing.T) {
		out := (&Verifier{}).checkLoadBalancers(context.Background(), testVHost, []string{
			serve(t, 0, wpMarkup), serve(t, http.StatusBadGateway, "bad gateway"),
		})
		if out.pass || !strings.Contains(out.detail, "HTTP 502") {
			t.Errorf("expected a 502 failure, got pass=%v detail=%q", out.pass, out.detail)
		}
	})
}

func TestCheckMonitoringProbes(t *testing.T) {
	serverFor := func(t *testing.T, probes, rules string) string {
		t.Helper()
		return newTestServer(t, false, routeHandler(
			httpRoute{path: "/api/v1/rules", body: rules},
			httpRoute{path: "/api/v1/query", body: probes},
		)).URL
	}

	t.Run("rules loaded and probes up", func(t *testing.T) {
		out := (&Verifier{}).checkMonitoringProbes(context.Background(), serverFor(t, probesAllUp, alertRulesDoc))
		if !out.pass {
			t.Fatalf("expected pass, got %q", out.detail)
		}
		for _, want := range []string{"3 rule(s) in 2 group(s)", "2/2 blackbox probes up"} {
			if !strings.Contains(out.detail, want) {
				t.Errorf("detail %q does not mention %q", out.detail, want)
			}
		}
	})

	t.Run("a probe is down", func(t *testing.T) {
		out := (&Verifier{}).checkMonitoringProbes(context.Background(), serverFor(t, probesOneDown, alertRulesDoc))
		if out.pass || !strings.Contains(out.detail, "1 of 2 blackbox probes are failing") {
			t.Errorf("expected a probe failure, got pass=%v detail=%q", out.pass, out.detail)
		}
	})

	t.Run("no probe series", func(t *testing.T) {
		empty := `{"status":"success","data":{"resultType":"vector","result":[]}}`
		out := (&Verifier{}).checkMonitoringProbes(context.Background(), serverFor(t, empty, alertRulesDoc))
		if out.pass || !strings.Contains(out.detail, "not being scraped") {
			t.Errorf("expected a missing-series failure, got pass=%v detail=%q", out.pass, out.detail)
		}
	})

	t.Run("no rules loaded", func(t *testing.T) {
		noRules := `{"status":"success","data":{"groups":[]}}`
		out := (&Verifier{}).checkMonitoringProbes(context.Background(), serverFor(t, probesAllUp, noRules))
		if out.pass || !strings.Contains(out.detail, "no alert rules loaded") {
			t.Errorf("expected a missing-rules failure, got pass=%v detail=%q", out.pass, out.detail)
		}
	})
}
