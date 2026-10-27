package logging

import "testing"

func TestRunIDRoundTrip(t *testing.T) {
	t.Cleanup(func() { SetRunID("") })
	SetRunID("run-test")
	if got := RunID(); got != "run-test" {
		t.Fatalf("RunID() = %q, want run-test", got)
	}
	SetRunID("")
	if got := RunID(); got != "" {
		t.Fatalf("RunID() after clear = %q, want empty", got)
	}
}
