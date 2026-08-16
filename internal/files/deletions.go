// Deletion propagation: a synced file deleted locally is deleted on every
// machine. Two records make that safe. The deletion log in the sync repo
// (.lichen-deleted.json, ignored by chezmoi, tracked by git) says WHICH
// departures from the managed set are real deletions, as opposed to
// `lichen remove`, which keeps local copies. And each machine's local
// manifest of the managed set it saw last pass makes acting on a deletion
// a one-shot transition, so a file the user recreates at the same path
// later is never re-deleted. Content is never lost: the deleting
// machine's copy survives in the sync repo's git history (`lichen
// recover` brings it back), every other machine moves its copy into its
// backups dir.

package files

import (
	"encoding/json"
	"errors"
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
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(deletionLogPath(src), append(data, '\n'), 0o644)
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
			if key == t || strings.HasPrefix(key, t+"/") || strings.HasPrefix(t, key+"/") {
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

// ok=false (absent or unreadable) means no baseline yet: nothing can
// depart, so no deletion can fire. Unreadable deliberately degrades the
// same way, it is the direction that deletes nothing.
func loadManifest() ([]string, bool, error) {
	p, err := manifestPath()
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false, nil
	}
	var paths []string
	if json.Unmarshal(data, &paths) != nil {
		return nil, false, nil
	}
	return paths, true, nil
}

func saveManifest(prev []string, hadPrev bool, current []string) error {
	cur := slices.Sorted(slices.Values(current))
	if hadPrev && slices.Equal(slices.Sorted(slices.Values(prev)), cur) {
		return nil
	}
	p, err := manifestPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}

// dropNested returns sorted paths minus any that sit under another listed
// path: forgetting the top-most path covers its subtree in one entry.
func dropNested(paths []string) []string {
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	var out []string
	for _, p := range sorted {
		if len(out) > 0 && (p == out[len(out)-1] || strings.HasPrefix(p, out[len(out)-1]+"/")) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// dropEntryStateUnder clears chezmoi's memory of having written anything
// at or under the given paths (directories have entries of their own), so
// a file the user recreates at one later counts as foreign (backed up
// before any overwrite) instead of fair game, and so nothing lingering
// there reads as a local deletion on a later pass. Best-effort: a stale
// entry only weakens that backup, it breaks nothing.
func dropEntryStateUnder(paths []string) {
	all, err := entryStatePaths()
	if err != nil {
		return
	}
	for key := range all {
		for _, p := range paths {
			if key == p || strings.HasPrefix(key, p+"/") {
				chezmoi("state", "delete", "--bucket=entryState", "--key="+key)
				break
			}
		}
	}
}

// topMissing climbs from a missing file to the top-most missing managed
// ancestor: a deleted directory arrives from the watcher as its files,
// but the whole subtree should propagate as one deletion.
func topMissing(abs string) string {
	for {
		parent := filepath.Dir(abs)
		if _, err := os.Lstat(parent); err == nil {
			return abs
		}
		if _, err := chezmoi("source-path", parent); err != nil {
			return abs
		}
		abs = parent
	}
}

// PropagateDeletions handles synced paths deleted locally: forget them,
// record them on the deletion log, and push, so every other machine
// deletes its copy too. The one exception is lichen's own config, the
// keystone every machine needs to run at all: that one is restored.
func PropagateDeletions(cfg *config.Config, lg *log.Logger, paths []string) error {
	if !Active() {
		return nil
	}
	src, err := SourcePath()
	if err != nil {
		return err
	}
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	climbed := make([]string, 0, len(paths))
	for _, abs := range paths {
		if _, err := os.Lstat(abs); err == nil {
			continue // reappeared since it was seen missing: not a deletion
		}
		climbed = append(climbed, topMissing(abs))
	}
	var doomed []string
	sources := map[string]string{}
	for _, abs := range dropNested(climbed) {
		if abs == cfgPath {
			lg.Printf("files: restoring %s (lichen cannot run without its config, so its deletion never propagates)", abs)
			if _, err := chezmoi("apply", "--force", abs); err != nil {
				lg.Printf("files: restore: %v", err)
			}
			continue
		}
		sp, err := chezmoi("source-path", abs)
		if err != nil {
			continue // not managed (already forgotten): nothing to propagate
		}
		rel, err := filepath.Rel(src, sp)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		doomed = append(doomed, abs)
		sources[abs] = rel
	}
	if len(doomed) == 0 {
		return nil
	}
	dlog, err := loadDeletionLog(src)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	at := time.Now().UTC().Format(time.RFC3339)
	for _, abs := range doomed {
		dlog[config.ContractHome(abs)] = deletionEntry{Source: sources[abs], Host: host, At: at}
	}
	if err := saveDeletionLog(src, dlog); err != nil {
		return err
	}
	lg.Printf("files: deleted locally, deleting everywhere (`lichen recover` brings one back): %v", doomed)
	if _, err := chezmoi(append([]string{"forget", "--force"}, doomed...)...); err != nil {
		return err
	}
	dropEntryStateUnder(doomed)
	subject, body := commitMsg("delete", doomed)
	return commitPush(cfg, subject, body, lg)
}

// applyIncomingDeletions carries out deletions other machines pushed:
// every path that left the managed set since the last pass AND is on the
// deletion log gets its local copy moved to backups. Departures without a
// log entry (`lichen remove`, an ignore rule) keep their local copy. The
// first pass on a machine only records the baseline. Returns the previous
// pass's managed set (nil without a baseline) so the caller can tell a
// file this machine once saw from one that is newly arriving.
func applyIncomingDeletions(lg *log.Logger) (map[string]bool, error) {
	src, err := SourcePath()
	if err != nil {
		return nil, err
	}
	current, err := Managed()
	if err != nil {
		return nil, err
	}
	prev, ok, err := loadManifest()
	if err != nil {
		return nil, err
	}
	var prevSet map[string]bool
	if ok {
		prevSet = map[string]bool{}
		for _, p := range prev {
			prevSet[p] = true
		}
		curSet := map[string]bool{}
		for _, p := range current {
			curSet[p] = true
		}
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
	}
	return prevSet, saveManifest(prev, ok, current)
}

// addToManifest records freshly synced or recovered paths (for a
// directory, the managed files under it) as seen on this machine without
// waiting for the next full pass: deleting one right away must propagate,
// not read as a new arrival to restore.
func addToManifest(absPaths []string) error {
	prev, ok, err := loadManifest()
	if err != nil || !ok {
		return err // no baseline yet: the first full pass records one
	}
	current, err := Managed()
	if err != nil {
		return err
	}
	set := map[string]bool{}
	for _, p := range prev {
		set[p] = true
	}
	added := false
	for _, m := range current {
		if set[m] {
			continue
		}
		for _, p := range absPaths {
			if m == p || strings.HasPrefix(m, p+"/") {
				set[m] = true
				added = true
				break
			}
		}
	}
	if !added {
		return nil
	}
	return saveManifest(nil, false, slices.Collect(maps.Keys(set)))
}

// inManifest reports whether p, or any managed file under it (for a
// directory), was part of the machine's previous pass.
func inManifest(m map[string]bool, p string) bool {
	if m[p] {
		return true
	}
	for f := range m {
		if strings.HasPrefix(f, p+"/") {
			return true
		}
	}
	return false
}

func deleteDeparted(src string, departed []string, lg *log.Logger) error {
	dlog, err := loadDeletionLog(src)
	if err != nil || len(dlog) == 0 {
		return err
	}
	for _, target := range slices.Sorted(maps.Keys(dlog)) {
		abs, err := config.ExpandHome(target)
		if err != nil {
			continue
		}
		// A log entry for a directory is matched by the departure of any
		// managed file that lived under it.
		hit := slices.ContainsFunc(departed, func(d string) bool {
			return d == abs || strings.HasPrefix(d, abs+"/")
		})
		if !hit {
			continue
		}
		dropEntryStateUnder([]string{abs})
		if _, err := os.Lstat(abs); err != nil {
			continue // already gone here
		}
		to, err := backup.Move(abs)
		if err != nil {
			lg.Printf("files: deleting %s: %v", abs, err)
			continue
		}
		lg.Printf("files: deleted %s (kept at %s; `lichen recover` re-syncs it)", abs, to)
	}
	return nil
}

// Recover brings back files that were deleted everywhere: the last
// version is lifted out of the sync repo's git history, synced again, and
// applied, here and (through the push) on every other machine. Returns
// the paths it brought back.
func Recover(cfg *config.Config, lg *log.Logger, paths []string) ([]string, error) {
	if !Active() {
		return nil, fmt.Errorf("no sync repo initialized")
	}
	src, err := SourcePath()
	if err != nil {
		return nil, err
	}
	abortStaleRebase(src, lg)
	if Origin() != "" {
		if _, err := gitutil.Run(src, "pull", "--rebase", "-X", "theirs", "--quiet"); err != nil {
			abortStaleRebase(src, lg)
			lg.Printf("files: pull: %v (recovering from the local clone's history)", err)
		}
	}
	dlog, err := loadDeletionLog(src)
	if err != nil {
		return nil, err
	}
	var recovered []string
	for _, p := range paths {
		abs, err := absPath(p)
		if err != nil {
			return nil, err
		}
		if _, err := chezmoi("source-path", abs); err == nil {
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
			return nil, fmt.Errorf("%s has no recorded deletion; if it was ever synced, its history is still in the sync repo (git log)", p)
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
		if _, err := gitutil.Run(src, "checkout", sha+"^", "--", entry.Source); err != nil {
			return nil, err
		}
		delete(dlog, key)
		recovered = append(recovered, abs)
	}
	if len(recovered) == 0 {
		return nil, nil
	}
	if err := saveDeletionLog(src, dlog); err != nil {
		return nil, err
	}
	subject, body := commitMsg("recover", recovered)
	if err := commitPush(cfg, subject, body, lg); err != nil {
		return nil, err
	}
	if _, err := chezmoi(append([]string{"apply", "--force"}, recovered...)...); err != nil {
		return nil, err
	}
	if err := addToManifest(recovered); err != nil {
		return nil, err
	}
	return recovered, nil
}

// RestoreConfig puts a deleted config.json back from the sync repo.
// Without it no pass can run at all, so a missing config would otherwise
// wedge this machine until a reinstall. Only an ABSENT file is restored:
// an unparseable one may be mid-rewrite by a concurrent apply.
func RestoreConfig(lg *log.Logger) {
	if !Active() {
		return
	}
	p, err := config.Path()
	if err != nil {
		return
	}
	if _, err := os.Stat(p); err == nil || !os.IsNotExist(err) {
		return
	}
	if _, err := chezmoi("source-path", p); err != nil {
		return
	}
	lg.Printf("files: config missing, restoring it from the sync repo")
	if _, err := chezmoi("apply", "--force", p); err != nil {
		lg.Printf("files: restore config: %v", err)
	}
}

// LocalChange handles paths the watcher saw change: files still present
// are captured with re-add, files now missing are deleted everywhere.
func LocalChange(cfg *config.Config, lg *log.Logger, paths []string) error {
	existing, missing := partitionExisting(paths)
	var errs []error
	if len(existing) > 0 {
		errs = append(errs, ReAddPush(cfg, lg, existing))
	}
	if len(missing) > 0 {
		errs = append(errs, PropagateDeletions(cfg, lg, missing))
	}
	return errors.Join(errs...)
}
