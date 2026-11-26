package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/javiermolinar/hush/app/internal/backendstart"
)

func TestReviewPaneOpensOnlyExplicitReadOnlyCommand(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	rex := filepath.Join(dir, "rex")
	if err := os.WriteFile(rex, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TEST_REX_ARGS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("REX_BLOCK", "")
	t.Setenv("TEST_REX_ARGS", log)
	socket := filepath.Join(dir, "fixture socket")
	target := backendstart.Options{Socket: socket}
	if reviewPaneOpener(t.Context(), target, "rex") != nil {
		t.Fatal("pane opener guessed an anchor without a Rex block")
	}
	t.Setenv("REX_BLOCK", "exact-block")
	if reviewPaneOpener(t.Context(), target, "tmux") != nil {
		t.Fatal("offered a Rex action inside another multiplexer")
	}
	open := reviewPaneOpener(t.Context(), target, "rex")
	if open == nil {
		t.Fatal("Rex pane hook unavailable")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("Rex ran before the explicit action")
	}
	handle := strings.Repeat("a", 32)
	if err := open(t.Context(), "7", handle); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--autostart=false", "split", "--block=exact-block", "--split=right", "--focus=false", "--keep-open",
		"--shell=none", "--label=Review transcript", "--", exe, "review", "transcript", handle, "--thread", "7", "--socket", socket}
	if !slices.Equal(args, want) {
		t.Fatalf("wrong pane invocation: %+v", args)
	}
}
