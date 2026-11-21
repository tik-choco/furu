package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWindowsShim(t *testing.T) {
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FURU_TEST_SHIM_HELPER", helper)
	t.Setenv("FURU_TEST_SHIM_SENTINEL", "unexpected expansion")
	dir := t.TempDir()
	t.Setenv("PATH", dir+";"+os.Getenv("PATH"))
	name := "shim & %FURU_TEST_SHIM_SENTINEL% !"
	shim := filepath.Join(dir, name+".cmd")
	const script = "@echo off\r\nsetlocal\r\n" +
		`"%FURU_TEST_SHIM_HELPER%" "-test.run=^TestShimProcess$" -- %*` + "\r\n"
	if err := os.WriteFile(shim, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"exec", "-c", `model_reasoning_effort="high"`, "-C",
		`C:\作業 & %FURU_TEST_SHIM_SENTINEL% !\`, "-o", `C:\dir^with^carets\result.txt`, "-"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, err := Command(ctx, path, args...)
	if err != nil {
		t.Fatal(err)
	}
	const prompt = "日本語 & %FURU_TEST_SHIM_SENTINEL% !\r\nsecond line"
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim: %v, output %s", err, out)
	}
	var result shimResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("shim output %q: %v", out, err)
	}
	if !slices.Equal(result.Args, args) || result.Stdin != prompt {
		t.Fatalf("shim got args %q and stdin %q, want %q and %q", result.Args, result.Stdin, args, prompt)
	}
}

type shimResult struct {
	Args  []string
	Stdin string
}

func TestShimProcess(t *testing.T) {
	separator := slices.Index(os.Args, "--")
	if os.Getenv("FURU_TEST_SHIM_HELPER") == "" || separator < 0 {
		return
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(shimResult{Args: os.Args[separator+1:], Stdin: string(input)}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
