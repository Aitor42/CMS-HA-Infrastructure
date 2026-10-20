// Package dashboards owns the Grafana dashboards the monitoring role ships and
// the checks that prove them usable: the same JSON files Puppet copies to the
// monitor node are embedded here, so the wording of a panel query can be
// asserted against what Prometheus actually stores instead of being trusted.
//
// Two classes of mistake motivated this package and are covered by its tests:
//
//   - A panel that queries a series nobody exports (k6 trend metrics are gauges
//     named k6_*_p95, not histogram _sum/_count pairs) renders "No data" the
//     first time the dashboard is opened, which looks like a broken stack.
//   - A Grafana datasource UID in the dashboard that does not match the UID the
//     provisioning file creates leaves every panel with "Datasource not found".
package dashboards

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
)

// DatasourceUID is the Prometheus datasource created by the provisioning file
// (puppet/modules/role/files/grafana/datasource.yaml). Every panel must point at
// it; a mismatch is invisible on disk and obvious in the browser.
const DatasourceUID = "prometheus-cms-infra"

// HomeUID is the dashboard the monitor node opens Grafana on (Puppet sets
// [dashboards] default_home_dashboard_path to its file). The verification
// asserts the setting took effect, so "first open in the browser" means the HA
// graphs, not Grafana's welcome page.
const HomeUID = "cms-ha-overview"

// dashboardGlob selects the dashboard JSON files from the embedded tree.
const dashboardGlob = "puppet/modules/role/files/grafana/*.json"

// QueryTimeout bounds a single Prometheus API call.
const QueryTimeout = 10 * time.Second

