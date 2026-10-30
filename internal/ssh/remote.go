package ssh

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/shell"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ReadFile reads remote file content.
func (p *Pool) ReadFile(ctx context.Context, host, remotePath string) ([]byte, error) {
	if p.elevated() {
		out, stderr, code, err := p.RunCommand(ctx, host, "cat -- "+shell.Quote(remotePath))
		if err != nil || code != 0 {
			return nil, fmt.Errorf("failed to read remote file %s (exit %d): %v %s", remotePath, code, err, strings.TrimSpace(stderr))
		}
		if len(out) > maxRemoteFileRead {
			return nil, fmt.Errorf("remote file %s exceeds the %d MiB read limit", remotePath, maxRemoteFileRead>>20)
		}
		return []byte(out), nil
	}

	client, err := p.getClient(host)
	if err != nil {
		return nil, err
	}

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		p.retryCount.Add(1)
		p.invalidateClient(host)
		client, err = p.getClient(host)
		if err != nil {
			return nil, err
		}
		sftpClient, err = sftp.NewClient(client)
		if err != nil {
			return nil, fmt.Errorf("failed to create sftp client on %s: %w", host, err)
		}
	}
	defer sftpClient.Close()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	file, err := sftpClient.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open remote file %s: %w", remotePath, err)
	}
	defer file.Close()

	// Cap the read: remote logs can be huge and unbounded io.Copy would
	// exhaust memory when the caller expects a config-like file.
	data, err := io.ReadAll(io.LimitReader(file, maxRemoteFileRead+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read remote file %s: %w", remotePath, err)
	}
	if len(data) > maxRemoteFileRead {
		return nil, fmt.Errorf("remote file %s exceeds the %d MiB read limit", remotePath, maxRemoteFileRead>>20)
	}

	return data, nil
}

// WaitForSSH polls until SSH connection succeeds using adaptive backoff (starting at 1s up to 5s).
func (p *Pool) WaitForSSH(ctx context.Context, host string, timeout time.Duration) error {
	slog.Info("waiting for ssh to become available", "host", host, "timeout", timeout)

	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	interval := 1 * time.Second
	maxInterval := 5 * time.Second
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		client, err := p.getClient(host)
		if err == nil {
			session, err := client.NewSession()
			if err == nil {
				session.Close()
				slog.Info("ssh is available", "host", host)
				return nil
			}
			p.invalidateClient(host)
		}

		select {
		case <-pollCtx.Done():
			return fmt.Errorf("timed out waiting for ssh on %s: %w", host, pollCtx.Err())
		case <-timer.C:
			interval = time.Duration(float64(interval) * 1.5)
			if interval > maxInterval {
				interval = maxInterval
			}
			timer.Reset(interval)
		}
	}
}

// IsListening reports whether host accepts a TCP connection on the SSH port,
// without attempting to authenticate. During a batch install this is what
// tells the live installer (reachable, but refusing our key by design) apart
// from a guest whose network has already gone away.
func IsListening(ctx context.Context, host string, timeout time.Duration) bool {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", ensurePort(host, "22"))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// CanLogin reports whether the pool can authenticate and open a session on
// host, i.e. whether the system answering is the installed one (which trusts
// the deployment key) rather than the live installer.
func (p *Pool) CanLogin(ctx context.Context, host string) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	client, err := p.getClient(host)
	if err != nil {
		return false
	}
	session, err := client.NewSession()
	if err != nil {
		p.invalidateClient(host)
		return false
	}
	_ = session.Close()
	return true
}

// ForgetHost drops any host key pinned for host, so a machine whose identity
// legitimately changed (a node that was reinstalled) is trusted again on first
// contact. It is a no-op when no pinned store is configured.
func (p *Pool) ForgetHost(host string) {
	p.invalidateClient(host)

	if p.knownHostsPath == "" {
		return
	}

	// Serialise with verifyHostKey: a concurrent first-contact handshake may be
	// about to append the old key. Dropping the file entry before taking the
	// lock would let that append resurrect it after the removal.
	p.knownHostsMu.Lock()
	defer p.knownHostsMu.Unlock()

	if err := RemoveHostsFromKnownHosts(p.knownHostsPath, []string{host}); err != nil {
		slog.Warn("could not drop the pinned host key",
			"host", host, "path", p.knownHostsPath, "error", err)
	}

	p.knownHostsCallback = nil
}

// AptLockWaitSnippet is the shared shell preamble that waits for
// unattended-upgrades, dpkg or cloud-init to release apt locks before apt
// operations; callers embed it at the top of their install scripts.
//
// `cloud-init status --wait` has no timeout of its own and can stay in
// "running" indefinitely on a host whose seed or snapd seeding is stuck; an
// unbounded wait would hang the deploy at a log line with no progress. Bound
// it and fall back to the lock probe below.
const AptLockWaitSnippet = `timeout 120 cloud-init status --wait >/dev/null 2>&1 || true
for i in {1..30}; do
    if ! fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1 && ! fuser /var/lib/apt/lists/lock >/dev/null 2>&1; then
        break
    fi
    sleep 2
done
`

// aptLockWaitScript waits for cloud-init and then for the dpkg/apt locks. It
// fails instead of reporting success when a lock is still held after 120s.
const aptLockWaitScript = `
timeout 120 cloud-init status --wait >/dev/null 2>&1 || true
for i in {1..60}; do
    if ! fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1 && ! fuser /var/lib/apt/lists/lock >/dev/null 2>&1; then
        exit 0
    fi
    sleep 2
done
echo "apt lock still held after 120s (unattended-upgrades/dpkg?)" >&2
exit 1
`

// aptLockWaitTimeout bounds the whole wait. The remote script is bounded too,
// but an SSH session that never returns (frozen guest, dropped network) would
// otherwise block the deploy forever; the context makes that a normal error.
var aptLockWaitTimeout = 5 * time.Minute

// WaitForAptLock waits for unattended-upgrades, dpkg or cloud-init to release
// apt locks on the target host. Fails (exit 1) if no lock is released in
// time, instead of falsely reporting success.
func (p *Pool) WaitForAptLock(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, aptLockWaitTimeout)
	defer cancel()

	_, err := p.RunScript(ctx, host, aptLockWaitScript)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("apt lock wait on %s exceeded %s: %w", host, aptLockWaitTimeout, err)
	}
	return err
}

// Close closes all connections in the pool.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for host, client := range p.clients {
		client.Close()
		slog.Debug("closed ssh connection", "host", host)
	}
	p.clients = make(map[string]*ssh.Client)
	for _, client := range p.evicted {
		client.Close()
	}
	p.evicted = nil
}
