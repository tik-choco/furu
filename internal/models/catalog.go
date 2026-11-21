package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/tik-choco/furu/internal/atomicfile"
	"github.com/tik-choco/furu/internal/cli"
)

const CacheLifetime = 24 * time.Hour

type Model struct {
	Slug     string   `json:"slug"`
	Priority int      `json:"priority"`
	Efforts  []string `json:"efforts"`
}

type Catalog struct {
	FetchedAt time.Time `json:"fetched_at"`
	Codex     []Model   `json:"codex"`

	// Non-listed models from a live catalog can still be pinned by exact ID.
	unlisted []Model
}

var (
	familyPattern = regexp.MustCompile(`^[a-z]+$`)
	slugPattern   = regexp.MustCompile(`^gpt-(\d+(?:\.\d+)*)-([a-z]+)$`)
)

func IsFamily(value string) bool {
	return familyPattern.MatchString(value)
}

func (c Catalog) Available() bool {
	return c.Codex != nil
}

func (c Catalog) Find(slug string) (Model, bool) {
	for _, entries := range [][]Model{c.Codex, c.unlisted} {
		for _, model := range entries {
			if model.Slug == slug {
				return model, true
			}
		}
	}
	return Model{}, false
}

func (c Catalog) Resolve(family string) (Model, bool) {
	var best Model
	var version string
	for _, model := range c.Codex {
		match := slugPattern.FindStringSubmatch(model.Slug)
		if match == nil || match[2] != family {
			continue
		}
		cmp := cli.CompareVersions(match[1], version)
		if best.Slug == "" || cmp > 0 || (cmp == 0 && model.Priority < best.Priority) {
			best, version = model, match[1]
		}
	}
	return best, best.Slug != ""
}

func (c Catalog) Families() []string {
	var names []string
	for _, model := range c.Codex {
		match := slugPattern.FindStringSubmatch(model.Slug)
		if match != nil {
			names = append(names, match[2])
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func parseCatalog(data []byte) (Catalog, error) {
	var response struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
			Priority   int    `json:"priority"`
			Reasoning  []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return Catalog{}, err
	}
	if response.Models == nil {
		return Catalog{}, errors.New("missing models array")
	}
	catalog := Catalog{Codex: make([]Model, 0, len(response.Models))}
	for _, entry := range response.Models {
		if entry.Slug == "" {
			continue
		}
		model := Model{Slug: entry.Slug, Priority: entry.Priority, Efforts: make([]string, 0, len(entry.Reasoning))}
		for _, level := range entry.Reasoning {
			if level.Effort != "" {
				model.Efforts = append(model.Efforts, level.Effort)
			}
		}
		if entry.Visibility == "list" {
			catalog.Codex = append(catalog.Codex, model)
		} else {
			catalog.unlisted = append(catalog.unlisted, model)
		}
	}
	return catalog, nil
}

type Command func(context.Context, string, ...string) (*exec.Cmd, error)

func Fetch(path string, command Command) (Catalog, error) {
	if path == "" {
		return Catalog{}, errors.New("codex was not found on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd, err := command(ctx, path, "debug", "models")
	if err != nil {
		return Catalog{}, err
	}
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return Catalog{}, ctx.Err()
	}
	if err != nil {
		return Catalog{}, fmt.Errorf("codex debug models: %w", err)
	}
	catalog, err := parseCatalog(output)
	if err != nil {
		return Catalog{}, fmt.Errorf("decode codex catalog: %w", err)
	}
	return catalog, nil
}

func CachePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "models-cache.json")
}

func ReadCache(path string) (Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, err
	}
	var catalog Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return Catalog{}, err
	}
	if catalog.FetchedAt.IsZero() || !catalog.Available() {
		return Catalog{}, errors.New("invalid models cache")
	}
	return catalog, nil
}

func (c Catalog) Fresh() bool {
	return c.Available() && time.Since(c.FetchedAt) < CacheLifetime
}

// Refresh keeps the previous catalog when fetching fails, and returns the live
// catalog even if persisting it fails.
func Refresh(path string, previous Catalog, fetch func() (Catalog, error)) (Catalog, error) {
	catalog, err := fetch()
	if err != nil {
		return previous, err
	}
	catalog.FetchedAt = time.Now().UTC().Truncate(time.Second)
	data, err := json.Marshal(catalog)
	if err == nil {
		err = atomicfile.Write(path, append(data, '\n'))
	}
	if err != nil {
		return catalog, fmt.Errorf("save models cache: %w", err)
	}
	return catalog, nil
}
