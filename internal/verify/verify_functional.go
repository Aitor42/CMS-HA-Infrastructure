package verify

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// This file implements the verifier's functional phase. Phase 08 proves that the
// request path answers from the outside; this phase proves that the application
// behind it works: the WordPress features a visitor or an administrator touches
// (login form, feed, permalinks, REST API), every load balancer and every
// frontend on its own - not only whichever node the VIP happened to select -,
// the database read/write path, the PHP runtime the frontends run, and the
// monitoring stack's own probes.
//
// Every check is read-only against the lab except one SQL round-trip on a
// throwaway table that is dropped immediately, so the phase is safe to run
// against a live deployment.

const (
	// functionalTimeout bounds each HTTP probe of this phase. A converged lab
	// answers in milliseconds; the timeout only exists so that a black-holed
	// entry point cannot stall the whole verification.
	functionalTimeout = 30 * time.Second
	// functionalDBTimeout bounds the database round-trip, which runs through
	// SSH and `kubectl exec` and is therefore slower than a direct connection.
	functionalDBTimeout = 60 * time.Second
)

const (
	// phpPatchedPackage is the first Ubuntu Noble revision of the PHP 8.3
	// branch that carries the fix for CVE-2026-109618 and CVE-2026-109619
	// ("php2shell"). Ubuntu backports the upstream patch instead of bumping
	// the upstream version, so a patched node still answers "PHP 8.3.6": the
	// Debian revision is what tells a patched node from a vulnerable one.
	phpPatchedPackage = "8.3.6-0ubuntu0.24.04.13"

	// phpHardeningINI is the PHP SAPI configuration the frontends deploy with
	// the mitigations for those CVEs. The Apache SAPI reads it at startup, so
	// the setting is in place before the request is parsed.
	phpHardeningINI = "/etc/php/8.3/apache2/conf.d/99-cms-ha-php-hardening.ini"

	// phpHardeningApacheConf is the Apache configuration that carries the
	// mitigations the application cannot override. The path is the enabled
	// one, so a file that was deployed but never enabled does not pass.
	phpHardeningApacheConf = "/etc/apache2/conf-enabled/php-hardening.conf"
)

// requiredPHPMitigations are the mitigations every frontend has to carry on
// top of the patched package: the session path CVE-2026-109619 was confirmed
// with, and the out-of-memory bailout CVE-2026-109618 abuses.
var requiredPHPMitigations = []string{"upload_progress_off", "memory_limit_unlimited"}

// phpPatchedPackages are all the packages pinned to the patched revision. The
// Apache SAPI is the one that answers web requests, so checking only the CLI
// (which shares the same source package but is a separate binary package)
// would let a frontend serve a vulnerable mod_php while verify passed.
var phpPatchedPackages = []string{"php8.3-cli", "php8.3-common", "libapache2-mod-php8.3"}

// phpPatchProbe asks a frontend for the state that decides whether it is
// exposed to CVE-2026-109618 / CVE-2026-109619: the installed revision of
// every PHP package the role pins, whether Apache serves PHP through mod_php
// (the setup the PHP team rates as the highest risk) and whether the
// mitigations are in place. The version comparison is left to dpkg on the node
// itself, so the answer follows Debian's own version rules instead of a
// reimplementation of them.
var phpPatchProbe = `for pkg in ` + strings.Join(phpPatchedPackages, " ") + `; do ` +
	`ver=$(/usr/bin/dpkg-query -W -f='${Version}' "$pkg" 2>/dev/null); ` +
	`printf 'php_package=%s|%s\n' "$pkg" "$ver"; ` +
	`if [ -n "$ver" ] && /usr/bin/dpkg --compare-versions "$ver" ge "` + phpPatchedPackage + `"; then ` +
	`printf 'php_patched=%s|yes\n' "$pkg"; ` +
	`else printf 'php_patched=%s|no\n' "$pkg"; fi; ` +
	`done; ` +
	`if /usr/sbin/apache2ctl -M 2>/dev/null | /usr/bin/grep -q php_module; then ` +
	`printf 'php_mod_php=yes\n'; else printf 'php_mod_php=no\n'; fi; ` +
	`if /usr/bin/grep -Eqi '^[[:space:]]*session\.upload_progress\.enabled[[:space:]]*=[[:space:]]*(off|0)[[:space:]]*$' ` + phpHardeningINI + ` 2>/dev/null; then ` +
	`printf 'php_mitigation=upload_progress_off\n'; else printf 'php_mitigation=upload_progress_missing\n'; fi; ` +
	`if /usr/bin/grep -Eqi 'php_admin_value[[:space:]]+memory_limit[[:space:]]+-1' ` + phpHardeningApacheConf + ` 2>/dev/null; then ` +
	`printf 'php_mitigation=memory_limit_unlimited\n'; else printf 'php_mitigation=memory_limit_missing\n'; fi`

