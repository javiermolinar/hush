package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/javiermolinar/hush/sdk"
)

func detectClientEnvironment(ctx context.Context) sdk.ClientEnvironment {
	// Only an immediate, named shell is evidence of the invoking shell.
	// Wrappers, launchers and shell exec optimizations can leave it unknown.
	var parent string
	if ppid := os.Getppid(); ppid > 1 {
		ctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(ppid), "-o", "comm=")
		cmd.Env = []string{"LC_ALL=C"}
		if out, err := cmd.Output(); err == nil {
			parent = string(out)
		}
	}
	return clientEnvironment(runtime.GOOS, os.Getenv, parent)
}

func clientEnvironment(goos string, getenv func(string) string, parent string) sdk.ClientEnvironment {
	var result sdk.ClientEnvironment
	switch goos {
	case "darwin":
		result.OS = "macos"
	case "linux":
		result.OS = "linux"
	}
	program, term := strings.ToLower(getenv("TERM_PROGRAM")), getenv("TERM")
	switch program {
	case "apple_terminal":
		result.Terminal = "apple-terminal"
	case "iterm.app":
		result.Terminal = "iterm2"
	case "ghostty", "kitty", "wezterm", "alacritty", "foot", "xterm", "vscode", "warp":
		result.Terminal = program
	case "warpterminal":
		result.Terminal = "warp"
	default:
		switch term {
		case "xterm-ghostty":
			result.Terminal = "ghostty"
		case "xterm-kitty":
			result.Terminal = "kitty"
		case "alacritty", "foot", "foot-direct", "wezterm":
			result.Terminal = strings.TrimSuffix(term, "-direct")
		}
	}
	switch {
	case program == "tmux" || getenv("TMUX") != "" && (term == "screen" || strings.HasPrefix(term, "screen-") || strings.HasPrefix(term, "tmux-")):
		result.Multiplexer = "tmux"
	case program == "rex" || program == "zellij" || program == "cmux":
		result.Multiplexer = program
	default:
		// Environment markers can survive nesting. When they disagree and
		// the terminal did not identify the mux, don't guess which is active.
		var candidates []string
		if getenv("ZELLIJ") != "" {
			candidates = append(candidates, "zellij")
		}
		if getenv("CMUX_WORKSPACE_ID") != "" || getenv("CMUX_SURFACE_ID") != "" {
			candidates = append(candidates, "cmux")
		}
		if getenv("TMUX") != "" {
			candidates = append(candidates, "tmux")
		}
		if len(candidates) == 1 {
			result.Multiplexer = candidates[0]
		}
	}
	result.Shell = shellFact(strings.TrimSpace(parent))
	if result.Shell == "" {
		result.LoginShellHint = shellFact(getenv("SHELL"))
	}
	return result
}

func shellFact(observation string) string {
	name := strings.TrimPrefix(filepath.Base(observation), "-")
	switch name {
	case "bash", "zsh", "fish", "sh", "dash", "ash", "ksh", "csh", "tcsh", "nu", "pwsh":
		return name
	default:
		return ""
	}
}
