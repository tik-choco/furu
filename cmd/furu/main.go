package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tik-choco/furu/internal/commitcmd"
	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/gitsync"
	"github.com/tik-choco/furu/internal/launch"
	"github.com/tik-choco/furu/internal/mcp"
	"github.com/tik-choco/furu/internal/modelcmd"
	"github.com/tik-choco/furu/internal/models"
	"github.com/tik-choco/furu/internal/preflight"
	usagepkg "github.com/tik-choco/furu/internal/usage"
	"github.com/tik-choco/furu/internal/worker"
)

const usage = `使い方
  furu [--no-update] [--no-pull] [Claude の引数 ...]  確認後、Claude を起動（利用枠切れなら Codex）
  furu codex [Codex の引数 ...]                      Codex を司令塔として起動
  furu codex --from-claude <session-id|transcript.jsonl> Claude の会話を移行して再開
  furu doctor                                       バージョンとログイン状態を表示
  furu update                                       古い CLI を更新
  furu config                                       設定ファイルのパスと有効な設定を表示
  furu model [list | set | reset]                    モデル設定を表示・変更
  furu status [--cached]                            利用枠と委譲実績を表示
  furu mcp                                          司令塔が起動する stdio MCP サーバー
  furu help | -h | --help                           ヘルプを表示
`

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "furu:", err)
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			fmt.Fprint(os.Stdout, usage)
			return 0, nil
		case "handoff-hook":
			if len(args) != 2 {
				return 1, fmt.Errorf("usage: furu handoff-hook <marker>")
			}
			if err := launch.CaptureHandoff(args[1], os.Stdin); err != nil {
				return 1, err
			}
			return 0, nil
		case "doctor":
			preflight.Check(preflight.Options{Versions: true, Login: true}, os.Stdout, os.Stderr)
			cfg, _, err := config.Load()
			if err != nil {
				return 1, err
			}
			showCommit(commitcmd.Resolve(cfg.CommitCommand))
			return 0, nil
		case "update":
			preflight.Check(preflight.Options{Versions: true, Update: true}, os.Stdout, os.Stderr)
			return 0, nil
		}
	}

	cfg, path, err := config.Load()
	if err != nil {
		return 1, err
	}
	cachePath := models.CachePath(path)
	usageStore := usagepkg.New(path)
	if len(args) > 0 {
		switch args[0] {
		case "codex":
			codexPath, err := exec.LookPath("codex")
			if err != nil {
				return 1, fmt.Errorf("codex: PATH に見つかりません: %w", err)
			}
			commit := commitcmd.Resolve(cfg.CommitCommand)
			showCommit(commit)
			if len(args) > 1 && args[1] == "--from-claude" {
				if len(args) != 3 {
					return 1, fmt.Errorf("usage: furu codex --from-claude <session-id|transcript.jsonl>")
				}
				cwd, err := os.Getwd()
				if err != nil {
					return 1, err
				}
				event, err := launch.HandoffFromTranscript(args[2], cwd)
				if err != nil {
					return 1, err
				}
				return resumeHandoff(codexPath, cfg, commit, event)
			}
			return launch.RunCodex(codexPath, cfg, commit, args[1:])
		case "config":
			fmt.Fprintln(os.Stdout, path)
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(cfg); err != nil {
				return 1, err
			}
			commit := commitcmd.Resolve(cfg.CommitCommand)
			command := commit.Command
			if command == "" {
				command = "git"
			}
			fmt.Fprintf(os.Stdout, "commit: %s (%s)\n", command, commit.Source)
			return 0, nil
		case "mcp":
			if err := mcp.Serve(os.Stdin, os.Stdout, worker.New(cfg, cachePath, usageStore)); err != nil {
				return 1, err
			}
			return 0, nil
		case "status":
			if len(args) > 2 || (len(args) == 2 && args[1] != "--cached") {
				return 1, fmt.Errorf("usage: furu status [--cached]")
			}
			if len(args) == 1 {
				cl, _ := exec.LookPath("claude")
				cx, _ := exec.LookPath("codex")
				usageStore.RefreshBoth(context.Background(), cl, cx, true)
			}
			fmt.Fprintln(os.Stdout, usageStore.Summary(time.Now()))
			usageStore.PrintStats(os.Stdout)
			return 0, nil
		case "usage-statusline":
			return 0, usageStore.StatusLine(os.Stdin, os.Stdout, time.Now())
		case "usage-hook":
			var input struct {
				Event string `json:"hook_event_name"`
			}
			if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
				return 1, err
			}
			if input.Event != "UserPromptSubmit" {
				return 0, nil
			}
			return 0, json.NewEncoder(os.Stdout).Encode(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "UserPromptSubmit", "additionalContext": usageStore.Guidance(time.Now())}})
		case "model":
			if err := modelcmd.Run(args[1:], cfg, path, stdinIsTerminal(), os.Stdin, os.Stdout, os.Stderr); err != nil {
				return 1, err
			}
			return 0, nil
		}
	}

	claudeArgs, noUpdate, noPull := launchArgs(args)
	result := preflight.Check(preflight.Options{
		Versions: !noUpdate, Update: !noUpdate, Login: true,
	}, os.Stdout, os.Stderr)
	preflight.ShowModels(cfg, cachePath, result.CodexPath, !noUpdate, os.Stdout, os.Stderr)
	commit := commitcmd.Resolve(cfg.CommitCommand)
	showCommit(commit)
	var claudeUsage, codexUsage usagepkg.Snapshot
	if cfg.AdaptiveEnabled() {
		claudeUsage, codexUsage = usageStore.RefreshBoth(context.Background(), result.ClaudePath, result.CodexPath, false)
		fmt.Fprintln(os.Stderr, usageStore.Summary(time.Now()))
	}
	fallback, fallbackArgs, err := codexFallback(result, claudeUsage, codexUsage, cfg.Orchestrator.Model, claudeArgs, time.Now())
	if err != nil {
		return 1, err
	}
	if fallback != "" {
		fmt.Fprintf(os.Stderr, "furu: %s。Codex を司令塔として起動します。\n", fallback)
	}
	if !noPull && cfg.GitSyncEnabled() {
		gitsync.Run(os.Stdin, os.Stderr, stdinIsTerminal())
	}
	if cfg.AdaptiveEnabled() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); usageStore.Poll(ctx, result.ClaudePath, result.CodexPath) }()
		defer func() { cancel(); <-done }()
	}
	if fallback != "" {
		if len(fallbackArgs) == 2 && fallbackArgs[0] == "--from-claude" {
			cwd, err := os.Getwd()
			if err != nil {
				return 1, err
			}
			event, err := launch.HandoffFromTranscript(fallbackArgs[1], cwd)
			if err != nil {
				return 1, err
			}
			return resumeHandoff(result.CodexPath, cfg, commit, event)
		}
		return launch.RunCodex(result.CodexPath, cfg, commit, fallbackArgs)
	}
	if cfg.AdaptiveEnabled() && stdinIsTerminal() && interactiveClaudeArgs(claudeArgs) {
		return runClaudeWithHandoff(result.ClaudePath, cfg, commit, usageStore, claudeArgs)
	}
	return launch.Run(result.ClaudePath, cfg, commit, claudeArgs)
}

