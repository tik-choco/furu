package commitcmd

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"testing"
	"time"
)

func TestResolveDetection(t *testing.T) {
	const guruHelp = "Usage: guru save\n  -n, --no-push\n  -y, --yes\n"
	for _, tc := range []struct {
		name      string
		lookupErr error
		output    string
		runErr    error
		want      Result
	}{
		{name: "not found", lookupErr: exec.ErrNotFound, want: Result{Source: SourceGit}},
		{name: "unrelated guru", output: "Run 'guru -help' for more information.", want: Result{Source: SourceGit}},
		{name: "missing --no-push", output: "-y, --yes", want: Result{Source: SourceGit}},
		{name: "missing --yes", output: "-n, --no-push", want: Result{Source: SourceGit}},
		{name: "commit wrapper", output: guruHelp, want: Result{Command: "guru s -y", Source: SourceAuto}},
		{name: "runner error", output: guruHelp, runErr: errors.New("exit status 1"), want: Result{Source: SourceGit}},
		{name: "timeout", output: guruHelp, runErr: context.DeadlineExceeded, want: Result{Source: SourceGit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const path = `C:\Program Files\tools\guru.cmd`
			start := time.Now()
			lookups, runs := 0, 0
			got := resolve("auto", func(name string) (string, error) {
				lookups++
				if name != "guru" {
					t.Errorf("lookup name = %q, want guru", name)
				}
				return path, tc.lookupErr
			}, func(ctx context.Context, gotPath string, args ...string) ([]byte, error) {
				runs++
				if gotPath != path || !slices.Equal(args, []string{"save", "--help"}) {
					t.Errorf("probe = %q %q, want %q save --help", gotPath, args, path)
				}
				deadline, ok := ctx.Deadline()
				if !ok || deadline.Before(start.Add(5*time.Second)) || deadline.After(time.Now().Add(5*time.Second)) {
					t.Errorf("probe deadline = %v, want 5 seconds after the probe starts", deadline)
				}
				return []byte(tc.output), tc.runErr
			})
			wantRuns := 1
			if tc.lookupErr != nil {
				wantRuns = 0
			}
			if lookups != 1 || runs != wantRuns || got != tc.want {
				t.Errorf("resolve = %+v, %d lookups, %d runs; want %+v, 1 lookup, %d runs", got, lookups, runs, tc.want, wantRuns)
			}
		})
	}
}

func TestResolveConfigValue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		want       Result
	}{
		{"empty", "", Result{Command: "guru s -y", Source: SourceAuto}},
		{"auto", "auto", Result{Command: "guru s -y", Source: SourceAuto}},
		{"git", "git", Result{Source: SourceGit}},
		{"explicit", "  my-wrapper -y  ", Result{Command: "  my-wrapper -y  ", Source: SourceConfig}},
		{"explicit guru", "guru s -y", Result{Command: "guru s -y", Source: SourceConfig}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookedUp := false
			got := resolve(tc.configured, func(string) (string, error) {
				lookedUp = true
				return "guru", nil
			}, func(context.Context, string, ...string) ([]byte, error) {
				return []byte("--no-push --yes"), nil
			})
			if got != tc.want || lookedUp != (tc.want.Source == SourceAuto) {
				t.Errorf("resolve(%q) = %+v, looked up guru = %v, want %+v", tc.configured, got, lookedUp, tc.want)
			}
		})
	}
}
