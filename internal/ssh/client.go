package ssh

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/shell"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Pool manages a thread-safe pool of SSH connections to multiple hosts.
type Pool struct {
	mu             sync.Mutex
	keyPath        string
	user           string
	knownHostsPath string
	clients        map[string]*ssh.Client
	config         *ssh.ClientConfig
	connectTimeout time.Duration
	// knownHostsMu guards knownHostsCallback. It is deliberately separate from
	// mu so host-key verification (which does disk I/O) never serializes
	// connection setup across parallel handshakes.
	knownHostsMu sync.Mutex
	// knownHostsCallback verifies (and pins on first contact) host keys.
	knownHostsCallback ssh.HostKeyCallback
	// evicted holds clients removed from the pool but potentially still in use
	// by in-flight operations; they are definitively closed in Close(). Closing
	// here instead would sever sessions of concurrent goroutines.
	evicted []*ssh.Client

	// Counters exposed through Stats. They are read after a deployment to
	// populate timings.json and are updated from parallel host tasks.
	commandCount atomic.Int64
	retryCount   atomic.Int64
	bytesSent    atomic.Int64
}

// Stats is a snapshot of the pool's activity counters.
type Stats struct {
	Commands       int64 `json:"commands"`
	ConnectRetries int64 `json:"connect_retries"`
	BytesUploaded  int64 `json:"bytes_uploaded"`
}

// Stats returns the pool's activity counters. They are cumulative for the
// lifetime of the pool and safe to read while commands are in flight.
func (p *Pool) Stats() Stats {
	return Stats{
		Commands:       p.commandCount.Load(),
		ConnectRetries: p.retryCount.Load(),
		BytesUploaded:  p.bytesSent.Load(),
	}
}

// countingReader reports every byte read through it to an atomic counter, so
// uploads of arbitrarily large files (the 3.9 GB ISO) stay O(1) in memory.
type countingReader struct {
	reader io.Reader
	total  *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.total.Add(int64(n))
	return n, err
}

// Options configures an SSH connection pool.
type Options struct {
	// User is the non-root deploy account. Empty defaults to "root" (legacy).
	User string
	// KnownHostsFile is the TOFU known_hosts database. Empty disables host-key
	// pinning entirely: every connection is treated as first contact.
	KnownHostsFile string
	// ConnectTimeout bounds the TCP connect and handshake.
	ConnectTimeout time.Duration
}

const maxCapturedStderr = 64 * 1024

// maxRemoteFileRead caps SFTP reads and rejects larger files after a sudo `cat`
// so a huge remote file cannot exhaust memory.
const maxRemoteFileRead = 32 << 20 // 32 MiB

type cappedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	originalLength := len(p)
	remaining := maxCapturedStderr - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
			b.truncated = true
		}
		_, _ = b.Buffer.Write(p)
	} else if originalLength > 0 {
		b.truncated = true
	}
	return originalLength, nil
}

func (b *cappedBuffer) String() string {
	result := b.Buffer.String()
	if b.truncated {
		result += "\n[stderr truncated at 64 KiB]"
	}
	return result
}

// NewPool initializes a pool using the provided private key. It keeps the
// historical signature (root user, no persistent host-key store); callers that
// want the hardened non-root/TOFU behaviour use NewPoolWithOptions.
func NewPool(keyPath string, connectTimeout time.Duration) (*Pool, error) {
	return NewPoolWithOptions(keyPath, Options{ConnectTimeout: connectTimeout})
}

// NewPoolWithOptions initializes a new SSH connection pool using the provided
// private key and options.
func NewPoolWithOptions(keyPath string, opts Options) (*Pool, error) {
	user := strings.TrimSpace(opts.User)
	if user == "" {
		user = "root"
	}

	p := &Pool{
		keyPath:        keyPath,
		user:           user,
		knownHostsPath: opts.KnownHostsFile,
		clients:        make(map[string]*ssh.Client),
		connectTimeout: opts.ConnectTimeout,
	}

	if _, err := os.Stat(keyPath); err == nil {
		if err := p.initKey(); err != nil {
			return nil, err
		}
	}

	return p, nil
}

// elevated reports whether commands must be run through sudo (any non-root
// deploy account).
func (p *Pool) elevated() bool {
	return p.user != "" && p.user != "root"
}

