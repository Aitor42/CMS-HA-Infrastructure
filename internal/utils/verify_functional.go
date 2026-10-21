package utils

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/dashboards"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
)

// This file implements the verifier's functional phase. Phase 08 proves that the
// request path answers from the outside; this phase proves that the application
// behind it works: the WordPress features a visitor or an administrator touches
// (login form, feed, permalinks, REST API), every load balancer and every
// frontend on its own - not only whichever node the VIP happened to select -,
// the database read/write path, and the monitoring stack's own probes.
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

// checkLoadBalancers asks every load balancer for the site directly. The VIP
// only ever resolves to the active node, so without this a broken backup
// balancer would stay hidden until a failover happened in the middle of an
// incident. The VIP's own HTTPS answer is already covered by phase08.
func (v *Verifier) checkLoadBalancers(ctx context.Context, vhost string, ips []string) e2eOutcome {
	o := e2eOutcome{label: "Load balancers"}
	if len(ips) == 0 {
		o.detail = "no load balancer configured"
		return o
	}

	// Each goroutine writes its own index of both slices, so no lock is needed.
	oks := make([]bool, len(ips))
	msg := make([]string, len(ips))
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func(idx int, ip string) {
			defer wg.Done()
			status, body, err := httpGetWithClient(ctx, cmsHTTPClient, "https://"+ip+"/", vhost, functionalTimeout)
			switch {
			case err != nil:
				msg[idx] = fmt.Sprintf("%s: %v", ip, err)
			case status != http.StatusOK:
				msg[idx] = fmt.Sprintf("%s: HTTP %d", ip, status)
			case !strings.Contains(strings.ToLower(body), "wp-"):
				msg[idx] = fmt.Sprintf("%s: HTTP 200 without WordPress markup", ip)
			default:
				oks[idx] = true
				msg[idx] = fmt.Sprintf("%s: HTTP 200", ip)
			}
		}(i, ip)
	}
	wg.Wait()

	o.pass = true
	for i := range ips {
		if !oks[i] {
			o.pass = false
		}
	}
	o.detail = strings.Join(msg, ", ")
	return o
}

