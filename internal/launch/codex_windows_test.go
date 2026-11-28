package launch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tik-choco/furu/internal/cli"
	"github.com/tik-choco/furu/internal/commitcmd"
	"github.com/tik-choco/furu/internal/config"
)

func TestCodexOrchestratorWindowsShim(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FURU_CODEX_ARGS_TEST_HELPER", executable)
	shim := filepath.Join(t.TempDir(), "codex.cmd")
	const script = "@echo off\r\n" +
		`"%FURU_CODEX_ARGS_TEST_HELPER%" "-test.run=^TestCodexArgsHelper$" -- %*` + "\r\n"
	if err := os.WriteFile(shim, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	args := CodexArgs(config.Defaults(), commitcmd.Result{Command: `wrapper & %name% "quotes"`},
		`C:\Program Files\日本語\furu.exe`, []string{"--", "日本語の依頼"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, err := cli.Command(ctx, shim, args...)
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim: %v, %s", err, output)
	}
	var received []string
	if err := json.Unmarshal(output, &received); err != nil || !slices.Equal(received, args) {
		t.Fatalf("arguments did not survive shim: %v, %s", err, output)
	}
}

func TestCodexArgsHelper(t *testing.T) {
	separator := slices.Index(os.Args, "--")
	if os.Getenv("FURU_CODEX_ARGS_TEST_HELPER") == "" || separator < 0 {
		return
	}
	if err := json.NewEncoder(os.Stdout).Encode(os.Args[separator+1:]); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