func interactiveClaudeArgs(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "-p" || arg == "--print" || strings.HasPrefix(arg, "--print=") {
			return false
		}
	}
	return true
}

func runClaudeWithHandoff(claudePath string, cfg config.Config, commit commitcmd.Result, store *usagepkg.Store, args []string) (int, error) {
	dir, err := os.MkdirTemp("", "furu-handoff-")
	if err != nil {
		return 1, err
	}
	marker := filepath.Join(dir, "rate-limit.json")
	defer func() { _ = os.Remove(marker); _ = os.Remove(dir) }()
	ctx, cancel := context.WithCancel(context.Background())
	model := effectiveClaudeModel(cfg.Orchestrator.Model, args)
	limited := func(event launch.Handoff) bool {
		return shouldHandoff(event, store.Read("claude"), model, time.Now())
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		notified := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				event, err := launch.ReadHandoff(marker)
				if err == nil && limited(event) && !notified {
					fmt.Fprintln(os.Stderr, "\nfuru: Claude の利用上限を検知しました。Claude を /exit で終了すると、会話を引き継いで Codex を自動起動します。")
					notified = true
				} else if os.IsNotExist(err) || (err == nil && !limited(event)) {
					notified = false
				}
			}
		}
	}()
	code, runErr := launch.Run(claudePath, cfg, commit, args, marker)
	cancel()
	<-done
	event, err := launch.ReadHandoff(marker)
	if os.IsNotExist(err) {
		return code, runErr
	}
	if err != nil {
		if code != 0 || runErr != nil {
			return code, runErr
		}
		return 1, fmt.Errorf("read Claude handoff: %w", err)
	}
	if !limited(event) || !allowAutoHandoff(code, runErr, event, os.Stderr) {
		return code, runErr
	}
	// StopFailure cannot exit Claude. Wait for the original process to exit
	// normally before starting a second terminal UI or reading its transcript.
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		return 1, handoffError(event, fmt.Errorf("Codex not found: %w", err))
	}
	if _, _, blocked := store.Read("codex").Pressure("", time.Now()); blocked {
		return 1, handoffError(event, fmt.Errorf("Codex の利用枠も上限に達しました"))
	}
	return resumeHandoff(codexPath, cfg, commit, event)
}

func allowAutoHandoff(code int, runErr error, event launch.Handoff, stderr io.Writer) bool {
	if code == 0 && runErr == nil {
		return true
	}
	fmt.Fprintln(stderr, "furu:", handoffError(event, fmt.Errorf("Claude が正常終了しませんでした")))
	return false
}

