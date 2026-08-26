package ssh

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ParallelResult represents the outcome of an operation on a specific host.
type ParallelResult struct {
	Host     string
	Output   string
	ExitCode int
	Err      error
}

// RunParallel runs the same command on multiple hosts concurrently using goroutines + sync.WaitGroup.
func (p *Pool) RunParallel(ctx context.Context, hosts []string, cmd string) []ParallelResult {
	tasks := make(map[string]func(ctx context.Context, pool *Pool) error)
	results := make([]ParallelResult, len(hosts))

	var mu sync.Mutex
	for i, host := range hosts {
		h := host
		idx := i
		tasks[h] = func(c context.Context, pool *Pool) error {
			stdout, _, code, err := pool.RunCommand(c, h, cmd)
			mu.Lock()
			results[idx] = ParallelResult{
				Host:     h,
				Output:   stdout,
				ExitCode: code,
				Err:      err,
			}
			mu.Unlock()
			return err
		}
	}

	p.RunParallelFunc(ctx, tasks)
	// Return the results mapped properly
	return results
}

// RunParallelFunc runs different functions per host.
func (p *Pool) RunParallelFunc(ctx context.Context, tasks map[string]func(ctx context.Context, pool *Pool) error) []ParallelResult {
	var wg sync.WaitGroup
	results := make([]ParallelResult, 0, len(tasks))
	var mu sync.Mutex

	total := len(tasks)
	completed := 0

	for host, taskFunc := range tasks {
		wg.Add(1)
		go func(h string, f func(context.Context, *Pool) error) {
			defer wg.Done()
			err := f(ctx, p)
			
			mu.Lock()
			completed++
			slog.Info("parallel task progress", "completed", completed, "total", total, "host", h)
			results = append(results, ParallelResult{
				Host: h,
				Err:  err,
			})
			mu.Unlock()
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
	for _, res := range results {
		if res.Err != nil {
			return res.Err // Return on first error, although all finished
		}
	}
	return nil
}
