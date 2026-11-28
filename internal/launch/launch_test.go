package launch

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tik-choco/furu/internal/commitcmd"
	"github.com/tik-choco/furu/internal/config"
)

const guruPromptHint = "commit のみは --no-push、ステージ済みの変更だけなら -s を付ける。"

func TestArgs(t *testing.T) {
	cfg := config.Defaults()
	cfg.WorkerTimeoutMinutes = 12
	commit := commitcmd.Result{Command: "wrap commit", Source: commitcmd.SourceConfig}
	executable := `C:\Program Files\tools\furu.exe`
	user := []string{"--resume", "session-id", "日本語の依頼"}
	args := Args(cfg, commit, executable, user)
	if !slices.Equal(args[:4], []string{"--model", "opus", "--effort", "high"}) {
		t.Errorf("orchestrator defaults: %v", args[:4])
	}
	if args[4] != "--append-system-prompt" || !strings.Contains(args[5], "mcp__furu__delegate") ||
		!strings.Contains(args[5], "画像ファイルのパスを images に渡す") ||
		!strings.Contains(args[5], "パスなしで会話に貼られた画像はファイルとして存在せず") ||
		!strings.Contains(args[5], "`wrap commit \"<message>\"`") ||
		!strings.Contains(args[5], "git commit / git push を直接使わない。") || strings.Contains(args[5], guruPromptHint) {
		t.Errorf("missing orchestrator prompt: %v", args[4:6])
	}
	var gotSettings settings
	if args[6] != "--settings" || json.Unmarshal([]byte(args[7]), &gotSettings) != nil || gotSettings.Env["MCP_TOOL_TIMEOUT"] != "780000" || !slices.Equal(gotSettings.Permissions.Deny, []string{"Bash(git commit:*)", "Bash(git push:*)"}) || !slices.Equal(gotSettings.Permissions.Allow, []string{"mcp__furu__delegate"}) {
		t.Fatalf("settings = %v", args[6:8])
	}
	if len(gotSettings.Hooks["UserPromptSubmit"]) != 1 || !slices.Equal(gotSettings.Hooks["UserPromptSubmit"][0].Hooks[0].Args, []string{"usage-hook"}) || gotSettings.StatusLine == nil || !strings.Contains(gotSettings.StatusLine.Command, "usage-statusline") {
		t.Errorf("missing usage integration: %s", args[7])
	}
	var mcpConfig struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[9]), &mcpConfig); err != nil {
		t.Fatal(err)
	}
	wantServer := mcpServer{Type: "stdio", Command: executable, Args: []string{"mcp"}, AlwaysLoad: true}
	if args[8] != "--mcp-config" || len(mcpConfig.MCPServers) != 1 || !reflect.DeepEqual(mcpConfig.MCPServers["furu"], wantServer) {
		t.Errorf("MCP config = %v", args[8:10])
	}
	if !slices.Equal(args[10:], user) {
		t.Errorf("pass-through = %q, want %q", args[10:], user)
	}
}

func TestAdaptiveDisabled(t *testing.T) {
	cfg := config.Defaults()
	off := false
	cfg.AdaptiveDelegation = &off
	args := Args(cfg, commitcmd.Result{}, "/tools/furu", nil)
	var s settings
	if err := json.Unmarshal([]byte(args[slices.Index(args, "--settings")+1]), &s); err != nil {
		t.Fatal(err)
	}
	if s.StatusLine != nil || s.Hooks != nil {
		t.Fatal("adaptive integration should be disabled")
	}
}

func TestHandoffHooks(t *testing.T) {
	marker := `C:\User Data\furu\handoff.json`
	args := Args(config.Defaults(), commitcmd.Result{}, "/tools/furu", nil, marker)
	var s settings
	if err := json.Unmarshal([]byte(args[slices.Index(args, "--settings")+1]), &s); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"StopFailure", "Stop", "SessionStart"} {
		hooks := s.Hooks[event]
		if len(hooks) != 1 || len(hooks[0].Hooks) != 1 || !slices.Equal(hooks[0].Hooks[0].Args, []string{"handoff-hook", marker}) {
			t.Fatalf("missing handoff hook %s: %+v", event, hooks)
		}
	}
	if s.Hooks["StopFailure"][0].Matcher != "rate_limit" {
		t.Fatal("StopFailure does not filter rate limits")
	}
	if hooks := s.Hooks["UserPromptSubmit"][0].Hooks; len(hooks) != 2 || !slices.Equal(hooks[1].Args, []string{"handoff-hook", marker}) {
		t.Fatalf("missing pending prompt capture: %+v", hooks)
	}
}

