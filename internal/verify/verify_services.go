package verify

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/httpcheck"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/prom"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/shell"
)

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
		shell.Quote(v.cfg.Database.Password), shell.Quote(user), shell.Quote(dbName), shell.Quote(mariadbRoundTripSQL))

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
	master, ok := v.cfg.FirstMaster()
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

	status, body, err := httpcheck.Get(ctx, base+"/api/v1/rules", "", functionalTimeout, false)
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
	status, body, err = httpcheck.Get(ctx, base+"/api/v1/query?"+query.Encode(), "", functionalTimeout, false)
	if err != nil || status != http.StatusOK {
		o.detail = fmt.Sprintf("query probe_success failed (HTTP %d): %v", status, err)
		return o
	}
	values, err := prom.Values(body)
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
		samples, err := prom.Series(ctx, base, series.name)
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

// checkGrafanaProvisioning proves the Grafana an operator opens really shows
// the dashboards, not just that the files describing them exist. Phase 08 only
// asks /api/health, which is happy with an instance that has no datasource and
// no dashboard, and a file on disk says nothing about whether Grafana loaded it
// or whether its panels point at a datasource that exists.
//
// The API is queried with the deployment's admin credential, taken from
// monitoring.grafana_admin_password or, when that is empty, from the
// root:grafana env file the monitoring phase generated: the datasource must be
// provisioned and default, every shipped dashboard must be loaded with its
// panels pointing at that datasource, and the HA overview must be the home
// dashboard so the first page in the browser shows graphs instead of Grafana's
// welcome screen.
