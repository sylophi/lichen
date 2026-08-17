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

// gate orders this build against the sync repo's version marker. A build
// ahead of the marker (or a repo without one) records itself and pushes,
// which is what tells every other machine to update. A build behind it
// gets *OutdatedError and must not touch the repo. Dev builds are
// exempt: they cannot be ordered against release tags, nor replaced by
// the self-updater. Every mutating entry point runs this; the pull in
// Reconcile is what delivers a newer machine's bump.
func gate(cfg *config.Config, lg *log.Logger) error {
	if !version.IsRelease() {
		return nil
	}
	src, err := SourcePath()
	if err != nil {
		return err
	}
	marker := filepath.Join(src, version.Marker)
	repoV := ""
	if data, err := os.ReadFile(marker); err == nil {
		repoV = strings.TrimSpace(string(data))
		if repoV != "" && !version.Valid(repoV) {
			lg.Printf("files: ignoring malformed %s: %q", version.Marker, repoV)
			repoV = ""
		}
	}
	if repoV == "" || version.Compare(version.Current, repoV) > 0 {
		if err := os.WriteFile(marker, []byte(version.Current+"\n"), 0o644); err != nil {
			return err
		}
		lg.Printf("files: recording lichen %s in the sync repo", version.Current)
		return commitPush(cfg, "version "+version.Current, "", lg)
	}
	if version.Compare(version.Current, repoV) < 0 {
		return &OutdatedError{Build: version.Current, Repo: repoV}
	}
	return nil
}
