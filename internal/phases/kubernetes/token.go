package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
)

// tokenFingerprint returns a short, non-reversible identifier for logs. The
// token itself must never be logged.
func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6])
}

func (p *Phase) readJoinToken(ctx context.Context, master1 config.NodeDetail) (string, error) {
	var token string
	err := retry.DoContext(ctx, retry.Config{MaxAttempts: 20, Interval: 3 * time.Second, Timeout: 60 * time.Second}, func(attemptCtx context.Context) error {
		tok, _, _, err := p.pool.RunCommand(attemptCtx, master1.IP, "cat /var/lib/rancher/k3s/server/node-token")
		if err != nil || strings.TrimSpace(tok) == "" {
			return fmt.Errorf("node-token not ready yet")
		}
		token = strings.TrimSpace(tok)
		if strings.ContainsAny(token, " \t\r\n") {
			return fmt.Errorf("node-token contains whitespace; refusing to distribute a corrupt token")
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to get k3s token: %w", err)
	}
	return token, nil
}

// distributeJoinToken writes the token through a 0600 file instead of argv so
// it is not exposed in /proc/*/cmdline on the joining nodes.
func (p *Phase) distributeJoinToken(ctx context.Context, token string) error {
	logging.Info("Distributing K3s join token to joining nodes (file 0600, not argv)...")
	const joinTokenPath = "/root/.k3s-join-token"
	for _, joinIP := range p.joiningNodeIPs() {
		if err := p.pool.CopyContent(ctx, joinIP, []byte(token), joinTokenPath, 0600); err != nil {
			return fmt.Errorf("failed to upload k3s token to %s: %w", joinIP, err)
		}
	}
	return nil
}

func (p *Phase) joiningNodeIPs() []string {
	var ips []string
	for _, m := range p.cfg.Nodes.Masters[1:] {
		if m.IP != "" {
			ips = append(ips, m.IP)
		}
	}
	for _, w := range p.cfg.Nodes.Workers {
		if w.IP != "" {
			ips = append(ips, w.IP)
		}
	}
	return ips
}