// noRedirectClient stops at the first response: the caller has to see the
// redirect itself, and a following client would chase the canonical vhost name,
// turning "plain HTTP hands over to HTTPS" into a DNS lookup of the lab domain.
var noRedirectClient = &http.Client{
	Timeout: functionalTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- the lab certificate is issued by the project's own CA
	},
}

// wpAssetPattern matches the first asset a page loads from WordPress core or the
// theme. The optional scheme and host are there because WordPress emits absolute
// URLs (content_url()) as often as root-relative ones, and fetching an asset the
// theme really references proves static file serving without guessing a file
// name that a WordPress release may rename.
var wpAssetPattern = regexp.MustCompile(`(?:src|href)=["'](?:https?://[^"']*?)?(/wp-(?:content|includes)/[^"']+)["']`)

// mariadbRoundTripSQL reads the application's own tables and proves the database
// accepts writes: `wp_user` is granted ALL on the wordpress schema, so the
// round-trip creates a throwaway table, writes a row, reads it back and drops
// the table again. The leading DROP IF EXISTS keeps a rerun idempotent after a
// failed attempt left the table behind.
const mariadbRoundTripSQL = "DROP TABLE IF EXISTS cms_verify_tmp; " +
	"CREATE TABLE cms_verify_tmp (id INT); " +
	"INSERT INTO cms_verify_tmp VALUES (1); " +
	"SELECT COUNT(*) FROM cms_verify_tmp; " +
	"SELECT COUNT(*) FROM wp_posts; " +
	"SELECT COUNT(*) FROM wp_options; " +
	"DROP TABLE cms_verify_tmp"

// cmsSite is the pair of entry points the CMS checks talk to: HTTPS through
// the load balancer and plain HTTP on the same address, which has to redirect.
// The unit tests build it from local test servers, so the request logic can be
// exercised without a lab.
type cmsSite struct {
	httpsBase string
	httpBase  string
	vhost     string
}

// site derives the CMS entry points from the configured VIP.
func (v *Verifier) site() cmsSite {
	vip := v.cfg.LBVIP()
	return cmsSite{
		httpsBase: "https://" + vip,
		httpBase:  "http://" + vip,
		vhost:     v.cfg.TrafficVHost(),
	}
}

// phase10 runs the functional checks of the deployed stack concurrently and
// aggregates them the way phase08 does: one phase, every sub-check reported in
// its detail so a failure names the feature that broke.
func (v *Verifier) phase10(ctx context.Context) CheckResult {
	const name = "Stack functional checks (CMS, database, probes)"

	vip := v.cfg.LBVIP()
	monitorIP := v.cfg.Nodes.Monitor.IP
	if vip == "" {
		return CheckResult{"Phase 10", name, false, "network.main.vip is not configured"}
	}
	if monitorIP == "" {
		return CheckResult{"Phase 10", name, false, "nodes.monitor.ip is not configured"}
	}

	// Ask every balancer on its own, excluding the VIP: it is covered through
	// the checks that go through the site's HTTPS base.
	var lbIPs []string
	for _, lb := range v.cfg.Nodes.LBs {
		if lb.IP != "" && lb.IP != vip {
			lbIPs = append(lbIPs, lb.IP)
		}
	}
	site := v.site()
	vhost := site.vhost
	promBase := "http://" + monitorIP + ":9090"

	checks := []func() e2eOutcome{
		func() e2eOutcome { return v.checkCMSPages(ctx, site) },
		func() e2eOutcome { return v.checkCMSREST(ctx, site) },
		func() e2eOutcome { return v.checkCMSFrontends(ctx) },
		func() e2eOutcome { return v.checkPHPSecurityPatch(ctx) },
		func() e2eOutcome { return v.checkLoadBalancers(ctx, vhost, lbIPs) },
		func() e2eOutcome { return v.checkMariaDB(ctx) },
		func() e2eOutcome { return v.checkBackupStore(ctx) },
		func() e2eOutcome { return v.checkMonitoringProbes(ctx, promBase) },
		func() e2eOutcome { return v.checkGrafanaProvisioning(ctx, monitorIP) },
	}

	outcomes := make([]e2eOutcome, len(checks))
	var wg sync.WaitGroup
	for i, chk := range checks {
		idx, f := i, chk
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[idx] = f()
		}()
	}
	wg.Wait()

	pass := true
	details := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		if !o.pass {
			pass = false
		}
		details = append(details, fmt.Sprintf("%s: %s", o.label, o.detail))
	}
	return CheckResult{"Phase 10", name, pass, strings.Join(details, ", ")}
}

