// Package files is lichen's sync engine: it wraps chezmoi, which owns the
// hard parts (source state, templates, per-machine data), and adds WHEN it
// runs. Local edits are captured by fsnotify-driven re-add and push,
// local deletions propagate to every machine (deletions.go),
// remote changes are pulled and applied on an event. The sync repo is
// chezmoi's source repo. lichen's own config.json is managed inside it,
// which is what carries a new machine's settings to it.
package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"lichen/internal/backup"
	"lichen/internal/config"
	"lichen/internal/events"
	"lichen/internal/gitutil"
)

func chezmoi(args ...string) (string, error) {
	out, err := chezmoiRaw(args...)
	return strings.TrimSpace(out), err
}

// chezmoiRaw preserves leading whitespace: `chezmoi status` output is
// column-positional and its first column can be a space. Bounded by a
// timeout so a chezmoi call that blocks (a git operation inside an apply
// hitting a dead network) cannot wedge the cross-process lock indefinitely.
//
// chezmoi holds an exclusive persistent-state lock with a ~1s timeout, so a
// concurrent chezmoi invocation from another lichen process (a read-only
// `lichen status` runs source-path, which takes the lock) can make a call
// fail. The cross-process lock does not cover read commands, so this
// retries a lock-timeout a few times: the contending call is always quick,
// and this keeps an apply from failing after foreign files were already
// moved to backups.
func chezmoiRaw(args ...string) (string, error) {
	var out []byte
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, "chezmoi", append([]string{"--no-tty"}, args...)...)
		out, err = cmd.CombinedOutput()
		cancel()
		if err == nil {
			return string(out), nil
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return string(out), fmt.Errorf("chezmoi %s: timed out", strings.Join(args, " "))
		}
		if !strings.Contains(string(out), "persistent state lock") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	return string(out), fmt.Errorf("chezmoi %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
}

// The source dir is constant for the life of the process, so a SUCCESSFUL
// resolution is cached forever. Failures are retried on every call so a
// daemon that started before chezmoi was installed/initialized heals
// itself.
var srcCache struct {
	sync.Mutex
	path string
}

// SourcePath resolves chezmoi's source directory: the sync repo's local
// checkout.
func SourcePath() (string, error) {
	srcCache.Lock()
	defer srcCache.Unlock()
	if srcCache.path != "" {
		return srcCache.path, nil
	}
	if _, err := exec.LookPath("chezmoi"); err != nil {
		return "", err
	}
	p, err := chezmoi("source-path")
	if err != nil {
		return "", err
	}
	srcCache.path = p
	return p, nil
}

// Active reports whether lichen can run here: chezmoi installed and its
// source directory initialized as a git repo.
func Active() bool {
	src, err := SourcePath()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(src, ".git"))
	return err == nil
}

