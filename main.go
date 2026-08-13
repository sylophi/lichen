// lichen keeps the files on your dev machines in sync: dotfiles, agent
// skills, slash commands, anything under your home directory. One daemon
// per machine, edits propagate within seconds via ntfy.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"lichen/internal/backup"
	"lichen/internal/config"
	"lichen/internal/daemon"
	"lichen/internal/events"
	"lichen/internal/files"
	"lichen/internal/gitutil"
	"lichen/internal/proclock"
)

// version is stamped by the release workflow via
// -ldflags "-X main.version=<tag>". Source builds stay "dev".
var version = "dev"

func main() {
	log.SetFlags(0)
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	var err error
	switch args[0] {
	case "status":
		err = cmdStatus(slices.Contains(args[1:], "--secrets"))
	case "sync":
		err = withLock(func() error { return cmdSync(args[1:]) })
	case "remove", "rm":
		err = withLock(func() error { return cmdRemove(args[1:]) })
	case "list", "ls":
		err = cmdList()
	case "logs":
		err = cmdLogs()
	case "daemon":
		err = daemon.Run(version)
	case "start", "stop", "restart":
		err = cmdDaemonCtl(args[0])
	case "update":
		err = cmdUpdate()
	case "uninstall":
		err = cmdUninstall(args[1:])
	case "version", "--version":
		fmt.Println("lichen " + version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "lichen: unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("lichen: %v", err)
	}
}

func usage() {
	fmt.Print(`lichen keeps the files on your machines in sync

  lichen sync <path...>            start syncing files across machines
  lichen sync                      pull and apply everything now
  lichen remove <path...>          stop syncing (local copies stay)
  lichen list                      show every synced file

  lichen status [--secrets]        daemon health and webhook setup
                                    (--secrets reveals the topic URL)
  lichen logs                      tail the daemon log
  lichen start | stop | restart    control the background daemon
  lichen update                    self-update to the latest release
  lichen uninstall [--yes]         remove lichen, keep your files
  lichen version                   print the installed version

aliases: rm = remove, ls = list
setup: install.sh   teardown: uninstall.sh (or lichen uninstall)
`)
	// `lichen daemon` is deliberately absent: it's launchd's entry point
	// (see install.sh's plist), not a command users run themselves.
}

func clilog() *log.Logger { return log.New(os.Stdout, "", 0) }

// withLock runs a mutating command holding the cross-process lock, so a
// CLI command and a daemon pass never race on the chezmoi git repo. It
// prints a hint if the daemon is mid-pass.
func withLock(fn func() error) error {
	release, err := proclock.Acquire(context.Background(), func() {
		fmt.Fprintln(os.Stderr, "waiting for the lichen daemon to finish its current sync...")
	})
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// Colors are for interactive status output only: never in daemon logs,
// never when piped, never when NO_COLOR is set.
var useColor = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}()

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func bold(s string) string { return paint("1", s) }
func dim(s string) string  { return paint("2", s) }

// cmdSync with paths starts managing them. With none it runs a full pass:
// pull, capture local edits, apply.
func cmdSync(paths []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	lg := clilog()
	if len(paths) == 0 {
		if err := files.Reconcile(cfg, lg); err != nil {
			return err
		}
		lg.Printf("ok")
		return nil
	}
	if err := files.Sync(cfg, lg, paths); err != nil {
		return err
	}
	fmt.Printf("syncing: %s\n", strings.Join(paths, ", "))
	return nil
}

func cmdRemove(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("usage: lichen remove <path...>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := files.Remove(cfg, clilog(), paths); err != nil {
		return err
	}
	fmt.Printf("stopped syncing: %s (local copies left in place)\n", strings.Join(paths, ", "))
	return nil
}

func cmdList() error {
	if !files.Active() {
		return fmt.Errorf("no sync repo initialized (run install.sh with LICHEN_REPO=<git url>)")
	}
	managed, err := files.Managed()
	if err != nil {
		return err
	}
	for _, p := range managed {
		fmt.Println(config.ContractHome(p))
	}
	return nil
}

func cmdLogs() error {
	p, err := config.LogPath()
	if err != nil {
		return err
	}
	tail, err := exec.LookPath("tail")
	if err != nil {
		return err
	}
	return syscall.Exec(tail, []string{"tail", "-n", "50", "-F", p}, os.Environ())
}

func cmdStatus(showSecrets bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	state := launchdState()
	if strings.HasPrefix(state, "running") {
		state = paint("32", state)
	} else {
		state = paint("33", state)
	}
	fmt.Printf("%s  %s\n", bold("daemon:"), state)
	// The topic is the shared secret gating the event channel: shown only
	// with --secrets so pasted status output leaks nothing.
	note := ""
	if showSecrets {
		note = "  " + dim("(topic: "+cfg.Topic+")")
	}
	fmt.Printf("%s  %s%s\n", bold("events:"), cfg.Server(), note)
	if p, err := config.LogPath(); err == nil {
		fmt.Printf("%s     %s\n", bold("log:"), dim(p))
	}
	for _, tool := range []string{"git", "chezmoi"} {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Printf("%s\n", paint("31", tool+" not found on PATH (brew install "+tool+")"))
		}
	}

	origin := files.Origin()
	if files.Active() {
		managed, _ := files.Managed()
		originNote := origin
		if originNote == "" {
			originNote = "(no origin: commits stay local)"
		}
		fmt.Printf("\n%s sync repo %s\n", bold(fmt.Sprintf("files (%d synced):", len(managed))), originNote)
	} else {
		fmt.Printf("\n%s %s\n", bold("files:"), paint("33", "not initialized (run install.sh with LICHEN_REPO=<git url>)"))
	}

	// The webhook is optional: lichen already nudges the other machines
	// after its own pushes. It covers pushes made outside lichen, from the
	// web UI or a machine without it.
	if key, ok := gitutil.HostedKey(origin); ok {
		url := events.Client{Server: cfg.Server(), Topic: cfg.Topic}.TopicURL()
		header := "webhook (optional, for pushes made outside lichen):"
		if !showSecrets {
			url = cfg.MaskTopic(url)
			header = "webhook (optional, for pushes made outside lichen, reveal with: lichen status --secrets):"
		}
		fmt.Printf("\n%s\n%s\n", bold(header),
			fmt.Sprintf("  %s\n    → github.com/%s → Settings → Webhooks (push events, any content type)", url, key))
	}
	if backup.NonEmpty() {
		if root, err := backup.Root(); err == nil {
			fmt.Printf("\n%s\n", dim("backups of overwritten files exist in "+root))
		}
	}
	return nil
}

