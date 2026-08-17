// Package daemon is lichen's long-running core: reconcile on start, then
// react to the sync repo moving (via one idle ntfy stream) and to local
// edits of managed files (via fsnotify). An hourly pass is the backstop
// for events that never arrived. Each pass re-reads the config. Only the
// topic and server are fixed until restart.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"lichen/internal/config"
	"lichen/internal/events"
	"lichen/internal/files"
	"lichen/internal/proclock"
	"lichen/internal/selfupdate"
	"lichen/internal/version"
)

// pollInterval catches whatever the event stream missed: a push that
// happened while this machine was asleep, or a webhook that was never
// configured.
const pollInterval = time.Hour

func Run() error {
	lg := log.New(os.Stdout, "", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	lg.Printf("lichen %s starting (server %s)", version.Current, cfg.Server())
	hostname, _ := os.Hostname()

	// One mutex serializes every pass: a startup pass, the hourly pass,
	// watcher flushes, and event bursts must never run git or chezmoi
	// concurrently. rewatch nudges the file watcher to refresh its list
	// whenever the managed set may have changed.
	var mu sync.Mutex
	rewatch := make(chan struct{}, 1)
	// nudgeWatch asks the file watcher to refresh its managed-path list.
	// Non-blocking: a full buffer already means a refresh is pending.
	nudgeWatch := func() {
		select {
		case rewatch <- struct{}{}:
		default:
		}
	}
	// runLocked is the single doorway for daemon work that touches the
	// sync repo: in-process mutex, then the cross-process lock (inside mu
	// so this process never double-acquires it), then a fresh config load.
	// Every pass goes through it, so none can race a CLI command.
	runLocked := func(fn func(c *config.Config)) {
		mu.Lock()
		defer mu.Unlock()
		release, err := proclock.Acquire(ctx, nil)
		if err != nil {
			if ctx.Err() == nil {
				lg.Printf("lock: %v", err)
			}
			return
		}
		defer release()
		c, err := config.Load()
		if err != nil {
			// The config may be mid-rewrite by an apply: skip this cycle.
			lg.Printf("config: %v", err)
			return
		}
		fn(c)
	}
	reconcile := func() {
		var repoV string
		runLocked(func(c *config.Config) {
			err := files.Reconcile(c, lg)
			var outdated *files.OutdatedError
			if errors.As(err, &outdated) {
				lg.Printf("files: %v", outdated)
				repoV = outdated.Repo
				return
			}
			if err != nil {
				lg.Printf("files: reconcile: %v", err)
			}
			nudgeWatch()
		})
		// The update runs AFTER the locks are released: the download needs
		// neither, and holding them through a slow network would block
		// every CLI command and queued pass for its duration.
		if repoV != "" {
			autoUpdate(lg, repoV)
		}
	}

	// The startup pass runs in the background so the daemon is subscribed
	// and watching from the first seconds: on a cold machine the first
	// apply can take a while, and events arriving meanwhile just queue on
	// the mutex.
	go reconcile()

	go watchFiles(ctx, lg, runLocked, rewatch)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval):
				reconcile()
			}
		}
	}()

	// The topic is fixed for the daemon's lifetime (changing it needs a
	// restart, which the KeepAlive'd launchd agent makes a kill away).
	// Subscribe reconnects internally with backoff until ctx ends.
	handle := func(ev events.Event) {
		var n events.Nudge
		json.Unmarshal([]byte(ev.Message), &n)
		if n.Origin != "" && n.Origin == hostname {
			// This machine's own push. The work is already done, but a
			// CLI command may have added a path this daemon is not
			// watching yet.
			nudgeWatch()
			return
		}
		lg.Printf("events: sync repo moved")
		reconcile()
	}
	var lastErr string
	events.Client{Server: cfg.Server(), Topic: cfg.Topic}.Subscribe(ctx,
		func() {
			lastErr = ""
			lg.Printf("events: connected")
		},
		func(err error) {
			// Mask the topic: a net/url error embeds the full request
			// URL, and the log is tailed by `lichen logs` and pasted.
			if err.Error() != lastErr {
				lastErr = err.Error()
				lg.Printf("events: %s (retrying with backoff)", cfg.MaskTopic(err.Error()))
			}
		},
		handle)
	lg.Printf("lichen stopped")
	return nil
}

