package ssh

import (
	"archive/tar"
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
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
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
	return "sudo -n -H bash -c " + shellQuote(cmd)
}

// shellQuote wraps s in single quotes, escaping embedded single quotes, so it
// can be safely embedded in a `bash -c '...'` invocation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// CopyFile uploads a local file to the remote host using direct streaming (O(1) memory).
func (p *Pool) CopyFile(ctx context.Context, host, localPath, remotePath string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("failed to stat local file %s: %w", localPath, err)
	}

	src, err := os.Open(filepath.Clean(localPath))
	if err != nil {
		return fmt.Errorf("failed to open local file %s: %w", localPath, err)
	}
	defer src.Close()

	return p.writeRemoteFile(ctx, host, src, remotePath, info.Mode())
}

// WithSFTP opens an SFTP client and executes fn, allowing multiple file transfers
// to reuse a single SFTP session and avoid repeated subsystem negotiation roundtrips.
//
// Note: when the pool uses a non-root deploy account, SFTP can only write where
// that account has permission (e.g. /tmp). Use CopyContent/CopyFile for
// root-owned destinations, which elevate transparently.
func (p *Pool) WithSFTP(ctx context.Context, host string, fn func(sftpClient *sftp.Client) error) error {
	return p.withSFTPClient(ctx, host, fn)
}

// withSFTPClient dials (retrying once on a stale connection) and runs fn with a
// fresh SFTP session.
func (p *Pool) withSFTPClient(ctx context.Context, host string, fn func(sftpClient *sftp.Client) error) error {
	client, err := p.getClient(host)
	if err != nil {
		return err
	}

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		p.retryCount.Add(1)
		p.invalidateClient(host)
		client, err = p.getClient(host)
		if err != nil {
			return err
		}
		sftpClient, err = sftp.NewClient(client)
		if err != nil {
			return fmt.Errorf("failed to create sftp client on %s: %w", host, err)
		}
	}
	defer sftpClient.Close()

	if err := ctx.Err(); err != nil {
		return err
	}

	return fn(sftpClient)
}

func copyFileWithSFTP(ctx context.Context, sftpClient *sftp.Client, localPath, remotePath string, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	srcFile, err := os.Open(filepath.Clean(localPath))
	if err != nil {
		return fmt.Errorf("failed to open local file %s: %w", localPath, err)
	}
	defer srcFile.Close()
	return writeRemoteFile(sftpClient, srcFile, remotePath, mode)
}

func writeRemoteFile(sftpClient *sftp.Client, src io.Reader, remotePath string, mode os.FileMode) error {
	// Write to a sidecar file and rename afterwards: on any failure the
	// previous version of remotePath stays intact (never half-overwritten).
	tmpPath := remotePath + ".cms-tmp"
	if err := writeRemoteFileAt(sftpClient, src, tmpPath, mode); err != nil {
		return err
	}
	if err := sftpClient.PosixRename(tmpPath, remotePath); err != nil {
		_ = sftpClient.Remove(tmpPath)
		return fmt.Errorf("failed to replace remote file %s: %w", remotePath, err)
	}
	return nil
}

// copyBufPool recycles the 256 KiB streaming buffer used by SFTP writes so a
// directory copy does not allocate a fresh buffer per file. The pooled value is
// a *[]byte (staticcheck SA6002: pointer-like values avoid boxing allocations).
var copyBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 256*1024)
		return &buf
	},
}

func writeRemoteFileAt(sftpClient *sftp.Client, src io.Reader, remotePath string, mode os.FileMode) error {
	remoteFile, err := sftpClient.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("failed to open remote file %s: %w", remotePath, err)
	}
	if err := remoteFile.Chmod(mode); err != nil {
		_ = remoteFile.Close()
		_ = sftpClient.Remove(remotePath)
		return fmt.Errorf("failed to set permissions on remote file %s: %w", remotePath, err)
	}

	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	if _, err := io.CopyBuffer(remoteFile, src, *bufp); err != nil {
		_ = remoteFile.Close()
		_ = sftpClient.Remove(remotePath)
		return fmt.Errorf("failed to stream content to %s: %w", remotePath, err)
	}
	if err := remoteFile.Close(); err != nil {
		_ = sftpClient.Remove(remotePath)
		return fmt.Errorf("failed to close remote file %s: %w", remotePath, err)
	}
	return nil
}

// CopyContent writes in-memory content to a remote file.
func (p *Pool) CopyContent(ctx context.Context, host string, content []byte, remotePath string, mode os.FileMode) error {
	return p.writeRemoteFile(ctx, host, bytes.NewReader(content), remotePath, mode)
}

// writeRemoteFile uploads src to remotePath atomically. With a non-root deploy
// account the bytes are streamed through sudo: `cat` into a sidecar file,
// `chmod`, then `mv` into place (mirroring the SFTP sidecar+rename strategy).
func (p *Pool) writeRemoteFile(ctx context.Context, host string, src io.Reader, remotePath string, mode os.FileMode) error {
	src = &countingReader{reader: src, total: &p.bytesSent}
	if !p.elevated() {
		return p.withSFTPClient(ctx, host, func(sftpClient *sftp.Client) error {
			return writeRemoteFile(sftpClient, src, remotePath, mode)
		})
	}

	tmpPath := remotePath + ".cms-tmp"
	script := fmt.Sprintf("set -e; mkdir -p %s; cat > %s; chmod %o %s; mv -f %s %s",
		shellQuote(path.Dir(remotePath)),
		shellQuote(tmpPath),
		mode.Perm(),
		shellQuote(tmpPath),
		shellQuote(tmpPath),
		shellQuote(remotePath),
	)
	stderr, code, err := p.RunCommandStdin(ctx, host, script, src, io.Discard)
	if err != nil {
		return fmt.Errorf("failed to write remote file %s: %w", remotePath, err)
	}
	if code != 0 {
		return fmt.Errorf("failed to write remote file %s (exit %d): %s", remotePath, code, strings.TrimSpace(stderr))
	}
	return nil
}

