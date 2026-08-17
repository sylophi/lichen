// Package skills is lichen's skills module: it installs agent skills
// (SKILL.md directories, the format the Vercel skills CLI popularized)
// from public git repos and keeps them fresh by polling upstream. The
// canonical copy of a skill lives in ~/.agents/skills/<name>, and each
// configured harness sees it through a symlink (default: Claude Code's
// ~/.claude/skills/<name>), matching the skills CLI's own layout. WHICH
// repos to sync and which harnesses to link live in this module's own
// config file (~/.config/lichen/skills.json), which the files module
// carries to every machine. Clones and install bookkeeping stay
// machine-local. Only skills recorded in the local manifest are ever
// touched, so skills installed by other means (npx skills, a manual
// copy) always coexist with lichen's.
package skills

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"lichen/internal/config"
	"lichen/internal/gitutil"
)

// pollEvery throttles upstream checks. Reconciles run much more often
// than skill repos change (startup, every sync-repo event), and each
// check is a network round-trip per repo, so between polls a reconcile
// works from the local clone. `lichen skills update` bypasses it.
const pollEvery = time.Hour

func agentsSkillsDir() (string, error) {
	return config.HomeJoin(".agents", "skills")
}

func claudeSkillsDir() (string, error) {
	return config.HomeJoin(".claude", "skills")
}

// Source selects agent skills to sync from one public git repo. An
// empty Only means every skill the repo publishes, minus Except.
// Except records removals from an every-skill source as INTENT: pinning
// the remaining skills instead would bake one machine's install state
// (name collisions, foreign dirs it skipped) into the shared config and
// uninstall skills other machines legitimately have.
type Source struct {
	Repo   string   `json:"repo"`
	Only   []string `json:"only,omitempty"`
	Except []string `json:"except,omitempty"`
}

// skillsConfig is the synced skills config file. Harnesses lists the
// directories that get a per-skill symlink to the canonical copy, in ~/
// form (the file travels between machines). Absent means Claude Code's
// ~/.claude/skills. Hand-edited for now: lichen has no CLI for it.
type skillsConfig struct {
	Sources   []Source `json:"sources"`
	Harnesses []string `json:"harnesses,omitempty"`
}

// loadConfig reads the synced skills config. The exists flag lets the
// caller tell "no file yet" (a fresh machine before its first sync, or
// a file briefly moved aside) apart from a file that says no sources.
func loadConfig() (cfg *skillsConfig, exists bool, err error) {
	p, err := config.SkillsPath()
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &skillsConfig{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	cfg = &skillsConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, true, fmt.Errorf("parsing %s: %w", p, err)
	}
	return cfg, true, nil
}

func loadSources() ([]Source, error) {
	cfg, _, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return cfg.Sources, nil
}

// harnessDirs resolves the configured harness link directories to
// absolute paths, defaulting to Claude Code's. Unresolvable entries are
// logged and dropped rather than failing the pass.
func (c *skillsConfig) harnessDirs(lg *log.Logger) []string {
	entries := c.Harnesses
	if len(entries) == 0 {
		if d, err := claudeSkillsDir(); err == nil {
			return []string{d}
		}
		return nil
	}
	var dirs []string
	for _, e := range entries {
		d, err := config.ExpandHome(e)
		if err != nil || !filepath.IsAbs(d) {
			lg.Printf("skills: ignoring harness dir %q (not an absolute or ~/ path)", e)
			continue
		}
		d = filepath.Clean(d)
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// saveSources rewrites the sources list inside skills.json, keeping any
// top-level field this build doesn't know about: the file is shared by
// machines that may run different lichen versions.
func saveSources(sources []Source) error {
	p, err := config.SkillsPath()
	if err != nil {
		return err
	}
	raw := map[string]json.RawMessage{}
	if data, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parsing %s: %w", p, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if len(sources) == 0 {
		// The file stays (deleting a managed file makes the files module
		// restore it), just with no sources.
		delete(raw, "sources")
	} else {
		enc, err := json.Marshal(sources)
		if err != nil {
			return err
		}
		raw["sources"] = enc
	}
	return writeJSON(p, raw)
}

// writeJSON writes v as pretty JSON via a temp file and rename, so a
// crash never leaves a half-written file behind.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// The manifest records what THIS machine has installed and from where.
// It is the ownership boundary: a directory not listed here was put in
// place by something else and is never written or removed.
type manifest struct {
	// Repos maps a repo key to its last upstream poll. The zero time
	// means never polled.
	Repos  map[string]time.Time   `json:"repos,omitempty"`
	Skills map[string]*skillState `json:"skills,omitempty"`
}

type skillState struct {
	Repo  string   `json:"repo"`            // canonical "host/owner/repo" key
	Tree  string   `json:"tree"`            // git tree hash of the installed skill dir
	Links []string `json:"links,omitempty"` // harness symlinks lichen maintains
}

func manifestPath() (string, error) {
	d, err := config.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "skills-manifest.json"), nil
}