// Dashboard is the subset of the Grafana dashboard model the checks need.
type Dashboard struct {
	UID    string  `json:"uid"`
	Title  string  `json:"title"`
	Panels []Panel `json:"panels"`
	Time   struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"time"`
	Templating struct {
		List []json.RawMessage `json:"list"`
	} `json:"templating"`
}

// Panel is a dashboard panel. Only the fields that can break the first render
// are modelled: the datasource, the queries and the value mappings.
type Panel struct {
	ID          int        `json:"id"`
	Type        string     `json:"type"`
	Title       string     `json:"title"`
	Datasource  Datasource `json:"datasource"`
	Targets     []Target   `json:"targets"`
	FieldConfig struct {
		Defaults struct {
			Mappings []Mapping `json:"mappings"`
		} `json:"defaults"`
	} `json:"fieldConfig"`
}

// Target is one query of a panel (refId A, B, ...).
type Target struct {
	RefID      string     `json:"refId"`
	Expr       string     `json:"expr"`
	Datasource Datasource `json:"datasource"`
}

// Datasource is the datasource reference a panel or target carries.
type Datasource struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

// Mapping is a Grafana value mapping. Options is kept raw because Grafana uses
// an object for "value" mappings and an array for "range" ones.
type Mapping struct {
	Type    string          `json:"type"`
	Options json.RawMessage `json:"options"`
}

// ValueOption is one entry of a "value" mapping.
type ValueOption struct {
	Text  string `json:"text"`
	Color string `json:"color"`
}

// ValueOptions decodes a "value" mapping's options keyed by the matched value.
func (m Mapping) ValueOptions() (map[string]ValueOption, error) {
	if m.Type != "value" {
		return nil, fmt.Errorf("mapping type %q is not a value mapping", m.Type)
	}
	var opts map[string]ValueOption
	if err := json.Unmarshal(m.Options, &opts); err != nil {
		return nil, fmt.Errorf("decode value mapping: %w", err)
	}
	return opts, nil
}

// Load reads every shipped dashboard. It fails on invalid JSON instead of
// skipping the file: a dashboard Grafana cannot parse is a dashboard Grafana
// will not show.
func Load() ([]Dashboard, error) {
	paths, err := fs.Glob(cms.GrafanaFS, dashboardGlob)
	if err != nil {
		return nil, fmt.Errorf("list embedded dashboards: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no Grafana dashboard JSON found (glob %s)", dashboardGlob)
	}
	sort.Strings(paths)

	out := make([]Dashboard, 0, len(paths))
	for _, path := range paths {
		data, err := cms.GrafanaFS.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read embedded dashboard %s: %w", path, err)
		}
		var d Dashboard
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("parse dashboard %s: %w", path, err)
		}
		if d.UID == "" {
			return nil, fmt.Errorf("dashboard %s has no uid: Grafana cannot address it", path)
		}
		out = append(out, d)
	}
	return out, nil
}

// Home returns the dashboard Grafana is configured to open on and whether it is
// part of the shipped set.
func Home(ds []Dashboard) (Dashboard, bool) {
	for _, d := range ds {
		if d.UID == HomeUID {
			return d, true
		}
	}
	return Dashboard{}, false
}

// Queries returns every panel query with a human-readable origin, so a failing
// gate can name the panel an operator would have to look at.
type Query struct {
	Dashboard string
	Panel     string
	Expr      string
}

// Queries flattens the dashboards into their panel queries.
func Queries(ds []Dashboard) []Query {
	var out []Query
	for _, d := range ds {
		for _, p := range d.Panels {
			for _, t := range p.Targets {
				if strings.TrimSpace(t.Expr) == "" {
					continue
				}
				out = append(out, Query{Dashboard: d.Title, Panel: p.Title, Expr: t.Expr})
			}
		}
	}
	return out
}

// Report summarises a data check: how many queries ran and which of them did
// not return a series.
type Report struct {
	Queries int
	Empty   []string
	Errors  []string
}

// OK reports whether every panel query returned data.
func (r Report) OK() bool { return len(r.Empty) == 0 && len(r.Errors) == 0 }

// Error renders the failures; the caller adds the success wording. Long lists
// are truncated: a Prometheus outage fails every query with the same error, and
// the CI log does not need fifteen copies of it.
func (r Report) Error() string {
	if r.OK() {
		return ""
	}
	var parts []string
	if len(r.Errors) > 0 {
		parts = append(parts, "queries that could not be evaluated: "+joinCapped(r.Errors))
	}
	if len(r.Empty) > 0 {
		parts = append(parts, "panels with no data (the dashboard would open empty): "+joinCapped(r.Empty))
	}
	return strings.Join(parts, " | ")
}

// maxReported caps how many entries of one category are spelled out.
const maxReported = 3

func joinCapped(items []string) string {
	if len(items) <= maxReported {
		return strings.Join(items, "; ")
	}
	return fmt.Sprintf("%s; (+%d more)", strings.Join(items[:maxReported], "; "), len(items)-maxReported)
}

// PrometheusBaseURL turns the configured remote-write endpoint into the base
// URL of the Prometheus HTTP API (http://host:9090/api/v1/write becomes
// http://host:9090), tolerating a path prefix in front of the API.
func PrometheusBaseURL(rwURL string) string {
	u, err := url.Parse(rwURL)
	if err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	trimmed := strings.TrimSuffix(rwURL, "/api/v1/write")
	return strings.TrimSuffix(trimmed, "/")
}

// CheckPrometheus runs every panel query against a live Prometheus and fails
// when one of them returns no series. An empty instant vector is exactly what
// Grafana renders as "No data", so this is the same question the browser asks.
func CheckPrometheus(ctx context.Context, promBase string) (Report, error) {
	ds, err := Load()
	if err != nil {
		return Report{}, err
	}
	return checkQueries(ctx, promBase, Queries(ds), http.DefaultClient)
}

// checkQueries is the testable core: it takes the client so tests can point it
// at an httptest server instead of a lab.
func checkQueries(ctx context.Context, promBase string, queries []Query, client *http.Client) (Report, error) {
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimSuffix(promBase, "/")
	report := Report{Queries: len(queries)}

	for _, q := range queries {
		series, err := queryPrometheus(ctx, client, base, q.Expr)
		origin := q.Dashboard + " / " + q.Panel
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s (%q): %v", origin, q.Expr, err))
			continue
		}
		if series == 0 {
			report.Empty = append(report.Empty, fmt.Sprintf("%s (%q)", origin, q.Expr))
		}
	}
	return report, nil
}

// queryPrometheus returns the number of series an instant query yields.
func queryPrometheus(ctx context.Context, client *http.Client, base, expr string) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()

	u := base + "/api/v1/query?" + url.Values{"query": {expr}}.Encode()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("unreadable Prometheus response: %w", err)
	}
	if payload.Status != "success" {
		return 0, fmt.Errorf("prometheus rejected the query: %s", payload.Error)
	}
	return len(payload.Data.Result), nil
}
