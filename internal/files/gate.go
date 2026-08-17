package files

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"lichen/internal/config"
	"lichen/internal/version"
)

// OutdatedError refuses a mutating operation because this binary is
// older than the version recorded in the sync repo: a stale build must
// never capture, apply, or push over what a newer machine wrote. Callers
// react by self-updating.
type OutdatedError struct{ Build, Repo string }

func (e *OutdatedError) Error() string {
	return fmt.Sprintf("this build (%s) is older than the sync repo requires (%s): syncing is paused until it updates", e.Build, e.Repo)
}

// gate refuses with *OutdatedError when the sync repo's version marker
// outversions this build. Read-only, so every mutating entry point can
// afford it; recording a NEWER build into the marker is recordVersion's
// job. Dev builds are exempt: they cannot be ordered against release
// tags, nor replaced by the self-updater.
func gate() error {
	repoV, err := markerVersion()
	if err != nil {
		return err
	}
	if version.IsRelease() && repoV != "" && version.Compare(version.Current, repoV) < 0 {
		return &OutdatedError{Build: version.Current, Repo: repoV}
	}
	return nil
}

// recordVersion bumps the sync repo's marker to this build and pushes,
// which is what tells every other machine to update. Only Reconcile
// calls it, right after the pull: a bump computed against a marker that
// was never pulled could be stale and, replayed by the local-wins
// rebase, would downgrade a newer machine's bump. Rewriting a missing or
// malformed marker also heals corruption.
func recordVersion(cfg *config.Config, lg *log.Logger) error {
	if !version.IsRelease() {
		return nil
	}
	repoV, err := markerVersion()
	if err != nil {
		return err
	}
	if repoV != "" && version.Compare(version.Current, repoV) <= 0 {
		return nil
	}
	src, err := SourcePath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(src, version.Marker), []byte(version.Current+"\n"), 0o644); err != nil {
		return err
	}
	lg.Printf("files: recording lichen %s in the sync repo", version.Current)
	return commitPush(cfg, "version "+version.Current, "", lg)
}

// markerVersion reads the sync repo's version marker, returning "" for a
// missing or malformed one (both mean "no requirement").
func markerVersion() (string, error) {
	src, err := SourcePath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(src, version.Marker))
	if err != nil {
		return "", nil
	}
	if v := strings.TrimSpace(string(data)); version.Valid(v) {
		return v, nil
	}
	return "", nil
}
