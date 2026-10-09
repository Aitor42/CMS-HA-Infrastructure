package retry

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

// Config holds settings for retry and poll operations.
type Config struct {
	MaxAttempts int
	Interval    time.Duration
	Timeout     time.Duration
	// MaxInterval enables exponential backoff: the wait between attempts doubles
	// from Interval up to this ceiling (plus jitter). Zero keeps a constant
	// Interval, preserving the historic behaviour.
	MaxInterval time.Duration
}

// backoffInterval returns the wait to apply after the given (1-based) attempt,
// i.e. before the next attempt. With MaxInterval unset it is the constant
// Interval; otherwise it grows exponentially up to that ceiling and is
// randomised by ±10% to avoid synchronised retries.
func backoffInterval(cfg Config, attempt int) time.Duration {
	interval := cfg.Interval
	if interval <= 0 {
		return time.Millisecond
	}
	if cfg.MaxInterval > interval {
		for i := 1; i < attempt; i++ {
			interval *= 2
			if interval >= cfg.MaxInterval {
				break
			}
		}
		if interval > cfg.MaxInterval {
			interval = cfg.MaxInterval
		}
		// ±10% jitter only while backing off, so constant-interval callers
		// keep their exact historic timing.
		if jitter := interval / 5; jitter > 0 {
			interval += time.Duration(rand.Int63n(int64(jitter)+1)) - jitter/2
		}
	}
	if interval <= 0 {
		return time.Millisecond
	}
	return interval
}

// Do executes fn and retries it until it returns nil or the limits in cfg are reached.
func Do(ctx context.Context, cfg Config, fn func() error) error {
	return DoContext(ctx, cfg, func(context.Context) error { return fn() })
}

// DoContext is Do with the run context passed to fn, so the operation itself
// can honour cancellation. When Config.Timeout is set, that context carries the
// overall timeout budget shared by every attempt; it is not reset per attempt.
func DoContext(ctx context.Context, cfg Config, fn func(context.Context) error) error {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("retry context cancelled: %w (last error: %v)", err, lastErr)
		}
		if attempt > 1 {
			logging.Debug("Retry attempt %d/%d", attempt, cfg.MaxAttempts)
		}

		err := fn(ctx)
		if err == nil {
			return nil
		}
		lastErr = err

		if attempt == cfg.MaxAttempts {
			break
		}

		timer := time.NewTimer(backoffInterval(cfg, attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("retry context cancelled: %w (last error: %v)", ctx.Err(), lastErr)
		case <-timer.C:
			// backoff elapsed; retry the operation
		}
	}

	return fmt.Errorf("max attempts (%d) reached: %w", cfg.MaxAttempts, lastErr)
}

// Poll executes fn and retries until fn returns (true, nil) or the limits in cfg are reached.
func Poll(ctx context.Context, cfg Config, fn func() (bool, error)) error {
	return PollContext(ctx, cfg, func(context.Context) (bool, error) { return fn() })
}

// PollContext is Poll with the run context passed to fn so the operation can
// honour cancellation; the same context is reused for every attempt.
func PollContext(ctx context.Context, cfg Config, fn func(context.Context) (bool, error)) error {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("poll context cancelled: %w", err)
		}
		if attempt > 1 {
			logging.Debug("Poll attempt %d/%d", attempt, cfg.MaxAttempts)
		}

		ok, err := fn(ctx)
		if err != nil {
			return fmt.Errorf("poll failed on attempt %d: %w", attempt, err)
		}
		if ok {
			return nil
		}

		if attempt == cfg.MaxAttempts {
			break
		}

		timer := time.NewTimer(backoffInterval(cfg, attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("poll context cancelled: %w", ctx.Err())
		case <-timer.C:
			// backoff elapsed; re-check the condition
		}
	}

	return fmt.Errorf("poll max attempts (%d) reached without success", cfg.MaxAttempts)
}
