package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
)

func TestRetry_Do(t *testing.T) {
	ctx := context.Background()
	cfg := retry.Config{
		MaxAttempts: 3,
		Interval:    10 * time.Millisecond,
		Timeout:     1 * time.Second,
	}

	t.Run("success on first try", func(t *testing.T) {
		calls := 0
		err := retry.Do(ctx, cfg, func() error {
			calls++
			return nil
		})
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
		if calls != 1 {
			t.Errorf("expected 1 call, got %d", calls)
		}
	})

	t.Run("success on third try", func(t *testing.T) {
		calls := 0
		err := retry.Do(ctx, cfg, func() error {
			calls++
			if calls < 3 {
				return errors.New("fail")
			}
			return nil
		})
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
		if calls != 3 {
			t.Errorf("expected 3 calls, got %d", calls)
		}
	})

	t.Run("fails max attempts", func(t *testing.T) {
		calls := 0
		err := retry.Do(ctx, cfg, func() error {
			calls++
			return errors.New("fail")
		})
		if err == nil {
			t.Errorf("expected error, got nil")
		}
		if calls != 3 {
			t.Errorf("expected 3 calls, got %d", calls)
		}
	})
}
