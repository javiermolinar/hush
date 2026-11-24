package main

import (
	"testing"

	"github.com/javiermolinar/hush/sdk"
)

func TestClientEnvironmentHints(t *testing.T) {
	for _, tc := range []struct {
		name, os, parent string
		env              map[string]string
		want             sdk.ClientEnvironment
	}{
		{"rex invoking shell", "darwin", "/opt/homebrew/bin/fish\n",
			map[string]string{"TERM_PROGRAM": "rex", "SHELL": "/bin/bash"},
			sdk.ClientEnvironment{OS: "macos", Multiplexer: "rex", Shell: "fish"}},
		{"zellij login shell hint", "linux", "python3",
			map[string]string{"TERM_PROGRAM": "WezTerm", "ZELLIJ": "raw-session-id", "SHELL": "/home/subject/bin/zsh"},
			sdk.ClientEnvironment{OS: "linux", Terminal: "wezterm", Multiplexer: "zellij", LoginShellHint: "zsh"}},
		{"cmux", "darwin", "-zsh",
			map[string]string{"TERM_PROGRAM": "ghostty", "CMUX_SURFACE_ID": "raw-surface-id"},
			sdk.ClientEnvironment{OS: "macos", Terminal: "ghostty", Multiplexer: "cmux", Shell: "zsh"}},
		{"tmux inside rex", "linux", "bash",
			map[string]string{"TERM_PROGRAM": "rex", "TERM": "tmux-256color", "TMUX": "/raw/socket-path,123,1"},
			sdk.ClientEnvironment{OS: "linux", Multiplexer: "tmux", Shell: "bash"}},
		{"ambiguous and unknown", "unknown", "node",
			map[string]string{"TERM_PROGRAM": "raw-terminal-id", "ZELLIJ": "1", "TMUX": "/raw/socket-path", "SHELL": "/raw/unrecognized"},
			sdk.ClientEnvironment{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := clientEnvironment(tc.os, func(key string) string { return tc.env[key] }, tc.parent)
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