// checkCMSPages exercises the pages a visitor or an administrator asks for: the
// login form, the feed, an asset the theme references and the plain-HTTP entry
// point.
func (v *Verifier) checkCMSPages(ctx context.Context, site cmsSite) e2eOutcome {
	o := e2eOutcome{label: "CMS pages"}
	vhost := site.vhost
	base := site.httpsBase
	var steps []string

	// The login form is rendered by PHP from the theme: an answer that is not
	// the form means PHP or the theme is broken even though the home page still
	// returns 200.
	status, body, err := httpGetWithClient(ctx, cmsHTTPClient, base+"/wp-login.php", vhost, functionalTimeout)
	if err != nil {
		o.detail = fmt.Sprintf("GET %s/wp-login.php failed: %v", base, err)
		return o
	}
	if status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s/wp-login.php returned HTTP %d", base, status)
		return o
	}
	if !loginFormPresent(body) {
		o.detail = "/wp-login.php answered but does not contain the credential fields"
		return o
	}
	steps = append(steps, "login form 200")

	// The feed is generated from the database, so it proves content is read from
	// MariaDB and rendered, not just that a static file is served.
	status, body, err = httpGetWithClient(ctx, cmsHTTPClient, base+"/feed/", vhost, functionalTimeout)
	if err != nil {
		o.detail = fmt.Sprintf("GET %s/feed/ failed: %v", base, err)
		return o
	}
	if status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s/feed/ returned HTTP %d", base, status)
		return o
	}
	if !strings.Contains(body, "<rss") && !strings.Contains(body, "<feed") {
		o.detail = "/feed/ did not return an RSS or Atom document"
		return o
	}
	steps = append(steps, "feed 200")

	// A static asset the home page references has to be served as well: a broken
	// asset path ruins the site long before the page stops returning 200.
	status, body, err = httpGetWithClient(ctx, cmsHTTPClient, base+"/", vhost, functionalTimeout)
	if err != nil {
		o.detail = fmt.Sprintf("GET %s/ failed: %v", base, err)
		return o
	}
	if status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s/ returned HTTP %d", base, status)
		return o
	}
	asset := firstAssetPath(body)
	if asset == "" {
		o.detail = "the home page references no /wp-content or /wp-includes asset"
		return o
	}
	status, _, err = httpGetWithClient(ctx, cmsHTTPClient, base+asset, vhost, functionalTimeout)
	if err != nil {
		o.detail = fmt.Sprintf("GET %s%s failed: %v", base, asset, err)
		return o
	}
	if status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s%s returned HTTP %d", base, asset, status)
		return o
	}
	steps = append(steps, "asset 200")

	// Plain HTTP has to hand the client over to HTTPS.
	status, location, err := httpGetLocation(ctx, noRedirectClient, site.httpBase+"/", vhost, functionalTimeout)
	if err != nil {
		o.detail = fmt.Sprintf("GET %s/ failed: %v", site.httpBase, err)
		return o
	}
	if status != http.StatusMovedPermanently && status != http.StatusFound {
		o.detail = fmt.Sprintf("GET %s/ returned HTTP %d, want a redirect to HTTPS", site.httpBase, status)
		return o
	}
	if !strings.HasPrefix(location, "https://") {
		o.detail = fmt.Sprintf("the HTTP entry point redirects to %q, not to HTTPS", location)
		return o
	}
	steps = append(steps, fmt.Sprintf("http->https %d", status))

	o.pass = true
	o.detail = strings.Join(steps, ", ")
	return o
}

