// Package testutil holds in-memory implementations shared by tests. Nothing in
// the production binary imports it.
package testutil

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
	"github.com/pkg/sftp"
)

// Command is one recorded remote execution.
type Command struct {
	Host string
	Cmd  string
}

// FakeExecutor is an in-memory ssh.Executor for phase tests. It records every
// command and every remote file it is asked to write, and lets a test program
// the command output per host.
//
// The zero value is usable: commands succeed with empty output, waits return
// nil and writes are recorded in Files.
type FakeExecutor struct {
	mu sync.Mutex

	// RunFunc, when set, supplies RunCommand results.
	RunFunc func(ctx context.Context, host, cmd string) (stdout, stderr string, exitCode int, err error)
	// WaitErr maps a host to the error WaitForSSH must return.
	WaitErr map[string]error
	// CopyErr maps a host to the error CopyContent and CopyFile must return.
	CopyErr map[string]error
	// ReadFiles supplies ReadFile contents, keyed by host and remote path.
	ReadFiles map[string]map[string][]byte

	Commands   []Command
	Scripts    []Command
	CopiedDirs []Command
	CopiedTars []Command
	// Files maps host -> remote path -> content written through CopyFile,
	// CopyContent or CopyDir.
	Files map[string]map[string][]byte

	StatsValue ssh.Stats
	Closed     bool
}

var _ ssh.Executor = (*FakeExecutor)(nil)

func (f *FakeExecutor) recordCommand(host, cmd string) func(context.Context, string, string) (string, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Commands = append(f.Commands, Command{Host: host, Cmd: cmd})
	return f.RunFunc
}

func (f *FakeExecutor) storeFile(host, remotePath string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Files == nil {
		f.Files = make(map[string]map[string][]byte)
	}
	if f.Files[host] == nil {
		f.Files[host] = make(map[string][]byte)
	}
	f.Files[host][remotePath] = append([]byte(nil), content...)
}

// RunCommand records the command and delegates to RunFunc when it is set.
func (f *FakeExecutor) RunCommand(ctx context.Context, host, cmd string) (string, string, int, error) {
	fn := f.recordCommand(host, cmd)
	if fn != nil {
		return fn(ctx, host, cmd)
	}
	return "", "", 0, nil
}

// RunCommandToWriter mirrors RunCommand and forwards stdout to dst.
func (f *FakeExecutor) RunCommandToWriter(ctx context.Context, host, cmd string, dst io.Writer) (string, int, error) {
	stdout, _, code, err := f.RunCommand(ctx, host, cmd)
	if dst != nil && stdout != "" {
		if _, werr := io.WriteString(dst, stdout); werr != nil && err == nil {
			err = werr
		}
	}
	return stdout, code, err
}

// RunCommandStdin mirrors RunCommand; the fake ignores stdin.
func (f *FakeExecutor) RunCommandStdin(ctx context.Context, host, cmd string, _ io.Reader, dst io.Writer) (string, int, error) {
	return f.RunCommandToWriter(ctx, host, cmd, dst)
}

// RunScript records the script separately and delegates to RunFunc when set.
func (f *FakeExecutor) RunScript(ctx context.Context, host, script string) (string, error) {
	f.mu.Lock()
	f.Scripts = append(f.Scripts, Command{Host: host, Cmd: script})
	fn := f.RunFunc
	f.mu.Unlock()
	if fn != nil {
		stdout, _, code, err := fn(ctx, host, script)
		if err != nil {
			return stdout, err
		}
		if code != 0 {
			return stdout, fmt.Errorf("script exited with code %d", code)
		}
		return stdout, nil
	}
	return "", nil
}

// CopyContent records the remote write and honours CopyErr for the host.
func (f *FakeExecutor) CopyContent(_ context.Context, host string, content []byte, remotePath string, _ os.FileMode) error {
	f.mu.Lock()
	err := f.CopyErr[host]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.storeFile(host, remotePath, content)
	return nil
}

// CopyFile reads the local file, then records it as a remote write.
func (f *FakeExecutor) CopyFile(_ context.Context, host, localPath, remotePath string) error {
	f.mu.Lock()
	err := f.CopyErr[host]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	content, err := os.ReadFile(filepath.Clean(localPath)) // #nosec G304 -- test helper reading a test path
	if err != nil {
		return err
	}
	f.storeFile(host, remotePath, content)
	return nil
}

