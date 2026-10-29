// Package httpcheck holds the HTTP probe used by verification and operational
// drills, so both report status codes and bodies the same way.
package httpcheck

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"time"
)

// Get performs a one-shot GET with a per-request timeout. insecure skips TLS
// verification for endpoints whose certificate comes from the project's own CA.
func Get(ctx context.Context, rawURL, hostHeader string, timeout time.Duration, insecure bool) (int, string, error) {
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

// lbClient is shared across load-balancer reachability probes so its connection
// pool is reused instead of being rebuilt on every call.
var lbClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- lab LB serves the project CA
	},
}

// LBReachable reports whether the load balancer answers for vhost over HTTPS or
// HTTP with a page a browser would accept. Failover and the TLS drill both ask
// the same question, so they share one definition of "reachable".
func LBReachable(ctx context.Context, lbIP, vhost string) bool {
	return lbReachable(ctx, lbIP, vhost, "https", "http")
}

// LBReachableHTTPS is the strict variant: it accepts only an HTTPS answer and
// does not fall back to plain HTTP. The LB VIP scenario claims that the
// surviving balancer terminates TLS, so an HTTP-only answer (typically the
// nginx redirect) must not satisfy it.
func LBReachableHTTPS(ctx context.Context, lbIP, vhost string) bool {
	return lbReachable(ctx, lbIP, vhost, "https")
}

// lbReachable probes the given schemes in order and reports the first one that
// answers with a status a browser would accept.
func lbReachable(ctx context.Context, lbIP, vhost string, schemes ...string) bool {
	for _, scheme := range schemes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+lbIP, nil)
		if err != nil {
			continue
		}
		req.Host = vhost
		resp, err := lbClient.Do(req)
		if err != nil {
			continue
		}
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusOK || status == http.StatusMovedPermanently || status == http.StatusFound {
			return true
		}
	}
	return false
}
