// Package container locates a local container runtime (docker or podman) used
// to run pinned tool images on the hypervisor (the Garage backup store, the k6
// load generator, ...).
package container

import (
	"fmt"
	"os/exec"
)

// Runtime returns the first available container runtime binary ("docker" or
// "podman"). It returns an error when neither is installed.
func Runtime() (string, error) {
	for _, rt := range []string{"docker", "podman"} {
		if path, err := exec.LookPath(rt); err == nil && path != "" {
			return rt, nil
		}
	}
	return "", fmt.Errorf("no container runtime found (docker or podman is required)")
}
