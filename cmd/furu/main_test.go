package main

import (
	"slices"
	"testing"
)

func TestLaunchArgs(t *testing.T) {
	input := []string{"--resume", "session", "--no-update", "-c", "--", "--no-update"}
	want := []string{"--resume", "session", "-c", "--", "--no-update"}
	if got, noUpdate := launchArgs(input); !noUpdate || !slices.Equal(got, want) {
		t.Fatalf("launchArgs = %q, %v, want %q, true", got, noUpdate, want)
	}
	input = []string{"--", "--no-update"}
	if got, noUpdate := launchArgs(input); noUpdate || !slices.Equal(got, input) {
		t.Fatalf("launchArgs = %q, %v, want %q, false", got, noUpdate, input)
	}
}
