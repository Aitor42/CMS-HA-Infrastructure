package tests

import (
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		39 * time.Microsecond:                                                 "39.00µs",
		215352303 * time.Nanosecond:                                           "215.4ms",
		(9*time.Second + 421*time.Millisecond):                                "9.42s",
		(69*time.Second + 4*time.Millisecond):                                 "1m 9.00s",
		(14*time.Minute + 5*time.Second + 250*time.Millisecond):               "14m 5.25s",
		(2*time.Hour + 17*time.Minute + 9*time.Second + 666*time.Millisecond): "2h 17m 9.67s",
		// Rounding must not print a "60.00" seconds field.
		(59*time.Second + 999*time.Millisecond):                  "1m 0.00s",
		(59*time.Minute + 59*time.Second + 999*time.Millisecond): "1h 0m 0.00s",
	}
	for d, want := range cases {
		if got := logging.FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
