package logging

import (
	"os"
	"strings"
	"sync"
)

var (
	colorOnce    sync.Once
	colorEnabled bool
)

// colorSupported reports whether ANSI colour should be emitted. Colour is
// disabled by NO_COLOR (https://no-color.org) and TERM=dumb, forced on by
// CLICOLOR_FORCE, and otherwise enabled only when stdout is a terminal (so
// redirected logs and CI output stay free of escape codes).
func colorSupported() bool {
	colorOnce.Do(func() {
		force := os.Getenv("CLICOLOR_FORCE")
		switch {
		case os.Getenv("NO_COLOR") != "":
			colorEnabled = false
		case force != "" && force != "0":
			colorEnabled = true
		case strings.EqualFold(os.Getenv("TERM"), "dumb"):
			colorEnabled = false
		default:
			fi, err := os.Stdout.Stat()
			colorEnabled = err == nil && fi.Mode()&os.ModeCharDevice != 0
		}
	})
	return colorEnabled
}

// colorize wraps s in the given SGR code unless colour output is disabled.
func colorize(code, s string) string {
	if !colorSupported() {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func cyan(s string) string   { return colorize("36", s) }
func green(s string) string  { return colorize("32", s) }
func yellow(s string) string { return colorize("33", s) }
func red(s string) string    { return colorize("31", s) }
