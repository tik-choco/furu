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
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tik-choco/furu/internal/cli"
	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/models"
	"github.com/tik-choco/furu/internal/preflight"
	"github.com/tik-choco/furu/internal/usage"
)

type Request struct {
	Task    string
	Tier    string
	Write   bool
	CWD     string
	Backend string
	Images  []string
}

type Result struct {
	Text    string
	IsError bool
}

type Runner struct {
	cfg      config.Config
	lookPath func(string) (string, error)
	command  func(context.Context, string, ...string) (*exec.Cmd, error)
	usage    *usage.Store

	codexCache   probeCache[loginProbe]
	cachePath    string
	catalogCache probeCache[catalogProbe]
}

type loginProbe struct{ path, reason string }
type catalogProbe struct {
	catalog models.Catalog
	err     error
}
type probeEntry[T any] struct {
	ready   chan struct{}
	value   T
	expires time.Time
}
type probeCache[T any] struct {
	mu      sync.Mutex
	current *probeEntry[T]
}

// Share an in-flight probe without letting a cancelled delegate poison it.
// Expired successes and failures are rechecked instead of cached forever.
func (c *probeCache[T]) get(ctx context.Context, load func() (T, time.Duration)) (T, error) {
	c.mu.Lock()
	e := c.current
	if e != nil {
		select {
		case <-e.ready:
			if !time.Now().Before(e.expires) {
				e = nil
			}
		default:
		}
	}
	if e == nil {
		e = &probeEntry[T]{ready: make(chan struct{})}
		c.current = e
		go func(entry *probeEntry[T]) {
			value, ttl := load()
			entry.value = value
			entry.expires = time.Now().Add(ttl)
			close(entry.ready)
		}(e)
	}
	c.mu.Unlock()
	select {
	case <-e.ready:
		return e.value, ctx.Err()
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func New(cfg config.Config, cachePath string, stores ...*usage.Store) *Runner {
	r := &Runner{
		cfg: cfg, lookPath: exec.LookPath, command: cli.Command,
		cachePath: cachePath,
	}
	if cfg.AdaptiveEnabled() && len(stores) > 0 {
		r.usage = stores[0]
	}
	return r
}

func (r *Runner) Run(ctx context.Context, request Request) Result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, r.cfg.WorkerTimeout())
	defer cancel()

	tier := r.cfg.Tiers[request.Tier]
	backend, model := "codex", tier.Codex
	if request.Backend == "claude" {
		backend, model = "claude", tier.Claude
	}
	cwd := request.CWD
	if cwd == "" {
		cwd = "."
	}
	cwd, err := filepath.Abs(cwd)
	var extraDirs []string
	if err == nil {
		request.Images, extraDirs, err = resolveImages(cwd, request.Images)
	}

	var path, fallback, family, routing string
	var claudeUsage, codexUsage usage.Snapshot
	auto := request.Backend == "" || request.Backend == "auto"
	if err == nil && r.usage != nil {
		claudePath, claudeErr := r.lookPath("claude")
		if claudeErr != nil {
			claudePath = ""
		}
		codexPath, codexErr := r.lookPath("codex")
		if codexErr != nil {
			codexPath = ""
		}
		claudeUsage, codexUsage = r.usage.RefreshBoth(ctx, claudePath, codexPath, false)
		if auto {
			backend, routing = usage.Decide(claudeUsage, codexUsage, tier.Claude.Model, tier.Codex.Model, time.Now())
			if backend == "" {
				err = errors.New(routing)
				backend = "none"
			} else if backend == "claude" {
				model = tier.Claude
				fallback = routing
			}
		} else {
			snapshot := codexUsage
			if backend == "claude" {
				snapshot = claudeUsage
			}
			if _, _, blocked := snapshot.Pressure(model.Model, time.Now()); blocked {
				err = fmt.Errorf("%s usage exhausted", backend)
			}
		}
	}
	var report string
	var stderr tailBuffer
	for retried := false; err == nil; retried = true {
		if backend == "codex" {
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
				err = errors.New(reason)
			}
		} else {
			path, err = r.lookPath("claude")
			if err != nil {
				err = fmt.Errorf("claude unavailable: %w", err)
			}
		}
		var started bool
		if err == nil {
			report, started, err = r.run(ctx, backend, path, model, cwd, request, extraDirs, &stderr)
		}
		// Never retry a task once the worker process has started: it may
		// already have made changes even if it exits with an error.
		if err == nil || started || !auto || retried || ctx.Err() != nil {
			break
		}
		other, otherModel, snapshot := "claude", tier.Claude, claudeUsage
		if backend == "claude" {
			other, otherModel, snapshot = "codex", tier.Codex, codexUsage
		}
		if _, _, blocked := snapshot.Pressure(otherModel.Model, time.Now()); blocked {
			err = fmt.Errorf("%w; %s usage exhausted", err, other)
			break
		}
		fallback = err.Error()
		backend, model, family = other, otherModel, ""
		stderr = tailBuffer{}
		err = nil
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
	if routing != "" {
		header += "routing: " + routing + "\n"
	}
	if r.usage != nil {
		reason := routing
		if fallback != "" {
			reason = fallback
		}
		r.usage.Record(usage.Event{At: start, Backend: backend, Tier: request.Tier, Model: model.Model, ElapsedMS: time.Since(start).Milliseconds(), Failed: failed, Fallback: fallback != "", Reason: reason})
	}
	return Result{Text: header + report, IsError: failed}
}

