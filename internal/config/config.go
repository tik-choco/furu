package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Model struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

type Tier struct {
	Codex  Model `json:"codex"`
	Claude Model `json:"claude"`
}

type Config struct {
	Orchestrator         Model           `json:"orchestrator"`
	WorkerTimeoutMinutes int             `json:"worker_timeout_minutes"`
	Tiers                map[string]Tier `json:"tiers"`
	// CommitCommand selects auto-detection (empty or "auto"), plain "git",
	// or an explicit commit wrapper.
	CommitCommand      string `json:"commit_command,omitempty"`
	GitSync            *bool  `json:"git_sync,omitempty"`
	AdaptiveDelegation *bool  `json:"adaptive_delegation,omitempty"`
	UsageStatusLine    *bool  `json:"usage_statusline,omitempty"`
}

func TierNames() []string {
	return []string{"simple", "normal", "hard", "very_hard"}
}

func Defaults() Config {
	return Config{
		Orchestrator:         Model{Model: "opus", Effort: "high"},
		WorkerTimeoutMinutes: 30,
		Tiers: map[string]Tier{
			"simple": {
				Codex:  Model{Model: "sol", Effort: "medium"},
				Claude: Model{Model: "haiku", Effort: "max"},
			},
			"normal": {
				Codex:  Model{Model: "sol", Effort: "high"},
				Claude: Model{Model: "sonnet", Effort: "high"},
			},
			"hard": {
				Codex:  Model{Model: "sol", Effort: "xhigh"},
				Claude: Model{Model: "opus", Effort: "medium"},
			},
			"very_hard": {
				Codex:  Model{Model: "astra", Effort: "xhigh"},
				Claude: Model{Model: "opus", Effort: "high"},
			},
		},
	}
}

func (c Config) WorkerTimeout() time.Duration {
	return time.Duration(c.WorkerTimeoutMinutes) * time.Minute
}

// GitSyncEnabled reports whether launch should sync the current work tree.
func (c Config) GitSyncEnabled() bool {
	return c.GitSync == nil || *c.GitSync
}

func (c Config) AdaptiveEnabled() bool   { return c.AdaptiveDelegation == nil || *c.AdaptiveDelegation }
func (c Config) StatusLineEnabled() bool { return c.UsageStatusLine == nil || *c.UsageStatusLine }

func Load() (Config, string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Config{}, "", fmt.Errorf("find config directory: %w", err)
	}
	path := filepath.Join(dir, "furu", "config.json")
	cfg, err := loadFile(path)
	return cfg, path, err
}

func (c Config) Model(target, backend string) Model {
	if target == "orchestrator" {
		return c.Orchestrator
	}
	tier := c.Tiers[target]
	if backend == "codex" {
		return tier.Codex
	}
	return tier.Claude
}

func loadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return Config{}, err
	}
	return decode(path, data)
}

func decode(path string, data []byte) (Config, error) {
	cfg := Defaults()
	var overrides *struct {
		Orchestrator         Model           `json:"orchestrator"`
		WorkerTimeoutMinutes *int            `json:"worker_timeout_minutes"`
		Tiers                map[string]Tier `json:"tiers"`
		CommitCommand        string          `json:"commit_command"`
		GitSync              *bool           `json:"git_sync"`
		AdaptiveDelegation   *bool           `json:"adaptive_delegation"`
		UsageStatusLine      *bool           `json:"usage_statusline"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&overrides); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if overrides == nil {
		return Config{}, fmt.Errorf("%s: config must be a JSON object", path)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		if err == nil {
			err = errors.New("expected a single JSON object")
		}
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}

	if minutes := overrides.WorkerTimeoutMinutes; minutes != nil {
		const maxMinutes = (1<<63 - 1) / int64(time.Minute)
		if *minutes <= 0 || int64(*minutes) > maxMinutes {
			return Config{}, fmt.Errorf("%s: worker_timeout_minutes must be between 1 and %d", path, maxMinutes)
		}
		cfg.WorkerTimeoutMinutes = *minutes
	}
	cfg.Orchestrator.merge(overrides.Orchestrator)
	cfg.CommitCommand = overrides.CommitCommand
	cfg.GitSync = overrides.GitSync
	cfg.AdaptiveDelegation = overrides.AdaptiveDelegation
	cfg.UsageStatusLine = overrides.UsageStatusLine
	for name, override := range overrides.Tiers {
		tier := cfg.Tiers[name]
		tier.Codex.merge(override.Codex)
		tier.Claude.merge(override.Claude)
		cfg.Tiers[name] = tier
	}
	return cfg, nil
}

func (m *Model) merge(override Model) {
	if override.Model != "" {
		m.Model = override.Model
	}
	if override.Effort != "" {
		m.Effort = override.Effort
	}
}
