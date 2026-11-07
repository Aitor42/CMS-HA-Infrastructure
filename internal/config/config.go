// Package config loads, validates and persists the lab configuration.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandPath expands environment variables and tilde (~) prefix in file paths.
func ExpandPath(path string) string {
	if path == "" {
		return ""
	}
	path = os.ExpandEnv(path)
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	} else if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		}
	}
	return filepath.Clean(path)
}
