package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseClaude(t *testing.T) {
	now := time.Date(2026, 11, 28, 13, 0, 0, 0, time.FixedZone("JST", 9*3600))
	data := []byte(`{"num_turns":0,"usage":{"input_tokens":0,"output_tokens":0},"result":"Current session: 100% used · resets Nov 28, 2:39pm (Etc/GMT-9)\nCurrent week (all models): 73% used · resets Dec 3, 8:59am (Etc/GMT-9)\nCurrent week (Opus): 42% used"}`)
	s, err := parseClaude(data, now)
	if err != nil || len(s.Windows) != 3 {
		t.Fatalf("snapshot %+v, %v", s, err)
	}
	if !s.Windows[0].ResetsAt.Equal(now.Add(99 * time.Minute)) {
		t.Fatalf("reset %v", s.Windows[0].ResetsAt)
	}
	if used, known, blocked := s.Pressure("opus", now); used != 100 || !known || !blocked {
		t.Fatal(used, known, blocked)
	}
	if _, err := parseClaude([]byte(`{"num_turns":1,"result":"Current session: 10% used"}`), now); err == nil {
		t.Fatal("model turn must not be accepted")
	}
	if _, err := parseClaude([]byte(`{"num_turns":0,"result":"unrecognized"}`), now); err == nil {
		t.Fatal("unknown output must not be treated as 0%")
	}
}

func TestParseCodex(t *testing.T) {
	now := time.Now()
	data := fmt.Sprintf(`{"ordinaryUsageAllowed":true,"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":%d},"secondary":null},"other":{"primary":{"usedPercent":100}}}}`, now.Add(time.Hour).Unix())
	s, err := parseCodex([]byte(data), now)
	if err != nil || len(s.Windows) != 1 {
		t.Fatal(s, err)
	}
	if u, k, b := s.Pressure("sol", now); u != 25 || !k || b {
		t.Fatal(u, k, b)
	}
}

func TestDecide(t *testing.T) {
	now := time.Now()
	snap := func(p float64) Snapshot {
		return Snapshot{ObservedAt: now, Windows: []Window{{Name: "5h", Used: p, ResetsAt: now.Add(time.Hour)}}}
	}
	for _, tc := range []struct {
		cl, cx Snapshot
		want   string
	}{{snap(80), snap(20), "codex"}, {snap(10), snap(95), "claude"}, {snap(90), snap(95), "codex"}, {snap(10), snap(100), "claude"}, {snap(100), snap(100), ""}, {Snapshot{}, snap(95), "codex"}, {snap(50), Snapshot{}, "codex"}} {
		got, _ := Decide(tc.cl, tc.cx, "opus", "sol", now)
		if got != tc.want {
			t.Errorf("got %s want %s for %+v %+v", got, tc.want, tc.cl, tc.cx)
		}
	}
	expired := snap(100)
	expired.Windows[0].ResetsAt = now
	if _, known, blocked := expired.Pressure("opus", now); known || blocked {
		t.Fatal("expired window must be unknown")
	}
}

func TestStatusLine(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "config.json"))
	now := time.Now()
	var out bytes.Buffer
	input := fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":74.5,"resets_at":%d}}}`, now.Add(time.Hour).Unix())
	if err := store.StatusLine(strings.NewReader(input), &out, now); err != nil {
		t.Fatal(err)
	}
	if got := store.Read("claude"); len(got.Windows) != 1 || got.Windows[0].Used != 74.5 {
		t.Fatal(got)
	}
	if err := store.StatusLine(strings.NewReader(`{}`), &out, now); err != nil || len(store.Read("claude").Windows) != 1 {
		t.Fatal("missing rates overwrote snapshot", err)
	}
}

func TestStatusLineRefreshesModelSpecificUsage(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	store := New(filepath.Join(t.TempDir(), "config.json"))
	now := time.Now()
	probe := Snapshot{ObservedAt: now.Add(-MaxAge + time.Minute), Source: "claude /usage", Windows: []Window{
		{Name: "5h", Used: 70}, {Name: "7d opus", Model: "opus", Used: 100},
	}}
	if err := store.Save("claude", probe); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := store.StatusLine(strings.NewReader(`{"rate_limits":{"five_hour":{"used_percentage":20}}}`), &out, now); err != nil {
		t.Fatal(err)
	}
	merged := store.Read("claude")
	if used, known, blocked := merged.Pressure("opus", now); used != 100 || !known || !blocked {
		t.Fatal(used, known, blocked)
	}
	if used, known, blocked := merged.Pressure("opus", now.Add(2*time.Minute)); used != 20 || !known || blocked {
		t.Fatal("status line incorrectly extended model-specific freshness", used, known, blocked)
	}
	probe.ObservedAt = now.Add(-MaxAge - time.Minute)
	if err := store.Save("claude", probe); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	probes := 0
	store.command = func(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
		probes++
		if path != "claude" || args[1] != "/usage" {
			t.Fatalf("unexpected command: %s %v", path, args)
		}
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestClaudeUsageChild$")
		cmd.Env = append(os.Environ(), "FURU_USAGE_CLAUDE_CHILD=1")
		return cmd, nil
	}
	got := store.Refresh(context.Background(), "claude", "claude", false)
	if used, known, blocked := got.Pressure("opus", time.Now()); probes != 1 || used != 100 || !known || !blocked {
		t.Fatal("stale model-specific limits not refreshed", probes, got)
	}
	if err := store.StatusLine(strings.NewReader(`{"rate_limits":{"five_hour":{"used_percentage":30}}}`), &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	store.Refresh(context.Background(), "claude", "claude", false)
	if probes != 1 {
		t.Fatal("fresh /usage data should not be queried again", probes)
	}
}

func TestClaudeUsageChild(t *testing.T) {
	if os.Getenv("FURU_USAGE_CLAUDE_CHILD") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, `{"num_turns":0,"result":"Current session: 25% used\nCurrent week (Opus): 100% used"}`)
	os.Exit(0)
}

