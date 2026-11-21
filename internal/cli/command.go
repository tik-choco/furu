package cli

import (
	"context"
	"os/exec"
	"time"
)

// Command runs a path already resolved by exec.LookPath.
func Command(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
	cmd, err := command(ctx, path, args)
	if err != nil {
		return nil, err
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd, nil
}
