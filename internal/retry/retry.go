package retry

import (
	"context"
	"fmt"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

// Config holds settings for retry and poll operations.
type Config struct {
	MaxAttempts int
	Interval    time.Duration
	Timeout     time.Duration
}

// Do executes fn and retries it until it returns nil or the limits in cfg are reached.
func Do(ctx context.Context, cfg Config, fn func() error) error {
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if attempt > 1 {
			logging.Info("Retry attempt %d/%d", attempt, cfg.MaxAttempts)
		}
		
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err

		if attempt == cfg.MaxAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("retry context cancelled: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(cfg.Interval):
			// wait before next attempt
		}
	}

	return fmt.Errorf("max attempts (%d) reached: %w", cfg.MaxAttempts, lastErr)
}

// Poll executes fn and retries until fn returns (true, nil) or the limits in cfg are reached.
func Poll(ctx context.Context, cfg Config, fn func() (bool, error)) error {
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if attempt > 1 {
			logging.Info("Poll attempt %d/%d", attempt, cfg.MaxAttempts)
		}
		
		ok, err := fn()
		if err != nil {
			return fmt.Errorf("poll failed on attempt %d: %w", attempt, err)
		}
		if ok {
			return nil
		}

		if attempt == cfg.MaxAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("poll context cancelled: %w", ctx.Err())
		case <-time.After(cfg.Interval):
			// wait before next attempt
		}
	}

	return fmt.Errorf("poll max attempts (%d) reached without success", cfg.MaxAttempts)
}
