package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/models"
)

func TestCodexArgs(t *testing.T) {
	model := config.Model{Model: "gpt-6.2-sol", Effort: "high"}
	for _, tc := range []struct {
		write   bool
		sandbox string
	}{{false, "read-only"}, {true, "workspace-write"}} {
		want := []string{"exec", "-m", "gpt-6.2-sol", "-c", `model_reasoning_effort="high"`,
			"-s", tc.sandbox, "--skip-git-repo-check", "-C", `C:\some project`, "-o", `C:\temp dir\output.txt`, "-"}
		if got := codexArgs(model, tc.write, `C:\some project`, `C:\temp dir\output.txt`); !slices.Equal(got, want) {
			t.Errorf("codex args = %q, want %q", got, want)
		}
	}
}

func TestClaudeArgs(t *testing.T) {
	model := config.Model{Model: "claude-opus-5-5", Effort: "medium"}
	for _, tc := range []struct {
		write bool
		flags []string
	}{
		{false, []string{"--disallowedTools", "Edit,Write,NotebookEdit"}},
		{true, []string{"--permission-mode", "acceptEdits"}},
	} {
		want := append([]string{"-p", "--model", "claude-opus-5-5", "--effort", "medium", "--output-format", "text"}, tc.flags...)
		if got := claudeArgs(model, tc.write); !slices.Equal(got, want) {
			t.Errorf("claude args = %q, want %q", got, want)
		}
	}
}