// Origin returns the sync repo's remote URL ("" when none is set).
func Origin() string {
	src, err := SourcePath()
	if err != nil {
		return ""
	}
	url, err := gitutil.Run(src, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return url
}

// managedPaths lists chezmoi-managed paths of one kind ("files" or
// "dirs") as absolute paths.
func managedPaths(include string) ([]string, error) {
	out, err := chezmoi("managed", "--include="+include, "--path-style=absolute")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
}

// Managed returns absolute paths of all chezmoi-managed files.
func Managed() ([]string, error) {
	return managedPaths("files")
}

// entryStatePaths returns the set of destination paths chezmoi has itself
// written (its persistent state). Presence here is what separates "user
// edited a file we manage" from "file predates chezmoi on this machine".
func entryStatePaths() (map[string]bool, error) {
	out, err := chezmoi("state", "dump")
	if err != nil {
		return nil, err
	}
	var dump struct {
		EntryState map[string]json.RawMessage `json:"entryState"`
	}
	if err := json.Unmarshal([]byte(out), &dump); err != nil {
		return nil, fmt.Errorf("parsing chezmoi state: %w", err)
	}
	paths := map[string]bool{}
	for p := range dump.EntryState {
		paths[p] = true
	}
	return paths, nil
}

// underSymlink reports whether any directory between home (exclusive)
// and abs is a symlink: capturing through one publishes whatever the
// link points at.
func underSymlink(home, abs string) bool {
	for d := filepath.Dir(abs); len(d) > len(home); d = filepath.Dir(d) {
		if fi, err := os.Lstat(d); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// classify buckets every pending chezmoi change:
//
//	localEdits: chezmoi wrote the file, the user edited it since
//	            (status col 1 is M/D). Capture with re-add, never revert.
//	foreign:    a file exists that chezmoi never wrote (absent from its
//	            entry state, i.e. a fresh machine's pre-existing dotfile).
//	            Move to backups, then apply over it.
//	remote:     destination is exactly what chezmoi last wrote. Apply.
//	skipped:    a symlink sits at (or above) the path. Re-adding one
//	            would capture and publish the link TARGET's content
//	            to every machine, and applying would clobber the link,
//	            so these are left strictly alone and reported.
func classify(home string, written, managedDirs map[string]bool) (localEdits, foreign, remote, skipped []string, err error) {
	status, err := chezmoiRaw("status")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 4 {
			continue
		}
		col1, col2, rel := line[0], line[1], line[3:]
		if col2 == ' ' || col2 == 'R' {
			continue
		}
		abs := filepath.Join(home, rel)
		fi, statErr := os.Lstat(abs)
		exists := statErr == nil
		if (exists && fi.Mode()&os.ModeSymlink != 0) || underSymlink(home, abs) {
			skipped = append(skipped, abs)
			continue
		}
		if exists && fi.IsDir() {
			// A managed directory whose mode differs across machines
			// (e.g. ~/.ssh at 0755 vs the source's 0700): never move it
			// to backups (that would take every unmanaged file under
			// it), never re-add it (that would push the looser mode
			// upstream). Apply fixes only the dir's own mode. But a
			// directory sitting where the source has a FILE would make
			// apply prompt (and wedge under --no-tty): that one is
			// foreign, so it gets moved to backups first.
			if managedDirs[abs] {
				remote = append(remote, abs)
			} else {
				foreign = append(foreign, abs)
			}
			continue
		}
		switch {
		case col1 == 'M' || col1 == 'D':
			localEdits = append(localEdits, abs)
		case exists && !written[abs]:
			foreign = append(foreign, abs)
		default:
			remote = append(remote, abs)
		}
	}
	return localEdits, foreign, remote, skipped, nil
}

// managedDirSet returns chezmoi's managed directories, used to tell a
// managed dir apart from a foreign dir squatting on a managed file path.
func managedDirSet() (map[string]bool, error) {
	dirs, err := managedPaths("dirs")
	if err != nil {
		return nil, err
	}
	return toSet(dirs), nil
}

// toSet turns a path list into a membership set.
func toSet(paths []string) map[string]bool {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return set
}

// abortStaleRebase recovers a source repo left mid-rebase by a conflicted
// or interrupted pull. A wedged repo would otherwise block every commit
// and pull until a human runs `git rebase --abort`.
func abortStaleRebase(src string, lg *log.Logger) {
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(src, ".git", d)); err == nil {
			lg.Printf("files: aborting stale rebase in sync repo")
			gitutil.Run(src, "rebase", "--abort")
			return
		}
	}
}

// pullRebase pulls the sync repo under lichen's local-wins policy: rebase
// (not ff-only) so a locally-committed-but-unpushed change doesn't wedge
// the pull forever, with -X theirs preferring OUR replayed commits on
// conflict (rebase swaps sides). A stale rebase is aborted before and
// after, so a failed pull never leaves the repo wedged. No-op without an
// origin.
func pullRebase(src string, lg *log.Logger) error {
	abortStaleRebase(src, lg)
	if Origin() == "" {
		return nil
	}
	if _, err := gitutil.Run(src, "pull", "--rebase", "-X", "theirs", "--quiet"); err != nil {
		abortStaleRebase(src, lg)
		return err
	}
	return nil
}

