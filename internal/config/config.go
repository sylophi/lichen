// Package config defines lichen's config files under ~/.config/lichen,
// one per owner: config.json (the event channel, seeded by the
// installer) and skills.json (skill sources, rewritten by the skills
// CLI). Both are synced across machines, so everything in them is
// machine-portable, and an apply can replace either with another
// machine's version.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Config struct {
	NtfyServer string `json:"ntfy_server,omitempty"`
	// Topic is the one event channel every machine shares: lichen's own
	// push nudges and the sync repo's webhook both land here. The randomly
	// generated topic is the channel's only secret. The JSON key is
	// historical, kept so existing configs stay valid.
	Topic string `json:"topic_prefix"`
}

var homeOnce = sync.OnceValues(os.UserHomeDir)

func Home() (string, error) { return homeOnce() }

// HomeJoin resolves a path under the home directory.
func HomeJoin(parts ...string) (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}

func Path() (string, error) {
	return HomeJoin(".config", "lichen", "config.json")
}

// SkillsPath is the skills module's config: which skill repos to sync.
// A separate file so each synced config file has exactly one writer.
func SkillsPath() (string, error) {
	return HomeJoin(".config", "lichen", "skills.json")
}

// OwnedPaths are the config files lichen itself writes: the set the
// files module keeps in the sync repo. Empty when home can't resolve.
func OwnedPaths() []string {
	var paths []string
	if p, err := Path(); err == nil {
		paths = append(paths, p)
	}
	if p, err := SkillsPath(); err == nil {
		paths = append(paths, p)
	}
	return paths
}

// DataDir holds lichen's machine-local state: the cross-process lock,
// the managed-set manifest, the skills manifest, and the skill repo
// clones.
func DataDir() (string, error) {
	return HomeJoin(".local", "share", "lichen")
}

func LogPath() (string, error) {
	return HomeJoin("Library", "Logs", "lichen.log")
}

func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no config at %s (run install.sh first)", p)
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	if cfg.Topic == "" {
		return nil, fmt.Errorf("%s: topic_prefix is required", p)
	}
	return &cfg, nil
}

// Server resolves the default at point of use, so the config file stays
// exactly what the user wrote.
func (c *Config) Server() string {
	if c.NtfyServer == "" {
		return "https://ntfy.sh"
	}
	return strings.TrimRight(c.NtfyServer, "/")
}

// MaskTopic redacts the topic from s, for output that may be pasted or
// logged (a net/url error embeds the whole request URL).
func (c *Config) MaskTopic(s string) string {
	return strings.ReplaceAll(s, c.Topic, "lichen-****")
}

func ExpandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := Home()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}
	return p, nil
}

// ContractHome is ExpandHome's inverse: paths under the home directory
// become ~/-relative (portable across machines), others pass through.
func ContractHome(abs string) string {
	home, err := Home()
	if err != nil {
		return abs
	}
	rel, err := filepath.Rel(home, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return abs
	}
	if rel == "." {
		return "~"
	}
	return "~/" + filepath.ToSlash(rel)
}