// checkMariaDB proves the database accepts reads and writes. The NodePort has to
// be reachable from the orchestrator (the frontends use the same path) and the
// mysql client only exists inside the pod, so the SQL runs there through
// kubectl, with MYSQL_PWD keeping the password out of the process list.
func (v *Verifier) checkMariaDB(ctx context.Context) e2eOutcome {
	o := e2eOutcome{label: "MariaDB"}
	if len(v.cfg.Nodes.Masters) == 0 || v.cfg.Nodes.Masters[0].IP == "" {
		o.detail = "no master node configured"
		return o
	}
	master := v.cfg.Nodes.Masters[0].IP

	user := v.cfg.Database.User
	if user == "" {
		user = "wp_user"
	}
	dbName := v.cfg.Database.Name
	if dbName == "" {
		dbName = "wordpress"
	}
	port := v.cfg.Database.Port
	if port == 0 {
		port = 30306
	}
	if v.cfg.Database.Password == "" {
		o.detail = "database.password is required for the functional database check"
		return o
	}

	if err := dialTCP(ctx, master, port, 10*time.Second); err != nil {
		o.detail = fmt.Sprintf("database NodePort %s is unreachable: %v", net.JoinHostPort(master, strconv.Itoa(port)), err)
		return o
	}

	script := fmt.Sprintf(
		"POD=$(kubectl get pod -n cms -l app=mariadb,role=database -o jsonpath='{.items[0].metadata.name}') && "+
			"kubectl exec -n cms \"$POD\" -- env MYSQL_PWD=%s mysql -h 127.0.0.1 -P 3306 -u %s %s -N -B -e %s",
		shellQuote(v.cfg.Database.Password), shellQuote(user), shellQuote(dbName), shellQuote(mariadbRoundTripSQL))

	// The API server is unreachable for a few seconds after a k3s restart, and
	// on the small CI guests k3s is the first process to go when the runner is
	// starved: kubectl then reports "The connection to the server
	// 127.0.0.1:6443 was refused" and the whole verification fails for a blip
	// the cluster heals on its own. Retry within a bounded budget (the phase 04
	// check does the same for the same reason); a database that is really
	// broken fails every attempt. The SQL drops its throwaway table first, so
	// running it again is safe.
	var (
		out      string
		errOut   string
		exitCode int
		lastErr  string
	)
	retryErr := retry.DoContext(ctx, retry.Config{MaxAttempts: 10, Interval: 5 * time.Second, MaxInterval: 15 * time.Second, Timeout: 120 * time.Second}, func(attemptCtx context.Context) error {
		attemptCtx, attemptCancel := context.WithTimeout(attemptCtx, functionalDBTimeout)
		defer attemptCancel()
		var runErr error
		out, errOut, exitCode, runErr = v.ssh.RunCommand(attemptCtx, master, script)
		if runErr == nil && exitCode == 0 {
			return nil
		}
		lastErr = fmt.Sprintf("SQL round-trip failed (exit %d): %v %s", exitCode, runErr, strings.TrimSpace(errOut))
		return fmt.Errorf("%s", lastErr)
	})
	if retryErr != nil {
		o.detail = lastErr
		return o
	}
	writes, posts, options, parseErr := parseMariadbRoundTrip(out)
	if parseErr != nil {
		o.detail = fmt.Sprintf("SQL round-trip returned unreadable output: %v", parseErr)
		return o
	}
	if writes != 1 {
		o.detail = fmt.Sprintf("the write round-trip read back %d rows, want 1", writes)
		return o
	}
	if posts < 1 {
		o.detail = "the wordpress schema holds no posts: MariaDB does not carry the application's data"
		return o
	}
	if options < 1 {
		o.detail = "the wordpress schema holds no wp_options rows"
		return o
	}

	o.pass = true
	o.detail = fmt.Sprintf("NodePort %d open, wp_posts=%d, wp_options=%d, write round-trip ok", port, posts, options)
	return o
}

// checkBackupStore proves the off-DRBD S3 store is reachable from the cluster,
// not just from the hypervisor. The backup and restore CronJobs run inside the
// cluster, so a Garage instance that only answers on the host itself passes the
// backup phase's health check while every backup fails with "connection
// refused" (which is exactly what the first run of the restore drill caught).
func (v *Verifier) checkBackupStore(ctx context.Context) e2eOutcome {
	o := e2eOutcome{label: "Backup store"}
	if !v.cfg.BackupEnabled() {
		o.pass = true
		o.detail = "disabled"
		return o
	}
	master, ok := firstMaster(v.cfg)
	if !ok {
		o.detail = "no master node with an address is configured"
		return o
	}
	endpoint := v.cfg.BackupS3Endpoint()
	// Garage answers an anonymous request with 403 Forbidden: reaching the S3
	// API (and getting an answer from it) is the proof we want.
	cmd := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' -m 5 %s/", endpoint)
	out, errOut, code, err := v.ssh.RunCommand(ctx, master.IP, cmd)
	if err != nil || code != 0 {
		o.detail = fmt.Sprintf("%s cannot reach %s (exit %d): %v %s", master.Name, endpoint, code, err, strings.TrimSpace(errOut))
		return o
	}
	if status := strings.TrimSpace(out); status != "403" {
		o.detail = fmt.Sprintf("%s got HTTP %q from %s (expected 403 from the S3 API)", master.Name, status, endpoint)
		return o
	}
	o.pass = true
	o.detail = fmt.Sprintf("%s reaches the S3 API at %s", master.Name, endpoint)
	return o
}

