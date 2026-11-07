package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// stateLock serializes deploy runs that share a checkpoint file. Save() is
// atomic, but two orchestrators still race on the in-memory phase list and can
// lose updates; the lock makes the second invocation fail before it touches
// the lab.
type stateLock struct {
	file *os.File
	path string
}

// acquireStateLock takes an exclusive, non-blocking lock next to the checkpoint.
// The lock file is intentionally left on disk: removing it while another
// process is waiting would make two different inodes look like the same lock.
func acquireStateLock(statePath string) (*stateLock, error) {
	lockPath := filepath.Clean(statePath) + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open state lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("deployment already running (state lock %s is held)", lockPath)
		}
		return nil, fmt.Errorf("lock state file %s: %w", lockPath, err)
	}
	return &stateLock{file: f, path: lockPath}, nil
}

// Close releases the lock. It is safe to call once; a nil lock is a no-op.
func (l *stateLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf("unlock state file %s: %w", l.path, unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close state lock %s: %w", l.path, closeErr)
	}
	return nil
}