const daemonLabel = "dev.lichen"

func guiDomain() string    { return fmt.Sprintf("gui/%d", os.Getuid()) }
func daemonTarget() string { return guiDomain() + "/" + daemonLabel }
func daemonLoaded() bool   { return exec.Command("launchctl", "print", daemonTarget()).Run() == nil }

func plistPath() (string, error) {
	home, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", daemonLabel+".plist"), nil
}

func launchctlRun(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func launchdState() string {
	out, err := exec.Command("launchctl", "print", daemonTarget()).CombinedOutput()
	if err != nil {
		return "not registered (run install.sh)"
	}
	state := "registered"
	if m := regexp.MustCompile(`state = (\w+)`).FindSubmatch(out); m != nil {
		state = string(m[1])
	}
	if m := regexp.MustCompile(`pid = (\d+)`).FindSubmatch(out); m != nil {
		state += " (pid " + string(m[1]) + ")"
	}
	return state
}

// cmdDaemonCtl controls the launchd agent: stop unloads it, start loads it,
// restart kickstarts a loaded one (or loads it if it was stopped).
func cmdDaemonCtl(action string) error {
	plist, err := plistPath()
	if err != nil {
		return err
	}
	loaded := daemonLoaded()
	switch action {
	case "stop":
		if !loaded {
			fmt.Println("daemon already stopped")
			return nil
		}
		if err := launchctlRun("bootout", daemonTarget()); err != nil {
			return err
		}
		fmt.Println("daemon stopped")
	case "start":
		if _, err := os.Stat(plist); err != nil {
			return fmt.Errorf("no launchd plist at %s (run install.sh first)", plist)
		}
		if loaded {
			fmt.Println("daemon already running")
			return nil
		}
		if err := launchctlRun("bootstrap", guiDomain(), plist); err != nil {
			return err
		}
		fmt.Println("daemon started")
	case "restart":
		if _, err := os.Stat(plist); err != nil {
			return fmt.Errorf("no launchd plist at %s (run install.sh first)", plist)
		}
		if loaded {
			if err := launchctlRun("kickstart", "-k", daemonTarget()); err != nil {
				return err
			}
		} else if err := launchctlRun("bootstrap", guiDomain(), plist); err != nil {
			return err
		}
		fmt.Println("daemon restarted")
	}
	return nil
}

const releaseRepo = "sylophi/lichen"

// httpGet fetches url, treating any non-200 as an error. The timeout
// covers the whole exchange, body read included. Callers close the body.
func httpGet(url, accept string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return resp, nil
}

// latestReleaseTag returns the tag_name of the latest GitHub release.
func latestReleaseTag() (string, error) {
	resp, err := httpGet("https://api.github.com/repos/"+releaseRepo+"/releases/latest", "application/vnd.github+json", 10*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var data struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.TagName == "" {
		return "", fmt.Errorf("release metadata has no tag_name")
	}
	return data.TagName, nil
}

// downloadRelease streams the tagged release asset for this platform
// into path (mode 0755), returning the byte count. The caller cleans up
// on error. Asset names here, install.sh's download URL, and the
// release workflow's build matrix must all agree.
func downloadRelease(tag, path string) (int64, error) {
	var arch string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		arch = "arm64"
	case "darwin/amd64":
		arch = "x64"
	default:
		return 0, fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/lichen-darwin-%s", releaseRepo, tag, arch)
	resp, err := httpGet(url, "", 5*time.Minute)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

// cmdUpdate self-updates to the latest GitHub release: fetch the latest
// tag, download this platform's asset, and rename it over the running
// binary.
func cmdUpdate() error {
	if version == "dev" {
		return fmt.Errorf("dev build (built from source): update by re-running dev.sh, or install the released binary via install.sh")
	}
	fmt.Println("checking the latest release...")
	tag, err := latestReleaseTag()
	if err != nil {
		return fmt.Errorf("fetching release info: %w", err)
	}
	if tag == version {
		fmt.Println("already at the latest release: " + version)
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	fmt.Printf("downloading %s...\n", tag)
	tmp := self + ".update"
	n, err := downloadRelease(tag, tmp)
	if err != nil {
		os.Remove(tmp)
		return err
	}
	// Reject a download too small to be a real build (an error page or a
	// truncated transfer).
	if n < 1_000_000 {
		os.Remove(tmp)
		return fmt.Errorf("downloaded file is suspiciously small (%d bytes), aborting", n)
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return err
	}
	fmt.Printf("updated %s -> %s\n", version, tag)
	if daemonLoaded() {
		if err := launchctlRun("kickstart", "-k", daemonTarget()); err == nil {
			fmt.Println("daemon restarted on the new build")
		}
	}
	return nil
}

// cmdUninstall unwinds lichen from this machine so it can later be
// reinstalled against the same or a different sync repo: it stops the
// daemon and removes the binary, lichen's config and local state, and the
// chezmoi source and state. The files lichen applied to $HOME are KEPT, as
// plain files, as is the sync repo itself.
func cmdUninstall(args []string) error {
	yes := len(args) > 0 && (args[0] == "--yes" || args[0] == "-y")
	if !yes {
		fmt.Println("This removes lichen from this machine:")
		fmt.Println("  daemon + launchd agent, the lichen binary, config (~/.config/lichen),")
		fmt.Println("  local state (~/.local/share/lichen), and the chezmoi source and state.")
		fmt.Println("Kept: every synced file in your home directory, and the sync repo on GitHub.")
		fmt.Print("Proceed? [y/N] ")
		reply, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil || !slices.Contains([]string{"y", "yes"}, strings.ToLower(strings.TrimSpace(reply))) {
			return fmt.Errorf("aborted (pass --yes to skip this prompt)")
		}
	}
	if daemonLoaded() {
		launchctlRun("bootout", daemonTarget())
	}
	var removed []string
	rm := func(path, label string) {
		if path == "" {
			return
		}
		if _, err := os.Stat(path); err != nil {
			return
		}
		if os.RemoveAll(path) == nil {
			removed = append(removed, label)
		}
	}
	if plist, err := plistPath(); err == nil {
		rm(plist, "launchd agent")
	}
	if p, err := config.Path(); err == nil {
		rm(filepath.Dir(p), "config (~/.config/lichen)")
	}
	if d, err := config.DataDir(); err == nil {
		rm(d, "local state (~/.local/share/lichen)")
	}
	// The chezmoi source and state, so a later install can initialize a
	// different sync repo cleanly. The applied files in $HOME are not here,
	// they stay put.
	if p, err := files.SourcePath(); err == nil {
		rm(p, "chezmoi source repo")
	}
	if home, err := config.Home(); err == nil {
		rm(filepath.Join(home, ".config", "chezmoi"), "chezmoi state")
	}
	// Binary last: on macOS a running executable removing its own path is
	// fine, the inode lives until the process exits.
	if self, err := os.Executable(); err == nil {
		rm(self, "binary")
	}
	fmt.Println("lichen uninstalled.")
	if len(removed) > 0 {
		fmt.Println("Removed: " + strings.Join(removed, ", "))
	}
	fmt.Println("Kept: the files in your home directory (now plain files), and your sync repo on GitHub.")
	fmt.Println("Re-run install.sh anytime to sync again, pointing at the same or a different repo.")
	return nil
}
