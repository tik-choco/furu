package modelcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/models"
)

func TestSetAndResetPreserveOtherFields(t *testing.T) {
	e, _, _ := testEditor(t)
	run := func(args ...string) {
		t.Helper()
		if err := e.run(args, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	const initial = `{
		"worker_timeout_minutes": 45,
		"orchestrator": {"model":"claude-test-id"},
		"tiers": {
			"hard": {"codex":{"effort":"high"},"claude":{"model":"claude-custom"}},
			"normal":{"claude":{"effort":"low"}},
			"custom":{"codex":{"model":"gpt-custom"}}
		}
	}`
	if err := os.MkdirAll(filepath.Dir(e.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	run("set", "h", "cx", "gpt-6.3-sol")
	assertConfig(t, e.path, `{
		"worker_timeout_minutes": 45,
		"orchestrator": {"model":"claude-test-id"},
		"tiers": {
			"hard": {"codex":{"model":"gpt-6.3-sol","effort":"high"},"claude":{"model":"claude-custom"}},
			"normal":{"claude":{"effort":"low"}},
			"custom":{"codex":{"model":"gpt-custom"}}
		}
	}`)
	run("set", "o", "cl", "--effort", "max")
	if got := e.cfg.Orchestrator; got != (config.Model{Model: "claude-test-id", Effort: "max"}) {
		t.Errorf("effort-only edit changed model: %+v", got)
	}
	err := e.run([]string{"set", "o", "codex", "sol"}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "only supports claude") {
		t.Fatalf("orchestrator+codex was accepted: %v", err)
	}
	run("reset", "h")
	assertConfig(t, e.path, `{
		"worker_timeout_minutes": 45,
		"orchestrator": {"effort":"max","model":"claude-test-id"},
		"tiers": {
			"normal":{"claude":{"effort":"low"}},
			"custom":{"codex":{"model":"gpt-custom"}}
		}
	}`)
	run("reset")
	assertConfig(t, e.path, `{"worker_timeout_minutes":45,"tiers":{"custom":{"codex":{"model":"gpt-custom"}}}}`)
	if e.cfg.Orchestrator != config.Defaults().Orchestrator || e.cfg.Tiers["hard"] != config.Defaults().Tiers["hard"] {
		t.Errorf("defaults were not restored: %+v", e.cfg)
	}
}

func TestTargetAliases(t *testing.T) {
	for alias, want := range map[string]string{
		"o": "orchestrator", "s": "simple", "n": "normal", "h": "hard", "vh": "very_hard",
	} {
		if got, err := config.ParseTarget(alias); err != nil || got != want {
			t.Errorf("target %q = %q, %v, want %q", alias, got, err, want)
		}
	}
}

func TestScriptedInteractiveEdit(t *testing.T) {
	e, out, _ := testEditor(t)
	// Hard, Codex, first choice (sol family), keep existing effort. Then
	// orchestrator, Claude, type a pinned model, choose max, and exit.
	input := strings.NewReader("4\n1\n1\n\n1\n1\nclaude-custom-5\n5\n\n")
	if err := e.run(nil, true, input); err != nil {
		t.Fatal(err)
	}
	assertConfig(t, e.path, `{
		"orchestrator":{"model":"claude-custom-5","effort":"max"},
		"tiers":{"hard":{"codex":{"model":"sol"}}}
	}`)
	for _, want := range []string{
		"1) sol → gpt-6.3-sol", "2) gpt-6.3-sol", "Effort (Enter to keep xhigh)",
		"2) xhigh", "5) max", "config: " + e.path,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("interactive output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out.String(), "config: ") != 3 {
		t.Errorf("expected initial table and two updated tables:\n%s", out)
	}
}

func TestCatalogValidation(t *testing.T) {
	e, out, warnings := testEditor(t)
	if err := e.run([]string{"set", "vh", "codex", "gpt-unknown"}, false, nil); err == nil {
		t.Fatal("accepted a pin absent from an available catalog")
	}
	if err := e.run([]string{"set", "vh", "codex", "luna"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings.String(), `family "luna" could not be resolved`) ||
		!strings.Contains(out.String(), "luna → ?") {
		t.Fatalf("missing unresolved family warning or label. Output %q  Warnings %q", out, warnings)
	}
	if err := e.run([]string{"set", "vh", "codex", "sol", "ultra"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings.String(), `does not list effort "ultra"`) {
		t.Fatalf("missing unsupported effort warning: %s", warnings)
	}
	out.Reset()
	if err := e.run([]string{"list"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "gpt-6.3-sol\thigh, xhigh") || !strings.Contains(out.String(), "opus, sonnet, haiku") {
		t.Fatalf("catalog list = %q", out)
	}

	offline, _, warnings := testEditor(t)
	offline.fetch = func() (models.Catalog, error) { return models.Catalog{}, errors.New("offline") }
	if err := offline.run([]string{"set", "s", "codex", "gpt-unknown"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings.String(), "could not verify model") {
		t.Fatalf("missing offline pin warning: %s", warnings)
	}
}

func testEditor(t *testing.T) (*editor, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, warnings := new(bytes.Buffer), new(bytes.Buffer)
	e := &editor{
		cfg:  config.Defaults(),
		path: filepath.Join(t.TempDir(), "furu", "config.json"),
		out:  out, stderr: warnings,
		fetch: func() (models.Catalog, error) {
			return models.Catalog{Codex: []models.Model{{Slug: "gpt-6.3-sol", Efforts: []string{"high", "xhigh"}}}}, nil
		},
	}
	return e, out, warnings
}

func assertConfig(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(r io.Reader) map[string]any {
		t.Helper()
		var doc map[string]any
		if err := json.NewDecoder(r).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	if !reflect.DeepEqual(decode(bytes.NewReader(data)), decode(strings.NewReader(expected))) {
		t.Fatalf("config = %s, want %s", data, expected)
	}
}
