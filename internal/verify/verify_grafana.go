package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/dashboards"
)

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

// grafanaAdminPassword returns the configured Grafana admin password, or reads
// the per-deployment secret the monitoring phase wrote when the config leaves
// it empty. The file is root:grafana 0640, so the read goes through the
// verifier's existing sudo path.
func (v *Verifier) grafanaAdminPassword(ctx context.Context, monitorIP string) (string, error) {
	if pw := strings.TrimSpace(v.cfg.Monitoring.GrafanaAdminPassword); pw != "" {
		return pw, nil
	}
	out, stderr, code, err := v.ssh.RunCommand(ctx, monitorIP,
		"sudo sed -n 's/^GF_SECURITY_ADMIN_PASSWORD=//p' /etc/grafana/grafana-admin.env")
	if err != nil || code != 0 {
		return "", fmt.Errorf("read Grafana admin credential (exit %d): %v %s", code, err, strings.TrimSpace(stderr))
	}
	pw := strings.TrimSpace(out)
	if pw == "" {
		return "", fmt.Errorf("grafana admin credential is empty: set monitoring.grafana_admin_password or run the monitoring phase")
	}
	return pw, nil
}

// inspectGrafana walks the Grafana API and returns the one-line summary the
// phase prints. Every failure names the dashboard and panel at fault.
func (v *Verifier) inspectGrafana(ctx context.Context, monitorIP string, shipped []dashboards.Dashboard) (string, error) {
	password, err := v.grafanaAdminPassword(ctx, monitorIP)
	if err != nil {
		return "", err
	}
	status, body, err := grafanaGET(ctx, monitorIP, "/api/health", password)
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
	status, body, err = grafanaGET(ctx, monitorIP, "/api/datasources", password)
	if err != nil {
		return "", fmt.Errorf("listing Grafana datasources failed: %w", err)
	}
	if status == http.StatusUnauthorized {
		return "", fmt.Errorf("grafana rejected the deployment's admin credentials: the dashboards cannot be inspected")
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
	status, body, err = grafanaGET(ctx, monitorIP, "/api/search?type=dash-db", password)
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

		status, body, err := grafanaGET(ctx, monitorIP, "/api/dashboards/uid/"+url.PathEscape(d.UID), password)
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
	home, err := v.readHomeDashboard(ctx, monitorIP, password)
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
func (v *Verifier) readHomeDashboard(ctx context.Context, monitorIP, password string) (dashboards.Dashboard, error) {
	status, body, err := grafanaGET(ctx, monitorIP, "/api/dashboards/home", password)
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
	status, body, err = grafanaGET(ctx, monitorIP, "/api/dashboards/uid/"+url.PathEscape(uid), password)
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
func grafanaGET(ctx context.Context, monitorIP, path, password string) (int, []byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, functionalTimeout)
	defer cancel()

	base := "http://" + net.JoinHostPort(monitorIP, "3000")
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth("admin", password)

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
