package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/tik-choco/furu/internal/cli"
	"github.com/tik-choco/furu/internal/commitcmd"
	"github.com/tik-choco/furu/internal/config"
)

type settings struct {
	Attribution struct {
		Commit string `json:"commit"`
		PR     string `json:"pr"`
	} `json:"attribution"`
	Env         map[string]string `json:"env"`
	Permissions struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny,omitempty"`
	} `json:"permissions"`
	Hooks      map[string][]hookMatcher `json:"hooks,omitempty"`
	StatusLine *statusLine              `json:"statusLine,omitempty"`
}

type hookMatcher struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []commandHook `json:"hooks"`
}
type commandHook struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}
type statusLine struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type mcpServer struct {
	Type       string   `json:"type"`
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	AlwaysLoad bool     `json:"alwaysLoad"`
}

func Args(cfg config.Config, commit commitcmd.Result, executable string, userArgs []string, handoffMarker ...string) []string {
	var args []string
	if !hasFlag(userArgs, "--model") {
		args = append(args, "--model", cfg.Orchestrator.Model)
	}
	if !hasFlag(userArgs, "--effort") {
		args = append(args, "--effort", cfg.Orchestrator.Effort)
	}

	var s settings
	s.Env = map[string]string{
		"MCP_TOOL_TIMEOUT": strconv.FormatInt(cfg.WorkerTimeout().Milliseconds()+time.Minute.Milliseconds(), 10),
	}
	s.Permissions.Allow = []string{"mcp__furu__delegate"}
	if commit.Command != "" {
		s.Permissions.Deny = []string{"Bash(git commit:*)", "Bash(git push:*)"}
	}
	if cfg.AdaptiveEnabled() {
		s.Hooks = map[string][]hookMatcher{"UserPromptSubmit": {{Hooks: []commandHook{{Type: "command", Command: executable, Args: []string{"usage-hook"}, Timeout: 5}}}}}
		if len(handoffMarker) > 0 && handoffMarker[0] != "" {
			hook := commandHook{Type: "command", Command: executable, Args: []string{"handoff-hook", handoffMarker[0]}, Timeout: 5}
			s.Hooks["StopFailure"] = []hookMatcher{{Matcher: "rate_limit", Hooks: []commandHook{hook}}}
			s.Hooks["Stop"] = []hookMatcher{{Hooks: []commandHook{hook}}}
			s.Hooks["SessionStart"] = []hookMatcher{{Hooks: []commandHook{hook}}}
			s.Hooks["UserPromptSubmit"][0].Hooks = append(s.Hooks["UserPromptSubmit"][0].Hooks, hook)
		}
		if cfg.StatusLineEnabled() {
			s.StatusLine = &statusLine{Type: "command", Command: statusLineCommand(executable)}
		}
	}
	settingsJSON, _ := json.Marshal(s)
	mcpJSON, _ := json.Marshal(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{
		MCPServers: map[string]mcpServer{
			"furu": {Type: "stdio", Command: executable, Args: []string{"mcp"}, AlwaysLoad: true},
		},
	})
	args = append(args, "--append-system-prompt", orchestratorPrompt(commit),
		"--settings", string(settingsJSON), "--mcp-config", string(mcpJSON))
	return append(args, userArgs...)
}

func statusLineCommand(executable string) string {
	path := filepath.ToSlash(executable)
	if runtime.GOOS == "windows" {
		// Claude uses Git Bash when installed, PowerShell otherwise.
		if !hasGitBash() {
			return "& '" + strings.ReplaceAll(path, "'", "''") + "' usage-statusline"
		}
	}
	return "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "' usage-statusline"
}

func hasGitBash() bool {
	if p := os.Getenv("CLAUDE_CODE_GIT_BASH_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if git, err := exec.LookPath("git"); err == nil {
		base := filepath.Dir(filepath.Dir(git))
		for _, p := range []string{filepath.Join(base, "bin", "bash.exe"), filepath.Join(base, "usr", "bin", "bash.exe")} {
			if _, err := os.Stat(p); err == nil {
				return true
			}
		}
	}
	for _, key := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if base := os.Getenv(key); base != "" {
			if _, err := os.Stat(filepath.Join(base, "Git", "bin", "bash.exe")); err == nil {
				return true
			}
		}
	}
	return false
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

func Run(claudePath string, cfg config.Config, commit commitcmd.Result, userArgs []string, handoffMarker ...string) (int, error) {
	if cli.IsShim(claudePath) {
		return 1, fmt.Errorf("interactive Claude requires claude.exe on PATH (found %s)", claudePath)
	}
	executable, err := os.Executable()
	if err != nil {
		return 1, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return 1, err
	}
	cmd, err := cli.Command(context.Background(), claudePath, Args(cfg, commit, executable, userArgs, handoffMarker...)...)
	if err != nil {
		return 1, err
	}
	return runInteractive(cmd)
}

// CodexArgs configures the same furu MCP server and orchestration instructions
// for this session only, without modifying the user's Codex config.toml.
func CodexArgs(cfg config.Config, commit commitcmd.Result, executable string, userArgs []string) []string {
	args := []string{
		"-c", "developer_instructions=" + tomlQuoted(codexOrchestratorPrompt(commit)),
		"-c", "mcp_servers.furu.command=" + tomlQuoted(executable),
		"-c", `mcp_servers.furu.args=["mcp"]`,
		"-c", `mcp_servers.furu.enabled=true`,
		"-c", `mcp_servers.furu.supports_parallel_tool_calls=true`,
		"-c", "mcp_servers.furu.tool_timeout_sec=" + strconv.FormatInt(int64(cfg.WorkerTimeout()/time.Second)+60, 10),
		// The MCP process must be able to find workers and their local configs.
		"-c", `mcp_servers.furu.env_vars=["PATH","HOME","USERPROFILE","APPDATA","LOCALAPPDATA","XDG_CONFIG_HOME","XDG_DATA_HOME","CODEX_HOME","CLAUDE_CONFIG_DIR","SystemRoot","ComSpec","TEMP","TMP"]`,
	}
	return append(args, userArgs...)
}

// TOML basic strings support Unicode escapes. Encode whitespace, quotes and
// shell metacharacters so config overrides also survive Windows npm shims.
func tomlQuoted(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r <= 0x20 || r == 0x7f || strings.ContainsRune(`"&|<>^()%!`, r):
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// RunCodex starts Codex as an orchestrator, rather than a standalone worker.
func RunCodex(codexPath string, cfg config.Config, commit commitcmd.Result, userArgs []string) (int, error) {
	executable, err := os.Executable()
	if err != nil {
		return 1, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return 1, err
	}
	cmd, err := cli.Command(context.Background(), codexPath, CodexArgs(cfg, commit, executable, userArgs)...)
	if err != nil {
		return 1, err
	}
	return runInteractive(cmd)
}

func runInteractive(cmd *exec.Cmd) (int, error) {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	// The child receives terminal interrupts independently.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}