func clonesRoot() (string, error) {
	d, err := config.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "skill-repos"), nil
}

func loadManifest() (*manifest, error) {
	m := &manifest{Repos: map[string]time.Time{}, Skills: map[string]*skillState{}}
	p, err := manifestPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	if m.Repos == nil {
		m.Repos = map[string]time.Time{}
	}
	if m.Skills == nil {
		m.Skills = map[string]*skillState{}
	}
	return m, nil
}

func (m *manifest) save() error {
	p, err := manifestPath()
	if err != nil {
		return err
	}
	return writeJSON(p, m)
}

// parseSpec canonicalizes a repo spec to a "host/owner/repo" key and a
// cloneable URL. Accepted: "owner/repo" (GitHub), "host/owner/repo", full
// https:// URLs (a trailing /tree/<branch>/... is dropped), and scp-style
// git@host:owner/repo. Skills come from public repos, so the URL is
// always anonymous HTTPS and never carries credentials.
func parseSpec(spec string) (key, url string, err error) {
	s := strings.TrimSpace(spec)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if at := strings.Index(s, "@"); at >= 0 {
		s = strings.Replace(s[at+1:], ":", "/", 1)
	}
	s = strings.TrimSuffix(strings.Trim(s, "/"), ".git")
	parts := strings.Split(s, "/")
	switch {
	case len(parts) == 2 && !strings.Contains(parts[0], "."):
		parts = append([]string{"github.com"}, parts...)
	case len(parts) >= 3 && strings.Contains(parts[0], "."):
		// host/owner/repo, possibly with a path tail to ignore.
	default:
		return "", "", fmt.Errorf("unrecognized repo spec %q (want owner/repo or a git URL)", spec)
	}
	host, owner, repo := strings.ToLower(parts[0]), parts[1], strings.TrimSuffix(parts[2], ".git")
	if host == "" || owner == "" || repo == "" {
		return "", "", fmt.Errorf("unrecognized repo spec %q (want owner/repo or a git URL)", spec)
	}
	key = host + "/" + owner + "/" + repo
	return key, "https://" + key + ".git", nil
}

// repoName is the key's last segment, the fallback skill name for a
// single-skill repo whose SKILL.md sits at the clone root.
func repoName(key string) string { return path.Base(key) }

// cloneDirName flattens a repo key into one path component.
func cloneDirName(key string) string { return strings.ReplaceAll(key, "/", "__") }

// sanitizeName normalizes a skill name into a safe directory name:
// lowercase, [a-z0-9._-] only, never empty and never a path.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), ".-")
}

// frontmatterName pulls the name out of SKILL.md's YAML frontmatter
// ("" when absent). A full YAML parser is overkill for one flat key.
func frontmatterName(md []byte) string {
	lines := strings.Split(string(md), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		t := strings.TrimSpace(line)
		if t == "---" {
			return ""
		}
		if after, ok := strings.CutPrefix(t, "name:"); ok {
			return strings.Trim(strings.TrimSpace(after), `"'`)
		}
	}
	return ""
}

// discover walks a clone for skill directories: any directory at most
// three levels deep holding a SKILL.md, which covers the containers the
// skills CLI uses (a single-skill repo root, skills/<name>,
// .claude/skills/<name>, skills/<category>/<name>). A skill's own
// subtree is not searched further, so a shallow skill shadows nested
// duplicates. Returns skill name → dir relative to root.
func discover(root, repo string) map[string]string {
	found := map[string]string{}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if md, err := os.ReadFile(filepath.Join(root, dir, "SKILL.md")); err == nil {
			fallback := filepath.Base(dir)
			if dir == "." {
				fallback = repo
			}
			name := frontmatterName(md)
			if name = sanitizeName(name); name == "" {
				name = sanitizeName(fallback)
			}
			if name != "" && found[name] == "" {
				found[name] = dir
			}
			return
		}
		if depth == 3 {
			return
		}
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == ".git" || e.Name() == "node_modules" {
				continue
			}
			walk(filepath.Join(dir, e.Name()), depth+1)
		}
	}
	walk(".", 0)
	return found
}

