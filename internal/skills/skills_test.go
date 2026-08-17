package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSpec(t *testing.T) {
	cases := []struct {
		spec, key string
		bad       bool
	}{
		{spec: "vercel-labs/agent-skills", key: "github.com/vercel-labs/agent-skills"},
		{spec: "github.com/owner/repo", key: "github.com/owner/repo"},
		{spec: "https://github.com/owner/repo", key: "github.com/owner/repo"},
		{spec: "https://github.com/owner/repo.git", key: "github.com/owner/repo"},
		{spec: "https://github.com/owner/repo/tree/main/skills/x", key: "github.com/owner/repo"},
		{spec: "git@github.com:owner/repo.git", key: "github.com/owner/repo"},
		{spec: "gitlab.com/org/repo", key: "gitlab.com/org/repo"},
		{spec: "GitHub.com/Owner/Repo", key: "github.com/Owner/Repo"},
		{spec: "just-a-name", bad: true},
		{spec: "a/b/c", bad: true},
		{spec: "", bad: true},
	}
	for _, c := range cases {
		key, url, err := parseSpec(c.spec)
		if c.bad {
			if err == nil {
				t.Errorf("parseSpec(%q): expected error, got %q", c.spec, key)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSpec(%q): %v", c.spec, err)
			continue
		}
		if key != c.key {
			t.Errorf("parseSpec(%q) key = %q, want %q", c.spec, key, c.key)
		}
		if want := "https://" + c.key + ".git"; url != want {
			t.Errorf("parseSpec(%q) url = %q, want %q", c.spec, url, want)
		}
	}
}

func TestFrontmatterName(t *testing.T) {
	cases := []struct{ md, want string }{
		{"---\nname: my-skill\ndescription: d\n---\nbody", "my-skill"},
		{"---\nname: \"quoted\"\n---\n", "quoted"},
		{"---\ndescription: d\n---\nname: not-frontmatter", ""},
		{"no frontmatter here", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := frontmatterName([]byte(c.md)); got != c.want {
			t.Errorf("frontmatterName(%q) = %q, want %q", c.md, got, c.want)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"My Skill", "my-skill"},
		{"frontend-design", "frontend-design"},
		{"  weird/../path  ", "weird..path"},
		{"---", ""},
	}
	for _, c := range cases {
		if got := sanitizeName(c.in); got != c.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("skills/alpha/SKILL.md", "---\nname: alpha\n---\n")
	// Frontmatter name wins over the directory name.
	write("skills/beta-dir/SKILL.md", "---\nname: beta\n---\n")
	// No frontmatter: the directory name is the skill name.
	write(".claude/skills/gamma/SKILL.md", "instructions only")
	// A nested SKILL.md inside a skill dir is shadowed by its parent.
	write("skills/alpha/nested/SKILL.md", "---\nname: shadowed\n---\n")
	// Too deep to be discovered.
	write("a/b/c/d/SKILL.md", "---\nname: too-deep\n---\n")

	found := discover(root, "repo")
	want := map[string]string{
		"alpha": filepath.Join("skills", "alpha"),
		"beta":  filepath.Join("skills", "beta-dir"),
		"gamma": filepath.Join(".claude", "skills", "gamma"),
	}
	if len(found) != len(want) {
		t.Fatalf("discover found %v, want %v", found, want)
	}
	for name, dir := range want {
		if found[name] != dir {
			t.Errorf("discover[%q] = %q, want %q", name, found[name], dir)
		}
	}
}

func TestDiscoverRootSkill(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	found := discover(root, "single-skill-repo")
	if found["single-skill-repo"] != "." {
		t.Fatalf("discover = %v, want the repo name mapping to the root", found)
	}
}
