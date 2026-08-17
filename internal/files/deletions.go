// Deletion propagation: a synced file deleted locally is deleted on every
// machine. Three records make that safe. The deletion log in the sync
// repo (.lichen-deleted.json, ignored by chezmoi, tracked by git) says
// WHICH departures from the managed set are real deletions, as opposed
// to a path merely unmanaged (an ignore rule, a source file removed by
// hand), which keeps local copies. Each machine's local manifest of the
// managed set it saw last pass makes acting on a deletion a one-shot
// transition, so a file recreated at the same path later is never
// re-deleted. And chezmoi's entry state gates the sending side: a
// managed path only propagates as a deletion if chezmoi actually wrote
// it here once, so a path that merely FAILED to materialize (a foreign
// file moved to backups, an apply that errored) is never bounced at the
// fleet as a deletion. Content is never lost: the deleting machine's
// copy survives in the sync repo's git history (`lichen recover` brings
// it back), every other machine moves its copy into its backups dir.

package files

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"lichen/internal/backup"
	"lichen/internal/config"
	"lichen/internal/gitutil"
)

// Host and At are for humans reading the log file. No code depends on
// them.
type deletionEntry struct {
	Source string `json:"source"` // repo-relative source path at deletion time
	Host   string `json:"host"`
	At     string `json:"at"`
}

// The log lives at the sync repo root, keyed by ~/-relative target path.
// The leading dot keeps chezmoi from treating it as a source entry.
func deletionLogPath(src string) string {
	return filepath.Join(src, ".lichen-deleted.json")
}

func loadDeletionLog(src string) (map[string]deletionEntry, error) {
	data, err := os.ReadFile(deletionLogPath(src))
	if os.IsNotExist(err) {
		return map[string]deletionEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]deletionEntry{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", deletionLogPath(src), err)
	}
	return m, nil
}

