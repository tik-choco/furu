package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadMissing(t *testing.T) {
	got, err := loadFile(filepath.Join(t.TempDir(), "config.json"))
	if err != nil || !reflect.DeepEqual(got, Defaults()) {
		t.Fatalf("missing config: got %+v, err %v", got, err)
	}
}

func TestLoadMerge(t *testing.T) {
	path := writeConfig(t, `{
		"orchestrator": {"model": "custom"},
		"worker_timeout_minutes": 45,
		"tiers": {
			"normal": {"codex": {"model": "", "effort": "medium"}},
			"hard": {"claude": {"model": "custom-worker"}}
		}
	}`)
	want := Defaults()
	want.Orchestrator.Model = "custom"
	want.WorkerTimeoutMinutes = 45
	normal, hard := want.Tiers["normal"], want.Tiers["hard"]
	normal.Codex.Effort = "medium"
	hard.Claude.Model = "custom-worker"
	want.Tiers["normal"], want.Tiers["hard"] = normal, hard

	got, err := loadFile(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("merged config: got %+v, err %v, want %+v", got, err, want)
	}
}

func TestLoadErrorsIncludePath(t *testing.T) {
	for name, input := range map[string]string{
		"unknown top-level field": `{"unknown": true}`,
		"unknown nested field":    `{"tiers":{"normal":{"codex":{"unknown":true}}}}`,
		"multiple values":         `{} {}`,
		"null root":               `null`,
		"invalid timeout":         `{"worker_timeout_minutes":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, input)
			if _, err := loadFile(path); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("expected an error containing %s, got %v", path, err)
			}
		})
	}
}

func TestLoadCommitCommand(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"absent", `{}`, ""},
		{"empty", `{"commit_command":""}`, ""},
		{"auto", `{"commit_command":"auto"}`, "auto"},
		{"git", `{"commit_command":"git"}`, "git"},
		{"explicit", `{"commit_command":"  my-wrapper -y  "}`, "  my-wrapper -y  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadFile(writeConfig(t, tc.input))
			if err != nil || got.CommitCommand != tc.want {
				t.Errorf("loaded commit_command = %q, error = %v, want %q", got.CommitCommand, err, tc.want)
			}
		})
	}
}

func writeConfig(t *testing.T, input string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