// remoteCommand wraps cmd so it runs as root when the pool uses a non-root
// deploy account. `-n` makes sudo fail fast instead of blocking on a password
// prompt, and `-H` sets HOME to the target user's home so root-owned paths
// (kubeconfig, .step, ...) resolve correctly.
func (p *Pool) remoteCommand(cmd string) string {
	if !p.elevated() {
		return cmd
	}
	return "sudo -n -H bash -c " + shell.Quote(cmd)
}

func (p *Pool) initKey() error {
	keyBytes, err := os.ReadFile(p.keyPath)
	if err != nil {
		return fmt.Errorf("failed to read private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return fmt.Errorf("failed to parse private key: %w", err)
	}

	p.config = &ssh.ClientConfig{
		User: p.user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: p.verifyHostKey,
		Timeout:         p.connectTimeout,
	}
	return nil
}

// verifyHostKey implements trust-on-first-use (TOFU): a host key seen for the
// first time is pinned (and persisted to known_hosts), while a key that differs
// from the pinned one is rejected to defeat man-in-the-middle attacks.
func (p *Pool) verifyHostKey(hostname string, remote net.Addr, key ssh.PublicKey) error {
	p.knownHostsMu.Lock()
	defer p.knownHostsMu.Unlock()

	if p.knownHostsCallback == nil {
		cb, err := p.loadKnownHostsCallback()
		if err != nil {
			return err
		}
		p.knownHostsCallback = cb
	}

	err := p.knownHostsCallback(hostname, remote, key)
	if err == nil {
		return nil
	}

	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
		// Unknown host: this is the first time we see its key.
		return p.trustHostKey(hostname, remote, key)
	}
	// Known host with a different key (or a malformed store): fail closed.
	return fmt.Errorf("SSH host key verification failed for %s: %w", hostname, err)
}

// loadKnownHostsCallback builds a verifier from the on-disk store. When the
// file does not exist yet every host is reported as unknown (first contact).
func (p *Pool) loadKnownHostsCallback() (ssh.HostKeyCallback, error) {
	if p.knownHostsPath == "" {
		return func(string, net.Addr, ssh.PublicKey) error { return &knownhosts.KeyError{} }, nil
	}
	if _, err := os.Stat(p.knownHostsPath); err != nil {
		if os.IsNotExist(err) {
			return func(string, net.Addr, ssh.PublicKey) error { return &knownhosts.KeyError{} }, nil
		}
		return nil, fmt.Errorf("failed to stat known_hosts %s: %w", p.knownHostsPath, err)
	}
	cb, err := knownhosts.New(p.knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load known_hosts %s: %w", p.knownHostsPath, err)
	}
	return cb, nil
}

// trustHostKey accepts a newly discovered host key. When a store is configured
// it persists and pins the key (0600) so later connections verify it; without a
// store every connection is trusted as first contact.
func (p *Pool) trustHostKey(hostname string, remote net.Addr, key ssh.PublicKey) error {
	addr := hostname
	if remote != nil {
		addr = remote.String()
	}
	normalized := knownhosts.Normalize(addr)
	line := knownhosts.Line([]string{normalized}, key)

	if p.knownHostsPath != "" {
		if err := os.MkdirAll(filepath.Dir(p.knownHostsPath), 0o700); err != nil {
			return fmt.Errorf("failed to create known_hosts directory: %w", err)
		}
		f, err := os.OpenFile(p.knownHostsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("failed to open known_hosts %s: %w", p.knownHostsPath, err)
		}
		_, werr := fmt.Fprintln(f, line)
		cerr := f.Close()
		if werr != nil {
			return fmt.Errorf("failed to persist host key for %s: %w", hostname, werr)
		}
		if cerr != nil {
			return fmt.Errorf("failed to close known_hosts %s: %w", p.knownHostsPath, cerr)
		}
		if cb, err := knownhosts.New(p.knownHostsPath); err == nil {
			p.knownHostsCallback = cb
		}
	}

	slog.Warn("trusting SSH host key on first use (TOFU)",
		"host", hostname, "fingerprint", ssh.FingerprintSHA256(key))
	return nil
}

// getClient returns a cached connection or establishes a new one.
func (p *Pool) getClient(host string) (*ssh.Client, error) {
	p.mu.Lock()
	if p.config == nil {
		if err := p.initKey(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
	}

	client, exists := p.clients[host]
	cfg := p.config
	p.mu.Unlock()

	if exists {
		return client, nil
	}

	slog.Debug("establishing new ssh connection", "host", host)
	addr := ensurePort(host, "22")

	newClient, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to dial %s: %w", host, err)
	}

	p.mu.Lock()
	if existing, ok := p.clients[host]; ok {
		p.mu.Unlock()
		newClient.Close()
		return existing, nil
	}
	p.clients[host] = newClient
	p.mu.Unlock()

	return newClient, nil
}