// ensureClone makes the machine-local clone of a skill repo exist and,
// at most once per pollEvery (always when force), checks upstream for
// movement and fast-forwards to it. A poll that fails leaves the local
// clone in service: availability beats freshness for a laptop that went
// offline.
func ensureClone(key, url string, man *manifest, force bool, lg *log.Logger) (string, error) {
	root, err := clonesRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, cloneDirName(key))
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr != nil {
		os.RemoveAll(dir)
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", err
		}
		if _, err := gitutil.Run(root, "clone", "--quiet", "--depth", "1", url, dir); err != nil {
			return "", err
		}
		man.Repos[key] = time.Now()
		lg.Printf("skills: fetched %s", key)
		return dir, nil
	}
	// rev-parse doubles as a clone health check: a wedged clone would
	// otherwise discover nothing and look like every skill vanished.
	head, err := gitutil.Run(dir, "rev-parse", "HEAD")
	if err != nil {
		// A corrupt clone heals on the next pass: remove it so the clone
		// branch above runs then.
		os.RemoveAll(dir)
		return "", err
	}
	if force || time.Since(man.Repos[key]) >= pollEvery {
		out, err := gitutil.Run(dir, "ls-remote", "origin", "HEAD")
		if err != nil {
			lg.Printf("skills: %s: poll failed: %v (using local copy)", key, err)
			return dir, nil
		}
		man.Repos[key] = time.Now()
		remote, _, _ := strings.Cut(out, "\t")
		if remote != "" && remote != head {
			if _, err := gitutil.Run(dir, "fetch", "--quiet", "--depth", "1", "origin", "HEAD"); err != nil {
				lg.Printf("skills: %s: fetch failed: %v (using local copy)", key, err)
				return dir, nil
			}
			if _, err := gitutil.Run(dir, "reset", "--hard", "--quiet", "FETCH_HEAD"); err != nil {
				lg.Printf("skills: %s: reset failed: %v (using local copy)", key, err)
				return dir, nil
			}
			gitutil.Run(dir, "clean", "-fdq")
			lg.Printf("skills: %s moved to %.8s", key, remote)
		}
	}
	return dir, nil
}

// treeHash is a content id for one skill's files: the git tree hash of
// its directory at the clone's HEAD, free to compute and unchanged by
// commits that touch other skills. This is what makes updates per-skill
// (the Vercel skills CLI keeps an equivalent folder hash in its
// lockfile): a repo moving only reinstalls the skills whose trees moved.
func treeHash(clone, rel string) (string, error) {
	spec := "HEAD^{tree}"
	if rel != "." {
		spec = "HEAD:" + filepath.ToSlash(rel)
	}
	return gitutil.Run(clone, "rev-parse", spec)
}

// copyDir installs src as dst via a sibling temp dir and rename, so a
// half-copied skill is never left at the final path. Only regular files
// and directories are copied: symlinks in a repo have no business being
// installed into an agent's skill dir.
func copyDir(src, dst string) error {
	tmp := dst + ".lichen-new"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(tmp, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, info.Mode().Perm())
	})
	if err == nil {
		if err = os.RemoveAll(dst); err == nil {
			err = os.Rename(tmp, dst)
		}
	}
	if err != nil {
		os.RemoveAll(tmp)
	}
	return err
}

// realpath resolves symlinks in p, falling back to p unchanged. Link
// targets must be computed against the directory the kernel actually
// resolves them in: a harness dir like ~/.claude may itself be a
// symlink into a dotfiles tree, where the logical parent lies.
func realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// linksTo reports whether the symlink at link points at canonical,
// resolving a relative destination against the link's real directory.
// The Vercel skills CLI writes relative links
// (../../.agents/skills/<name>), so an equivalent link from either tool
// is recognized as the same.
func linksTo(link, canonical string) bool {
	dest, err := os.Readlink(link)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(realpath(filepath.Dir(link)), dest)
	}
	dest = filepath.Clean(dest)
	return dest == canonical || dest == realpath(canonical)
}

