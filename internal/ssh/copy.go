package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/shell"
	"github.com/pkg/sftp"
)

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
		shell.Quote(path.Dir(remotePath)),
		shell.Quote(tmpPath),
		mode.Perm(),
		shell.Quote(tmpPath),
		shell.Quote(tmpPath),
		shell.Quote(remotePath),
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
		shell.Quote(remoteDir), shell.Quote(remoteDir))
	stderr, code, err := p.RunCommandStdin(ctx, host, script, bytes.NewReader(tarball), io.Discard)
	if err != nil {
		return fmt.Errorf("failed to upload archive to %s: %w", remoteDir, err)
	}
	if code != 0 {
		return fmt.Errorf("failed to extract archive at %s (exit %d): %s", remoteDir, code, strings.TrimSpace(stderr))
	}
	return nil
}
