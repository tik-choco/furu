package launch

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tik-choco/furu/internal/config"
)

func TestArgs(t *testing.T) {
	cfg := config.Defaults()
	cfg.WorkerTimeoutMinutes = 12
	cfg.CommitCommand = "wrap commit"
	executable := `C:\Program Files\tools\furu.exe`
	user := []string{"--resume", "session-id", "日本語の依頼"}
	args := Args(cfg, executable, user)
	if !slices.Equal(args[:4], []string{"--model", "opus", "--effort", "high"}) {
		t.Errorf("orchestrator defaults: %v", args[:4])
	}
	if args[4] != "--append-system-prompt" || !strings.Contains(args[5], "mcp__furu__delegate") || !strings.Contains(args[5], "`wrap commit \"<message>\"`") {
		t.Errorf("missing orchestrator prompt: %v", args[4:6])
	}
	const wantSettings = `{"attribution":{"commit":"","pr":""},"env":{"MCP_TOOL_TIMEOUT":"780000"},"permissions":{"allow":["mcp__furu__delegate"]}}`
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
		args := Args(config.Defaults(), "/tools/furu", tc.user)
		prompt := slices.Index(args, "--append-system-prompt")
		if !slices.Equal(args[:prompt], tc.defaults) || !slices.Equal(args[len(args)-len(tc.user):], tc.user) {
			t.Errorf("args for %v = %q", tc.user, args)
		}
	}
}

func TestPromptWithoutCommitCommand(t *testing.T) {
	args := Args(config.Defaults(), "/tools/furu", nil)
	prompt := args[slices.Index(args, "--append-system-prompt")+1]
	if strings.Contains(prompt, "を直接使わない") || !strings.Contains(prompt, "Co-Authored-By") {
		t.Errorf("unexpected git rules:\n%s", prompt)
	}
}