// ensureLink points <dir>/<name> at the canonical copy so that harness
// picks the skill up. The link is relative, exactly what the Vercel
// skills CLI would create. Anything already at that path that does not
// resolve to the canonical copy belongs to another installer and is left
// alone. Reports whether the link is in place (and thus ours to manage).
func ensureLink(name, canonical, dir string, lg *log.Logger) bool {
	link := filepath.Join(dir, name)
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 && linksTo(link, canonical) {
			return true
		}
		lg.Printf("skills: NOT linking %s into %s (something else is already there)", name, config.ContractHome(dir))
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		lg.Printf("skills: %v", err)
		return false
	}
	rel, err := filepath.Rel(realpath(dir), realpath(canonical))
	if err != nil {
		rel = canonical
	}
	if err := os.Symlink(rel, link); err != nil {
		lg.Printf("skills: %v", err)
		return false
	}
	return true
}

// syncLinks makes a skill's harness symlinks match the configured dirs:
// links we maintained in a dir no longer configured are removed (only
// when they still resolve to our canonical copy), missing ones are
// created. Returns the links now under lichen's management.
func syncLinks(name, canonical string, prev, dirs []string, lg *log.Logger) []string {
	for _, old := range prev {
		if !slices.Contains(dirs, filepath.Dir(old)) && linksTo(old, canonical) {
			os.Remove(old)
		}
	}
	var links []string
	for _, d := range dirs {
		if ensureLink(name, canonical, d, lg) {
			links = append(links, filepath.Join(d, name))
		}
	}
	return links
}

// uninstall removes a manifest-owned skill: its harness symlinks first
// (only those that still resolve to our copy), then the canonical copy.
func uninstall(name string, links []string, lg *log.Logger) {
	agents, err := agentsSkillsDir()
	if err != nil {
		return
	}
	canonical := filepath.Join(agents, name)
	for _, link := range links {
		if linksTo(link, canonical) {
			os.Remove(link)
		}
	}
	os.RemoveAll(canonical)
	lg.Printf("skills: removed %s", name)
}

// target is one skill the config wants installed.
type target struct {
	repo string // canonical repo key
	dir  string // absolute skill dir inside the clone
	tree string // tree hash of the skill dir
}

// Reconcile makes the installed skills match the skills config: clone or
// poll each configured repo (polls are throttled), install or refresh
// every selected skill, and remove manifest-owned skills the config no
// longer wants. Per-repo failures are logged and skipped so one dead
// repo never blocks the rest, and a repo that failed keeps its installed
// skills.
func Reconcile(lg *log.Logger) error {
	return run(lg, false)
}

// Update is Reconcile without the poll throttle: every repo is checked
// against upstream now.
func Update(lg *log.Logger) error {
	return run(lg, true)
}

