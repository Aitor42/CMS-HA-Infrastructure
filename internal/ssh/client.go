package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Pool manages a thread-safe pool of SSH connections to multiple hosts.
type Pool struct {
	mu             sync.Mutex
	keyPath        string
	clients        map[string]*ssh.Client
	config         *ssh.ClientConfig
	connectTimeout time.Duration
}

// NewPool initializes a new SSH connection pool using the provided private key.
func NewPool(keyPath string, connectTimeout time.Duration) (*Pool, error) {
	p := &Pool{
		keyPath:        keyPath,
		clients:        make(map[string]*ssh.Client),
		connectTimeout: connectTimeout,
	}

	// If key file exists, initialize immediately
	if _, err := os.Stat(keyPath); err == nil {
		if err := p.initKey(); err != nil {
			return nil, err
		}
	}

	return p, nil
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
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         p.connectTimeout,
	}
	return nil
}

// getClient returns a cached connection or establishes a new one.
func (p *Pool) getClient(host string) (*ssh.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.config == nil {
		if err := p.initKey(); err != nil {
			return nil, err
		}
	}

	client, exists := p.clients[host]
	if exists {
		// Basic liveness check could be added here
		return client, nil
	}

	slog.Debug("establishing new ssh connection", "host", host)
	addr := host
	if filepath.Ext(host) == "" && !containsPort(host) { // simple check, assumes port 22 if not specified
		addr = host + ":22"
	}

	client, err := ssh.Dial("tcp", addr, p.config)
	if err != nil {
		return nil, fmt.Errorf("failed to dial %s: %w", host, err)
	}

	p.clients[host] = client
	return client, nil
}

func containsPort(host string) bool {
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return true
		}
	}
	return false
}

func (p *Pool) invalidateClient(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if client, ok := p.clients[host]; ok {
		client.Close()
		delete(p.clients, host)
	}
}

// RunCommand executes a command remotely and captures output.
func (p *Pool) RunCommand(ctx context.Context, host, cmd string) (string, string, int, error) {
	client, err := p.getClient(host)
	if err != nil {
		return "", "", -1, err
	}

	session, err := client.NewSession()
	if err != nil {
		// Stale connection retry once
		p.invalidateClient(host)
		client, err = p.getClient(host)
		if err != nil {
			return "", "", -1, err
		}
		session, err = client.NewSession()
		if err != nil {
			return "", "", -1, fmt.Errorf("failed to create session on %s: %w", host, err)
		}
	}
	defer session.Close()

	var stdoutBuf, stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	done := make(chan error, 1)
	go func() {
		done <- session.Run(cmd)
	}()

	select {
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return "", "", -1, ctx.Err()
	case err := <-done:
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*ssh.ExitError); ok {
				exitCode = exitErr.ExitStatus()
			} else {
				exitCode = -1
			}
		}
		return stdoutBuf.String(), stderrBuf.String(), exitCode, err
	}
}

// RunScript creates a temporary script, executes it, and cleans it up.
func (p *Pool) RunScript(ctx context.Context, host, script string) (string, error) {
	tmpPath := fmt.Sprintf("/tmp/script_%d.sh", time.Now().UnixNano())

	err := p.CopyContent(ctx, host, []byte(script), tmpPath, 0700)
	if err != nil {
		return "", fmt.Errorf("failed to upload script to %s: %w", host, err)
	}
	defer func() {
		// Clean up
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p.RunCommand(cleanupCtx, host, "rm -f "+tmpPath)
	}()

	stdout, stderr, exitCode, err := p.RunCommand(ctx, host, tmpPath)
	if err != nil || exitCode != 0 {
		return stdout, fmt.Errorf("script execution failed on %s (exit %d): %v\nStderr: %s", host, exitCode, err, stderr)
	}

	return stdout, nil
}

// CopyFile uploads a local file to the remote host.
func (p *Pool) CopyFile(ctx context.Context, host, localPath, remotePath string) error {
	content, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("failed to read local file %s: %w", localPath, err)
	}
	
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	return p.CopyContent(ctx, host, content, remotePath, info.Mode())
}

// CopyContent writes in-memory content to a remote file via SFTP.
func (p *Pool) CopyContent(ctx context.Context, host string, content []byte, remotePath string, mode os.FileMode) error {
	client, err := p.getClient(host)
	if err != nil {
		return err
	}

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		// Stale connection retry once
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

	file, err := sftpClient.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("failed to open remote file %s: %w", remotePath, err)
	}
	defer file.Close()

	file.Chmod(mode)

	_, err = io.Copy(file, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("failed to write content to %s: %w", remotePath, err)
	}

	return nil
}

// CopyDir recursively uploads a directory.
func (p *Pool) CopyDir(ctx context.Context, host, localDir, remoteDir string) error {
	client, err := p.getClient(host)
	if err != nil {
		return err
	}

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("failed to create sftp client for %s: %w", host, err)
	}
	defer sftpClient.Close()

	return filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		
		remPath := filepath.ToSlash(filepath.Join(remoteDir, relPath))

		if info.IsDir() {
			if err := ctx.Err(); err != nil {
				return err
			}
			err = sftpClient.MkdirAll(remPath)
			if err != nil {
				slog.Warn("sftp mkdir issue (might exist)", "path", remPath, "err", err)
			}
			return nil
		}

		return p.CopyFile(ctx, host, path, remPath)
	})
}

// ReadFile reads remote file content via SFTP.
func (p *Pool) ReadFile(ctx context.Context, host, remotePath string) ([]byte, error) {
	client, err := p.getClient(host)
	if err != nil {
		return nil, err
	}

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("failed to create sftp client on %s: %w", host, err)
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

	var buf bytes.Buffer
	_, err = io.Copy(&buf, file)
	if err != nil {
		return nil, fmt.Errorf("failed to read remote file %s: %w", remotePath, err)
	}

	return buf.Bytes(), nil
}

// WaitForSSH polls until SSH connection succeeds, with configurable interval (default 10s).
func (p *Pool) WaitForSSH(ctx context.Context, host string, timeout time.Duration) error {
	slog.Info("waiting for ssh to become available", "host", host, "timeout", timeout)
	
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		client, err := p.getClient(host)
		if err == nil {
			session, err := client.NewSession()
			if err == nil {
				session.Close()
				slog.Info("ssh is available", "host", host)
				return nil
			}
		}

		select {
		case <-pollCtx.Done():
			return fmt.Errorf("timed out waiting for ssh on %s: %w", host, pollCtx.Err())
		case <-ticker.C:
			// Retry
		}
	}
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
}
