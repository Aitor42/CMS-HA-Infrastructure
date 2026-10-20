package dashboards

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestLoadShippedDashboards is the static half of the Grafana gate: whatever
// Puppet ships must be addressable, must open on a relative time window and
// must not depend on a template variable nobody fills in.
func TestLoadShippedDashboards(t *testing.T) {
	ds, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if len(ds) < 2 {
		t.Fatalf("expected at least 2 dashboards, got %d", len(ds))
	}

	seenUID := make(map[string]bool, len(ds))
	seenTitle := make(map[string]bool, len(ds))
	for _, d := range ds {
		if d.UID == "" || d.Title == "" {
			t.Errorf("dashboard without uid/title: %+v", d)
			continue
		}
		if seenUID[d.UID] {
			t.Errorf("duplicate dashboard uid %q", d.UID)
		}
		if seenTitle[d.Title] {
			t.Errorf("duplicate dashboard title %q", d.Title)
		}
		seenUID[d.UID] = true
		seenTitle[d.Title] = true

		// A dashboard without a relative window opens on whatever absolute
		// range was saved, which is empty a day later.
		if !strings.HasPrefix(d.Time.From, "now-") || !strings.HasPrefix(d.Time.To, "now") {
			t.Errorf("%s: time range %q..%q must be relative", d.Title, d.Time.From, d.Time.To)
		}
		if len(d.Templating.List) != 0 {
			t.Errorf("%s: templating variables are shipped without a value; Grafana asks for them on open", d.Title)
		}
		if len(d.Panels) == 0 {
			t.Errorf("%s: no panels", d.Title)
		}

		for _, p := range d.Panels {
			if p.Datasource.Type != "prometheus" || p.Datasource.UID != DatasourceUID {
				t.Errorf("%s/%s: datasource %q/%q, want prometheus/%s", d.Title, p.Title, p.Datasource.Type, p.Datasource.UID, DatasourceUID)
			}
			if len(p.Targets) == 0 {
				t.Errorf("%s/%s: panel has no query", d.Title, p.Title)
			}
			for _, tgt := range p.Targets {
				expr := strings.TrimSpace(tgt.Expr)
				if expr == "" {
					t.Errorf("%s/%s: empty query", d.Title, p.Title)
					continue
				}
				if strings.Contains(expr, "$") {
					t.Errorf("%s/%s: query %q uses a dashboard variable", d.Title, p.Title, expr)
				}
				if tgt.Datasource.UID != "" && tgt.Datasource.UID != DatasourceUID {
					t.Errorf("%s/%s: query datasource %q, want %q", d.Title, p.Title, tgt.Datasource.UID, DatasourceUID)
				}
			}
		}
	}

	if !seenUID[HomeUID] {
		t.Errorf("the home dashboard %q is not shipped, so the first page cannot be the HA overview", HomeUID)
	}
}

// TestK6PanelsQueryExportedSeries pins the k6 panels to the series the remote
// write output actually produces. With the default (non-native-histogram)
// conversion k6 exports one gauge per requested trend statistic - the panel
// used to rate k6_http_req_duration_seconds_sum/_count, which only exists for
// native histograms and therefore rendered "No data".
func TestK6PanelsQueryExportedSeries(t *testing.T) {
	ds, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Exported by k6 0.55 with `--out experimental-prometheus-rw` and
	// K6_PROMETHEUS_RW_TREND_STATS=p(95),p(99).
	exported := map[string]bool{
		"k6_http_reqs_total":       true,
		"k6_http_req_duration_p95": true,
		"k6_http_req_duration_p99": true,
	}

	nameRE := regexp.MustCompile(`k6_[a-zA-Z0-9_]+`)
	k6Panels := 0
	for _, d := range ds {
		for _, p := range d.Panels {
			var expr strings.Builder
			for _, tgt := range p.Targets {
				expr.WriteString(tgt.Expr)
				expr.WriteString("\n")
			}
			if !strings.Contains(expr.String(), "k6_") {
				continue
			}
			k6Panels++
			found := nameRE.FindAllString(expr.String(), -1)
			if len(found) == 0 {
				t.Errorf("%s/%s: no k6 metric name in %q", d.Title, p.Title, expr.String())
			}
			for _, name := range found {
				if !exported[name] {
					t.Errorf("%s/%s queries %q, which k6's remote-write output does not export", d.Title, p.Title, name)
				}
			}
		}
	}
	if k6Panels < 2 {
		t.Errorf("expected the two k6 panels (throughput and latency), found %d", k6Panels)
	}
}

