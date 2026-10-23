// Package prom wraps the small part of the Prometheus HTTP API used by the
// verifier and the operational drills: an instant query that returns samples.
package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/httpcheck"
)

// Timeout bounds each Prometheus probe. A converged lab answers the local API
// well below this, so a longer wait only delays a real failure.
const Timeout = 30 * time.Second

// Series evaluates an instant query and returns its samples. It is used for
// "this series must exist" checks, where an empty result is a failure with a
// different cause than a transport error.
func Series(ctx context.Context, base, expr string) ([]float64, error) {
	query := url.Values{"query": {expr}}
	status, body, err := httpcheck.Get(ctx, base+"/api/v1/query?"+query.Encode(), "", Timeout, false)
	if err != nil {
		return nil, fmt.Errorf("query %s failed: %w", expr, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("query %s failed (HTTP %d)", expr, status)
	}
	return Values(body)
}

// Values extracts every sample value of a Prometheus query result. phase08
// only needs the aggregate count of a query, but a functional check has to look
// at each series, because every blackbox probe has to be up.
func Values(body string) ([]float64, error) {
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
