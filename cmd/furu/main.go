package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/tik-choco/furu/internal/commitcmd"
	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/launch"
	"github.com/tik-choco/furu/internal/mcp"
	"github.com/tik-choco/furu/internal/modelcmd"
	"github.com/tik-choco/furu/internal/models"
	"github.com/tik-choco/furu/internal/preflight"
	"github.com/tik-choco/furu/internal/worker"
)

const usage = `使い方
  furu [--no-update] [Claude の引数 ...]  確認後、Claude を起動
  furu doctor                           バージョンとログイン状態を表示
  furu update                           古い CLI を更新
  furu config                           設定ファイルのパスと有効な設定を表示
  furu model [list | set | reset]        モデル設定を表示・変更
  furu mcp                              Claude が起動する stdio MCP サーバー
  furu help | -h | --help               ヘルプを表示
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
	if len(args) > 0 {
		switch args[0] {
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
			if err := mcp.Serve(os.Stdin, os.Stdout, worker.New(cfg, cachePath)); err != nil {
				return 1, err
			}
			return 0, nil
		case "model":
			stat, err := os.Stdin.Stat()
			interactive := err == nil && stat.Mode()&os.ModeCharDevice != 0
			if err := modelcmd.Run(args[1:], cfg, path, interactive, os.Stdin, os.Stdout, os.Stderr); err != nil {
				return 1, err
			}
			return 0, nil
		}
	}

	claudeArgs, noUpdate := launchArgs(args)
	result := preflight.Check(preflight.Options{
		Versions: !noUpdate, Update: !noUpdate, Login: true,
	}, os.Stdout, os.Stderr)
	preflight.ShowModels(cfg, cachePath, result.CodexPath, !noUpdate, os.Stdout, os.Stderr)
	commit := commitcmd.Resolve(cfg.CommitCommand)
	showCommit(commit)
	if result.ClaudePath == "" || result.ClaudeAuthFailed {
		return 1, nil
	}
	return launch.Run(result.ClaudePath, cfg, commit, claudeArgs)
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

func launchArgs(args []string) (claudeArgs []string, noUpdate bool) {
	for i, arg := range args {
		switch arg {
		case "--":
			return append(claudeArgs, args[i:]...), noUpdate
		case "--no-update":
			noUpdate = true
		default:
			claudeArgs = append(claudeArgs, arg)
		}
	}
	return claudeArgs, noUpdate
}