func shouldHandoff(event launch.Handoff, usage usagepkg.Snapshot, model string, now time.Time) bool {
	if event.RateLimited() {
		return true
	}
	_, _, blocked := usage.Pressure(model, now)
	return event.Hook == "UserPromptSubmit" && blocked
}

func handoffError(event launch.Handoff, err error) error {
	return fmt.Errorf("Claude の会話引き継ぎに失敗しました: %w\n同じプロジェクトで再試行: furu codex --from-claude %s", err, event.SessionID)
}

func resumeHandoff(codexPath string, cfg config.Config, commit commitcmd.Result, event launch.Handoff) (int, error) {
	fmt.Fprintln(os.Stderr, "furu: Claude の会話を Codex に移行しています...")
	id, err := launch.ImportHandoff(context.Background(), codexPath, event)
	if err != nil {
		return 1, handoffError(event, err)
	}
	fmt.Fprintln(os.Stderr, "furu: 移行済みの会話を Codex の司令塔として再開します。")
	code, err := launch.RunCodex(codexPath, cfg, commit, []string{"resume", "-C", event.CWD, id, "Claude の利用上限で中断した依頼を引き継いでください。会話と現在の作業状態を確認し、完了済みの操作は繰り返さず、残作業を続けてください。"})
	if code != 0 || err != nil {
		fmt.Fprintf(os.Stderr, "furu: 移行済みの会話を再開するには furu codex resume %s\n", id)
	}
	return code, err
}

// codexFallback checks the orchestrator rather than the worker model. Only
// portable arguments may cross the backend boundary; Claude resume IDs and
// permission/settings flags must never be interpreted as Codex arguments.
func effectiveClaudeModel(model string, args []string) string {
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--model" && i+1 < len(args) {
			model = args[i+1]
		} else if strings.HasPrefix(arg, "--model=") {
			model = strings.TrimPrefix(arg, "--model=")
		}
	}
	return model
}

func codexFallback(result preflight.Result, claude, codex usagepkg.Snapshot, model string, args []string, now time.Time) (reason string, codexArgs []string, err error) {
	model = effectiveClaudeModel(model, args)
	if result.ClaudePath == "" {
		reason = "Claude が見つかりません"
	} else if result.ClaudeAuthFailed {
		reason = "Claude のログインが無効です"
	} else if _, _, blocked := claude.Pressure(model, now); blocked {
		reason = "Claude の利用枠が上限に達しました"
	} else {
		return "", nil, nil
	}
	if result.CodexPath == "" {
		return "", nil, fmt.Errorf("%s。Codex も PATH に見つかりません", reason)
	}
	if result.CodexAuthFailed {
		return "", nil, fmt.Errorf("%s。Codex を使うには codex login を実行してください", reason)
	}
	if _, _, blocked := codex.Pressure("", now); blocked {
		return "", nil, fmt.Errorf("%s。Codex の利用枠も上限に達しました（furu status で確認できます）", reason)
	}
	switch {
	case len(args) == 0:
		return reason, nil, nil
	case len(args) == 2 && (args[0] == "--resume" || args[0] == "-r") && launch.SessionID(args[1]):
		return reason, []string{"--from-claude", args[1]}, nil
	case len(args) == 1 && strings.HasPrefix(args[0], "--resume=") && launch.SessionID(strings.TrimPrefix(args[0], "--resume=")):
		return reason, []string{"--from-claude", strings.TrimPrefix(args[0], "--resume=")}, nil
	case len(args) == 1 && !strings.HasPrefix(args[0], "-"):
		return reason, []string{"--", args[0]}, nil
	case len(args) == 2 && args[0] == "--":
		return reason, args, nil
	default:
		return "", nil, fmt.Errorf("%s。Claude 専用の引数は引き継げません。furu codex で Codex を起動してください", reason)
	}
}

func stdinIsTerminal() bool {
	stat, err := os.Stdin.Stat()
	return err == nil && stat.Mode()&os.ModeCharDevice != 0
}

func showCommit(commit commitcmd.Result) {
	switch commit.Source {
	case commitcmd.SourceAuto:
		fmt.Fprintf(os.Stderr, "furu: commit: %s (自動検知)\n", commit.Command)
	case commitcmd.SourceConfig:
		fmt.Fprintf(os.Stderr, "furu: commit: %s (設定)\n", commit.Command)
	default:
		fmt.Fprintln(os.Stderr, "furu: commit: git")
	}
}

func launchArgs(args []string) (claudeArgs []string, noUpdate, noPull bool) {
	for i, arg := range args {
		switch arg {
		case "--":
			return append(claudeArgs, args[i:]...), noUpdate, noPull
		case "--no-update":
			noUpdate = true
		case "--no-pull":
			noPull = true
		default:
			claudeArgs = append(claudeArgs, arg)
		}
	}
	return claudeArgs, noUpdate, noPull
}
