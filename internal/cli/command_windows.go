package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func IsShim(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cmd", ".bat":
		return true
	default:
		return false
	}
}

func command(ctx context.Context, path string, args []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	systemDir := filepath.Join(os.Getenv("SystemRoot"), "System32")
	if IsShim(path) {
		line, err := shimLine(path, args)
		if err != nil {
			return nil, err
		}
		cmd = exec.CommandContext(ctx, filepath.Join(systemDir, "cmd.exe"))
		// Expand the command once so percent signs in paths are literal. Npm
		// shims forward the resulting argument list to the native CLI via %*.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CmdLine: `cmd.exe /d /v:off /s /c "%FURU_INTERNAL_SHIM_COMMAND%"`,
		}
		cmd.Env = append(os.Environ(), "FURU_INTERNAL_SHIM_COMMAND="+line)
	}
	cmd.Cancel = func() error {
		killCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		kill := exec.CommandContext(killCtx, filepath.Join(systemDir, "taskkill.exe"),
			"/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	return cmd, nil
}

func shimLine(path string, args []string) (string, error) {
	quoted := make([]string, 0, len(args)+1)
	for _, arg := range append([]string{path}, args...) {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return "", errors.New("invalid line break or NUL in Windows shim argument")
		}
		escaped := syscall.EscapeArg(arg)
		if strings.ContainsAny(arg, "&|<>^()") && escaped[0] != '"' {
			// The leading space makes EscapeArg add surrounding quotes.
			withSpace := syscall.EscapeArg(" " + arg)
			escaped = withSpace[:1] + withSpace[2:]
		}
		// cmd.exe counts every double quote, including a CRT-escaped one.
		// Refuse shell syntax that would be exposed between such quotes.
		inQuotes := false
		for _, ch := range escaped {
			switch ch {
			case '"':
				inQuotes = !inQuotes
			case '&', '|', '<', '>', '^', '(', ')', ' ', '\t':
				if !inQuotes {
					return "", fmt.Errorf("argument cannot be passed safely to Windows shim %s", filepath.Base(path))
				}
			}
		}
		if inQuotes {
			return "", fmt.Errorf("unbalanced quoting for Windows shim %s", filepath.Base(path))
		}
		quoted = append(quoted, escaped)
	}
	return strings.Join(quoted, " "), nil
}
