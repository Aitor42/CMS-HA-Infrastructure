package ssh

import (
	"os"
	"path/filepath"
	"strings"
)

// RemoveHostsFromKnownHosts drops every entry for the given hosts from a
// known_hosts file, preserving comments and unrelated entries. A missing file
// is not an error.
//
// It exists for machines that legitimately change identity: a re-provisioned
// (or freshly installed) VM presents a new host key, and a pinned entry would
// otherwise make the connection fail closed (see verifyHostKey).
func RemoveHostsFromKnownHosts(knownHostsPath string, hosts []string) error {
	data, err := os.ReadFile(filepath.Clean(knownHostsPath)) // #nosec G304 -- operator's own ssh known_hosts paths
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	lines := strings.Split(string(data), "\n")
	var newLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			newLines = append(newLines, line)
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			newLines = append(newLines, line)
			continue
		}
		hostPart := fields[0]
		match := false
		for _, host := range hosts {
			if host != "" && hostMatchesHost(hostPart, host) {
				match = true
				break
			}
		}
		if !match {
			newLines = append(newLines, line)
		}
	}

	return os.WriteFile(filepath.Clean(knownHostsPath), []byte(strings.Join(newLines, "\n")), 0600) // #nosec G703 G304 -- safely write cleaned known_hosts in user ssh dir
}

// hostMatchesHost reports whether a known_hosts host token (which may list
// several hosts, optionally prefixed with a marker and optionally bracketed
// with a port) refers to target.
func hostMatchesHost(hostToken, target string) bool {
	hostToken = strings.TrimPrefix(hostToken, "@cert-authority ")
	hostToken = strings.TrimPrefix(hostToken, "@revoked ")
	parts := strings.Split(hostToken, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == target {
			return true
		}
		if strings.HasPrefix(p, "[") && strings.Contains(p, "]:") {
			idx := strings.Index(p, "]:")
			if p[1:idx] == target {
				return true
			}
		}
	}
	return false
}
