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
	"time"
)

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
