// Package gitutil shells out to the system git rather than reimplementing
// it: lichen's correctness story leans on git's fetch/merge semantics, and
// every machine lichen targets already has git.
package gitutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gitTimeout bounds every git subprocess: a black-holed network (a laptop
// that changed Wi-Fi mid-push) otherwise blocks in read() for hours, and
// because the caller holds the cross-process lock, that hang would wedge
// the whole daemon. A generous ceiling: real pushes and pulls finish well
// inside it.
const gitTimeout = 3 * time.Minute

func Run(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return out, fmt.Errorf("git %s: timed out after %s", strings.Join(args, " "), gitTimeout)
		}
		return out, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, out)
	}
	return out, nil
}

// HostedKey turns a remote URL into its "owner/repo" key, used to point at
// the sync repo's webhook settings page. ok=false for a local path or
// anything else without that shape.
func HostedKey(remote string) (string, bool) {
	s := strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	if !strings.Contains(s, "://") && !strings.Contains(s, "@") {
		return "", false
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// scp-style syntax: git@host:owner/repo
	if i := strings.Index(s, ":"); i >= 0 && !strings.Contains(s[:i], "/") {
		s = s[i+1:]
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 {
		return "", false
	}
	return strings.ToLower(parts[len(parts)-2] + "/" + parts[len(parts)-1]), true
}
