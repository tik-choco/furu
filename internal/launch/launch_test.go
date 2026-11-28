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
	const wantSettings = `{"attribution":{"commit":"","pr":""},"env":{"MCP_TOOL_TIMEOUT":"780000"},"permissions":{"allow":["mcp__furu__delegate"],"deny":["Bash(git commit:*)","Bash(git push:*)"]}}`
	if args[6] != "--settings" || args[7] != wantSettings {
		t.Errorf("settings = %v, want %s", args[6:8], wantSettings)
	}
	var mcpConfig struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[9]), &mcpConfig); err != nil {
		t.Fatal(err)
	}
	wantServer := mcpServer{Type: "stdio", Command: executable, Args: []string{"mcp"}}
	if args[8] != "--mcp-config" || len(mcpConfig.MCPServers) != 1 || !reflect.DeepEqual(mcpConfig.MCPServers["furu"], wantServer) {
		t.Errorf("MCP config = %v", args[8:10])
	}
	if !slices.Equal(args[10:], user) {
		t.Errorf("pass-through = %q, want %q", args[10:], user)
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
