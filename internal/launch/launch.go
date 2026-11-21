package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
}

type mcpServer struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func Args(cfg config.Config, commit commitcmd.Result, executable string, userArgs []string) []string {
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
	settingsJSON, _ := json.Marshal(s)
	mcpJSON, _ := json.Marshal(struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{
		MCPServers: map[string]mcpServer{
			"furu": {Type: "stdio", Command: executable, Args: []string{"mcp"}},
		},
	})
	args = append(args, "--append-system-prompt", orchestratorPrompt(commit),
		"--settings", string(settingsJSON), "--mcp-config", string(mcpJSON))
	return append(args, userArgs...)
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

func Run(claudePath string, cfg config.Config, commit commitcmd.Result, userArgs []string) (int, error) {
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
	cmd := exec.CommandContext(context.Background(), claudePath, Args(cfg, commit, executable, userArgs)...)
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