func run(lg *log.Logger, force bool) error {
	scfg, exists, err := loadConfig()
	if err != nil {
		return err
	}
	sources := scfg.Sources
	man, err := loadManifest()
	if err != nil {
		return err
	}
	if len(sources) == 0 && len(man.Skills) == 0 && len(man.Repos) == 0 {
		return nil
	}
	// An absent skills.json is not "zero sources": a fresh machine's
	// first pass runs before the sync repo delivers the file, and an
	// apply can briefly move it aside. Treating absence as emptiness
	// would turn those windows into a mass uninstall.
	if !exists && len(man.Skills) > 0 {
		lg.Printf("skills: no skills.json yet, leaving installed skills alone")
		return nil
	}
	harnesses := scfg.harnessDirs(lg)
	if len(harnesses) == 0 {
		// Every configured harness entry was unusable: touching links
		// now would strip every skill from every harness over a typo.
		lg.Printf("skills: no usable harness dir, leaving existing links alone")
	}

	// Fold config entries into one selection per repo (a hand-edited
	// config may list a repo twice).
	type selection struct {
		url          string
		all          bool
		only, except map[string]bool
	}
	selections := map[string]*selection{}
	var order []string
	// An entry this build can't parse may be a newer lichen's spec form,
	// covering installed skills we can't attribute: removals are off for
	// the whole pass rather than uninstalling what it covers.
	skipRemovals := false
	for _, src := range sources {
		key, url, err := parseSpec(src.Repo)
		if err != nil {
			skipRemovals = true
			lg.Printf("skills: %v", err)
			continue
		}
		sel := selections[key]
		if sel == nil {
			sel = &selection{url: url, only: map[string]bool{}, except: map[string]bool{}}
			selections[key] = sel
			order = append(order, key)
		}
		if len(src.Only) == 0 {
			sel.all = true
		}
		for _, n := range src.Only {
			sel.only[sanitizeName(n)] = true
		}
		for _, n := range src.Except {
			sel.except[sanitizeName(n)] = true
		}
	}

	failed := map[string]bool{}
	desired := map[string]target{}
	for _, key := range order {
		sel := selections[key]
		dir, err := ensureClone(key, sel.url, man, force, lg)
		if err != nil {
			failed[key] = true
			lg.Printf("skills: %s: %v (keeping what's installed)", key, err)
			continue
		}
		found := discover(dir, repoName(key))
		if !sel.all {
			for name := range sel.only {
				if found[name] == "" {
					lg.Printf("skills: %s has no skill %q", key, name)
				}
			}
		}
		for _, name := range slices.Sorted(maps.Keys(found)) {
			if sel.all {
				if sel.except[name] {
					continue
				}
			} else if !sel.only[name] {
				continue
			}
			// A duplicate can only come from an earlier repo: names are
			// unique within one repo's discovery.
			if prev, dup := desired[name]; dup {
				lg.Printf("skills: %s also provides %q (keeping the copy from %s)", key, name, prev.repo)
				continue
			}
			tree, err := treeHash(dir, found[name])
			if err != nil {
				// Failing to hash must not read as "the skill vanished":
				// protect the whole repo's installs from removal.
				failed[key] = true
				lg.Printf("skills: %s: %v", key, err)
				continue
			}
			desired[name] = target{repo: key, dir: filepath.Join(dir, found[name]), tree: tree}
		}
	}

	agents, err := agentsSkillsDir()
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(desired)) {
		t := desired[name]
		canonical := filepath.Join(agents, name)
		st := man.Skills[name]
		_, statErr := os.Lstat(canonical)
		if st == nil && statErr == nil {
			lg.Printf("skills: NOT installing %s (already at %s via another tool; remove it to let lichen manage it)", name, config.ContractHome(canonical))
			continue
		}
		if st == nil || st.Tree != t.tree || st.Repo != t.repo || statErr != nil {
			if err := os.MkdirAll(agents, 0o755); err != nil {
				return err
			}
			if err := copyDir(t.dir, canonical); err != nil {
				lg.Printf("skills: installing %s: %v", name, err)
				continue
			}
			var prevLinks []string
			if st != nil {
				prevLinks = st.Links
			}
			st = &skillState{Repo: t.repo, Tree: t.tree, Links: prevLinks}
			man.Skills[name] = st
			// Persist each install as it lands: a pass killed between
			// copy and a single final save would leave the directory on
			// disk but unowned, and the foreign-dir guard would then
			// refuse to ever manage it again.
			if err := man.save(); err != nil {
				return err
			}
			lg.Printf("skills: installed %s@%.8s from %s", name, t.tree, t.repo)
		}
		if len(harnesses) > 0 {
			st.Links = syncLinks(name, canonical, st.Links, harnesses, lg)
		}
	}

	if !skipRemovals {
		for _, name := range slices.Sorted(maps.Keys(man.Skills)) {
			if _, ok := desired[name]; ok {
				continue
			}
			// A configured repo that failed this pass keeps its skills:
			// an offline poll must never uninstall anything.
			if failed[man.Skills[name].Repo] {
				continue
			}
			uninstall(name, man.Skills[name].Links, lg)
			delete(man.Skills, name)
		}
		for _, key := range slices.Sorted(maps.Keys(man.Repos)) {
			if _, ok := selections[key]; ok {
				continue
			}
			if root, err := clonesRoot(); err == nil {
				os.RemoveAll(filepath.Join(root, cloneDirName(key)))
			}
			delete(man.Repos, key)
		}
	}
	return man.save()
}