// Reconcile pulls the sync repo, captures local edits, and applies remote
// changes. Backing up foreign files by MOVING them also means apply never
// sees a modified destination, so it cannot prompt (the daemon runs with
// --no-tty).
func Reconcile(cfg *config.Config, lg *log.Logger) error {
	if !Active() {
		return nil
	}
	src, err := SourcePath()
	if err != nil {
		return err
	}
	if err := pullRebase(src, lg); err != nil {
		lg.Printf("files: pull: %v (continuing with local state)", err)
	}
	if Origin() != "" {
		// A rebase (or an earlier offline commit) can leave us ahead of
		// origin with nothing new to stage. Push those commits now
		// rather than waiting for the next local edit.
		if out, err := gitutil.Run(src, "rev-list", "--count", "@{u}..HEAD"); err == nil && out != "0" {
			if _, err := gitutil.Run(src, "push", "--quiet"); err != nil {
				lg.Printf("files: push: %v (will retry on next sync)", err)
			} else {
				announce(cfg, lg)
			}
		}
	}
	// Deletions other machines pushed are carried out before anything
	// else looks at the destination state (see deletions.go). Failure
	// here (e.g. a merge-mangled deletion log) must not block the rest
	// of the pass: with a nil baseline every missing file downgrades to
	// the safe "apply it back" path below.
	prevManaged, err := applyIncomingDeletions(lg)
	if err != nil {
		lg.Printf("files: incoming deletions: %v (skipping)", err)
	}
	if err := ensureConfigManaged(cfg, lg); err != nil {
		lg.Printf("files: %v", err)
	}
	home, err := config.Home()
	if err != nil {
		return err
	}

	// The entry-state dump is fetched once per pass: re-add doesn't write
	// destinations, so the post-re-add re-classify can reuse it.
	written, err := entryStatePaths()
	if err != nil {
		return err
	}
	managedDirs, err := managedDirSet()
	if err != nil {
		return err
	}
	localEdits, foreign, remote, skipped, err := classify(home, written, managedDirs)
	if err != nil {
		return err
	}
	// re-add cannot capture deletions (missing files): split those out,
	// otherwise the call errors and blocks the whole pass.
	editable, deleted := partitionExisting(localEdits)
	if len(editable) > 0 {
		lg.Printf("files: capturing local edits: %v", editable)
		if err := reAddPush(cfg, lg, editable); err != nil {
			return err
		}
		var still, skipped2 []string
		if still, foreign, remote, skipped2, err = classify(home, written, managedDirs); err != nil {
			return err
		}
		skipped = append(skipped, skipped2...)
		// Anything still classified as an EXISTING local edit after
		// re-add is a template-sourced file re-add refuses to overwrite.
		var uncaptured []string
		uncaptured, deleted = partitionExisting(still)
		if len(uncaptured) > 0 {
			slices.Sort(uncaptured)
			lg.Printf("files: NOT synced (template-sourced edit. Use `lichen remove` or edit the template in the sync repo): %v", slices.Compact(uncaptured))
		}
	}
	if len(deleted) > 0 {
		// A locally deleted synced file is deleted everywhere: the other
		// machines move their copies to backups, and the content stays
		// recoverable from the sync repo's history (`lichen recover`).
		// `lichen remove` is the way to stop syncing while keeping every
		// machine's copy. A failure only defers the deletion to the next
		// pass, so it must not block the applies below.
		slices.Sort(deleted)
		if err := handleMissing(cfg, lg, prevManaged, slices.Compact(deleted)); err != nil {
			lg.Printf("files: deletions: %v (retrying next pass)", err)
		}
	}
	if len(skipped) > 0 {
		slices.Sort(skipped)
		lg.Printf("files: NOT synced (symlink at or above the path, refusing to capture or overwrite): %v", slices.Compact(skipped))
	}
	for _, abs := range foreign {
		if _, statErr := os.Lstat(abs); statErr == nil {
			to, berr := backup.Move(abs)
			if berr != nil {
				return fmt.Errorf("backing up %s: %w", abs, berr)
			}
			lg.Printf("files: backed up %s → %s", abs, to)
		}
	}
	targets := append(foreign, remote...)
	if len(targets) == 0 {
		return nil
	}
	// Apply exactly the paths that are safe to write, never a bare apply:
	// uncapturable local edits above must stay untouched.
	if _, err := chezmoi(append([]string{"apply"}, targets...)...); err != nil {
		return err
	}
	lg.Printf("files: applied %d change(s)", len(targets))
	return nil
}

// LoadConfig loads lichen's config, first restoring a DELETED config
// file from the sync repo: a machine without one could never run a pass
// (including the healing one) again. Any other load failure, e.g. a file
// mid-rewrite by a concurrent apply, stays an error. Callers must hold
// the cross-process lock: the restore writes to the destination.
func LoadConfig(lg *log.Logger) (*config.Config, error) {
	cfg, err := config.Load()
	if err == nil {
		return cfg, nil
	}
	restoreConfig(lg)
	return config.Load()
}