func TestAutoUsesResolvedCodexAndCachesProbes(t *testing.T) {
	r := testRunner(t, "success")
	command := r.command
	var probes, refreshes atomic.Int32
	var outputs []string
	r.command = func(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
		if args[0] == "login" {
			probes.Add(1)
		}
		if args[0] == "debug" {
			refreshes.Add(1)
		}
		if args[0] == "exec" {
			if len(args) < 3 || args[1] != "-m" || args[2] != "gpt-6.2-sol" {
				t.Errorf("worker argv did not use the resolved model: %q", args)
			}
			outputs = append(outputs, args[slices.Index(args, "-o")+1])
		}
		return command(ctx, path, args...)
	}
	task := "日本語のタスク\n--model value & %text%"
	dir := t.TempDir()
	for range 2 {
		result := r.Run(context.Background(), Request{Task: task, Tier: "normal", Backend: "auto", CWD: dir})
		if result.IsError || !strings.HasPrefix(result.Text, "[furu] backend=codex model=gpt-6.2-sol (sol) effort=high elapsed=") ||
			!strings.HasSuffix(result.Text, dir+"\n"+task) {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if probes.Load() != 1 || refreshes.Load() != 1 || len(outputs) != 2 || outputs[0] == outputs[1] {
		t.Errorf("login probes = %d, catalog refreshes = %d, output paths = %q", probes.Load(), refreshes.Load(), outputs)
	}
	for _, path := range outputs {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected temporary output removal, stat %q: %v", path, err)
		}
	}
}

func TestFallbackAndExplicitCodexUnavailable(t *testing.T) {
	r := testRunner(t, "logged-out")
	request := Request{Task: "test task", Tier: "hard", Backend: "auto"}
	result := r.Run(context.Background(), request)
	if result.IsError || !strings.HasPrefix(result.Text, "[furu] backend=claude model=opus effort=medium elapsed=") ||
		!strings.Contains(result.Text, "\nfallback: codex login status failed") ||
		!strings.HasSuffix(result.Text, "\n"+request.Task) {
		t.Fatalf("fallback result: %+v", result)
	}

	request.Backend = "codex"
	result = r.Run(context.Background(), request)
	if !result.IsError || !strings.Contains(result.Text, "codex login status failed") || strings.Contains(result.Text, "fallback:") {
		t.Fatalf("explicit codex result: %+v", result)
	}
}

func TestUnavailableFamilyFallsBackOnlyInAuto(t *testing.T) {
	r := testRunner(t, "catalog-unavailable")
	request := Request{Task: "test", Tier: "normal", Backend: "auto"}
	const reason = `codex model family "sol" could not be resolved`
	result := r.Run(context.Background(), request)
	if result.IsError || !strings.Contains(result.Text, "fallback: "+reason) ||
		!strings.Contains(result.Text, "backend=claude model=sonnet") {
		t.Fatalf("auto result: %+v", result)
	}
	request.Backend = "codex"
	result = r.Run(context.Background(), request)
	if !result.IsError || !strings.Contains(result.Text, reason) || strings.Contains(result.Text, "fallback:") {
		t.Fatalf("explicit codex result: %+v", result)
	}

	tier := r.cfg.Tiers["normal"]
	tier.Codex.Model = "custom-codex-id"
	r.cfg.Tiers["normal"] = tier
	result = r.Run(context.Background(), request)
	if result.IsError || !strings.Contains(result.Text, "backend=codex model=custom-codex-id effort=high") {
		t.Fatalf("pinned result: %+v", result)
	}
}

func TestCodexUsesFreshCacheWithoutRefreshing(t *testing.T) {
	r := testRunner(t, "catalog-unavailable")
	_, err := models.Refresh(r.cachePath, models.Catalog{}, func() (models.Catalog, error) {
		return models.Catalog{Codex: []models.Model{{Slug: "gpt-6.3-sol"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	command := r.command
	r.command = func(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
		if args[0] == "debug" {
			t.Error("attempted to refresh a fresh cache")
		}
		return command(ctx, path, args...)
	}
	result := r.Run(context.Background(), Request{Task: "test", Tier: "normal", Backend: "codex"})
	if result.IsError || !strings.Contains(result.Text, "model=gpt-6.3-sol (sol)") {
		t.Fatalf("cached result: %+v", result)
	}
}

func TestStaleCacheRefresh(t *testing.T) {
	for _, tc := range []struct {
		mode, want string
	}{
		{"success", "gpt-6.2-sol"},
		{"catalog-unavailable", "gpt-6.1-sol"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := testRunner(t, tc.mode)
			stale := models.Catalog{
				FetchedAt: time.Now().Add(-25 * time.Hour),
				Codex:     []models.Model{{Slug: "gpt-6.1-sol"}},
			}
			data, err := json.Marshal(stale)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(r.cachePath, data, 0600); err != nil {
				t.Fatal(err)
			}
			result := r.Run(context.Background(), Request{Task: "test", Tier: "normal", Backend: "codex"})
			if result.IsError || !strings.Contains(result.Text, "model="+tc.want+" (sol)") {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}

func TestCodexFailureDoesNotFallBack(t *testing.T) {
	r := testRunner(t, "worker-failure")
	result := r.Run(context.Background(), Request{Task: "test", Tier: "simple", Backend: "auto"})
	header, stderr, hasStderr := strings.Cut(result.Text, "\nstderr:\n")
	if !result.IsError || !strings.Contains(header, "exit code 23)") || strings.Contains(header, "fallback:") {
		t.Errorf("unexpected failure header: %q", header)
	}
	if !hasStderr || stderr != strings.Repeat("x", stderrLimit-4)+"tail" {
		t.Errorf("expected the final %d bytes of stderr, got %d bytes", stderrLimit, len(stderr))
	}
}

func TestWorkerTimeout(t *testing.T) {
	r := testRunner(t, "worker-wait")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := r.Run(ctx, Request{Task: "test", Tier: "simple", Backend: "claude"})
	if !result.IsError || !strings.Contains(result.Text, "\ntimeout") {
		t.Fatalf("timeout result: %+v", result)
	}
}

func testRunner(t *testing.T, mode string) *Runner {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := New(config.Defaults(), filepath.Join(t.TempDir(), "models-cache.json"))
	r.lookPath = func(name string) (string, error) { return name, nil }
	r.command = func(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
		testArgs := append([]string{"-test.run=^TestWorkerProcess$", "--", path}, args...)
		cmd := exec.CommandContext(ctx, executable, testArgs...)
		cmd.Env = append(os.Environ(), "FURU_TEST_WORKER_MODE="+mode)
		return cmd, nil
	}
	return r
}

func TestWorkerProcess(t *testing.T) {
	mode := os.Getenv("FURU_TEST_WORKER_MODE")
	separator := slices.Index(os.Args, "--")
	if mode == "" || separator < 0 {
		return
	}
	args := os.Args[separator+2:]
	if args[0] == "login" {
		if mode == "logged-out" {
			fmt.Fprintln(os.Stderr, "Not logged in")
		} else {
			fmt.Fprintln(os.Stdout, "Logged in")
		}
		os.Exit(0)
	}
	if args[0] == "debug" {
		if mode == "catalog-unavailable" {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, `{"models":[{"slug":"gpt-6.2-sol","visibility":"list"}]}`)
		os.Exit(0)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if mode == "worker-wait" {
		time.Sleep(10 * time.Second)
		t.Fatal("test process was not killed")
	}
	if args[0] == "exec" {
		if mode == "worker-failure" {
			fmt.Fprint(os.Stderr, "old output"+strings.Repeat("x", stderrLimit)+"tail")
			os.Exit(23)
		}
		output := args[slices.Index(args, "-o")+1]
		if err := os.WriteFile(output, append([]byte(filepath.Clean(cwd)+"\n"), input...), 0600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(os.Stdout, "discard this")
	} else {
		fmt.Fprint(os.Stdout, string(input))
	}
	os.Exit(0)
}
