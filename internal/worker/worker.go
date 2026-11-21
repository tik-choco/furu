package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tik-choco/furu/internal/cli"
	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/models"
	"github.com/tik-choco/furu/internal/preflight"
)

type Request struct {
	Task    string
	Tier    string
	Write   bool
	CWD     string
	Backend string
}

type Result struct {
	Text    string
	IsError bool
}

type Runner struct {
	cfg      config.Config
	lookPath func(string) (string, error)
	command  func(context.Context, string, ...string) (*exec.Cmd, error)

	codexOnce   sync.Once
	codexReady  chan struct{}
	codexPath   string
	codexReason string

	cachePath    string
	catalogOnce  sync.Once
	catalogReady chan struct{}
	catalog      models.Catalog
	catalogErr   error
}

func New(cfg config.Config, cachePath string) *Runner {
	return &Runner{
		cfg: cfg, lookPath: exec.LookPath, command: cli.Command,
		codexReady: make(chan struct{}), cachePath: cachePath, catalogReady: make(chan struct{}),
	}
}

func (r *Runner) Run(ctx context.Context, request Request) Result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, r.cfg.WorkerTimeout())
	defer cancel()

	tier := r.cfg.Tiers[request.Tier]
	backend, model := "codex", tier.Codex
	var path, fallback, family string
	var err error
	if request.Backend != "claude" {
		var reason string
		path, reason, err = r.codex(ctx)
		if err == nil && reason == "" && models.IsFamily(model.Model) {
			var slug string
			slug, reason, err = r.resolveCodex(ctx, path, model.Model)
			if slug != "" {
				family, model.Model = model.Model, slug
			}
		}
		if err == nil && reason != "" {
			if request.Backend == "codex" {
				err = errors.New(reason)
			} else {
				fallback = reason
			}
		}
	}
	if request.Backend == "claude" || fallback != "" {
		backend, model = "claude", tier.Claude
		path, err = r.lookPath("claude")
		if err != nil {
			err = fmt.Errorf("claude unavailable: %w", err)
		}
	}

	var report string
	var stderr tailBuffer
	if err == nil {
		cwd := request.CWD
		if cwd == "" {
			cwd = "."
		}
		cwd, err = filepath.Abs(cwd)
		if err == nil {
			report, err = r.run(ctx, backend, path, model, cwd, request, &stderr)
		}
	}
	contextErr := ctx.Err()
	failed := err != nil || contextErr != nil
	if failed {
		report = failure(contextErr, err, stderr.data)
	}
	label := model.Model
	if family != "" {
		label += " (" + family + ")"
	}
	header := fmt.Sprintf("[furu] backend=%s model=%s effort=%s elapsed=%s\n",
		backend, label, model.Effort, time.Since(start).Round(time.Millisecond))
	if fallback != "" {
		header += "fallback: " + fallback + "\n"
	}
	return Result{Text: header + report, IsError: failed}
}

func (r *Runner) resolveCodex(ctx context.Context, codexPath, family string) (slug, reason string, err error) {
	r.catalogOnce.Do(func() {
		// A delegate can stop waiting without cancelling the shared refresh.
		go func() {
			defer close(r.catalogReady)
			r.catalog, _ = models.ReadCache(r.cachePath)
			if !r.catalog.Fresh() {
				r.catalog, r.catalogErr = models.Refresh(r.cachePath, r.catalog, func() (models.Catalog, error) {
					return models.Fetch(codexPath, r.command)
				})
			}
		}()
	})
	select {
	case <-r.catalogReady:
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		if entry, ok := r.catalog.Resolve(family); ok {
			return entry.Slug, "", nil
		}
		reason := fmt.Sprintf("codex model family %q could not be resolved", family)
		if r.catalogErr != nil {
			reason += ": " + r.catalogErr.Error()
		}
		return "", reason, nil
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

func (r *Runner) codex(ctx context.Context) (path, reason string, err error) {
	r.codexOnce.Do(func() {
		// A cancelled delegate must not cache a failed login probe for
		// every subsequent delegate. Give the shared probe its own deadline.
		go func() {
			defer close(r.codexReady)
			r.codexPath, r.codexReason = r.probeCodex()
		}()
	})
	select {
	case <-r.codexReady:
		return r.codexPath, r.codexReason, ctx.Err()
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

func (r *Runner) probeCodex() (path, reason string) {
	path, err := r.lookPath("codex")
	if err != nil {
		return "", "codex was not found on PATH"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd, err := r.command(ctx, path, "login", "status")
	if err != nil {
		return "", fmt.Sprintf("codex login status could not run: %v", err)
	}
	out, err := cmd.CombinedOutput()
	if !preflight.CodexLoggedIn(string(out), err) {
		return "", "codex login status failed; run codex login"
	}
	return path, ""
}

func (r *Runner) run(ctx context.Context, backend, path string, model config.Model, cwd string, request Request, stderr *tailBuffer) (string, error) {
	var args []string
	var stdout bytes.Buffer
	var outputPath string
	if backend == "codex" {
		file, err := os.CreateTemp("", "furu-codex-*.txt")
		if err != nil {
			return "", err
		}
		outputPath = file.Name()
		defer os.Remove(outputPath)
		if err := file.Close(); err != nil {
			return "", err
		}
		args = codexArgs(model, request.Write, cwd, outputPath)
	} else {
		args = claudeArgs(model, request.Write)
	}
	cmd, err := r.command(ctx, path, args...)
	if err != nil {
		return "", err
	}
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(request.Task)
	cmd.Stderr = stderr
	if backend == "claude" {
		cmd.Stdout = &stdout
	}
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if backend == "claude" {
		return stdout.String(), nil
	}
	report, err := os.ReadFile(outputPath)
	if err != nil {
		return "", fmt.Errorf("read codex final message: %w", err)
	}
	return string(report), nil
}

func codexArgs(model config.Model, write bool, cwd, output string) []string {
	sandbox := "read-only"
	if write {
		sandbox = "workspace-write"
	}
	effort, _ := json.Marshal(model.Effort)
	return []string{"exec", "-m", model.Model, "-c", "model_reasoning_effort=" + string(effort),
		"-s", sandbox, "--skip-git-repo-check", "-C", cwd, "-o", output, "-"}
}

func claudeArgs(model config.Model, write bool) []string {
	args := []string{"-p", "--model", model.Model, "--effort", model.Effort, "--output-format", "text"}
	if write {
		return append(args, "--permission-mode", "acceptEdits")
	}
	return append(args, "--disallowedTools", "Edit,Write,NotebookEdit")
}

func failure(contextErr, err error, stderr []byte) string {
	var message string
	var exit *exec.ExitError
	switch contextErr {
	case context.DeadlineExceeded:
		message = "timeout"
	case context.Canceled:
		message = "cancelled"
	default:
		if errors.As(err, &exit) {
			message = fmt.Sprintf("worker failed (exit code %d)", exit.ExitCode())
		} else {
			message = err.Error()
		}
	}
	if contextErr != nil && errors.As(err, &exit) {
		message += fmt.Sprintf(" (exit code %d)", exit.ExitCode())
	}
	if len(stderr) > 0 {
		message += "\nstderr:\n" + string(stderr)
	}
	return message
}

const stderrLimit = 4 * 1024

type tailBuffer struct {
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= stderrLimit {
		b.data = append(b.data[:0], p[n-stderrLimit:]...)
		return n, nil
	}
	if drop := len(b.data) + n - stderrLimit; drop > 0 {
		b.data = b.data[:copy(b.data, b.data[drop:])]
	}
	b.data = append(b.data, p...)
	return n, nil
}
