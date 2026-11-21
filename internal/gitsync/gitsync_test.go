package gitsync

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSyncWorkTree(t *testing.T) {
	type step struct {
		command string
		output  string
		err     error
	}
	const (
		probe         = "rev-parse --is-inside-work-tree"
		fetch         = "fetch --quiet"
		head          = "symbolic-ref --short -q HEAD"
		local         = "show-ref --verify --quiet refs/heads/develop"
		remote        = "show-ref --verify --quiet refs/remotes/origin/develop"
		switchDevelop = "switch --no-overwrite-ignore develop"
		upstream      = "rev-parse --abbrev-ref --symbolic-full-name @{u}"
		count         = "rev-list --count HEAD..@{u}"
		merge         = "merge --ff-only --no-squash --no-autostash --no-overwrite-ignore @{u}"
		mainPrompt    = "furu: git: main ブランチにいます。develop に切り替えますか？ [Y/n]: "
	)
	workTree := step{command: probe, output: "true"}
	fetchOK := step{command: fetch}
	main := step{command: head, output: "main"}
	topic := step{command: head, output: "topic"}
	localDevelop := step{command: local}
	noLocalDevelop := step{command: local, err: gitExit(1)}
	noRemoteDevelop := step{command: remote, err: gitExit(1)}
	noUpstream := step{upstream, "fatal: no upstream configured for branch 'topic'", gitExit(128)}
	topicUpstream := step{command: upstream, output: "origin/topic"}

	for _, tc := range []struct {
		name        string
		lookupErr   error
		interactive bool
		input       string
		steps       []step
		want        string
	}{
		{
			name: "git not found", lookupErr: exec.ErrNotFound,
		},
		{
			name:  "not a repository",
			steps: []step{{probe, "fatal: not a git repository (or any of the parent directories): .git", gitExit(128)}},
		},
		{
			name:  "not a work tree",
			steps: []step{{command: probe, output: "false"}},
		},
		{
			name:  "detached HEAD",
			steps: []step{workTree, fetchOK, {command: head, err: gitExit(1)}},
		},
		{
			name: "yes switches to local develop and updates it", interactive: true, input: "y\n",
			steps: []step{workTree, fetchOK, main, localDevelop, {command: switchDevelop},
				{command: upstream, output: "origin/develop"}, {command: count, output: "2"}, {command: merge}},
			want: mainPrompt + "furu: git: develop を origin/develop に更新しました (2 commits)\n",
		},
		{
			name: "enter switches to remote develop on master", interactive: true, input: "\n",
			steps: []step{workTree, fetchOK, {command: head, output: "master"}, noLocalDevelop,
				{command: remote}, {command: switchDevelop}, {command: upstream, output: "origin/develop"}, {command: count, output: "0"}},
			want: "furu: git: master ブランチにいます。develop に切り替えますか？ [Y/n]: ",
		},
		{
			name: "no leaves main checked out", interactive: true, input: "n\n",
			steps: []step{workTree, fetchOK, main, localDevelop, {command: upstream, output: "origin/main"}, {command: count, output: "0"}},
			want:  mainPrompt,
		},
		{
			name: "EOF leaves main checked out", interactive: true, input: "y",
			steps: []step{workTree, fetchOK, main, localDevelop, {command: upstream, output: "origin/main"}, {command: count, output: "0"}},
			want:  mainPrompt,
		},
		{
			name: "no develop branch", interactive: true, input: "y\n",
			steps: []step{workTree, fetchOK, main, noLocalDevelop, noRemoteDevelop,
				{command: upstream, output: "origin/main"}, {command: count, output: "0"}},
		},
		{
			name: "non-interactive warning", input: "y\n",
			steps: []step{workTree, fetchOK, main, localDevelop, {command: upstream, output: "origin/main"}, {command: count, output: "0"}},
			want:  "furu: git: main ブランチにいます。develop があります\n",
		},
		{
			name: "switch failure warns and continues on main", interactive: true, input: "YES\r\n",
			steps: []step{workTree, fetchOK, main, localDevelop, {switchDevelop, "error: local changes would be overwritten", gitExit(1)},
				main, {command: upstream, output: "origin/main"}, {command: count, output: "0"}},
			want: mainPrompt + "furu: git: develop への切り替えに失敗しました: error: local changes would be overwritten\n",
		},
		{
			name:  "no upstream",
			steps: []step{workTree, fetchOK, topic, noUpstream},
		},
		{
			name:  "up to date",
			steps: []step{workTree, fetchOK, topic, topicUpstream, {command: count, output: "0"}},
		},
		{
			name:  "fast-forward success",
			steps: []step{workTree, fetchOK, topic, topicUpstream, {command: count, output: "3"}, {command: merge}},
			want:  "furu: git: topic を origin/topic に更新しました (3 commits)\n",
		},
		{
			name: "fast-forward failure",
			steps: []step{workTree, fetchOK, topic, topicUpstream, {command: count, output: "3"},
				{merge, "fatal: Not possible to fast-forward, aborting.", gitExit(128)}},
			want: "furu: git: topic の fast-forward に失敗しました: fatal: Not possible to fast-forward, aborting.\n",
		},
		{
			name: "failed fetch skips remote develop and upstream", interactive: true, input: "y\n",
			steps: []step{workTree, {fetch, "fatal: unable to access remote", gitExit(128)}, main, noLocalDevelop},
			want:  "furu: git: fetch に失敗しました: fatal: unable to access remote\n",
		},
		{
			name: "timed-out fetch still allows local switch", interactive: true, input: "Yes\n",
			steps: []step{workTree, {command: fetch, err: context.DeadlineExceeded}, main, localDevelop, {command: switchDevelop}},
			want:  "furu: git: fetch に失敗しました（タイムアウト）: context deadline exceeded\n" + mainPrompt,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const path = `C:\Program Files\Git\cmd\git.exe`
			var out strings.Builder
			in := strings.NewReader(tc.input)
			calls := 0
			syncWorkTree(in, &out, tc.interactive, func(name string) (string, error) {
				if name != "git" {
					t.Fatalf("lookup name = %q, want git", name)
				}
				return path, tc.lookupErr
			}, func(ctx context.Context, gotPath string, args ...string) (string, error) {
				if calls >= len(tc.steps) {
					t.Fatalf("unexpected command: %q", args)
				}
				want := tc.steps[calls]
				calls++
				if gotPath != path || !slices.Equal(args, strings.Fields(want.command)) {
					t.Fatalf("command = %q %q, want %q %s", gotPath, args, path, want.command)
				}
				if want.command == fetch {
					deadline, ok := ctx.Deadline()
					if remaining := time.Until(deadline); !ok || remaining > 20*time.Second || remaining < 19*time.Second {
						t.Errorf("fetch timeout remaining = %s, want about 20s", remaining)
					}
				}
				return want.output, want.err
			})
			if calls != len(tc.steps) {
				t.Errorf("ran %d commands, want %d", calls, len(tc.steps))
			}
			if got := out.String(); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
			if !tc.interactive && in.Len() != len(tc.input) {
				t.Error("read non-interactive stdin")
			}
		})
	}
}

type gitExit int

func (e gitExit) Error() string { return fmt.Sprintf("exit status %d", e) }
func (e gitExit) ExitCode() int { return int(e) }