func ensurePort(host, defaultPort string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, defaultPort)
}

func (p *Pool) invalidateClient(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if client, ok := p.clients[host]; ok {
		// Remove but do not close: another goroutine may still be using this
		// connection. Definitive close happens in Close() to avoid cutting
		// in-flight sessions of concurrent host tasks.
		delete(p.clients, host)
		p.evicted = append(p.evicted, client)
	}
}

// RunCommand executes a command remotely and captures output.
func (p *Pool) RunCommand(ctx context.Context, host, cmd string) (string, string, int, error) {
	var stdout bytes.Buffer
	stderr, exitCode, err := p.RunCommandToWriter(ctx, host, cmd, &stdout)
	return stdout.String(), stderr, exitCode, err
}

// RunCommandToWriter executes a remote command and streams stdout to dst.
// Memory use stays bounded by SSH's internal buffers rather than output size.
func (p *Pool) RunCommandToWriter(ctx context.Context, host, cmd string, dst io.Writer) (string, int, error) {
	return p.RunCommandStdin(ctx, host, cmd, nil, dst)
}

// RunCommandStdin executes a remote command, optionally feeding stdin and
// streaming stdout to dst. Privileged commands run through sudo when the pool
// uses a non-root deploy account.
func (p *Pool) RunCommandStdin(ctx context.Context, host, cmd string, stdin io.Reader, dst io.Writer) (string, int, error) {
	if dst == nil {
		return "", -1, fmt.Errorf("stdout writer cannot be nil")
	}
	p.commandCount.Add(1)
	client, err := p.getClient(host)
	if err != nil {
		return "", -1, err
	}

	session, err := client.NewSession()
	if err != nil {
		// Stale connection retry once
		p.retryCount.Add(1)
		p.invalidateClient(host)
		client, err = p.getClient(host)
		if err != nil {
			return "", -1, err
		}
		session, err = client.NewSession()
		if err != nil {
			return "", -1, fmt.Errorf("failed to create session on %s: %w", host, err)
		}
	}
	defer session.Close()

	var stderrBuf cappedBuffer
	session.Stdout = dst
	session.Stderr = &stderrBuf
	if stdin != nil {
		session.Stdin = stdin
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Run(p.remoteCommand(cmd))
	}()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		// Wait for the writer to finish, bounded: some remotes ignore the
		// kill signal and the caller would otherwise hang forever.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		// Preserve the captured stderr: it often explains why the command
		// hung before the context was cancelled.
		return stderrBuf.String(), -1, ctx.Err()
	case err := <-done:
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*ssh.ExitError); ok {
				exitCode = exitErr.ExitStatus()
			} else {
				exitCode = -1
			}
		}
		return stderrBuf.String(), exitCode, err
	}
}

// RunScript creates a temporary script, executes it, and cleans it up.
func (p *Pool) RunScript(ctx context.Context, host, script string) (string, error) {
	// Random name: a predictable path would let a local user pre-create a
	// symlink to a sensitive file before the SFTP write lands.
	var nameSuffix [12]byte
	if _, err := crand.Read(nameSuffix[:]); err != nil {
		return "", fmt.Errorf("generate script name: %w", err)
	}
	tmpPath := fmt.Sprintf("/tmp/script_%s.sh", hex.EncodeToString(nameSuffix[:]))

	err := p.CopyContent(ctx, host, []byte(script), tmpPath, 0700)
	if err != nil {
		return "", fmt.Errorf("failed to upload script to %s: %w", host, err)
	}

	// The remote shell deletes its own temporary script on exit (EXIT trap), so
	// no separate cleanup session is needed. An EXIT trap does not override the
	// exit status of the wrapped script, so failure detection is unchanged.
	stdout, stderr, exitCode, err := p.RunCommand(ctx, host, "trap 'rm -f "+tmpPath+"' EXIT; bash "+tmpPath)
	if err != nil || exitCode != 0 {
		return stdout, fmt.Errorf("script execution failed on %s (exit %d): %v\nStderr: %s", host, exitCode, err, stderr)
	}

	return stdout, nil
}
