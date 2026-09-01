package traffic

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	_ "github.com/go-sql-driver/mysql"
)

// Options configuration for the traffic test.
type Options struct {
	Mode        string // "internal" or "external"
	TargetIP    string // override IP
	Duration    int    // seconds (default 60)
	Concurrency int    // (default 5)
	WithDB      bool
	Verbose     bool
}

// Traffic phase implementation.
type Traffic struct {
	cfg  *config.Config
	opts Options
}

// New creates a new traffic generator.
func New(cfg *config.Config, opts Options) *Traffic {
	if opts.Duration <= 0 {
		opts.Duration = 60
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 5
	}
	return &Traffic{
		cfg:  cfg,
		opts: opts,
	}
}

var wpPaths = []string{
	"/", "/wp-login.php", "/?s=test", "/?s=empresa", "/?s=proyecto",
	"/?p=1", "/wp-admin/", "/wp-cron.php", "/xmlrpc.php",
	"/wp-json/wp/v2/posts", "/wp-json/wp/v2/pages", "/wp-json/wp/v2/users",
	"/feed/", "/wp-content/themes/", "/favicon.ico", "/?cat=1", "/?author=1",
}

func getPrefix(cidr string) string {
	parts := strings.Split(cidr, ".")
	if len(parts) >= 3 {
		return fmt.Sprintf("%s.%s.%s.", parts[0], parts[1], parts[2])
	}
	return "192.168.10."
}

// Run executes the traffic phase tests.
func (t *Traffic) Run(ctx context.Context) error {
	timer := logging.PhaseStart("Traffic Test Phase")
	defer timer.End()

	baseURL := ""
	if t.opts.TargetIP != "" {
		baseURL = fmt.Sprintf("https://%s", t.opts.TargetIP)
	} else if t.opts.Mode == "internal" {
		baseURL = fmt.Sprintf("https://%s", getPrefix(t.cfg.Network.Internal.CIDR)+"20")
	} else {
		baseURL = fmt.Sprintf("https://%s", getPrefix(t.cfg.Network.Main.CIDR)+"120") // Assuming LB IP
	}
	logging.Info("Target URL: %s", baseURL)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 5 * time.Second,
	}

	// Phase 1: Distributed random HTTP GET
	logging.Info("Phase 1: Distributed random HTTP GET")
	for i := 0; i < 18; i++ {
		path := wpPaths[rand.Intn(len(wpPaths))]
		reqURL := baseURL + path
		req, _ := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
		resp, err := client.Do(req)
		if err != nil {
			if t.opts.Verbose {
				logging.Warn("GET %s failed: %v", reqURL, err)
			}
			continue
		}
		resp.Body.Close()
		if t.opts.Verbose {
			logging.Info("GET %s -> %d", reqURL, resp.StatusCode)
		}
	}

	// Phase 2: Concurrent stress test
	logging.Info("Phase 2: Concurrent stress test")
	t.runStressTest(ctx, client, baseURL)

	// Phase 3: DB direct queries
	if t.opts.WithDB {
		logging.Info("Phase 3: Direct MariaDB SQL queries")
		dbHost := getPrefix(t.cfg.Network.Internal.CIDR) + "10" // Assuming master1
		if len(t.cfg.Nodes.Masters) > 0 && t.cfg.Nodes.Masters[0].IP != "" {
			dbHost = t.cfg.Nodes.Masters[0].IP
		}
		dbDSN := fmt.Sprintf("root:%s@tcp(%s:3306)/cms", t.cfg.Database.RootPassword, dbHost)
		db, err := sql.Open("mysql", dbDSN)
		if err == nil {
			err = db.PingContext(ctx)
			if err == nil {
				rows, err := db.QueryContext(ctx, "SELECT option_value FROM wp_options LIMIT 5")
				if err == nil {
					rows.Close()
					logging.Success("DB queries executed successfully")
				} else {
					logging.Warn("DB query failed: %v", err)
				}
			} else {
				logging.Warn("DB ping failed: %v", err)
			}
			db.Close()
		} else {
			logging.Warn("DB connection failed: %v", err)
		}
	}

	// Phase 4: POST form simulations
	logging.Info("Phase 4: POST form simulations")
	formURL := baseURL + "/wp-login.php"
	formData := url.Values{
		"log": {"admin"},
		"pwd": {"password"},
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", formURL, strings.NewReader(formData.Encode()))
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		if t.opts.Verbose {
			logging.Info("POST login -> %d", resp.StatusCode)
		}
	}

	return nil
}

func (t *Traffic) runStressTest(ctx context.Context, client *http.Client, baseURL string) {
	var wg sync.WaitGroup
	var successCount int64
	var failCount int64
	var mu sync.Mutex
	var latencies []time.Duration

	deadline := time.Now().Add(time.Duration(t.opts.Duration) * time.Second)
	stressCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	for i := 0; i < t.opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stressCtx.Done():
					return
				default:
					path := wpPaths[rand.Intn(len(wpPaths))]
					reqURL := baseURL + path
					req, err := http.NewRequestWithContext(stressCtx, "GET", reqURL, nil)
					if err != nil {
						continue
					}
					
					start := time.Now()
					resp, err := client.Do(req)
					elapsed := time.Since(start)

					if err != nil {
						atomic.AddInt64(&failCount, 1)
					} else {
						resp.Body.Close()
						if resp.StatusCode < 400 {
							atomic.AddInt64(&successCount, 1)
							mu.Lock()
							latencies = append(latencies, elapsed)
							mu.Unlock()
						} else {
							atomic.AddInt64(&failCount, 1)
						}
					}
				}
			}
		}()
	}
	wg.Wait()

	total := successCount + failCount
	var mean time.Duration
	var p99 time.Duration

	if total > 0 && len(latencies) > 0 {
		var totalLat time.Duration
		for _, l := range latencies {
			totalLat += l
		}
		mean = totalLat / time.Duration(len(latencies))

		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p99Idx := int(float64(len(latencies)) * 0.99)
		if p99Idx >= len(latencies) {
			p99Idx = len(latencies) - 1
		}
		p99 = latencies[p99Idx]
	}

	rate := float64(total) / float64(t.opts.Duration)
	logging.Info("Stress Test Results: %d reqs (%.2f req/s)", total, rate)
	logging.Info("Success: %d, Fail: %d", successCount, failCount)
	if successCount > 0 {
		logging.Info("Latency Mean: %v, P99: %v", mean, p99)
	}
}