// checkMonitoringProbes asks Prometheus what it thinks of the lab: the alert
// rules must be loaded and every blackbox probe must be up. The probes are an
// independent end-to-end view of the same VIP and frontends, evaluated by the
// monitoring stack itself.
func (v *Verifier) checkMonitoringProbes(ctx context.Context, base string) e2eOutcome {
	o := e2eOutcome{label: "Prometheus probes"}

	status, body, err := httpGet(ctx, base+"/api/v1/rules", "", functionalTimeout, false)
	if err != nil || status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s/api/v1/rules failed (HTTP %d): %v", base, status, err)
		return o
	}
	groups, rules, err := countAlertRules(body)
	if err != nil {
		o.detail = fmt.Sprintf("the rules payload is unreadable: %v", err)
		return o
	}
	if rules == 0 {
		o.detail = "Prometheus has no alert rules loaded"
		return o
	}

	query := url.Values{"query": {"probe_success"}}
	status, body, err = httpGet(ctx, base+"/api/v1/query?"+query.Encode(), "", functionalTimeout, false)
	if err != nil || status != http.StatusOK {
		o.detail = fmt.Sprintf("query probe_success failed (HTTP %d): %v", status, err)
		return o
	}
	values, err := prometheusValues(body)
	if err != nil {
		o.detail = fmt.Sprintf("the probe_success payload is unreadable: %v", err)
		return o
	}
	if len(values) == 0 {
		o.detail = "Prometheus has no probe_success series: blackbox_exporter is not being scraped"
		return o
	}
	up := 0
	for _, val := range values {
		if val == 1 {
			up++
		}
	}
	if up != len(values) {
		o.detail = fmt.Sprintf("%d of %d blackbox probes are failing", len(values)-up, len(values))
		return o
	}

	// The overview panels and the HA alerts do not read anything blackbox
	// exports: they read textfile series published by the nodes themselves
	// (DRBD on the masters, keepalived on the balancers). A publisher that is
	// not running leaves those panels blank without failing a single probe, so
	// ask Prometheus for the series by name.
	for _, series := range []struct {
		name   string
		source string
	}{
		{"cms_drbd_connected", "the DRBD textfile publisher on internal-master1/2"},
		{"cms_keepalived_vip_present", "the keepalived textfile publisher on main-lb1/2"},
	} {
		samples, err := prometheusSeries(ctx, base, series.name)
		if err != nil {
			o.detail = fmt.Sprintf("%s: %v", series.name, err)
			return o
		}
		if len(samples) == 0 {
			o.detail = fmt.Sprintf("Prometheus has no %s series: %s is not feeding node_exporter's textfile collector", series.name, series.source)
			return o
		}
	}

	o.pass = true
	o.detail = fmt.Sprintf("%d rule(s) in %d group(s), %d/%d blackbox probes up, DRBD and keepalived metrics present", rules, groups, up, len(values))
	return o
}