func resolveImages(cwd string, paths []string) (images, extraDirs []string, err error) {
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("image not found: %s", path)
		}
		images = append(images, path)

		dir := filepath.Dir(path)
		rel, err := filepath.Rel(cwd, dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if !slices.ContainsFunc(extraDirs, func(existing string) bool {
			if runtime.GOOS == "windows" {
				return strings.EqualFold(existing, dir)
			}
			return existing == dir
		}) {
			extraDirs = append(extraDirs, dir)
		}
	}
	return images, extraDirs, nil
}

func (r *Runner) resolveCodex(ctx context.Context, codexPath, family string) (slug, reason string, err error) {
	probe, err := r.catalogCache.get(ctx, func() (catalogProbe, time.Duration) {
		catalog, _ := models.ReadCache(r.cachePath)
		var err error
		if !catalog.Fresh() {
			catalog, err = models.Refresh(r.cachePath, catalog, func() (models.Catalog, error) { return models.Fetch(codexPath, r.command) })
		}
		// A short process-local cache also allows a failed fetch to recover.
		return catalogProbe{catalog, err}, usage.RefreshInterval
	})
	if err != nil {
		return "", "", err
	}
	if entry, ok := probe.catalog.Resolve(family); ok {
		return entry.Slug, "", nil
	}
	reason = fmt.Sprintf("codex model family %q could not be resolved", family)
	if probe.err != nil {
		reason += ": " + probe.err.Error()
	}
	return "", reason, nil
}

func (r *Runner) codex(ctx context.Context) (path, reason string, err error) {
	probe, err := r.codexCache.get(ctx, func() (loginProbe, time.Duration) {
		p, reason := r.probeCodex()
		return loginProbe{p, reason}, usage.RefreshInterval
	})
	return probe.path, probe.reason, err
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

func (r *Runner) run(ctx context.Context, backend, path string, model config.Model, cwd string, request Request, extraDirs []string, stderr *tailBuffer) (string, bool, error) {
	var args []string
	var stdout bytes.Buffer
	var outputPath string
	if backend == "codex" {
		file, err := os.CreateTemp("", "furu-codex-*.txt")
		if err != nil {
			return "", false, err
		}
		outputPath = file.Name()
		defer os.Remove(outputPath)
		if err := file.Close(); err != nil {
			return "", false, err
		}
		args = codexArgs(model, request.Write, cwd, outputPath, request.Images, extraDirs)
	} else {
		args = claudeArgs(model, request.Write, extraDirs)
	}
	task := request.Task
	if len(request.Images) > 0 {
		task += "\n\n## 添付画像\n- " + strings.Join(request.Images, "\n- ")
		if backend == "claude" {
			task += "\n各画像は Read ツールで開いて内容を確認すること。"
		}
	}
	cmd, err := r.command(ctx, path, args...)
	if err != nil {
		return "", false, err
	}
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(task)
	cmd.Stderr = stderr
	if backend == "claude" {
		cmd.Stdout = &stdout
	}
	if err := cmd.Start(); err != nil {
		return "", false, err
	}
	if err := cmd.Wait(); err != nil {
		return "", true, err
	}
	if backend == "claude" {
		return stdout.String(), true, nil
	}
	report, err := os.ReadFile(outputPath)
	if err != nil {
		return "", true, fmt.Errorf("read codex final message: %w", err)
	}
	return string(report), true, nil
}

func codexArgs(model config.Model, write bool, cwd, output string, images, extraDirs []string) []string {
	args := []string{"exec"}
	for _, image := range images {
		args = append(args, "-i", image)
	}
	sandbox := "read-only"
	if write {
		sandbox = "workspace-write"
		for _, dir := range extraDirs {
			args = append(args, "--add-dir", dir)
		}
	}
	effort, _ := json.Marshal(model.Effort)
	return append(args, "-m", model.Model, "-c", "model_reasoning_effort="+string(effort),
		"-s", sandbox, "--skip-git-repo-check", "-C", cwd, "-o", output, "-")
}

func claudeArgs(model config.Model, write bool, extraDirs []string) []string {
	args := []string{"-p"}
	for _, dir := range extraDirs {
		args = append(args, "--add-dir", dir)
	}
	args = append(args, "--model", model.Model, "--effort", model.Effort, "--output-format", "text")
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
