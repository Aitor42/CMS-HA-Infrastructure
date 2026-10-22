// Package shell provides helpers for building POSIX shell commands.
package shell

import "strings"

// Quote single-quotes s for a POSIX shell, escaping embedded single quotes, so
// it can be embedded in a `bash -c '...'` invocation or concatenated into a
// larger command without the value being expanded.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
