package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tik-choco/furu/internal/launch"
	"github.com/tik-choco/furu/internal/preflight"
	usagepkg "github.com/tik-choco/furu/internal/usage"
)

func TestLaunchArgs(t *testing.T) {
	for _, tc := range []struct {
		name             string
		input, want      []string
		noUpdate, noPull bool
	}{
		{
			name: "no update", noUpdate: true,
			input: []string{"--resume", "session", "--no-update", "-c", "--", "--no-update"},
			want:  []string{"--resume", "session", "-c", "--", "--no-update"},
		},
		{
			name: "no pull", noPull: true,
			input: []string{"--resume", "session", "--no-pull", "-c", "--", "--no-pull"},
			want:  []string{"--resume", "session", "-c", "--", "--no-pull"},
		},
		{
			name: "both flags before separator", noUpdate: true, noPull: true,
			input: []string{"--no-update", "--no-pull", "-c"},
			want:  []string{"-c"},
		},
		{
			name:  "both flags after separator",
			input: []string{"--", "--no-update", "--no-pull"},
			want:  []string{"--", "--no-update", "--no-pull"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, noUpdate, noPull := launchArgs(tc.input)
			if !slices.Equal(got, tc.want) || noUpdate != tc.noUpdate || noPull != tc.noPull {
				t.Fatalf("launchArgs = %q, %v, %v, want %q, %v, %v", got, noUpdate, noPull, tc.want, tc.noUpdate, tc.noPull)
			}
		})
	}
}

func TestShouldHandoff(t *testing.T) {
	now := time.Now()
	blocked := usagepkg.Snapshot{ObservedAt: now, Windows: []usagepkg.Window{{Used: 100, ResetsAt: now.Add(time.Hour)}}}
	pending := launch.Handoff{Hook: "UserPromptSubmit"}
	failure := launch.Handoff{Hook: "StopFailure", Error: "rate_limit"}
	if !shouldHandoff(failure, usagepkg.Snapshot{}, "opus", now) || !shouldHandoff(pending, blocked, "opus", now) {
		t.Fatal("missed confirmed rate limit or pending prompt with exhausted usage")
	}
	if shouldHandoff(pending, usagepkg.Snapshot{}, "opus", now) || shouldHandoff(launch.Handoff{}, blocked, "opus", now) {
		t.Fatal("handoff without a confirmed rate limit or unfinished prompt")
	}
}

func TestAllowAutoHandoff(t *testing.T) {
	event := launch.Handoff{SessionID: "11111111-2222-4333-8444-555555555555", Hook: "StopFailure", Error: "rate_limit"}
	for _, tc := range []struct {
		code int
		err  error
		want bool
	}{
		{0, nil, true},
		{1, nil, false},
		{0, errors.New("launch failed"), false},
	} {
		var stderr bytes.Buffer
		if got := allowAutoHandoff(tc.code, tc.err, event, &stderr); got != tc.want {
			t.Fatalf("allowAutoHandoff = %v, want %v", got, tc.want)
		}
		if tc.want && stderr.Len() != 0 || !tc.want && !strings.Contains(stderr.String(), "furu codex --from-claude "+event.SessionID) {
			t.Fatalf("unexpected stderr: %q", stderr.String())
		}
	}
}

func TestCodexFallback(t *testing.T) {
	now := time.Now()
	snapshot := func(model string, used float64) usagepkg.Snapshot {
		return usagepkg.Snapshot{ObservedAt: now, Windows: []usagepkg.Window{{Model: model, Used: used, ResetsAt: now.Add(time.Hour)}}}
	}
	ready := preflight.Result{ClaudePath: "claude", CodexPath: "codex"}
	for _, tc := range []struct {
		name          string
		result        preflight.Result
		claude, codex usagepkg.Snapshot
		args, want    []string
		fallback      bool
		wantError     string
	}{
		{name: "Claude available", result: ready, claude: snapshot("", 99)},
		{name: "unknown usage", result: ready},
		{name: "exhausted Claude", result: ready, claude: snapshot("", 100), fallback: true},
		{name: "initial prompt", result: ready, claude: snapshot("", 100), args: []string{"resume"}, want: []string{"--", "resume"}, fallback: true},
		{name: "literal prompt", result: ready, claude: snapshot("", 100), args: []string{"--", "-hello"}, want: []string{"--", "-hello"}, fallback: true},
		{name: "Claude resume", result: ready, claude: snapshot("", 100), args: []string{"--resume", "id"}, wantError: "furu codex"},
		{name: "Claude resume UUID", result: ready, claude: snapshot("", 100), args: []string{"--resume", "11111111-2222-4333-8444-555555555555"}, want: []string{"--from-claude", "11111111-2222-4333-8444-555555555555"}, fallback: true},
		{name: "different model exhausted", result: ready, claude: snapshot("sonnet", 100)},
		{name: "model override exhausted", result: ready, claude: snapshot("sonnet", 100), args: []string{"--model=sonnet"}, wantError: "furu codex"},
		{name: "both exhausted", result: ready, claude: snapshot("", 100), codex: snapshot("", 100), wantError: "Codex の利用枠も"},
		{name: "Codex missing", result: preflight.Result{ClaudePath: "claude"}, claude: snapshot("", 100), wantError: "PATH"},
		{name: "Codex signed out", result: preflight.Result{ClaudePath: "claude", CodexPath: "codex", CodexAuthFailed: true}, claude: snapshot("", 100), wantError: "codex login"},
		{name: "Claude missing", result: preflight.Result{CodexPath: "codex"}, fallback: true},
		{name: "Claude signed out", result: preflight.Result{ClaudePath: "claude", CodexPath: "codex", ClaudeAuthFailed: true}, fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, args, err := codexFallback(tc.result, tc.claude, tc.codex, "opus", tc.args, now)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil || (reason != "") != tc.fallback || !slices.Equal(args, tc.want) {
				t.Fatalf("fallback = %q, %q, %v", reason, args, err)
			}
		})
	}
}
