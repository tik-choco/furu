package commitcmd

import (
	"bytes"
	"context"
	"os/exec"
	"time"

	"github.com/tik-choco/furu/internal/cli"
)

const (
	SourceConfig = "config"
	SourceAuto   = "auto"
	SourceGit    = "git"
)

// Result identifies the selected wrapper. An empty Command selects plain git.
type Result struct {
	Command string
	Source  string
}

func Resolve(configured string) Result {
	return resolve(configured, exec.LookPath, combinedOutput)
}

func resolve(configured string, lookPath func(string) (string, error), run func(context.Context, string, ...string) ([]byte, error)) Result {
	git := Result{Source: SourceGit}
	switch configured {
	case "git":
		return git
	case "", "auto":
	default:
		return Result{Command: configured, Source: SourceConfig}
	}

	path, err := lookPath("guru")
	if err != nil {
		return git
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := run(ctx, path, "save", "--help")
	if err != nil || ctx.Err() != nil || !bytes.Contains(output, []byte("--no-push")) || !bytes.Contains(output, []byte("--yes")) {
		return git
	}
	return Result{Command: "guru s -y", Source: SourceAuto}
}

func combinedOutput(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd, err := cli.Command(ctx, path, args...)
	if err != nil {
		return nil, err
	}
	return cmd.CombinedOutput()
}