// CopyDir recursively uploads a directory, reusing a single SFTP session as
// root and streaming the whole tree as one sudo tar extraction otherwise.
func (p *Pool) CopyDir(ctx context.Context, host, localDir, remoteDir string) error {
	if p.elevated() {
		return p.copyDirElevated(ctx, host, localDir, remoteDir)
	}

	return p.withSFTPClient(ctx, host, func(sftpClient *sftp.Client) error {
		return filepath.Walk(localDir, func(localPath string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			relPath, err := filepath.Rel(localDir, localPath)
			if err != nil {
				return err
			}

			remPath := filepath.ToSlash(filepath.Join(remoteDir, relPath))

			if info.IsDir() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := sftpClient.MkdirAll(remPath); err != nil {
					// A real failure (permissions, ENOSPC) would surface much
					// later as a confusing file-copy error; fail here instead.
					return fmt.Errorf("failed to create remote directory %s: %w", remPath, err)
				}
				return nil
			}

			return copyFileWithSFTP(ctx, sftpClient, localPath, remPath, info.Mode())
		})
	})
}

// copyDirElevated uploads a directory using a single sudo tar extraction so it
// can land in root-owned locations (e.g. /etc/puppetlabs/code). This replaces
// one `sudo` round-trip per directory and per file with a single SSH session.
func (p *Pool) copyDirElevated(ctx context.Context, host, localDir, remoteDir string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	werr := filepath.Walk(localDir, func(localPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		relPath, err := filepath.Rel(localDir, localPath)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(localPath); err != nil {
				return err
			}
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(relPath)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(localPath) // #nosec G122 -- localPath is walked from the trusted repo tree (puppet/), not from untrusted input
		if err != nil {
			return err
		}
		_, cerr := io.Copy(tw, f)
		_ = f.Close()
		return cerr
	})
	if werr != nil {
		_ = tw.Close()
		return werr
	}
	if err := tw.Close(); err != nil {
		return err
	}

	return p.CopyTar(ctx, host, remoteDir, buf.Bytes())
}

// CopyTar extracts an in-memory tar archive under remoteDir in a single SSH
// session (through sudo for non-root deploy accounts). It lets callers push a
// whole tree — embedded manifests, directory uploads — without one round-trip
// per file. --no-same-owner keeps extracted files owned by the deploy/root
// account rather than the local uid/gid stored in the archive.
func (p *Pool) CopyTar(ctx context.Context, host, remoteDir string, tarball []byte) error {
	script := fmt.Sprintf("set -e; mkdir -p %s; tar -xf - -C %s --no-same-owner",
		shellQuote(remoteDir), shellQuote(remoteDir))
	stderr, code, err := p.RunCommandStdin(ctx, host, script, bytes.NewReader(tarball), io.Discard)
	if err != nil {
		return fmt.Errorf("failed to upload archive to %s: %w", remoteDir, err)
	}
	if code != 0 {
		return fmt.Errorf("failed to extract archive at %s (exit %d): %s", remoteDir, code, strings.TrimSpace(stderr))
	}
	return nil
}

// ReadFile reads remote file content.
func (p *Pool) ReadFile(ctx context.Context, host, remotePath string) ([]byte, error) {
	if p.elevated() {
		out, stderr, code, err := p.RunCommand(ctx, host, "cat -- "+shellQuote(remotePath))
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

	if err := RemoveHostsFromKnownHosts(p.knownHostsPath, []string{host}); err != nil {
		slog.Warn("could not drop the pinned host key",
			"host", host, "path", p.knownHostsPath, "error", err)
	}

	p.knownHostsMu.Lock()
	p.knownHostsCallback = nil
	p.knownHostsMu.Unlock()
}

// AptLockWaitSnippet is the shared shell preamble that waits for
// unattended-upgrades, dpkg or cloud-init to release apt locks before apt
// operations; callers embed it at the top of their install scripts.
const AptLockWaitSnippet = `cloud-init status --wait 2>/dev/null || true
for i in {1..30}; do
    if ! fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1 && ! fuser /var/lib/apt/lists/lock >/dev/null 2>&1; then
        break
    fi
    sleep 2
done
`

// WaitForAptLock waits for unattended-upgrades, dpkg or cloud-init to release
// apt locks on the target host. Fails (exit 1) if no lock is released in
// time, instead of falsely reporting success.
func (p *Pool) WaitForAptLock(ctx context.Context, host string) error {
	waitScript := `
cloud-init status --wait 2>/dev/null || true
for i in {1..60}; do
    if ! fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1 && ! fuser /var/lib/apt/lists/lock >/dev/null 2>&1; then
        exit 0
    fi
    sleep 2
done
echo "apt lock still held after 120s (unattended-upgrades/dpkg?)" >&2
exit 1
`
	_, err := p.RunScript(ctx, host, waitScript)
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
