package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tik-choco/furu/internal/cli"
	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/models"
)

func TestParseVersion(t *testing.T) {
	for input, want := range map[string]string{
		"2.1.330 (Claude Code)\n":        "2.1.330",
		"codex-cli 0.170.0":              "0.170.0",
		" 0.171.0 \r\n":                  "0.171.0",
		"codex-cli 0.170.0 (2026.11.21)": "0.170.0",
	} {
		got, err := parseVersion(input)
		if err != nil || got != want {
			t.Errorf("parseVersion(%q) = %q, %v, want %q", input, got, err, want)
		}
	}
	if _, err := parseVersion("unknown version"); err == nil {
		t.Error("expected error for missing version")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		want        int
	}{
		{"2.1.9", "2.1.10", -1},
		{"0.171.0", "0.170.9", 1},
		{"1.2", "1.2.0", 0},
		{"1.00.2", "1.0.02", 0},
		{"1.999999999999999999999999", "1.999", 1},
	} {
		got := cli.CompareVersions(tc.left, tc.right)
		if got = max(-1, min(1, got)); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestParseClaudeAuth(t *testing.T) {
	for _, tc := range []struct {
		output   string
		loggedIn bool
		expires  string
	}{
		{`{"loggedIn":true,"authMethod":"claude.ai","loginExpiresAt":"2030-01-02T03:04:05.678Z"}`, true, "2030-01-02T03:04:05.678Z"},
		{`{"loggedIn":true}`, true, ""},
		{`{"loggedIn":false}`, false, ""},
	} {
		got, err := parseClaudeAuth([]byte(tc.output))
		if err != nil || got.loggedIn != tc.loggedIn {
			t.Fatalf("parseClaudeAuth(%s) = %+v, %v", tc.output, got, err)
		}
		if tc.expires == "" {
			if got.expiresAt != nil {
				t.Errorf("unexpected expiration %v", got.expiresAt)
			}
		} else if got.expiresAt == nil || got.expiresAt.Format(time.RFC3339Nano) != tc.expires {
			t.Errorf("expiration = %v, want %s", got.expiresAt, tc.expires)
		}
	}
	for _, output := range []string{`{}`, `{"loggedIn":"yes"}`, `{"loggedIn":true,"loginExpiresAt":"invalid"}`, "not JSON"} {
		if _, err := parseClaudeAuth([]byte(output)); err == nil {
			t.Errorf("expected parse error for %q", output)
		}
	}
}

func TestPreflightUpdatesAndChecksAuth(t *testing.T) {
	var codexUpdated atomic.Bool
	var logs bytes.Buffer
	c := checker{
		lookPath: func(name string) (string, error) { return name, nil },
		run: func(_ context.Context, path string, args []string, stdout, _ io.Writer) error {
			switch path + " " + strings.Join(args, " ") {
			case "claude --version":
				fmt.Fprint(stdout, "2.1.330 (Claude Code)")
			case "codex --version":
				if codexUpdated.Load() {
					fmt.Fprint(stdout, "codex-cli 0.171.0")
				} else {
					fmt.Fprint(stdout, "codex-cli 0.170.0")
				}
			case "npm view @anthropic-ai/claude-code version":
				fmt.Fprintln(stdout, "2.1.330")
			case "npm view @openai/codex version":
				fmt.Fprintln(stdout, "0.171.0")
			case "codex update":
				codexUpdated.Store(true)
			case "claude auth status":
				fmt.Fprint(stdout, `{"loggedIn":false}`)
				return errors.New("exit status 1")
			case "codex login status":
				fmt.Fprint(stdout, "Not LOGGED in")
			default:
				t.Errorf("unexpected command: %s %v", path, args)
				return errors.New("unexpected command")
			}
			return nil
		},
		stdout: io.Discard,
		stderr: &logs,
	}
	result := c.check(Options{Versions: true, Update: true, Login: true})
	if result.ClaudePath != "claude" || !result.ClaudeAuthFailed || result.CodexPath != "codex" || !result.CodexAuthFailed || !codexUpdated.Load() {
		t.Fatalf("preflight result = %+v, updated = %v", result, codexUpdated.Load())
	}
	for _, want := range []string{"claude 2.1.330", "codex 0.170.0 → 0.171.0", "codex 0.171.0 に更新しました", "claude auth login", "codex login"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q: %s", want, logs.String())
		}
	}
}

func TestClaudeAuthCommandFailure(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		failed       bool
	}{
		{"unknown", "unexpected output", false},
		{"expired", `{"loggedIn":true,"loginExpiresAt":"2000-01-01T00:00:00Z"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			c := checker{
				run: func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) error {
					fmt.Fprint(stdout, tc.output)
					return errors.New("exit status 1")
				},
				stderr: &logs,
			}
			if got := c.checkClaudeAuth("claude"); got != tc.failed {
				t.Fatalf("authFailed = %v, logs = %s", got, &logs)
			}
			if !tc.failed && !strings.Contains(logs.String(), "ログイン状態を確認できません") {
				t.Fatal(&logs)
			}
		})
	}
}

func TestLoginOnlyDoesNotCheckVersions(t *testing.T) {
	var logs bytes.Buffer
	c := checker{
		lookPath: func(name string) (string, error) {
			if name == "npm" {
				t.Error("npm lookup with version checks disabled")
			}
			return name, nil
		},
		run: func(_ context.Context, path string, args []string, stdout, _ io.Writer) error {
			switch path + " " + strings.Join(args, " ") {
			case "claude auth status":
				fmt.Fprintf(stdout, `{"loggedIn":true,"loginExpiresAt":%q}`, time.Now().Add(-time.Hour).Format(time.RFC3339))
			case "codex login status":
				return errors.New("exit status 1")
			default:
				t.Errorf("unexpected command: %s %v", path, args)
			}
			return nil
		},
		stderr: &logs,
	}
	result := c.check(Options{Login: true})
	if !result.ClaudeAuthFailed || !result.CodexAuthFailed || !strings.Contains(logs.String(), "codex login") {
		t.Fatalf("result = %+v, logs = %s", result, &logs)
	}
}

func TestModelPreflightReportsChangesAndKeepsCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models-cache.json")
	fetch := func(slug string) func() (models.Catalog, error) {
		return func() (models.Catalog, error) {
			return models.Catalog{Codex: []models.Model{{Slug: slug, Efforts: []string{"high"}}}}, nil
		}
	}
	if _, err := models.Refresh(path, models.Catalog{}, fetch("gpt-6.2-sol")); err != nil {
		t.Fatal(err)
	}
	var out, warnings bytes.Buffer
	showModels(config.Defaults(), path, true, &out, &warnings, fetch("gpt-6.3-sol"))
	if !strings.Contains(warnings.String(), "furu: sol: gpt-6.2-sol → gpt-6.3-sol") ||
		!strings.Contains(warnings.String(), `does not list effort "medium"`) ||
		!strings.Contains(out.String(), "sol → gpt-6.3-sol") {
		t.Fatalf("preflight output %q, warnings %q", &out, &warnings)
	}
	out.Reset()
	warnings.Reset()
	showModels(config.Defaults(), path, true, &out, &warnings, func() (models.Catalog, error) {
		return models.Catalog{}, errors.New("offline")
	})
	if !strings.Contains(warnings.String(), "refresh Codex model catalog: offline") ||
		!strings.Contains(out.String(), "sol → gpt-6.3-sol") {
		t.Fatalf("failed refresh output %q, warnings %q", &out, &warnings)
	}
}
