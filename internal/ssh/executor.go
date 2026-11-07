package ssh

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/pkg/sftp"
)

// Executor is the subset of Pool the deployment phases and the utils helpers
// need. Phases depend on this interface instead of the concrete pool so a test
// can drive them with a fake; production code keeps using *Pool, which
// implements it.
//
// Keep this list to operations that are meaningful to both a real pool and a
// fake. Anything that inspects libvirt or the host belongs in its own
// dependency (the phases already take a *libvirt.Client separately).
type Executor interface {
	RunCommand(ctx context.Context, host, cmd string) (stdout, stderr string, exitCode int, err error)
	RunCommandToWriter(ctx context.Context, host, cmd string, dst io.Writer) (stdout string, exitCode int, err error)
	RunCommandStdin(ctx context.Context, host, cmd string, stdin io.Reader, dst io.Writer) (stdout string, exitCode int, err error)
	RunScript(ctx context.Context, host, script string) (string, error)

	CopyFile(ctx context.Context, host, localPath, remotePath string) error
	CopyContent(ctx context.Context, host string, content []byte, remotePath string, mode os.FileMode) error
	CopyDir(ctx context.Context, host, localDir, remoteDir string) error
	CopyTar(ctx context.Context, host, remoteDir string, tarball []byte) error
	ReadFile(ctx context.Context, host, remotePath string) ([]byte, error)
	WithSFTP(ctx context.Context, host string, fn func(client *sftp.Client) error) error

	WaitForSSH(ctx context.Context, host string, timeout time.Duration) error
	WaitForAptLock(ctx context.Context, host string) error
	CanLogin(ctx context.Context, host string) bool
	ForgetHost(host string)

	RunParallel(ctx context.Context, hosts []string, cmd string) []ParallelResult
	RunParallelFunc(ctx context.Context, tasks map[string]func(ctx context.Context, exec Executor) error) []ParallelResult
	WaitForAllSSH(ctx context.Context, hosts []string, timeout time.Duration) error

	Stats() Stats
	Close()
}

var _ Executor = (*Pool)(nil)
