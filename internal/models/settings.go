package models

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/tik-choco/furu/internal/config"
)

func ClaudeFamilies() []string {
	return []string{"opus", "sonnet", "haiku"}
}

func (c Catalog) Efforts(backend, value string) ([]string, bool) {
	if backend == "claude" {
		return []string{"low", "medium", "high", "xhigh", "max"}, true
	}
	entry, ok := c.Find(value)
	if IsFamily(value) {
		entry, ok = c.Resolve(value)
	}
	return entry.Efforts, ok
}

func (c Catalog) Label(backend, value string) string {
	if backend != "codex" || !IsFamily(value) {
		return value
	}
	return value + " → " + c.resolvedSlug(value)
}

func (c Catalog) resolvedSlug(family string) string {
	if entry, ok := c.Resolve(family); ok {
		return entry.Slug
	}
	return "?"
}

func PrintChanges(out io.Writer, previous, current Catalog) {
	if !previous.Available() || !current.Available() {
		return
	}
	names := append(previous.Families(), current.Families()...)
	slices.Sort(names)
	for _, family := range slices.Compact(names) {
		old, new := previous.resolvedSlug(family), current.resolvedSlug(family)
		if old != new {
			fmt.Fprintf(out, "furu: %s: %s → %s\n", family, old, new)
		}
	}
}

type row struct {
	target, backend string
	model           config.Model
}

func configuredModels(cfg config.Config) []row {
	rows := []row{{"orchestrator", "claude", cfg.Orchestrator}}
	for _, target := range config.TierNames() {
		tier := cfg.Tiers[target]
		rows = append(rows, row{target, "codex", tier.Codex}, row{target, "claude", tier.Claude})
	}
	return rows
}

func PrintTable(out io.Writer, cfg config.Config, catalog Catalog) {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "target\tbackend\tmodel\teffort")
	lastTarget := ""
	for _, entry := range configuredModels(cfg) {
		target := entry.target
		if target == lastTarget {
			target = ""
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", target, entry.backend, catalog.Label(entry.backend, entry.model.Model), entry.model.Effort)
		lastTarget = entry.target
	}
	table.Flush()
}

func WarnEfforts(out io.Writer, cfg config.Config, catalog Catalog) {
	for _, entry := range configuredModels(cfg) {
		value := entry.model.Model
		if efforts, ok := catalog.Efforts(entry.backend, value); ok && !slices.Contains(efforts, entry.model.Effort) {
			supported := strings.Join(efforts, ", ")
			if supported == "" {
				supported = "none"
			}
			fmt.Fprintf(out, "furu: warning: %s %s %s does not list effort %q (supported: %s)\n",
				entry.target, entry.backend, catalog.Label(entry.backend, value), entry.model.Effort, supported)
		}
	}
}
