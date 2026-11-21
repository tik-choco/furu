package main

import (
	"slices"
	"testing"
)

func TestLaunchArgs(t *testing.T) {
	for _, tc := range []struct {
		name             string
		input, want      []string
		noUpdate, noPull bool
	}{
		{
			name: "no update", noUpdate: true,
			input: []string{"--resume", "session", "--no-update", "-c", "--", "--no-update"},
			want:  []string{"--resume", "session", "-c", "--", "--no-update"},
		},
		{
			name: "no pull", noPull: true,
			input: []string{"--resume", "session", "--no-pull", "-c", "--", "--no-pull"},
			want:  []string{"--resume", "session", "-c", "--", "--no-pull"},
		},
		{
			name: "both flags before separator", noUpdate: true, noPull: true,
			input: []string{"--no-update", "--no-pull", "-c"},
			want:  []string{"-c"},
		},
		{
			name:  "both flags after separator",
			input: []string{"--", "--no-update", "--no-pull"},
			want:  []string{"--", "--no-update", "--no-pull"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, noUpdate, noPull := launchArgs(tc.input)
			if !slices.Equal(got, tc.want) || noUpdate != tc.noUpdate || noPull != tc.noPull {
				t.Fatalf("launchArgs = %q, %v, %v, want %q, %v, %v", got, noUpdate, noPull, tc.want, tc.noUpdate, tc.noPull)
			}
		})
	}
}
