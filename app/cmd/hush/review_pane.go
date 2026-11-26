package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/javiermolinar/hush/app/internal/backendstart"
	"github.com/javiermolinar/hush/sdk"
)

// A Rex block is optional and only opened by the explicit transcript action.
// Pin the same socket and anchor block. Never start a Rex or Hush service on
// behalf of the pane, and do not send commands through an interactive shell.
func reviewPaneOpener(ctx context.Context, target backendstart.Options, multiplexer string) func(context.Context, sdk.ID, string) error {
	block := os.Getenv("REX_BLOCK")
	if block == "" || multiplexer != "" && multiplexer != "rex" {
		return nil
	}
	rex, err := exec.LookPath("rex")
	if err != nil {
		dir := os.Getenv("REX_BIN_DIR")
		if !filepath.IsAbs(dir) {
			return nil
		}
		if rex, err = exec.LookPath(filepath.Join(dir, "rex")); err != nil {
			return nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	target.AttachOnly = true
	socket, err := backendstart.Resolve(ctx, target)
	if err != nil {
		return nil
	}
	socket, err = filepath.Abs(socket)
	if err != nil {
		return nil
	}
	return func(ctx context.Context, parent sdk.ID, handle string) error {
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return exec.CommandContext(call, rex, reviewPaneArgs(block, exe, socket, parent, handle)...).Run()
	}
}

func reviewPaneArgs(block, exe, socket string, parent sdk.ID, handle string) []string {
	return []string{"--autostart=false", "split", "--block=" + block, "--split=right", "--focus=false", "--keep-open",
		"--shell=none", "--label=Review transcript", "--", exe, "review", "transcript", handle, "--thread", string(parent), "--socket", socket}
}
