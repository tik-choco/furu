package models

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const fixture = `{"models":[
	{"slug":"gpt-6.1-sol","visibility":"list","priority":1},
	{"slug":"gpt-6-sol","visibility":"list","priority":0},
	{"slug":"gpt-7-sol","visibility":"hide","priority":0},
	{"slug":"gpt-6.02-sol","visibility":"list","priority":5},
	{"slug":"gpt-6.2-sol","display_name":"Sol","visibility":"list","priority":3,
		"supported_reasoning_levels":[{"effort":"high","description":"High"},{"effort":"xhigh"}],
		"ignored":{"nested":true}},
	{"slug":"gpt-6.2-astra","visibility":"list"},
	{"slug":"gpt-6.10-astra","visibility":"list"},
	{"slug":"gpt-6.20-astra-preview","visibility":"list"},
	{"slug":"gpt-6.20-astra","visibility":"other"}
]}`

func TestFamilyResolution(t *testing.T) {
	for _, alias := range []string{"sol", "astra", "terra", "luna", "opus"} {
		if !IsFamily(alias) {
			t.Errorf("%q should be a family", alias)
		}
	}
	for _, id := range []string{"", "Sol", "sol2", "gpt-6.2-sol", "claude-opus-5-5", "sol\n"} {
		if IsFamily(id) {
			t.Errorf("%q should not be a family", id)
		}
	}
	catalog, err := parseCatalog([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if model, ok := catalog.Resolve("sol"); !ok || model.Slug != "gpt-6.2-sol" ||
		!slices.Equal(model.Efforts, []string{"high", "xhigh"}) {
		t.Errorf("sol resolved to %+v, %v", model, ok)
	}
	if model, ok := catalog.Resolve("astra"); !ok || model.Slug != "gpt-6.10-astra" {
		t.Errorf("astra resolved to %+v, %v", model, ok)
	}
	if _, ok := catalog.Resolve("terra"); ok {
		t.Error("terra should be unresolved")
	}
	if _, ok := catalog.Find("gpt-7-sol"); !ok {
		t.Error("live catalog should know the exact ID of an unlisted model")
	}
	if !slices.Equal(catalog.Families(), []string{"astra", "sol"}) || len(catalog.Codex) != 7 {
		t.Errorf("unexpected visible catalog: %+v", catalog.Codex)
	}
}

func TestCacheRefreshPreservesPreviousOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "furu", "models-cache.json")
	catalog, err := Refresh(path, Catalog{}, func() (Catalog, error) { return parseCatalog([]byte(fixture)) })
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.Fresh() || (Catalog{Codex: []Model{}, FetchedAt: time.Now().Add(-25 * time.Hour)}).Fresh() {
		t.Fatal("cache freshness did not honor the 24h lifetime")
	}
	previous, err := ReadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := previous.Find("gpt-7-sol"); ok {
		t.Error("an unlisted model leaked into the cache")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	offline := errors.New("offline")
	current, err := Refresh(path, previous, func() (Catalog, error) { return Catalog{}, offline })
	if !errors.Is(err, offline) || !current.FetchedAt.Equal(previous.FetchedAt) {
		t.Fatalf("failed refresh returned %+v, %v", current, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed refresh altered the cache: %q, %v", after, err)
	}
}
