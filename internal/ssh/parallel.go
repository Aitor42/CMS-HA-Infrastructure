package ssh

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxParallelHosts bounds how many hosts are processed concurrently, so a large
// node list (e.g. many hotdesks) cannot open an unbounded number of SSH
// sessions at once.
const maxParallelHosts = 16

func parallelLimit(n int) int {
	if n < maxParallelHosts {
		return n
	}
	return maxParallelHosts
}

// ParallelResult represents the outcome of an operation on a specific host.
type ParallelResult struct {
	Host     string
	Output   string
	ExitCode int
	Err      error
}

// RunParallel runs the same command on multiple hosts concurrently and returns
// one result per host, in input order. Concurrency is bounded by maxParallelHosts.
func (p *Pool) RunParallel(ctx context.Context, hosts []string, cmd string) []ParallelResult {
	results := make([]ParallelResult, len(hosts))
	if len(hosts) == 0 {
		return results
	}

	var wg sync.WaitGroup
	var completed atomic.Int64
	total := len(hosts)
	sem := make(chan struct{}, parallelLimit(len(hosts)))

	for i, host := range hosts {
		wg.Add(1)
		go func(idx int, h string) {
			defer wg.Done()
			sem <- struct{}{}
			stdout, _, code, err := p.RunCommand(ctx, h, cmd)
			<-sem

			// results[idx] is a distinct element per goroutine: no lock needed.
			results[idx] = ParallelResult{
				Host:     h,
				Output:   stdout,
				ExitCode: code,
				Err:      err,
			}
			slog.Info("parallel task progress", "completed", completed.Add(1), "total", total, "host", h)
		}(i, host)
	}

	wg.Wait()
	return results
}

// RunParallelFunc runs one task per host concurrently, with the same concurrency
// limit as RunParallel. Results are unordered because tasks finish in any order.
func (p *Pool) RunParallelFunc(ctx context.Context, tasks map[string]func(ctx context.Context, pool *Pool) error) []ParallelResult {
	var wg sync.WaitGroup
	results := make([]ParallelResult, 0, len(tasks))
	var mu sync.Mutex
	var completed atomic.Int64

	total := len(tasks)
	sem := make(chan struct{}, parallelLimit(len(tasks)))

	for host, taskFunc := range tasks {
		wg.Add(1)
		go func(h string, f func(context.Context, *Pool) error) {
			defer wg.Done()
			sem <- struct{}{}
			err := f(ctx, p)
			<-sem

			mu.Lock()
			results = append(results, ParallelResult{
				Host: h,
				Err:  err,
			})
			mu.Unlock()
			slog.Info("parallel task progress", "completed", completed.Add(1), "total", total, "host", h)
		}(host, taskFunc)
	}

	wg.Wait()
	return results
}

// WaitForAllSSH waits for all hosts to be SSH-reachable.
func (p *Pool) WaitForAllSSH(ctx context.Context, hosts []string, timeout time.Duration) error {
	tasks := make(map[string]func(ctx context.Context, pool *Pool) error)

	for _, host := range hosts {
		h := host
		tasks[h] = func(c context.Context, pool *Pool) error {
			return pool.WaitForSSH(c, h, timeout)
		}
	}

	results := p.RunParallelFunc(ctx, tasks)
	var failures []string
	for _, res := range results {
		if res.Err != nil {
			failures = append(failures, fmt.Sprintf("%s (%v)", res.Host, res.Err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("ssh wait failed for %d host(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}