// prometheusSeries evaluates an instant query and returns its samples. It is
// used for "this series must exist" checks, where an empty result is a failure
// with a different cause than a transport error.
func prometheusSeries(ctx context.Context, base, expr string) ([]float64, error) {
	query := url.Values{"query": {expr}}
	status, body, err := httpGet(ctx, base+"/api/v1/query?"+query.Encode(), "", functionalTimeout, false)
	if err != nil {
		return nil, fmt.Errorf("query %s failed: %w", expr, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("query %s failed (HTTP %d)", expr, status)
	}
	return prometheusValues(body)
}

// checkGrafanaProvisioning proves the Grafana an operator opens really shows
// the dashboards, not just that the files describing them exist. Phase 08 only
// asks /api/health, which is happy with an instance that has no datasource and
// no dashboard, and a file on disk says nothing about whether Grafana loaded it
// or whether its panels point at a datasource that exists.
//
// The API is queried with the lab's admin credentials (admin/admin, see
// MANUAL): the datasource must be provisioned and default, every shipped
// dashboard must be loaded with its panels pointing at that datasource, and the
// HA overview must be the home dashboard so the first page in the browser shows
// graphs instead of Grafana's welcome screen.
func (v *Verifier) checkGrafanaProvisioning(ctx context.Context, monitorIP string) e2eOutcome {
	o := e2eOutcome{label: "Grafana"}

	// Files first: a dashboard missing from the monitor node is a Puppet
	// problem, and this reports it without needing the API.
	cmd := "test -s /etc/grafana/provisioning/datasources/prometheus.yaml && " +
		"test -d /etc/grafana/provisioning/dashboards && " +
		"ls -1 /var/lib/grafana/dashboards/*.json | wc -l"

	out, errOut, exitCode, err := v.ssh.RunCommand(ctx, monitorIP, cmd)
	if err != nil || exitCode != 0 {
		o.detail = fmt.Sprintf("provisioning check failed (exit %d): %v %s", exitCode, err, strings.TrimSpace(errOut))
		return o
	}
	onDisk, convErr := strconv.Atoi(strings.TrimSpace(out))
	if convErr != nil {
		o.detail = fmt.Sprintf("unreadable dashboard count %q", strings.TrimSpace(out))
		return o
	}
	if onDisk < 1 {
		o.detail = "no provisioned Grafana dashboard found"
		return o
	}

	shipped, err := dashboards.Load()
	if err != nil {
		o.detail = fmt.Sprintf("the shipped dashboards are unreadable: %v", err)
		return o
	}
	if onDisk != len(shipped) {
		o.detail = fmt.Sprintf("the monitor has %d dashboard file(s) but %d are shipped: a stale file is being loaded as well", onDisk, len(shipped))
		return o
	}

	summary, err := v.inspectGrafana(ctx, monitorIP, shipped)
	if err != nil {
		o.detail = err.Error()
		return o
	}

	o.pass = true
	o.detail = summary
	return o
}

// inspectGrafana walks the Grafana API and returns the one-line summary the
// phase prints. Every failure names the dashboard and panel at fault.
func (v *Verifier) inspectGrafana(ctx context.Context, monitorIP string, shipped []dashboards.Dashboard) (string, error) {
	status, body, err := grafanaGET(ctx, monitorIP, "/api/health")
	if err != nil || status != http.StatusOK {
		return "", fmt.Errorf("grafana health check failed (HTTP %d): %v", status, err)
	}
	var health struct {
		Database string `json:"database"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return "", fmt.Errorf("unreadable Grafana health payload: %w", err)
	}
	if health.Database != "ok" {
		return "", fmt.Errorf("grafana reports database %q", health.Database)
	}

	// The datasource must exist with the UID the panels reference and be the
	// default one; otherwise Grafana answers "Datasource not found".
	status, body, err = grafanaGET(ctx, monitorIP, "/api/datasources")
	if err != nil {
		return "", fmt.Errorf("listing Grafana datasources failed: %w", err)
	}
	if status == http.StatusUnauthorized {
		return "", fmt.Errorf("grafana rejected the lab's admin credentials (admin/admin): the dashboards cannot be inspected")
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("listing Grafana datasources failed (HTTP %d): %s", status, strings.TrimSpace(string(body)))
	}
	var datasources []struct {
		UID       string `json:"uid"`
		Name      string `json:"name"`
		Type      string `json:"type"`
		IsDefault bool   `json:"isDefault"`
	}
	if err := json.Unmarshal(body, &datasources); err != nil {
		return "", fmt.Errorf("unreadable Grafana datasource payload: %w", err)
	}
	datasource := ""
	for _, ds := range datasources {
		if ds.UID == dashboards.DatasourceUID {
			if ds.Type != "prometheus" {
				return "", fmt.Errorf("datasource %q is type %q, not prometheus", ds.UID, ds.Type)
			}
			if !ds.IsDefault {
				return "", fmt.Errorf("datasource %q is not the default: new panels would open without data", ds.UID)
			}
			datasource = ds.Name + " (" + ds.UID + ")"
		}
	}
	if datasource == "" {
		return "", fmt.Errorf("datasource %q is not provisioned: every dashboard panel would show \"Datasource not found\"", dashboards.DatasourceUID)
	}

	// Grafana must have loaded every shipped dashboard.
	status, body, err = grafanaGET(ctx, monitorIP, "/api/search?type=dash-db")
	if err != nil || status != http.StatusOK {
		return "", fmt.Errorf("listing Grafana dashboards failed (HTTP %d): %v", status, err)
	}
	var loaded []struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(body, &loaded); err != nil {
		return "", fmt.Errorf("unreadable Grafana dashboard payload: %w", err)
	}
	loadedUIDs := make(map[string]bool, len(loaded))
	for _, d := range loaded {
		loadedUIDs[d.UID] = true
	}

	queries := 0
	for _, d := range shipped {
		if !loadedUIDs[d.UID] {
			return "", fmt.Errorf("dashboard %q (%s) is not loaded by Grafana", d.Title, d.UID)
		}

		status, body, err := grafanaGET(ctx, monitorIP, "/api/dashboards/uid/"+url.PathEscape(d.UID))
		if err != nil || status != http.StatusOK {
			return "", fmt.Errorf("reading dashboard %q failed (HTTP %d): %v", d.Title, status, err)
		}
		var payload struct {
			Dashboard dashboards.Dashboard `json:"dashboard"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", fmt.Errorf("unreadable dashboard %q payload: %w", d.Title, err)
		}

		// Compare with the shipped copy: a dashboard edited through the UI is
		// stored in Grafana's database and would silently override the file.
		want := make(map[string]string, len(d.Panels))
		for _, p := range d.Panels {
			want[p.Title] = panelQueries(p)
		}
		if len(payload.Dashboard.Panels) != len(d.Panels) {
			return "", fmt.Errorf("dashboard %q exposes %d panel(s) but %d are shipped", d.Title, len(payload.Dashboard.Panels), len(d.Panels))
		}
		for _, p := range payload.Dashboard.Panels {
			if p.Datasource.UID != dashboards.DatasourceUID {
				return "", fmt.Errorf("dashboard %q panel %q points at datasource %q instead of %q", d.Title, p.Title, p.Datasource.UID, dashboards.DatasourceUID)
			}
			if len(p.Targets) == 0 {
				return "", fmt.Errorf("dashboard %q panel %q has no query", d.Title, p.Title)
			}
			for _, t := range p.Targets {
				if strings.TrimSpace(t.Expr) == "" {
					return "", fmt.Errorf("dashboard %q panel %q has an empty query", d.Title, p.Title)
				}
				queries++
			}
			if got, ok := want[p.Title]; !ok {
				return "", fmt.Errorf("dashboard %q panel %q was not shipped (edited in the UI?)", d.Title, p.Title)
			} else if got != panelQueries(p) {
				return "", fmt.Errorf("dashboard %q panel %q no longer matches the shipped query (%q)", d.Title, p.Title, panelQueries(p))
			}
		}
	}

	// First open in the browser: the HA overview, not the welcome page.
	home, err := v.readHomeDashboard(ctx, monitorIP)
	if err != nil {
		return "", err
	}
	if err := checkHomeDashboard(home, shipped); err != nil {
		return "", err
	}

	return fmt.Sprintf("datasource %s, %d dashboard(s) loaded, home = %s, %d panel queries",
		datasource, len(shipped), home.Title, queries), nil
}