func TestFailedClaudeProbePreservesFreshWindows(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	store := New(filepath.Join(t.TempDir(), "config.json"))
	now := time.Now()
	probe := Snapshot{ObservedAt: now.Add(-3 * time.Minute), Windows: []Window{{Name: "7d opus", Model: "opus", Used: 100}}}
	if err := store.Save("claude", probe); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := store.StatusLine(strings.NewReader(`{"rate_limits":{"five_hour":{"used_percentage":20}}}`), &out, now); err != nil {
		t.Fatal(err)
	}
	probes := 0
	store.command = func(context.Context, string, ...string) (*exec.Cmd, error) {
		probes++
		return nil, errors.New("unavailable")
	}
	if _, _, blocked := store.Refresh(context.Background(), "claude", "claude", false).Pressure("opus", now); !blocked || probes != 0 {
		t.Fatal("fresh status line should avoid a redundant probe while model-specific usage is still fresh", probes)
	}
	got := store.Refresh(context.Background(), "claude", "claude", true)
	if _, _, blocked := got.Pressure("opus", now); !blocked || !store.read("claude").ObservedAt.Equal(probe.ObservedAt) {
		t.Fatal("failed probe lost or extended still-valid usage", got)
	}
	store.Refresh(context.Background(), "claude", "claude", false)
	if probes != 1 {
		t.Fatal("failed probes should be throttled", probes)
	}
}

func TestUsageCacheProfiles(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	path := filepath.Join(t.TempDir(), "config.json")
	store := New(path)
	now := time.Now()
	blocked := Snapshot{ObservedAt: now, Windows: []Window{{Used: 100}}}
	for _, backend := range []string{"codex", "claude", "claude-statusline"} {
		if filepath.Base(store.path(backend)) != backend+".json" {
			t.Fatal("default cache filename changed", store.path(backend))
		}
	}
	if err := store.Save("codex", blocked); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "codex-profile-a")
	t.Setenv("CLAUDE_CONFIG_DIR", "claude-profile-a")
	if _, known, _ := store.Read("codex").Pressure("", now); known {
		t.Fatal("default cache leaked into another profile")
	}
	if err := store.Save("codex", blocked); err != nil {
		t.Fatal(err)
	}
	probe := Snapshot{ObservedAt: now.Add(-time.Second), Windows: []Window{{Model: "opus", Used: 100}}}
	if err := store.Save("claude", probe); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := store.StatusLine(strings.NewReader(`{"rate_limits":{"five_hour":{"used_percentage":30}}}`), &out, now); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		abs, err := filepath.Abs(os.Getenv(key))
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, abs)
	}
	otherProcess := New(path)
	if _, _, blocked := otherProcess.Read("codex").Pressure("", now); !blocked {
		t.Fatal("equivalent absolute Codex profile did not share cache")
	}
	if used, known, blocked := otherProcess.Read("claude").Pressure("sonnet", now); used != 30 || !known || blocked {
		t.Fatal("Claude status line did not share its profile's cache", used, known, blocked)
	}
	if _, _, blocked := otherProcess.Read("claude").Pressure("opus", now); !blocked {
		t.Fatal("Claude /usage did not share its profile's cache")
	}
	t.Setenv("CODEX_HOME", "codex-profile-b")
	t.Setenv("CLAUDE_CONFIG_DIR", "claude-profile-b")
	for _, backend := range []string{"claude", "codex"} {
		if got := New(path).Read(backend); !got.ObservedAt.IsZero() || len(got.Windows) != 0 {
			t.Fatal("profile A usage leaked into profile B", backend, got)
		}
	}
	t.Setenv("CODEX_HOME", "")
	if _, _, blocked := store.Read("codex").Pressure("", now); !blocked {
		t.Fatal("default cache was lost")
	}
}

func TestCodexRPC(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := func(ctx context.Context, _ string, args ...string) (*exec.Cmd, error) {
		if strings.Join(args, " ") != "app-server --listen stdio://" {
			t.Fatal(args)
		}
		c := exec.CommandContext(ctx, exe, "-test.run=^TestRPCChild$")
		c.Env = append(os.Environ(), "FURU_USAGE_RPC_CHILD=1")
		return c, nil
	}
	s, err := fetchCodex(context.Background(), "codex", cmd)
	if err != nil || len(s.Windows) != 1 || s.Windows[0].Used != 20 {
		t.Fatal(s, err)
	}
}

func TestRPCChild(t *testing.T) {
	if os.Getenv("FURU_USAGE_RPC_CHILD") != "1" {
		return
	}
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for _, method := range []string{"initialize", "initialized", "account/rateLimits/read"} {
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		if dec.Decode(&req) != nil || req.Method != method {
			os.Exit(2)
		}
		switch method {
		case "initialize":
			_ = enc.Encode(map[string]any{"id": req.ID, "result": struct{}{}})
		case "account/rateLimits/read":
			_ = enc.Encode(map[string]any{"method": "account/updated"})
			_ = enc.Encode(map[string]any{"id": req.ID, "result": map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 20, "windowDurationMins": 300}}}})
		}
	}
	os.Exit(0)
}
