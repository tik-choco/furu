//go:build !windows

package cli

import (
	"context"
	"os/exec"
)

func IsShim(string) bool { return false }

func command(ctx context.Context, path string, args []string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, path, args...), nil
}