// TestScrapeTargetPanelMapsUpAsUp guards the value mapping of the `up` panel:
// it shipped inverted, so every healthy target rendered as DOWN.
func TestScrapeTargetPanelMapsUpAsUp(t *testing.T) {
	ds, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	for _, d := range ds {
		for _, p := range d.Panels {
			if len(p.Targets) != 1 || strings.TrimSpace(p.Targets[0].Expr) != "up" {
				continue
			}
			mappings := p.FieldConfig.Defaults.Mappings
			if len(mappings) == 0 {
				t.Fatalf("%s/%s: no value mapping, the panel shows 0/1 instead of UP/DOWN", d.Title, p.Title)
			}
			opts, err := mappings[0].ValueOptions()
			if err != nil {
				t.Fatalf("%s/%s: %v", d.Title, p.Title, err)
			}
			if got := opts["1"].Text; got != "UP" {
				t.Errorf("%s/%s: up=1 renders %q, want UP (the mapping was inverted)", d.Title, p.Title, got)
			}
			if got := opts["0"].Text; got != "DOWN" {
				t.Errorf("%s/%s: up=0 renders %q, want DOWN", d.Title, p.Title, got)
			}
			return
		}
	}
	t.Fatal("no panel queries `up`; the scrape-target panel is missing")
}

// TestCheckQueriesReportsEmptyPanels proves the runtime gate fails on the same
// answer the browser shows as "No data".
func TestCheckQueriesReportsEmptyPanels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Query().Get("query"), "missing") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]}]}}`))
	}))
	defer srv.Close()

	report, err := checkQueries(context.Background(), srv.URL, []Query{
		{Dashboard: "HA", Panel: "etcd", Expr: "count(up) == 1"},
		{Dashboard: "HA", Panel: "latency", Expr: "missing_series"},
	}, srv.Client())
	if err != nil {
		t.Fatalf("checkQueries() error: %v", err)
	}
	if report.OK() {
		t.Fatal("an empty panel must fail the check")
	}
	if report.Queries != 2 {
		t.Errorf("Queries = %d, want 2", report.Queries)
	}
	if len(report.Empty) != 1 || !strings.Contains(strings.Join(report.Empty, " "), "missing_series") {
		t.Errorf("Empty = %v, want the missing series", report.Empty)
	}
	if msg := report.Error(); !strings.Contains(msg, "no data") {
		t.Errorf("Error() = %q, want it to explain the empty panel", msg)
	}
}

func TestCheckQueriesPassesWhenEveryPanelHasData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]}]}}`))
	}))
	defer srv.Close()

	report, err := checkQueries(context.Background(), srv.URL, []Query{
		{Dashboard: "HA", Panel: "etcd", Expr: "count(up) == 1"},
	}, srv.Client())
	if err != nil {
		t.Fatalf("checkQueries() error: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected the check to pass: %s", report.Error())
	}
	if report.Queries != 1 {
		t.Errorf("Queries = %d, want 1", report.Queries)
	}
}

func TestCheckQueriesSurfacesPrometheusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","error":"parse error"}`))
	}))
	defer srv.Close()

	report, err := checkQueries(context.Background(), srv.URL, []Query{
		{Dashboard: "HA", Panel: "broken", Expr: "sum("},
	}, srv.Client())
	if err != nil {
		t.Fatalf("checkQueries() error: %v", err)
	}
	if report.OK() {
		t.Fatal("a rejected query must fail the check")
	}
	if len(report.Errors) != 1 || !strings.Contains(report.Errors[0], "HTTP 400") {
		t.Errorf("Errors = %v, want the HTTP status", report.Errors)
	}
}

func TestPrometheusBaseURL(t *testing.T) {
	tests := map[string]string{
		"http://192.168.10.20:9090/api/v1/write": "http://192.168.10.20:9090",
		"http://192.168.10.20:9090":              "http://192.168.10.20:9090",
		"https://prom.example/api/v1/write":      "https://prom.example",
		"http://192.168.10.20:9090/":             "http://192.168.10.20:9090",
	}
	for in, want := range tests {
		if got := PrometheusBaseURL(in); got != want {
			t.Errorf("PrometheusBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