// readHomeDashboard asks Grafana which page `/` opens on. The endpoint answers
// with the dashboard inline (`{"dashboard": {...}}`) when it serves a static
// home file, and with a redirect (`{"redirectUri": "/d/<uid>/<slug>"}`) when the
// home dashboard is a saved dashboard (the shape Grafana 13 returns). Both are
// resolved to the dashboard they point at, so the check asks the same question
// whichever shape the installed Grafana uses.
func (v *Verifier) readHomeDashboard(ctx context.Context, monitorIP string) (dashboards.Dashboard, error) {
	status, body, err := grafanaGET(ctx, monitorIP, "/api/dashboards/home")
	if err != nil || status != http.StatusOK {
		return dashboards.Dashboard{}, fmt.Errorf("reading Grafana's home dashboard failed (HTTP %d): %v", status, err)
	}

	var home struct {
		RedirectURI string               `json:"redirectUri"`
		Dashboard   dashboards.Dashboard `json:"dashboard"`
	}
	if err := json.Unmarshal(body, &home); err != nil {
		return dashboards.Dashboard{}, fmt.Errorf("unreadable Grafana home dashboard payload: %w", err)
	}
	if home.RedirectURI == "" {
		return home.Dashboard, nil
	}

	uid := dashboardUIDFromURL(home.RedirectURI)
	if uid == "" {
		return dashboards.Dashboard{}, fmt.Errorf("grafana's home dashboard redirect %q is not a /d/<uid>/<slug> URL", home.RedirectURI)
	}
	status, body, err = grafanaGET(ctx, monitorIP, "/api/dashboards/uid/"+url.PathEscape(uid))
	if err != nil || status != http.StatusOK {
		return dashboards.Dashboard{}, fmt.Errorf("resolving the home dashboard %q (uid %s) failed (HTTP %d): %v", home.RedirectURI, uid, status, err)
	}
	var payload struct {
		Dashboard dashboards.Dashboard `json:"dashboard"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return dashboards.Dashboard{}, fmt.Errorf("unreadable home dashboard %q payload: %w", uid, err)
	}
	return payload.Dashboard, nil
}

// dashboardUIDFromURL extracts the uid from a Grafana dashboard URL
// (/d/<uid>/<slug>).
func dashboardUIDFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "d" {
		return parts[1]
	}
	return ""
}

// checkHomeDashboard asserts that the page Grafana serves at `/` is the HA
// overview with the shipped panels, not the built-in welcome dashboard.
func checkHomeDashboard(home dashboards.Dashboard, shipped []dashboards.Dashboard) error {
	want, ok := dashboards.Home(shipped)
	if !ok {
		return fmt.Errorf("the shipped dashboards do not include the home dashboard %q", dashboards.HomeUID)
	}
	if home.Title != want.Title {
		return fmt.Errorf("grafana opens on %q instead of %q: set [dashboards] default_home_dashboard_path", home.Title, want.Title)
	}
	if len(home.Panels) != len(want.Panels) {
		return fmt.Errorf("the home dashboard %q exposes %d panel(s) but %d ship with it", home.Title, len(home.Panels), len(want.Panels))
	}

	wantQueries := make(map[string]string, len(want.Panels))
	for _, p := range want.Panels {
		wantQueries[p.Title] = panelQueries(p)
	}
	for _, p := range home.Panels {
		if got, ok := wantQueries[p.Title]; !ok || got != panelQueries(p) {
			return fmt.Errorf("the home dashboard %q panel %q does not match the shipped dashboard", home.Title, p.Title)
		}
	}
	return nil
}

// panelQueries renders a panel's queries for comparison with the shipped file.
func panelQueries(p dashboards.Panel) string {
	exprs := make([]string, 0, len(p.Targets))
	for _, t := range p.Targets {
		exprs = append(exprs, strings.TrimSpace(t.Expr))
	}
	return strings.Join(exprs, "\n")
}

// grafanaGET performs an authenticated GET against the monitor's Grafana.
func grafanaGET(ctx context.Context, monitorIP, path string) (int, []byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, functionalTimeout)
	defer cancel()

	base := "http://" + net.JoinHostPort(monitorIP, "3000")
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth("admin", "admin")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// httpGetLocation performs a GET and returns the response's status and Location
// header, which the redirect check needs. The body is read and discarded so the
// connection stays reusable.
func httpGetLocation(ctx context.Context, client *http.Client, rawURL, hostHeader string, timeout time.Duration) (int, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, "", err
	}
	if hostHeader != "" {
		req.Host = hostHeader
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 256<<10)); err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, resp.Header.Get("Location"), nil
}

// dialTCP verifies that host:port accepts a TCP connection within timeout.
func dialTCP(ctx context.Context, host string, port int, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}

// shellQuote single-quotes s for a POSIX shell, escaping embedded quotes. The
// database password reaches mysql through MYSQL_PWD, but it still has to survive
// the shell that launches kubectl.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// firstAssetPath returns the path of the first WordPress asset a page loads.
func firstAssetPath(page string) string {
	m := wpAssetPattern.FindStringSubmatch(page)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// loginFormPresent reports whether the response is WordPress' login form: the
// username and password fields are what the authentication path posts to.
func loginFormPresent(body string) bool {
	return strings.Contains(body, `name="log"`) && strings.Contains(body, `name="pwd"`)
}

// permalinkPath returns the request path of a permalink, so it can be fetched
// through the entry point under test regardless of the host the payload
// advertises. Plain permalinks (/?p=1) are handled too, because the query is
// part of the request then.
func permalinkPath(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}
	uri := u.RequestURI()
	if uri == "" || uri == "/" {
		return ""
	}
	return uri
}

// parseMariadbRoundTrip reads the three counts the SQL round-trip prints, in the
// order the SELECTs run: the rows written to the throwaway table, the posts and
// the options rows of the WordPress schema.
func parseMariadbRoundTrip(out string) (writes, posts, options int, err error) {
	var numbers []int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, convErr := strconv.Atoi(line)
		if convErr != nil {
			return 0, 0, 0, fmt.Errorf("unexpected output line %q", line)
		}
		numbers = append(numbers, n)
	}
	if len(numbers) < 3 {
		return 0, 0, 0, fmt.Errorf("expected three counts, got %d (%v)", len(numbers), numbers)
	}
	return numbers[0], numbers[1], numbers[2], nil
}

// parseFrontendProbe reads the two labelled values the frontend probe prints:
// the post count from wp-cli and the filesystem type of the uploads mount.
// Labelling keeps a PHP notice or a warning on the same stream from being
// misread as one of them.
func parseFrontendProbe(out string) (posts int, fstype string, err error) {
	postsSeen := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "posts="):
			n, convErr := strconv.Atoi(strings.TrimPrefix(line, "posts="))
			if convErr != nil {
				return 0, "", fmt.Errorf("unreadable post count in %q", line)
			}
			posts, postsSeen = n, true
		case strings.HasPrefix(line, "uploads_fstype="):
			fstype = strings.TrimPrefix(line, "uploads_fstype=")
		}
	}
	if !postsSeen {
		return 0, "", fmt.Errorf("no post count in the probe output %q", strings.TrimSpace(out))
	}
	if fstype == "" {
		return 0, "", fmt.Errorf("no filesystem type in the probe output %q", strings.TrimSpace(out))
	}
	return posts, fstype, nil
}

// countAlertRules reads the rule groups and rules Prometheus reports for
// /api/v1/rules.
func countAlertRules(body string) (groups, rules int, err error) {
	var res struct {
		Status string `json:"status"`
		Data   struct {
			Groups []struct {
				Rules []json.RawMessage `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return 0, 0, err
	}
	if res.Status != "success" {
		return 0, 0, fmt.Errorf("status %q", res.Status)
	}
	for _, group := range res.Data.Groups {
		groups++
		rules += len(group.Rules)
	}
	return groups, rules, nil
}

// prometheusValues extracts every sample value of a Prometheus query result.
// phase08 only needs the aggregate count of a query, but a functional check has
// to look at each series, because every blackbox probe has to be up.
func prometheusValues(body string) ([]float64, error) {
	var res struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return nil, err
	}
	if res.Status != "success" {
		return nil, fmt.Errorf("status %q", res.Status)
	}
	values := make([]float64, 0, len(res.Data.Result))
	for _, sample := range res.Data.Result {
		if len(sample.Value) < 2 {
			return nil, fmt.Errorf("sample %v carries no value", sample.Value)
		}
		raw, ok := sample.Value[1].(string)
		if !ok {
			return nil, fmt.Errorf("sample value %v is not a string", sample.Value[1])
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("sample value %q: %w", raw, err)
		}
		values = append(values, f)
	}
	return values, nil
}