// restoreConfig puts a deleted config.json back from the sync repo. Only
// an ABSENT file is restored: an unparseable one may be mid-rewrite by a
// concurrent apply.
func restoreConfig(lg *log.Logger) {
	if !Active() {
		return
	}
	p, err := config.Path()
	if err != nil {
		return
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		return
	}
	if _, err := chezmoi("source-path", p); err != nil {
		return
	}
	lg.Printf("files: config missing, restoring it from the sync repo")
	// chezmoi refuses to apply a target whose parent dir is missing on
	// disk (install.sh works around it the same way).
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		lg.Printf("files: restore config: %v", err)
		return
	}
	if _, err := chezmoi("apply", "--force", p); err != nil {
		lg.Printf("files: restore config: %v", err)
	}
}

// ensureConfigManaged self-heals the keystone invariant: lichen's config
// must be part of the sync repo. Covers first-machine bootstrap and the
// config being forgotten later. Checked every pass: `chezmoi source-path
// <target>` is one cheap call that fails iff the target is unmanaged.
func ensureConfigManaged(cfg *config.Config, lg *log.Logger) error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfgPath); err != nil {
		return nil
	}
	if _, err := chezmoi("source-path", cfgPath); err == nil {
		return nil
	}
	lg.Printf("files: adding %s to the sync repo", cfgPath)
	if _, err := chezmoi("add", cfgPath); err != nil {
		return err
	}
	return commitPush(cfg, "manage config.json", "", lg)
}

// reAddPush captures local edits to managed files back into the sync repo
// and pushes. Template-sourced files are skipped by chezmoi itself
// (re-add never overwrites templates), and the caller surfaces which paths
// were dropped.
func reAddPush(cfg *config.Config, lg *log.Logger, paths []string) error {
	if !Active() {
		return nil
	}
	args := append([]string{"re-add"}, paths...)
	if out, err := chezmoi(args...); err != nil {
		return err
	} else if out != "" {
		lg.Printf("files: re-add: %s", out)
	}
	return commitPush(cfg, "file sync", "", lg)
}

// LocalChange handles paths the watcher saw change: files still present
// are captured with re-add, files now missing are deleted everywhere
// (see deletions.go). Reports whether the managed set changed, so the
// watcher knows to rebuild its list.
func LocalChange(cfg *config.Config, lg *log.Logger, paths []string) (bool, error) {
	existing, missing := partitionExisting(paths)
	var readdErr error
	if len(existing) > 0 {
		readdErr = reAddPush(cfg, lg, existing)
	}
	changed, propErr := propagateDeletions(cfg, lg, missing)
	return changed, errors.Join(readdErr, propErr)
}

// Sync starts managing new paths (chezmoi add) and pushes. The pre-lichen
// original of each newly managed file is snapshotted first: from this
// moment on lichen may overwrite it, so this is the last chance to
// preserve what the machine had.
func Sync(cfg *config.Config, lg *log.Logger, paths []string) error {
	if _, err := exec.LookPath("chezmoi"); err != nil {
		return fmt.Errorf("chezmoi not installed (re-run install.sh)")
	}
	if !Active() {
		return fmt.Errorf("no sync repo initialized (re-run install.sh with LICHEN_REPO=<git url>)")
	}
	src, err := SourcePath()
	if err != nil {
		return err
	}
	// Pull first: the tombstone prune below must see deletions other
	// machines pushed but this one has not applied yet, or a stale entry
	// survives against the freshly re-synced path.
	if err := pullRebase(src, lg); err != nil {
		lg.Printf("files: pull: %v (continuing with local state)", err)
	}
	var absPaths []string
	for _, p := range paths {
		abs, err := absPath(p)
		if err != nil {
			return err
		}
		// Reject out-of-home paths BEFORE the backup snapshot: chezmoi
		// would refuse them anyway, and a possibly-sensitive copy must
		// not be left sitting in the backups dir for a failed sync.
		if config.ContractHome(abs) == abs {
			return fmt.Errorf("%s is outside the home directory, only files under ~ can be synced", p)
		}
		absPaths = append(absPaths, abs)
		if _, err := os.Stat(abs); err != nil {
			continue // chezmoi add will report the missing path
		}
		if _, err := chezmoi("source-path", abs); err == nil {
			continue // already managed: re-syncs don't re-backup
		}
		to, err := backup.Copy(abs)
		if err != nil {
			return fmt.Errorf("backing up %s: %w", p, err)
		}
		lg.Printf("files: backed up %s → %s", p, to)
	}
	if _, err := chezmoi(append([]string{"add"}, paths...)...); err != nil {
		return err
	}
	// A path syncing (again) supersedes any recorded deletion of it: the
	// prune rides along in the same commit.
	if err := pruneDeletionLog(src, absPaths); err != nil {
		return err
	}
	if err := addToManifest(absPaths); err != nil {
		return err
	}
	subject, body := commitMsg("sync", paths)
	return commitPush(cfg, subject, body, lg)
}

