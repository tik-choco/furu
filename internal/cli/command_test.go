package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCommandKeepsProfileAcrossWorkingDirectories(t *testing.T) {
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		t.Setenv(key, "relative-profile")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := Command(context.Background(), executable)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Dir = t.TempDir()
	abs, err := filepath.Abs("relative-profile")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		if !slices.Contains(cmd.Environ(), key+"="+abs) {
			t.Errorf("child's %s did not retain the profile used by the usage probe", key)
		}
	}
}