// AddSource records a repo (optionally narrowed to specific skills) as a
// skill source and saves the skills config. Install happens on the next
// reconcile, which the CLI runs immediately after.
func AddSource(spec string, only []string) (string, error) {
	key, _, err := parseSpec(spec)
	if err != nil {
		return "", err
	}
	sources, err := loadSources()
	if err != nil {
		return "", err
	}
	for i, raw := range only {
		if only[i] = sanitizeName(raw); only[i] == "" {
			// Dropping it silently would leave an empty pin, which
			// serializes away and reads back as "every skill".
			return "", fmt.Errorf("invalid skill name %q", raw)
		}
	}
	slices.Sort(only)
	only = slices.Compact(only)
	for i, src := range sources {
		k, _, err := parseSpec(src.Repo)
		if err != nil || k != key {
			continue
		}
		switch {
		case len(only) == 0:
			sources[i].Only, sources[i].Except = nil, nil // widen to every skill in the repo
		case src.Only != nil:
			sources[i].Only = slices.Compact(slices.Sorted(slices.Values(append(src.Only, only...))))
		default:
			// An every-skill source already covers the names: re-adding
			// them just lifts any standing exclusion.
			sources[i].Except = slices.DeleteFunc(src.Except, func(s string) bool { return slices.Contains(only, s) })
			if len(sources[i].Except) == 0 {
				sources[i].Except = nil
			}
		}
		return key, saveSources(sources)
	}
	sources = append(sources, Source{Repo: key, Only: only})
	return key, saveSources(sources)
}

// RemoveSources drops skills or whole repos from the skills config. An
// arg with a slash is a repo spec and removes the whole source. Anything
// else is a skill name. Removing one skill from an every-skill source records
// it in the source's Except list. That is intent, not a pin of this
// machine's manifest, so machines whose install sets differ (collisions,
// foreign dirs) all remove exactly the named skill and nothing else.
func RemoveSources(args []string) error {
	sources, err := loadSources()
	if err != nil {
		return err
	}
	man, err := loadManifest()
	if err != nil {
		return err
	}
	for _, arg := range args {
		if strings.Contains(arg, "/") {
			key, _, err := parseSpec(arg)
			if err != nil {
				return err
			}
			n := len(sources)
			sources = slices.DeleteFunc(sources, func(s Source) bool {
				k, _, err := parseSpec(s.Repo)
				return err == nil && k == key
			})
			if len(sources) == n {
				return fmt.Errorf("%s is not a synced skill repo (see: lichen skills list)", key)
			}
			continue
		}
		name := sanitizeName(arg)
		if idx := slices.IndexFunc(sources, func(s Source) bool { return slices.Contains(s.Only, name) }); idx >= 0 {
			sources[idx].Only = slices.DeleteFunc(sources[idx].Only, func(s string) bool { return s == name })
			// An emptied Only would serialize away and read back as
			// "every skill": a source with nothing left must go entirely.
			if len(sources[idx].Only) == 0 {
				sources = slices.Delete(sources, idx, idx+1)
			}
			continue
		}
		// Not pinned anywhere: the manifest says which repo installed it,
		// and that repo's every-skill source gets the exclusion.
		st := man.Skills[name]
		if st == nil {
			return fmt.Errorf("%q is not a synced skill (see: lichen skills list)", arg)
		}
		idx := slices.IndexFunc(sources, func(s Source) bool {
			k, _, err := parseSpec(s.Repo)
			return err == nil && k == st.Repo && s.Only == nil
		})
		if idx < 0 {
			return fmt.Errorf("%q is not a synced skill (see: lichen skills list)", arg)
		}
		sources[idx].Except = slices.Compact(slices.Sorted(slices.Values(append(sources[idx].Except, name))))
	}
	return saveSources(sources)
}

// Skill is one installed, lichen-managed skill.
type Skill struct {
	Name string
	Repo string
}

// Installed lists the skills this machine's manifest owns, sorted by name.
func Installed() ([]Skill, error) {
	man, err := loadManifest()
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, name := range slices.Sorted(maps.Keys(man.Skills)) {
		out = append(out, Skill{Name: name, Repo: man.Skills[name].Repo})
	}
	return out, nil
}

// ConfiguredKeys returns the canonical keys of the configured skill
// repos, in config order (unparseable entries pass through as written).
func ConfiguredKeys() []string {
	sources, err := loadSources()
	if err != nil {
		return nil
	}
	var keys []string
	for _, src := range sources {
		key, _, err := parseSpec(src.Repo)
		if err != nil {
			key = src.Repo
		}
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}
