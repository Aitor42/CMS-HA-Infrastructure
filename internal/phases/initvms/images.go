package initvms

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func findUbuntuISO(dir string) (string, error) {
	candidates := []string{
		filepath.Join(dir, "ubuntu-24.04.4-live-server-amd64.iso"),
		filepath.Join(dir, "ubuntu-24.04.2-live-server-amd64.iso"),
		filepath.Join(dir, "ubuntu-24.04-live-server-amd64.iso"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.Size() > 0 {
			return c, nil
		}
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "*ubuntu-24.04*.iso"))
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.Size() > 1<<30 { // >1GB
			return m, nil
		}
	}

	return "", fmt.Errorf("no Ubuntu 24.04 ISO found in %s; download it first", dir)
}

// generateRandomPasswordHash generates a random SHA-512 password hash using crypto/rand.
// The actual password is random and discarded; SSH key auth is enforced and late-commands locks the accounts.
func generateRandomPasswordHash() (string, error) {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		return "", fmt.Errorf("generate random password: %w", err)
	}
	randSecret := hex.EncodeToString(b)
	out, err := exec.Command("openssl", "passwd", "-6", randSecret).Output() // #nosec G204 -- openssl with generated random string
	if err != nil {
		return "", fmt.Errorf("openssl passwd -6 failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	hash := strings.TrimSpace(string(out))
	if hash == "" {
		return "", fmt.Errorf("openssl passwd -6 returned an empty hash")
	}
	return hash, nil
}
