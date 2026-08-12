// Package proclock is lichen's cross-PROCESS lock. The CLI and the daemon
// are separate processes that both drive git and chezmoi against the same
// source repo. The daemon's in-process mutex cannot see the CLI, so
// without this an interleaving of the two destroys work staged in the git
// repo or interleaves two chezmoi applies. Every mutating flow (a daemon
// pass, a CLI sync or remove) holds this lock for its duration.
package proclock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"lichen/internal/config"
)

// Acquire blocks until it holds the exclusive lock, returning a release
// func. ctx cancellation (or a hung holder outliving its subprocess
// timeouts) aborts the wait. notify, if non-nil, is called once if the
// wait exceeds a short grace period, so a CLI command can tell the user it
// is waiting on the daemon rather than appearing to hang.
func Acquire(ctx context.Context, notify func()) (release func(), err error) {
	d, err := config.DataDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(d, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// Poll a non-blocking flock so ctx stays honored: a plain blocking
	// LOCK_EX cannot be cancelled. The lock is advisory but exclusive
	// across processes regardless.
	deadline := time.Now().Add(500 * time.Millisecond)
	notified := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("lock: %w", err)
		}
		if !notified && notify != nil && time.Now().After(deadline) {
			notified = true
			notify()
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