// checkCMSREST exercises WordPress' REST API and, with the permalink it returns,
// the database-backed rendering of that content.
func (v *Verifier) checkCMSREST(ctx context.Context, site cmsSite) e2eOutcome {
	o := e2eOutcome{label: "CMS REST API"}
	vhost := site.vhost
	base := site.httpsBase

	// /wp-json/ is the API index and has to list the registered namespaces.
	status, body, err := httpGetWithClient(ctx, cmsHTTPClient, base+"/wp-json/", vhost, functionalTimeout)
	if err != nil {
		o.detail = fmt.Sprintf("GET %s/wp-json/ failed: %v", base, err)
		return o
	}
	if status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s/wp-json/ returned HTTP %d", base, status)
		return o
	}
	var index struct {
		Namespaces []string `json:"namespaces"`
	}
	if err := json.Unmarshal([]byte(body), &index); err != nil {
		o.detail = fmt.Sprintf("/wp-json/ did not return JSON: %v", err)
		return o
	}
	if len(index.Namespaces) == 0 {
		o.detail = "/wp-json/ lists no REST namespace"
		return o
	}

	// The post collection is served from MariaDB, and `wp core install` creates
	// the first post: an empty collection means the CMS reads a database that
	// the deployment never loaded.
	status, body, err = httpGetWithClient(ctx, cmsHTTPClient, base+"/wp-json/wp/v2/posts?per_page=1", vhost, functionalTimeout)
	if err != nil {
		o.detail = fmt.Sprintf("GET %s/wp-json/wp/v2/posts failed: %v", base, err)
		return o
	}
	if status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s/wp-json/wp/v2/posts returned HTTP %d", base, status)
		return o
	}
	var posts []struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal([]byte(body), &posts); err != nil {
		o.detail = fmt.Sprintf("/wp-json/wp/v2/posts did not return a JSON array: %v", err)
		return o
	}
	if len(posts) == 0 {
		o.detail = "the REST API reports no posts: the CMS database holds no application data"
		return o
	}

	// Rendering the permalink of that post closes the loop: the API read it from
	// MariaDB, and the frontend renders it as HTML. Only the path is kept so the
	// request goes through the VIP under test and not through whatever host the
	// payload advertises.
	permalink := permalinkPath(posts[0].Link)
	if permalink == "" {
		o.detail = fmt.Sprintf("the post payload carries no usable permalink (%q)", posts[0].Link)
		return o
	}
	status, body, err = httpGetWithClient(ctx, cmsHTTPClient, base+permalink, vhost, functionalTimeout)
	if err != nil {
		o.detail = fmt.Sprintf("GET %s%s failed: %v", base, permalink, err)
		return o
	}
	if status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s%s returned HTTP %d", base, permalink, status)
		return o
	}
	if !strings.Contains(strings.ToLower(body), "wp-") {
		o.detail = fmt.Sprintf("GET %s%s answered without WordPress markup", base, permalink)
		return o
	}

	o.pass = true
	o.detail = fmt.Sprintf("%d namespace(s), %d post(s), permalink %s 200", len(index.Namespaces), len(posts), permalink)
	return o
}

// checkCMSFrontends asks each frontend's own WordPress, so a broken second
// frontend cannot hide behind a healthy first one the way it would through the
// VIP. It also checks that the uploads directory really is the NFS export: a
// failed nfs mount is configured with nofail, so the site keeps working while
// uploads silently land on a single node's disk.
func (v *Verifier) checkCMSFrontends(ctx context.Context) e2eOutcome {
	o := e2eOutcome{label: "CMS frontends"}
	var ips []string
	for _, cms := range v.cfg.Nodes.CMSFrontends {
		if cms.IP != "" {
			ips = append(ips, cms.IP)
		}
	}
	if len(ips) == 0 {
		o.detail = "no CMS frontend configured"
		return o
	}

	// wp-cli talks to the database through the same NodePort the web server
	// uses, so `wp core is-installed` plus a post count proves that node's whole
	// application path. Both values are labelled, and the whole chain merges its
	// stderr into stdout, so a PHP notice cannot be mistaken for an answer and a
	// failure still reports the message that caused it.
	cmd := "{ /usr/local/bin/wp core is-installed --allow-root --path=/var/www/html && " +
		"posts=$(/usr/local/bin/wp post list --format=count --allow-root --path=/var/www/html) && " +
		"printf 'posts=%s\\n' \"$posts\" && " +
		"fstype=$(findmnt -n -o FSTYPE /var/www/html/wp-content/uploads) && " +
		"printf 'uploads_fstype=%s\\n' \"$fstype\" ; } 2>&1"

	res := v.ssh.RunParallel(ctx, ips, cmd)
	pass := true
	var details []string
	for _, r := range res {
		if r.Err != nil || r.ExitCode != 0 {
			pass = false
			details = append(details, fmt.Sprintf("%s: wp-cli probe failed (exit %d) %s", r.Host, r.ExitCode, strings.TrimSpace(r.Output)))
			continue
		}
		posts, fstype, err := parseFrontendProbe(r.Output)
		if err != nil {
			pass = false
			details = append(details, fmt.Sprintf("%s: %v", r.Host, err))
			continue
		}
		if posts < 1 {
			pass = false
			details = append(details, fmt.Sprintf("%s: WordPress reports no posts", r.Host))
			continue
		}
		if fstype != "nfs" && fstype != "nfs4" {
			pass = false
			details = append(details, fmt.Sprintf("%s: uploads is not on NFS (fstype %q)", r.Host, fstype))
			continue
		}
		details = append(details, fmt.Sprintf("%s: %d post(s), uploads on %s", r.Host, posts, fstype))
	}
	o.pass = pass
	o.detail = strings.Join(details, "; ")
	return o
}

// phpPackageState is the installed revision of one PHP binary package.
