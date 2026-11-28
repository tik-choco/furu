package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Command runs a path already resolved by exec.LookPath.
func Command(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
	cmd, err := command(ctx, path, args)
	if err != nil {
		return nil, err
	}
	// Workers may run in a different directory. Keep relative profile overrides
	// pointing at the same account as the usage and login probes.
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		if dir := ProfileDir(key); dir != os.Getenv(key) {
			cmd.Env = append(cmd.Environ(), key+"="+dir)
		}
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd, nil
}

// ProfileDir resolves an explicitly configured CLI profile against furu's cwd.
// An empty value means the CLI's default profile.
func ProfileDir(key string) string {
	dir := os.Getenv(key)
	if dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
	}
	return dir
}
