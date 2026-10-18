package utils

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file implements the verifier's end-to-end check: it asks the deployed
// services themselves, from the hypervisor, exactly what an operator would ask
// them. A green `systemctl is-active` only proves a unit started; these checks
// prove the request path works — nginx on the keepalived VIP forwarding to
// Apache/PHP/WordPress, Prometheus scraping something, and Grafana talking to
// its own database.

const (
	e2eTimeout = 30 * time.Second
	// cmsBurstRequests is the number of sequential page loads issued through the
	// load balancer VIP to prove the request path is not a one-off.
	cmsBurstRequests = 100
	// cmsBurstMinSuccessRatio tolerates a couple of failures (a keepalived
	// transition, a PHP worker restart) without hiding a broken CMS.
	cmsBurstMinSuccessRatio = 0.95
)

// e2eOutcome is one sub-check's result inside phase08.
type e2eOutcome struct {
	label  string
	pass   bool
	detail string
}

// phase08 verifies end-to-end HTTP through the deployed stack: the CMS behind
// the load balancer VIP, and the monitoring stack's own health endpoints.
func (v *Verifier) phase08(ctx context.Context) CheckResult {
	const name = "End-to-end HTTP (CMS, Grafana, Prometheus)"

	vip := v.cfg.LBVIP()
	monitorIP := v.cfg.Nodes.Monitor.IP
	if vip == "" || monitorIP == "" {
		return CheckResult{"Phase 08", name, false, "network.main.vip or nodes.monitor.ip is not configured"}
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		outcomes []e2eOutcome
	)
	record := func(o e2eOutcome) {
		mu.Lock()
		defer mu.Unlock()
		outcomes = append(outcomes, o)
	}

	wg.Add(3)
	go func() {
		defer wg.Done()
		record(v.checkCMS(ctx, vip))
	}()
	go func() {
		defer wg.Done()
		record(v.checkPrometheus(ctx, monitorIP))
	}()
	go func() {
		defer wg.Done()
		record(v.checkGrafana(ctx, monitorIP))
	}()
	wg.Wait()

	pass := true
	var details []string
	for _, o := range outcomes {
		if !o.pass {
			pass = false
		}
		details = append(details, fmt.Sprintf("%s: %s", o.label, o.detail))
	}
	return CheckResult{"Phase 08", name, pass, strings.Join(details, ", ")}
}

// checkCMS loads the WordPress home page through the keepalived VIP the way
// clients and the traffic generator do (TLS against the internal CA, so
// verification is skipped on purpose), then issues a short burst to prove the
// load-balancer path keeps working.
func (v *Verifier) checkCMS(ctx context.Context, vip string) e2eOutcome {
	o := e2eOutcome{label: "CMS"}
	base := "https://" + vip + "/"
	// The vhost is keyed on its server name, exactly as the load profile does
	// it: without the Host header WordPress answers with a canonical redirect.
	vhost := v.cfg.TrafficVHost()
	hostNote := ""
	if vhost != "" && vhost != vip {
		hostNote = fmt.Sprintf(" (Host: %s)", vhost)
	}

	status, body, err := httpGet(ctx, base, vhost, e2eTimeout, true)
	if err != nil {
		o.detail = fmt.Sprintf("GET %s%s failed: %v", base, hostNote, err)
		return o
	}
	if status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s%s returned HTTP %d", base, hostNote, status)
		return o
	}
	if !strings.Contains(strings.ToLower(body), "wp-") {
		o.detail = fmt.Sprintf("GET %s%s returned HTTP 200 but no WordPress markup (first bytes: %.60q)", base, hostNote, body)
		return o
	}

	// Burst: the same request repeated through the VIP, which exercises
	// keepalived + nginx + both frontends.
	success := 0
	lastErr := ""
	for i := 0; i < cmsBurstRequests; i++ {
		st, _, err := httpGet(ctx, base, vhost, e2eTimeout, true)
		if err == nil && st == http.StatusOK {
			success++
			continue
		}
		if err != nil {
			lastErr = err.Error()
		} else {
			lastErr = fmt.Sprintf("HTTP %d", st)
		}
	}

	ratio := float64(success) / float64(cmsBurstRequests)
	o.pass = ratio >= cmsBurstMinSuccessRatio
	o.detail = fmt.Sprintf("%d/%d requests OK (%.0f%%)", success, cmsBurstRequests, ratio*100)
	if !o.pass {
		o.detail += fmt.Sprintf(", last failure: %s", lastErr)
	}
	return o
}

// checkPrometheus asks Prometheus for its own health and for a query result:
// the query proves it is actually scraping the cluster's exporters.
func (v *Verifier) checkPrometheus(ctx context.Context, monitorIP string) e2eOutcome {
	o := e2eOutcome{label: "Prometheus"}
	base := "http://" + monitorIP + ":9090"

	if status, _, err := httpGet(ctx, base+"/-/healthy", "", e2eTimeout, false); err != nil || status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s/-/healthy failed (HTTP %d): %v", base, status, err)
		return o
	}

	query := url.Values{"query": {"count(up == 1)"}}
	status, body, err := httpGet(ctx, base+"/api/v1/query?"+query.Encode(), "", e2eTimeout, false)
	if err != nil || status != http.StatusOK {
		o.detail = fmt.Sprintf("query failed (HTTP %d): %v", status, err)
		return o
	}
	var res struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		o.detail = fmt.Sprintf("query returned unparsable JSON: %v", err)
		return o
	}
	if res.Status != "success" || len(res.Data.Result) == 0 || len(res.Data.Result[0].Value) < 2 {
		o.detail = fmt.Sprintf("query count(up == 1) returned no healthy target (status %q)", res.Status)
		return o
	}
	o.pass = true
	o.detail = fmt.Sprintf("healthy, %v target(s) up", res.Data.Result[0].Value[1])
	return o
}

// checkGrafana asks Grafana to report its own health, which includes its
// database connection.
func (v *Verifier) checkGrafana(ctx context.Context, monitorIP string) e2eOutcome {
	o := e2eOutcome{label: "Grafana"}
	endpoint := "http://" + monitorIP + ":3000/api/health"

	status, body, err := httpGet(ctx, endpoint, "", e2eTimeout, false)
	if err != nil || status != http.StatusOK {
		o.detail = fmt.Sprintf("GET %s failed (HTTP %d): %v", endpoint, status, err)
		return o
	}
	var health struct {
		Database string `json:"database"`
		Version  string `json:"version"`
	}
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		o.detail = fmt.Sprintf("unparsable health payload: %v", err)
		return o
	}
	if health.Database != "ok" {
		o.detail = fmt.Sprintf("database status %q", health.Database)
		return o
	}
	o.pass = true
	o.detail = fmt.Sprintf("database ok (v%s)", health.Version)
	return o
}

// httpGet is a one-shot GET with a per-request timeout. insecure skips TLS
// verification, which the CMS endpoint requires: its certificate is issued by
// the project's own internal CA, not by a public one.
func httpGet(ctx context.Context, rawURL, hostHeader string, timeout time.Duration, insecure bool) (int, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, "", err
	}
	if hostHeader != "" {
		req.Host = hostHeader
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		// #nosec G402 -- the lab serves its own internally-issued certificate.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(body), nil
}