func TestArgsOverride(t *testing.T) {
	for _, tc := range []struct {
		user, defaults []string
	}{
		{[]string{"--model", "custom", "-c"}, []string{"--effort", "high"}},
		{[]string{"--effort=max", "--model=custom"}, nil},
		{[]string{"--", "--model", "--effort"}, []string{"--model", "opus", "--effort", "high"}},
	} {
		args := Args(config.Defaults(), commitcmd.Result{Source: commitcmd.SourceGit}, "/tools/furu", tc.user)
		prompt := slices.Index(args, "--append-system-prompt")
		if !slices.Equal(args[:prompt], tc.defaults) || !slices.Equal(args[len(args)-len(tc.user):], tc.user) {
			t.Errorf("args for %v = %q", tc.user, args)
		}
	}
}

func TestArgsWithoutCommitCommand(t *testing.T) {
	cfg := config.Defaults()
	cfg.CommitCommand = "auto"
	args := Args(cfg, commitcmd.Result{Source: commitcmd.SourceGit}, "/tools/furu", nil)
	prompt := args[slices.Index(args, "--append-system-prompt")+1]
	if strings.Contains(prompt, "を直接使わない") || !strings.Contains(prompt, "Co-Authored-By") {
		t.Errorf("unexpected git rules:\n%s", prompt)
	}
	var s struct {
		Permissions map[string]json.RawMessage `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(args[slices.Index(args, "--settings")+1]), &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Permissions["deny"]; ok || string(s.Permissions["allow"]) != `["mcp__furu__delegate"]` {
		t.Errorf("unexpected permissions: %s", s.Permissions)
	}
}

func TestPromptWithAutoDetectedGuru(t *testing.T) {
	args := Args(config.Defaults(), commitcmd.Result{Command: "guru s -y", Source: commitcmd.SourceAuto}, "/tools/furu", nil)
	prompt := args[slices.Index(args, "--append-system-prompt")+1]
	if !strings.Contains(prompt, "`guru s -y \"<message>\"`") || !strings.Contains(prompt, guruPromptHint) {
		t.Errorf("missing auto-detected guru instructions:\n%s", prompt)
	}
}

func TestCodexOrchestratorArgs(t *testing.T) {
	cfg := config.Defaults()
	cfg.WorkerTimeoutMinutes = 12
	commit := commitcmd.Result{Command: `my & wrapper -y`, Source: commitcmd.SourceConfig}
	executable := `C:\Program Files\日本語\furu.exe`
	user := []string{"resume", "session-id", "日本語の依頼"}
	args := CodexArgs(cfg, commit, executable, user)
	if !slices.Equal(args[len(args)-len(user):], user) {
		t.Fatalf("pass-through = %v", args)
	}
	values := make(map[string]string)
	for i := 0; i < len(args)-len(user); i += 2 {
		key, value, found := strings.Cut(args[i+1], "=")
		if args[i] != "-c" || !found {
			t.Fatalf("bad config override: %v", args[i:i+2])
		}
		values[key] = value
	}
	var prompt, path string
	if err := json.Unmarshal([]byte(values["developer_instructions"]), &prompt); err != nil || prompt != codexOrchestratorPrompt(commit) ||
		!strings.Contains(prompt, "Codex の司令塔") || !strings.Contains(prompt, "mcp__furu__delegate") || !strings.Contains(prompt, commit.Command) ||
		strings.Contains(prompt, "Claude の利用枠は計画・統合・最終判断のため温存する。") {
		t.Fatalf("orchestration prompt = %q, error = %v", prompt, err)
	}
	if err := json.Unmarshal([]byte(values["mcp_servers.furu.command"]), &path); err != nil || path != executable {
		t.Fatalf("MCP executable = %q, error = %v", path, err)
	}
	if values["mcp_servers.furu.args"] != `["mcp"]` || values["mcp_servers.furu.tool_timeout_sec"] != "780" ||
		values["mcp_servers.furu.enabled"] != "true" || values["mcp_servers.furu.supports_parallel_tool_calls"] != "true" {
		t.Fatalf("MCP overrides = %v", values)
	}
	var env []string
	if err := json.Unmarshal([]byte(values["mcp_servers.furu.env_vars"]), &env); err != nil || !slices.Contains(env, "APPDATA") || !slices.Contains(env, "PATH") {
		t.Fatalf("MCP environment = %v, error = %v", env, err)
	}
}