func saveDeletionLog(src string, m map[string]deletionEntry) error {
	return writeJSON(deletionLogPath(src), m)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// atOrUnder reports whether p IS root or lies inside it. The trailing
// separator keeps ~/.zshrc.bak from matching ~/.zshrc. coveredBy and
// anyAtOrUnder name its two fold directions, the one axis that is easy
// to invert at a call site.
func atOrUnder(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}

// coveredBy reports whether p sits at or under any of roots.
func coveredBy(p string, roots []string) bool {
	return slices.ContainsFunc(roots, func(r string) bool { return atOrUnder(p, r) })
}

// anyAtOrUnder reports whether any of paths sits at or under root.
func anyAtOrUnder(paths []string, root string) bool {
	return slices.ContainsFunc(paths, func(p string) bool { return atOrUnder(p, root) })
}

// pruneDeletionLog drops recorded deletions that overlap paths, in either
// direction (a re-synced file under a tombstoned dir, or a re-synced dir
// over a tombstoned file). Syncing or recovering a path supersedes an old
// deletion of it, and a lingering entry would turn a later innocent
// departure at the same path into a delete-everywhere.
func pruneDeletionLog(src string, absPaths []string) error {
	dlog, err := loadDeletionLog(src)
	if err != nil || len(dlog) == 0 {
		return err
	}
	changed := false
	for _, abs := range absPaths {
		t := config.ContractHome(abs)
		for key := range dlog {
			if atOrUnder(key, t) || atOrUnder(t, key) {
				delete(dlog, key)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return saveDeletionLog(src, dlog)
}

// The manifest is this machine's record of the managed files it saw on
// its previous pass, the baseline that departures are computed against.
func manifestPath() (string, error) {
	d, err := config.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "managed.json"), nil
}

// ok=false (absent or unreadable) means no baseline: nothing can depart,
// so no deletion can fire. Unreadable deliberately degrades the same
// way, it is the direction that deletes nothing. (A DataDir failure also
// lands here and resurfaces from the saveManifest that follows.)
func loadManifest() ([]string, bool) {
	p, err := manifestPath()
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var paths []string
	if json.Unmarshal(data, &paths) != nil {
		return nil, false
	}
	return paths, true
}

func saveManifest(paths []string) error {
	p, err := manifestPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return writeJSON(p, slices.Sorted(slices.Values(paths)))
}

// dropNested returns paths minus any that sit under another listed path:
// forgetting the top-most path covers its subtree in one entry. Checked
// against every kept path, not just the previous one: byte order sorts
// "/a-old" between "/a" and "/a/b", so adjacency cannot be trusted.
func dropNested(paths []string) []string {
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	var out []string
	for _, p := range sorted {
		if !coveredBy(p, out) {
			out = append(out, p)
		}
	}
	return out
}

// dropEntryStateUnder clears chezmoi's memory of having written anything
// at or under the given paths (directories have entries of their own), so
// a file the user recreates at one later counts as foreign (backed up
// before any overwrite) instead of fair game, and so nothing lingering
// there reads as a local deletion on a later pass. The caller supplies
// the entry-state snapshot so one dump serves the whole batch (a
// slightly stale snapshot only costs no-op deletes). Best-effort: a
// stale entry only weakens that backup, it breaks nothing.
func dropEntryStateUnder(written map[string]bool, paths []string) {
	for key := range written {
		if coveredBy(key, paths) {
			chezmoi("state", "delete", "--bucket=entryState", "--key="+key)
		}
	}
}

// topMissing climbs from a missing file to the top-most missing managed
// ancestor: a deleted directory arrives from the watcher as its files,
// but the whole subtree should propagate as one deletion.
func topMissing(abs string, managedDirs map[string]bool) string {
	for {
		parent := filepath.Dir(abs)
		if !managedDirs[parent] {
			return abs
		}
		if _, err := os.Lstat(parent); err == nil {
			return abs
		}
		abs = parent
	}
}

// propagateDeletions handles synced paths deleted locally: forget them,
// record them on the deletion log, and push, so every other machine
// deletes its copy too. Only paths chezmoi's entry state proves were
// materialized here propagate (see the package comment). The one
// exception is lichen's own config, the keystone every machine needs to
// run at all: the config file is put back instead, and the rest of a
// deleted subtree containing it is left alone. Returns whether the
// managed set changed.
func propagateDeletions(cfg *config.Config, lg *log.Logger, paths []string) (bool, error) {
	// Re-check existence at the last moment. The paths were seen missing
	// earlier (at classify time, or at the watcher's debounce), and one
	// that reappeared since (an editor's slow save dance, a racing
	// apply) is not a deletion.
	_, missing := partitionExisting(paths)
	if len(missing) == 0 || !Active() {
		return false, nil
	}
	src, err := SourcePath()
	if err != nil {
		return false, err
	}
	cfgPath, err := config.Path()
	if err != nil {
		return false, err
	}
	managedDirs, err := managedDirSet()
	if err != nil {
		return false, err
	}
	for i, abs := range missing {
		missing[i] = topMissing(abs, managedDirs)
	}
	written, err := entryStatePaths()
	if err != nil {
		return false, err
	}
	writtenKeys := slices.Collect(maps.Keys(written))
	dlog, err := loadDeletionLog(src)
	if err != nil {
		return false, err
	}
	host, _ := os.Hostname()
	at := time.Now().UTC().Format(time.RFC3339)
	var doomed []string
	for _, abs := range dropNested(missing) {
		if atOrUnder(cfgPath, abs) {
			restoreConfig(lg)
			continue
		}
		if !anyAtOrUnder(writtenKeys, abs) {
			continue // never materialized here: not this machine's deletion to make
		}
		sp, err := chezmoi("source-path", abs)
		if err != nil {
			continue // not managed (already forgotten): nothing to propagate
		}
		rel, err := filepath.Rel(src, sp)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		dlog[config.ContractHome(abs)] = deletionEntry{Source: rel, Host: host, At: at}
		doomed = append(doomed, abs)
	}
	if len(doomed) == 0 {
		return false, nil
	}
	if err := saveDeletionLog(src, dlog); err != nil {
		return false, err
	}
	lg.Printf("files: deleted locally, deleting everywhere (`lichen recover` brings one back): %v", doomed)
	if _, err := chezmoi(append([]string{"forget", "--force"}, doomed...)...); err != nil {
		return false, err
	}
	dropEntryStateUnder(written, doomed)
	// Consume this machine's own departure right away: the manifest still
	// lists the doomed paths, and leaving them would make the next pass
	// read a file recreated in the meantime as an incoming deletion.
	if err := removeFromManifest(doomed); err != nil {
		lg.Printf("files: manifest: %v", err)
	}
	subject, body := commitMsg("delete", doomed)
	return true, commitPush(cfg, subject, body, lg)
}

// handleMissing routes classify's missing-file bucket. Only a path this
// machine saw on its previous pass can be a LOCAL deletion to propagate:
// a managed-but-missing path absent from that baseline is arriving from
// another machine (a recover, or stale chezmoi state), so it is applied
// rather than bounced back at the fleet as a deletion. The baseline is
// threaded in from applyIncomingDeletions because by now the manifest on
// disk already describes THIS pass.
func handleMissing(cfg *config.Config, lg *log.Logger, prev []string, deleted []string) error {
	var mine, incoming []string
	for _, p := range deleted {
		if anyAtOrUnder(prev, p) {
			mine = append(mine, p)
		} else {
			incoming = append(incoming, p)
		}
	}
	if len(incoming) > 0 {
		lg.Printf("files: applying incoming file(s): %v", incoming)
		if err := applyBack(incoming); err != nil {
			lg.Printf("files: apply: %v", err)
		}
	}
	_, err := propagateDeletions(cfg, lg, mine)
	return err
}

// applyIncomingDeletions carries out deletions other machines pushed:
// every path that left the managed set since the last pass AND is on the
// deletion log gets its local copy moved to backups. Departures without
// a log entry (an ignore rule, a hand-edit to the sync repo) keep their
// local copy. The first pass on a machine only records the baseline.
// Returns the previous pass's managed files (nil without a baseline) for
// handleMissing.
func applyIncomingDeletions(lg *log.Logger) ([]string, error) {
	src, err := SourcePath()
	if err != nil {
		return nil, err
	}
	current, err := Managed()
	if err != nil {
		return nil, err
	}
	prev, ok := loadManifest()
	if !ok {
		return nil, saveManifest(current)
	}
	curSet := toSet(current)
	var departed []string
	for _, p := range prev {
		if !curSet[p] {
			departed = append(departed, p)
		}
	}
	if len(departed) > 0 {
		if err := deleteDeparted(src, departed, lg); err != nil {
			return nil, err
		}
	}
	// The manifest was saved sorted, so prev compares directly.
	if !slices.Equal(prev, slices.Sorted(slices.Values(current))) {
		return prev, saveManifest(current)
	}
	return prev, nil
}

func deleteDeparted(src string, departed []string, lg *log.Logger) error {
	dlog, err := loadDeletionLog(src)
	if err != nil || len(dlog) == 0 {
		return err
	}
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	var matched []string
	for _, target := range slices.Sorted(maps.Keys(dlog)) {
		abs, err := config.ExpandHome(target)
		if err != nil {
			continue
		}
		// Never honor a tombstone covering lichen's own config, whatever
		// wrote it: carrying it out would brick this machine.
		if atOrUnder(cfgPath, abs) {
			lg.Printf("files: ignoring recorded deletion of %s (covers lichen's config)", target)
			continue
		}
		// A log entry for a directory is matched by the departure of any
		// managed file that lived under it.
		if !anyAtOrUnder(departed, abs) {
			continue
		}
		matched = append(matched, abs)
		fi, err := os.Lstat(abs)
		if err != nil {
			continue // already gone here
		}
		// For a directory, move only what lichen managed: files the user
		// kept in it but never synced must stay (same rule as classify's
		// foreign-dir handling). Emptied directories are then pruned.
		victims := []string{abs}
		if fi.IsDir() {
			victims = victims[:0]
			for _, d := range departed {
				if atOrUnder(d, abs) {
					victims = append(victims, d)
				}
			}
		}
		for _, v := range victims {
			if _, err := os.Lstat(v); err != nil {
				continue
			}
			to, err := backup.Move(v)
			if err != nil {
				lg.Printf("files: deleting %s: %v", v, err)
				continue
			}
			lg.Printf("files: deleted %s (kept at %s, `lichen recover` re-syncs it)", v, to)
		}
		if fi.IsDir() {
			removeEmptyDirs(abs)
		}
	}
	if len(matched) > 0 {
		if all, err := entryStatePaths(); err == nil {
			dropEntryStateUnder(all, matched)
		}
	}
	return nil
}

// removeEmptyDirs prunes now-empty directories under (and including)
// root, deepest first. WalkDir yields parents before children, so the
// reverse order is bottom-up. A dir still holding anything is left
// alone.
func removeEmptyDirs(root string) {
	var dirs []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for _, d := range slices.Backward(dirs) {
		os.Remove(d) // fails on non-empty dirs, which is the point
	}
}

// addToManifest records freshly synced or recovered paths (for a
// directory, the managed files under it) as seen on this machine without
// waiting for the next full pass: deleting one right away must propagate,
// not read as a new arrival to restore.
func addToManifest(absPaths []string) error {
	prev, ok := loadManifest()
	if !ok {
		return nil // no baseline yet: the first full pass records one
	}
	current, err := Managed()
	if err != nil {
		return err
	}
	set := toSet(prev)
	added := false
	for _, m := range current {
		if !set[m] && coveredBy(m, absPaths) {
			set[m] = true
			added = true
		}
	}
	if !added {
		return nil
	}
	return saveManifest(slices.Collect(maps.Keys(set)))
}

// removeFromManifest is addToManifest's shrink counterpart, for paths
// this machine itself stopped managing.
func removeFromManifest(absPaths []string) error {
	prev, ok := loadManifest()
	if !ok {
		return nil
	}
	kept := slices.DeleteFunc(slices.Clone(prev), func(m string) bool {
		return coveredBy(m, absPaths)
	})
	if len(kept) == len(prev) {
		return nil
	}
	return saveManifest(kept)
}

// Recover brings back files that were deleted everywhere: the last
// version is lifted out of the sync repo's git history, synced again, and
// applied, here and (through the push) on every other machine. All paths
// are resolved before anything is touched, so a bad argument cannot leave
// the sync repo half-restored. Returns the paths brought back, in ~/
// form.
func Recover(cfg *config.Config, lg *log.Logger, paths []string) ([]string, error) {
	if !Active() {
		return nil, fmt.Errorf("no sync repo initialized")
	}
	src, err := SourcePath()
	if err != nil {
		return nil, err
	}
	if err := pullRebase(src, lg); err != nil {
		lg.Printf("files: pull: %v (recovering from the local clone's history)", err)
	}
	if err := gate(); err != nil {
		return nil, err
	}
	dlog, err := loadDeletionLog(src)
	if err != nil {
		return nil, err
	}
	var recovered []string
	type restore struct{ source, sha string }
	var restores []restore
	for _, p := range paths {
		abs, err := absPath(p)
		if err != nil {
			return nil, err
		}
		if isManaged(abs) {
			// Still synced. If it is only missing locally, put it back.
			if _, statErr := os.Lstat(abs); statErr == nil {
				lg.Printf("files: %s is synced and present, nothing to recover", p)
				continue
			}
			recovered = append(recovered, abs)
			continue
		}
		key := config.ContractHome(abs)
		entry, found := dlog[key]
		if !found {
			for k := range dlog {
				if atOrUnder(key, k) {
					return nil, fmt.Errorf("%s was deleted as part of %s: run `lichen recover %s`", p, k, k)
				}
			}
			return nil, fmt.Errorf("%s has no recorded deletion (if it was ever synced, its history is still in the sync repo)", p)
		}
		// The last commit touching the source file is its deletion, so the
		// commit before that holds the last content.
		sha, err := gitutil.Run(src, "rev-list", "-1", "HEAD", "--", entry.Source)
		if err != nil {
			return nil, err
		}
		if sha == "" {
			return nil, fmt.Errorf("%s: no history for %s in the sync repo", p, entry.Source)
		}
		restores = append(restores, restore{entry.Source, sha})
		recovered = append(recovered, abs)
	}
	if len(recovered) == 0 {
		return nil, nil
	}
	for _, r := range restores {
		if _, err := gitutil.Run(src, "checkout", r.sha+"^", "--", r.source); err != nil {
			// Leave nothing half-checked-out: a dirty worktree would
			// block every future pull. The repo has no intentional
			// uncommitted state, so resetting to HEAD is safe.
			gitutil.Run(src, "reset", "--hard", "HEAD")
			return nil, err
		}
	}
	if err := pruneDeletionLog(src, recovered); err != nil {
		return nil, err
	}
	subject, body := commitMsg("recover", recovered)
	if err := commitPush(cfg, subject, body, lg); err != nil {
		return nil, err
	}
	if err := applyBack(recovered); err != nil {
		return nil, err
	}
	if err := addToManifest(recovered); err != nil {
		return nil, err
	}
	return portablePaths(recovered), nil
}
