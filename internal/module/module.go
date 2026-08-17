// Package module wires lichen's sync units together. A module owns one
// kind of synced thing: files (chezmoi-managed paths) and skills (agent
// skills from public repos). Every reconcile pass runs them as a set.
// Order matters: files goes first because it pulls the sync repo,
// which can deliver new config files (say, a skills.json with a source
// added on another machine), and skills reads its config from disk, so
// it sees what files just applied.
package module

import (
	"errors"
	"fmt"
	"log"

	"lichen/internal/config"
	"lichen/internal/files"
	"lichen/internal/skills"
)

// ReconcileAll runs every module in order. Modules fail independently: a
// files error must not keep skills stale (or the reverse), so errors are
// joined rather than short-circuiting. Callers hold the cross-process
// lock and pass a freshly loaded config.
func ReconcileAll(cfg *config.Config, lg *log.Logger) error {
	var errs []error
	if err := files.Reconcile(cfg, lg); err != nil {
		errs = append(errs, fmt.Errorf("files: %w", err))
	}
	if err := skills.Reconcile(lg); err != nil {
		errs = append(errs, fmt.Errorf("skills: %w", err))
	}
	return errors.Join(errs...)
}