// updateBackoff spaces out failed auto-update attempts: the release
// download is the biggest I/O the daemon ever does, so an event burst
// must not repeat it back-to-back. The hourly pass is the natural retry.
const updateBackoff = 10 * time.Minute

// updateMu serializes update attempts (the reconcile and watcher paths
// both trigger them, outside runLocked) and guards lastUpdateAttempt. A
// successful attempt never records itself: the process is replaced.
var updateMu sync.Mutex
var lastUpdateAttempt time.Time

// autoUpdate installs the release the sync repo requires and execs it in
// place of this process: launchd sees the same PID, so KeepAlive's
// restart throttle never enters the picture. On failure the daemon stays
// up with syncing paused until a later pass gets through.
//
// The env guard survives the exec, unlike lastUpdateAttempt: if the
// installed release does not actually clear the requirement it was
// installed for (a mis-stamped asset), the fresh process must not
// download it again in a tight loop. A LATER requirement re-arms it.
func autoUpdate(lg *log.Logger, repoV string) {
	updateMu.Lock()
	defer updateMu.Unlock()
	if os.Getenv("LICHEN_AUTOUPDATED") == repoV {
		lg.Printf("update: already self-updated for %s yet still outdated (mis-stamped release?), not retrying", repoV)
		return
	}
	if since := time.Since(lastUpdateAttempt); since < updateBackoff {
		lg.Printf("update: last attempt failed %s ago, backing off", since.Round(time.Second))
		return
	}
	lastUpdateAttempt = time.Now()
	tag, err := selfupdate.Required(repoV)
	if err != nil {
		lg.Printf("update: %v (syncing stays paused, retrying on a later pass)", err)
		return
	}
	lg.Printf("update: installed %s, restarting the daemon on it", tag)
	os.Setenv("LICHEN_AUTOUPDATED", repoV)
	if err := selfupdate.ExecSelf(); err != nil {
		lg.Printf("update: exec: %v", err)
	}
}

// watchFiles pushes local edits out: fsnotify on the parent directories of
// every managed file (watching dirs, not files, survives the
// write-tmp-then-rename dance editors do), debounced, then re-add + commit
// + push. The loop is self-settling: our own `chezmoi apply` fires events
// too, but re-add of an unmodified file is a no-op and git has nothing to
// commit.
func watchFiles(ctx context.Context, lg *log.Logger, runLocked func(func(*config.Config)), rewatch <-chan struct{}) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		lg.Printf("files: watcher: %v", err)
		return
	}
	defer w.Close()

	managed := map[string]bool{}
	refresh := func() {
		if !files.Active() {
			return
		}
		paths, err := files.Managed()
		if err != nil {
			lg.Printf("files: watcher: %v", err)
			return
		}
		nm := map[string]bool{}
		dirs := map[string]bool{}
		for _, f := range paths {
			nm[f] = true
			dirs[filepath.Dir(f)] = true
		}
		for _, d := range w.WatchList() {
			if !dirs[d] {
				w.Remove(d)
			}
		}
		for d := range dirs {
			w.Add(d) // errors (e.g. dir not created yet) are retried on next refresh
		}
		managed = nm
	}
	refresh()

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	pending := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-rewatch:
			refresh()
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if managed[ev.Name] && ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 {
				pending[ev.Name] = true
				debounce.Reset(1500 * time.Millisecond)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			lg.Printf("files: watcher: %v", err)
		case <-debounce.C:
			if len(pending) == 0 {
				continue
			}
			paths := slices.Sorted(maps.Keys(pending))
			pending = map[string]bool{}
			// runLocked provides the same mutex and cross-process lock
			// sequence as every other mutating flow. An outdated refusal
			// triggers the self-update here too (post-lock), so a machine
			// whose user is actively editing does not wait for the hourly
			// pass; the backoff keeps edit bursts from hammering it.
			var repoV string
			runLocked(func(c *config.Config) {
				lg.Printf("files: local edit: %v", paths)
				err := files.ReAddPush(c, lg, paths)
				var outdated *files.OutdatedError
				if errors.As(err, &outdated) {
					lg.Printf("files: %v", outdated)
					repoV = outdated.Repo
					return
				}
				if err != nil {
					lg.Printf("files: %v", err)
				}
			})
			if repoV != "" {
				autoUpdate(lg, repoV)
			}
		}
	}
}
