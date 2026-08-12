// Package config defines lichen's single config file at
// ~/.config/lichen/config.json. The file is itself synced across machines,
// so everything in it is machine-portable. lichen edits it only through
// chezmoi: the installer seeds it, and an apply can replace it with
// another machine's version.
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

func Path() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "lichen", "config.json"), nil
}

// DataDir holds lichen's machine-local scratch, currently just the
// cross-process lock.
func DataDir() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "lichen"), nil
}

func LogPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "lichen.log"), nil
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
