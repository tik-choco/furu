package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tik-choco/furu/internal/cli"
)

const (
	probeTimeout  = 20 * time.Second
	updateTimeout = 10 * time.Minute
)

type Options struct {
	Versions bool
	Update   bool
	Login    bool
}

type Result struct {
	ClaudePath       string
	ClaudeAuthFailed bool
	CodexPath        string
}

type checker struct {
	lookPath func(string) (string, error)
	run      func(context.Context, string, []string, io.Writer, io.Writer) error
	stdout   io.Writer
	stderr   io.Writer
	mu       sync.Mutex
}

func Check(options Options, stdout, stderr io.Writer) Result {
	c := checker{lookPath: exec.LookPath, run: runCommand, stdout: stdout, stderr: stderr}
	return c.check(options)
}

func (c *checker) check(options Options) Result {
	var result Result
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		result.ClaudePath, result.ClaudeAuthFailed = c.checkCLI("claude", "@anthropic-ai/claude-code", options)
	}()
	result.CodexPath, _ = c.checkCLI("codex", "@openai/codex", options)
	wg.Wait()
	return result
}

func (c *checker) checkCLI(name, npmPackage string, options Options) (path string, authFailed bool) {
	path, err := c.lookPath(name)
	if err != nil {
		suffix := ""
		if name == "codex" {
			suffix = "（GPT worker は Claude にフォールバックします）"
		}
		c.log("%s: PATH に見つかりません%s", name, suffix)
		return "", false
	}
	if options.Versions {
		c.checkVersion(name, path, npmPackage, options.Update)
	}
	if options.Login {
		if name == "claude" {
			return path, c.checkClaudeAuth(path)
		}
		c.checkCodexAuth(path)
	}
	return path, false
}

func (c *checker) checkVersion(name, path, npmPackage string, update bool) {
	installed, err := c.version(path, "--version")
	if err != nil {
		c.log("%s: インストール済みバージョンを取得できません: %v", name, err)
		return
	}
	npm, err := c.lookPath("npm")
	if err != nil {
		c.log("%s %s: npm が見つからないため最新版を確認できません", name, installed)
		return
	}
	latest, err := c.version(npm, "view", npmPackage, "version")
	if err != nil {
		c.log("%s %s: 最新版を確認できません: %v", name, installed, err)
		return
	}
	switch comparison := cli.CompareVersions(installed, latest); {
	case comparison == 0:
		c.log("%s %s (最新)", name, installed)
	case comparison > 0 || !update:
		c.log("%s %s (npm 最新版: %s)", name, installed, latest)
	default:
		c.log("%s %s → %s に更新します...", name, installed, latest)
		if err := c.execute(updateTimeout, path, []string{"update"}, c.stdout, c.stderr); err != nil {
			c.log("%s: 更新に失敗しました (%s): %v", name, installed, err)
			return
		}
		current, err := c.version(path, "--version")
		switch {
		case err != nil:
			c.log("%s: 更新後のバージョンを取得できません: %v", name, err)
		case cli.CompareVersions(current, installed) <= 0:
			c.log("%s %s: 更新後もバージョンが変わっていません (npm 最新版: %s)", name, current, latest)
		default:
			c.log("%s %s に更新しました", name, current)
		}
	}
}

func (c *checker) checkClaudeAuth(path string) bool {
	var out bytes.Buffer
	if err := c.execute(probeTimeout, path, []string{"auth", "status"}, &out, nil); err != nil {
		c.log("claude: ログイン状態を確認できません: %v", err)
		return false
	}
	auth, err := parseClaudeAuth(out.Bytes())
	if err != nil {
		c.log("claude: ログイン状態を読み取れません: %v", err)
		return false
	}
	now := time.Now()
	if !auth.loggedIn || (auth.expiresAt != nil && !auth.expiresAt.After(now)) {
		c.log("claude: ログインが切れています。`claude auth login` を実行してください")
		return true
	}
	if auth.expiresAt != nil && auth.expiresAt.Sub(now) <= 72*time.Hour {
		c.log("claude: ログインの有効期限が近づいています (%s)", auth.expiresAt.Local().Format("2006-01-02 15:04:05 MST"))
	} else {
		c.log("claude: ログイン済み")
	}
	return false
}

func (c *checker) checkCodexAuth(path string) {
	var out bytes.Buffer
	err := c.execute(probeTimeout, path, []string{"login", "status"}, &out, &out)
	if !CodexLoggedIn(out.String(), err) {
		c.log("codex: ログインしていません。`codex login` を実行してください（GPT worker は Claude にフォールバックします）")
		return
	}
	c.log("codex: ログイン済み")
}

func CodexLoggedIn(output string, err error) bool {
	return err == nil && !strings.Contains(strings.ToLower(output), "not logged in")
}

func (c *checker) version(path string, args ...string) (string, error) {
	var out bytes.Buffer
	if err := c.execute(probeTimeout, path, args, &out, nil); err != nil {
		return "", err
	}
	return parseVersion(out.String())
}

func (c *checker) execute(timeout time.Duration, path string, args []string, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := c.run(ctx, path, args, stdout, stderr)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (c *checker) log(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.stderr, "furu: "+format+"\n", args...)
}

func runCommand(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
	cmd, err := cli.Command(ctx, path, args...)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

var versionPattern = regexp.MustCompile(`\d+(\.\d+)+`)

func parseVersion(output string) (string, error) {
	version := versionPattern.FindString(output)
	if version == "" {
		return "", fmt.Errorf("バージョン番号が見つかりません")
	}
	return version, nil
}

type claudeAuth struct {
	loggedIn  bool
	expiresAt *time.Time
}

func parseClaudeAuth(output []byte) (claudeAuth, error) {
	var status struct {
		LoggedIn       *bool      `json:"loggedIn"`
		LoginExpiresAt *time.Time `json:"loginExpiresAt"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return claudeAuth{}, err
	}
	if status.LoggedIn == nil {
		return claudeAuth{}, fmt.Errorf("loggedIn が見つかりません")
	}
	return claudeAuth{loggedIn: *status.LoggedIn, expiresAt: status.LoginExpiresAt}, nil
}
