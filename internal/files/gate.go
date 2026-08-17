package files

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"lichen/internal/config"
	"lichen/internal/gitutil"
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
// outversions this build. Read-only and network-free, so it sees the
// marker as of the last pull: entry points get best-effort protection,
// and the two places a pull actually lands (Reconcile, and commitPush's
// conflict retry) re-check afterwards to close the gap on the push side.
// Dev builds are exempt: they cannot be ordered against release tags,
// nor replaced by the self-updater.
func gate() error {
	repoV, err := markerVersion()
	if err != nil {
		return err
	}
	return gateAgainst(repoV)
}

func gateAgainst(repoV string) error {
	if version.IsRelease() && repoV != "" && version.Compare(version.Current, repoV) < 0 {
		return &OutdatedError{Build: version.Current, Repo: repoV}
	}
	return nil
}

// syncMarker is Reconcile's post-pull pass over the marker, one read for
// all three steps: undo any rebase downgrade, refuse if this build is
// behind, and record this build if it is ahead. Recording is skipped
// when the pull failed (unless there is no origin to race): a bump
// computed against a marker that could not be refreshed may be stale
// and, replayed by the local-wins rebase, would downgrade a newer
// machine's bump. Record failures are logged, not fatal (version
// bookkeeping must never stop syncing), except when the record's own
// push retry discovered we are outdated after all.
func syncMarker(cfg *config.Config, lg *log.Logger, pulled bool) error {
	repairMarker(lg)
	repoV, err := markerVersion()
	if err != nil {
		return err
	}
	if err := gateAgainst(repoV); err != nil {
		return err
	}
	if !pulled || !version.IsRelease() {
		return nil
	}
	if repoV != "" && version.Compare(version.Current, repoV) <= 0 {
		return nil // equal: nothing to record (behind was refused above)
	}
	if err := recordVersion(cfg, lg); err != nil {
		var outdated *OutdatedError
		if errors.As(err, &outdated) {
			return err
		}
		lg.Printf("files: recording version: %v (continuing)", err)
	}
	return nil
}

// recordVersion bumps the sync repo's marker to this build and pushes,
// which is what tells every other machine to update.
func recordVersion(cfg *config.Config, lg *log.Logger) error {
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

// repairMarker undoes a marker downgrade left by a rebase: the sync
// repo's conflict policy keeps OUR replayed commits (local wins), but
// the marker's semantics are max-wins, so a concurrent bump from a
// newer machine must be restored from upstream. The restore is committed
// immediately so the next push carries it. Best-effort: with no
// upstream, or no marker there, there is nothing to compare against.
func repairMarker(lg *log.Logger) {
	src, err := SourcePath()
	if err != nil {
		return
	}
	up, err := gitutil.Run(src, "show", "@{u}:"+version.Marker)
	if err != nil {
		return
	}
	upV := strings.TrimSpace(up)
	if !version.Valid(upV) {
		return
	}
	local, err := markerVersion()
	if err == nil && local != "" && version.Compare(local, upV) >= 0 {
		return
	}
	if os.WriteFile(filepath.Join(src, version.Marker), []byte(upV+"\n"), 0o644) != nil {
		return
	}
	lg.Printf("files: restoring version marker %s from upstream (rebase kept a stale copy)", upV)
	if _, err := gitutil.Run(src, "add", version.Marker); err == nil {
		host, _ := os.Hostname()
		gitutil.Run(src, "commit", "--quiet", "-m", fmt.Sprintf("lichen(%s): restore version %s", host, upV))
	}
}

// markerVersion reads the sync repo's version marker: "" when the file
// is absent or malformed (no requirement), an error when it exists but
// cannot be read. An unreadable marker must not silently disable the
// gate.
func markerVersion() (string, error) {
	src, err := SourcePath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(src, version.Marker))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if v := strings.TrimSpace(string(data)); version.Valid(v) {
		return v, nil
	}
	return "", nil
}
