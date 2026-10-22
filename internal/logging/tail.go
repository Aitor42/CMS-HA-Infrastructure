package logging

import "strings"

// Tail returns the last max lines of s with trailing line breaks removed. It is
// for error reports: remote tools often print the useful failure after a wall
// of progress output, and the full log is already in the phase log.
func Tail(s string, max int) string {
	if max <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n\r"), "\n")
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return strings.Join(lines, "\n")
}
