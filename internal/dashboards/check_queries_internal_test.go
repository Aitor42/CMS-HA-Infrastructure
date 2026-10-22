package dashboards

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The checkQueries core takes an *http.Client so these tests can point it at an
// httptest server instead of a lab. The exported surface (Load, PrometheusBaseURL
// and the shipped dashboards) is covered from the black-box tests package.

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
