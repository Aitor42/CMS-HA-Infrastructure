package retry

import (
	"context"
	"testing"
	"time"
)

func TestWaitReturnsAsSoonAsPredicatePasses(t *testing.T) {
	calls := 0
	if !Wait(t.Context(), time.Second, time.Millisecond, func() bool {
		calls++
		return calls == 3
	}) {
		t.Fatal("Wait returned false, want true on the third probe")
	}
	if calls != 3 {
		t.Errorf("predicate calls = %d, want 3", calls)
	}
}

func TestWaitTimesOutAfterTheBudget(t *testing.T) {
	calls := 0
	start := time.Now()
	if Wait(t.Context(), 5*time.Millisecond, time.Millisecond, func() bool {
		calls++
		return false
	}) {
		t.Fatal("Wait returned true for a predicate that never passes")
	}
	if calls < 2 {
		t.Errorf("predicate calls = %d, want more than one probe", calls)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Errorf("Wait returned after %s, before the 5ms budget", elapsed)
	}
}

func TestWaitStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if Wait(ctx, time.Hour, time.Millisecond, func() bool { return false }) {
		t.Fatal("Wait returned true for a cancelled context")
	}
}