// CopyDir records the tree and mirrors regular files into Files.
func (f *FakeExecutor) CopyDir(_ context.Context, host, localDir, remoteDir string) error {
	f.mu.Lock()
	f.CopiedDirs = append(f.CopiedDirs, Command{Host: host, Cmd: localDir + " -> " + remoteDir})
	f.mu.Unlock()
	return filepath.Walk(localDir, func(localPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(localDir, localPath)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(filepath.Clean(localPath)) // #nosec G304 -- test helper reading a test path
		if err != nil {
			return err
		}
		f.storeFile(host, path.Join(remoteDir, filepath.ToSlash(rel)), content)
		return nil
	})
}

// CopyTar records the archive size; callers that need its contents should use
// CopyDir or assert on Tarball.
func (f *FakeExecutor) CopyTar(_ context.Context, host, remoteDir string, tarball []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CopiedTars = append(f.CopiedTars, Command{Host: host, Cmd: fmt.Sprintf("%s (%d bytes)", remoteDir, len(tarball))})
	return nil
}

// ReadFile returns the programmed content for host and remotePath.
func (f *FakeExecutor) ReadFile(_ context.Context, host, remotePath string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if byPath := f.ReadFiles[host]; byPath != nil {
		if content, ok := byPath[remotePath]; ok {
			return append([]byte(nil), content...), nil
		}
	}
	return nil, fmt.Errorf("read %s on %s: %w", remotePath, host, os.ErrNotExist)
}

// WithSFTP is not implemented: a real *sftp.Client cannot be faked without a
// server. Tests that need it should exercise the caller through a higher-level
// helper instead.
func (f *FakeExecutor) WithSFTP(_ context.Context, _ string, _ func(*sftp.Client) error) error {
	return fmt.Errorf("FakeExecutor.WithSFTP is not implemented")
}

// WaitForSSH returns the host's programmed error, or nil.
func (f *FakeExecutor) WaitForSSH(_ context.Context, host string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.WaitErr[host]
}

// WaitForAptLock always succeeds.
func (f *FakeExecutor) WaitForAptLock(_ context.Context, _ string) error { return nil }

// CanLogin reports whether WaitForSSH is programmed to fail for the host.
func (f *FakeExecutor) CanLogin(_ context.Context, host string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.WaitErr[host] == nil
}

// ForgetHost clears the programmed wait error for the host.
func (f *FakeExecutor) ForgetHost(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.WaitErr, host)
}

// RunParallel runs the command sequentially, preserving input order. Tests do
// not need real concurrency, and determinism keeps assertions simple.
func (f *FakeExecutor) RunParallel(ctx context.Context, hosts []string, cmd string) []ssh.ParallelResult {
	results := make([]ssh.ParallelResult, 0, len(hosts))
	for _, host := range hosts {
		stdout, _, code, err := f.RunCommand(ctx, host, cmd)
		results = append(results, ssh.ParallelResult{Host: host, Output: stdout, ExitCode: code, Err: err})
	}
	return results
}

// RunParallelFunc runs each task sequentially with the fake as the executor.
func (f *FakeExecutor) RunParallelFunc(ctx context.Context, tasks map[string]func(context.Context, ssh.Executor) error) []ssh.ParallelResult {
	results := make([]ssh.ParallelResult, 0, len(tasks))
	for host, task := range tasks {
		results = append(results, ssh.ParallelResult{Host: host, Err: task(ctx, f)})
	}
	return results
}

// WaitForAllSSH checks every host sequentially and returns the first failure.
func (f *FakeExecutor) WaitForAllSSH(ctx context.Context, hosts []string, timeout time.Duration) error {
	for _, host := range hosts {
		if err := f.WaitForSSH(ctx, host, timeout); err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
	}
	return nil
}

// Stats returns the programmed counters.
func (f *FakeExecutor) Stats() ssh.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.StatsValue
}

// Close marks the fake as closed.
func (f *FakeExecutor) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Closed = true
}

// FakeSecret assembles an obviously fake credential value from its parts.
// Tests only exercise plumbing — they must never carry real-looking secret
// material, so fixture values are built here instead of written as literals.
func FakeSecret(parts ...string) string {
	return strings.Join(append([]string{"test"}, parts...), "-")
}
