package gitsync

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tik-choco/furu/internal/cli"
)

const fetchTimeout = 20 * time.Second

type commandRunner func(context.Context, string, ...string) (string, error)

// Run synchronizes the current work tree before launch, reporting errors to out.
func Run(in io.Reader, out io.Writer, interactive bool) {
	syncWorkTree(in, out, interactive, exec.LookPath, commandOutput)
}

func syncWorkTree(in io.Reader, out io.Writer, interactive bool, lookPath func(string) (string, error), run commandRunner) {
	path, err := lookPath("git")
	if err != nil {
		if !errors.Is(err, exec.ErrNotFound) {
			warn(out, "git を探せませんでした", "", err)
		}
		return
	}

	ctx := context.Background()
	git := func(args ...string) (string, error) {
		return run(ctx, path, args...)
	}
	workTree, err := git("rev-parse", "--is-inside-work-tree")
	if err != nil {
		if !strings.Contains(workTree, "not a git repository") {
			warn(out, "作業ツリーを確認できませんでした", workTree, err)
		}
		return
	}
	if workTree != "true" {
		return
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	output, fetchErr := run(fetchCtx, path, "fetch", "--quiet")
	if fetchCtx.Err() != nil {
		fetchErr = fetchCtx.Err()
	}
	cancel()
	if fetchErr != nil {
		warn(out, "fetch に失敗しました", output, fetchErr)
	}

	branch := currentBranch(git, out)
	if branch == "main" || branch == "master" {
		if hasDevelop(git, out, fetchErr == nil) && confirmSwitch(in, out, branch, interactive) {
			output, err := git("switch", "--no-overwrite-ignore", "develop")
			if err != nil {
				warn(out, "develop への切り替えに失敗しました", output, err)
				// A post-checkout hook can fail after Git has switched.
				branch = currentBranch(git, out)
			} else {
				branch = "develop"
			}
		}
	}
	if branch != "" && fetchErr == nil {
		fastForward(git, out, branch)
	}
}

func currentBranch(git func(...string) (string, error), out io.Writer) string {
	branch, err := git("symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		if !exitedWith(err, 1) {
			warn(out, "現在のブランチを確認できませんでした", branch, err)
		}
		return ""
	}
	return branch
}

func hasDevelop(git func(...string) (string, error), out io.Writer, fetched bool) bool {
	refs := []string{"refs/heads/develop"}
	if fetched {
		refs = append(refs, "refs/remotes/origin/develop")
	}
	for _, ref := range refs {
		output, err := git("show-ref", "--verify", "--quiet", ref)
		if err == nil {
			return true
		}
		if !exitedWith(err, 1) {
			warn(out, "develop の有無を確認できませんでした", output, err)
		}
	}
	return false
}

func confirmSwitch(in io.Reader, out io.Writer, branch string, interactive bool) bool {
	if !interactive {
		fmt.Fprintf(out, "furu: git: %s ブランチにいます。develop があります\n", branch)
		return false
	}
	fmt.Fprintf(out, "furu: git: %s ブランチにいます。develop に切り替えますか？ [Y/n]: ", branch)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		if !errors.Is(err, io.EOF) {
			warn(out, "切り替えの回答を読み取れませんでした", "", err)
		}
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	default:
		return false
	}
}

func fastForward(git func(...string) (string, error), out io.Writer, branch string) {
	upstream, err := git("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		if !strings.Contains(upstream, "no upstream configured") &&
			!strings.Contains(upstream, "not stored as a remote-tracking branch") &&
			!strings.Contains(upstream, "ambiguous argument '@{u}'") {
			warn(out, "upstream を確認できませんでした", upstream, err)
		}
		return
	}
	if upstream == "" {
		return
	}

	countOutput, err := git("rev-list", "--count", "HEAD..@{u}")
	if err != nil {
		warn(out, "更新するコミット数を確認できませんでした", countOutput, err)
		return
	}
	count, err := strconv.Atoi(countOutput)
	if err != nil {
		warn(out, "更新するコミット数を読み取れませんでした", countOutput, err)
		return
	}
	if count == 0 {
		return
	}
	// Per-branch merge options may enable squash, which leaves HEAD unchanged.
	output, err := git("merge", "--ff-only", "--no-squash", "--no-autostash", "--no-overwrite-ignore", "@{u}")
	if err != nil {
		warn(out, fmt.Sprintf("%s の fast-forward に失敗しました", branch), output, err)
		return
	}
	fmt.Fprintf(out, "furu: git: %s を %s に更新しました (%d commits)\n", branch, upstream, count)
}

func exitedWith(err error, code int) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && exit.ExitCode() == code
}

func warn(out io.Writer, message, detail string, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		message += "（タイムアウト）"
	}
	if detail == "" {
		detail = err.Error()
	}
	fmt.Fprintf(out, "furu: git: %s: %s\n", message, detail)
}

func commandOutput(ctx context.Context, path string, args ...string) (string, error) {
	cmd, err := cli.Command(ctx, path, args...)
	if err != nil {
		return "", err
	}
	switch args[0] {
	case "rev-parse":
		// Match Git's expected absence diagnostics independently of its locale.
		cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	case "fetch":
		// Fail instead of waiting for credentials before Claude starts.
		cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(stdout) + stderr.String()), err
	}
	return strings.TrimSpace(string(stdout)), nil
}