// Remove stops managing paths. Local copies stay in place, on every
// machine: without a deletion-log entry the other machines treat the
// departure from the managed set as a forget, not a delete.
func Remove(cfg *config.Config, lg *log.Logger, paths []string) error {
	if !Active() {
		return fmt.Errorf("no sync repo initialized")
	}
	src, err := SourcePath()
	if err != nil {
		return err
	}
	// Pull first for the same reason as Sync: the defensive prune below
	// must see the latest deletion log.
	if err := pullRebase(src, lg); err != nil {
		lg.Printf("files: pull: %v (continuing with local state)", err)
	}
	if _, err := chezmoi(append([]string{"forget", "--force"}, paths...)...); err != nil {
		return err
	}
	// Defensive: a stale recorded deletion overlapping these paths would
	// otherwise turn this forget into a delete on the other machines.
	var absPaths []string
	for _, p := range paths {
		if abs, err := absPath(p); err == nil {
			absPaths = append(absPaths, abs)
		}
	}
	if err := pruneDeletionLog(src, absPaths); err != nil {
		return err
	}
	subject, body := commitMsg("forget", paths)
	return commitPush(cfg, subject, body, lg)
}

// commitMsg builds a subject and body for a multi-path operation: a single
// path stays in the subject, more get counted there and listed one per
// line in the body.
func commitMsg(verb string, paths []string) (string, string) {
	p := portablePaths(paths)
	if len(p) == 1 {
		return verb + " " + p[0], ""
	}
	return fmt.Sprintf("%s %d files", verb, len(p)), strings.Join(p, "\n")
}

// portablePaths rewrites paths to ~/ form for commit messages: the sync
// repo is shared across machines, so absolute home paths would leak one
// machine's layout into every clone's history.
func portablePaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		abs, err := absPath(p)
		if err != nil {
			out[i] = p
			continue
		}
		out[i] = config.ContractHome(abs)
	}
	return out
}

// absPath canonicalizes a user-supplied path: ~ expanded, made absolute.
func absPath(p string) (string, error) {
	abs, err := config.ExpandHome(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(abs)
}

// partitionExisting splits paths into those present on disk and those
// missing (deleted locally).
func partitionExisting(paths []string) (existing, missing []string) {
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			existing = append(existing, p)
		} else {
			missing = append(missing, p)
		}
	}
	return existing, missing
}

func commitPush(cfg *config.Config, subject, body string, lg *log.Logger) error {
	src, err := SourcePath()
	if err != nil {
		return err
	}
	if _, err := gitutil.Run(src, "add", "-A"); err != nil {
		return err
	}
	if _, err := gitutil.Run(src, "diff", "--cached", "--quiet"); err == nil {
		return nil // nothing staged
	}
	host, _ := os.Hostname()
	commitArgs := []string{"commit", "--quiet", "-m", fmt.Sprintf("lichen(%s): %s", host, subject)}
	if body != "" {
		commitArgs = append(commitArgs, "-m", body)
	}
	if _, err := gitutil.Run(src, commitArgs...); err != nil {
		return err
	}
	if Origin() == "" {
		lg.Printf("files: committed (no origin configured, not pushed)")
		return nil
	}
	if _, err := gitutil.Run(src, "push", "--quiet"); err != nil {
		// A racing push from another machine or being offline is routine:
		// try once behind a rebase, else leave it for the next pass.
		if pullRebase(src, lg) == nil {
			if _, err2 := gitutil.Run(src, "push", "--quiet"); err2 == nil {
				announce(cfg, lg)
				return nil
			}
		}
		lg.Printf("files: push failed (will retry on next sync): %v", err)
		return nil
	}
	announce(cfg, lg)
	return nil
}

// announce tells the other machines that the sync repo moved, so they
// apply within seconds instead of waiting for their next hourly pass. The
// sync repo's webhook does the same job for pushes lichen did not make.
// Failure is not an error: the hourly pass is the backstop.
func announce(cfg *config.Config, lg *log.Logger) {
	host, _ := os.Hostname()
	b, _ := json.Marshal(events.Nudge{Origin: host})
	if err := (events.Client{Server: cfg.Server(), Topic: cfg.Topic}).Publish(string(b)); err != nil {
		lg.Printf("files: event not published, other machines catch up on their next pass: %s", cfg.MaskTopic(err.Error()))
	}
}
