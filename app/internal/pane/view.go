package pane

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/javiermolinar/hush/sdk"
)

// ViewLaunch builds the same attach-only argv for direct and model pane actions.
// The executable and backend attachment must come from the interactive client.
func ViewLaunch(exe string, attachment sdk.PaneAttachment, view sdk.PaneLaunch) (Launch, error) {
	if !filepath.IsAbs(exe) || !filepath.IsAbs(attachment.Socket) || attachment.Epoch == "" {
		return Launch{}, sdk.ErrProtocol
	}
	var args []string
	switch view.Mode {
	case "hush":
		args = []string{"--local"}
	case "thread":
		if !view.ThreadID.Valid() {
			return Launch{}, sdk.ErrProtocol
		}
		args = []string{"--local", "--thread", string(view.ThreadID)}
	case "review":
		if !view.ThreadID.Valid() || len(view.Handle) != 32 || strings.Trim(view.Handle, "0123456789abcdef") != "" {
			return Launch{}, sdk.ErrProtocol
		}
		args = []string{"review", "transcript", view.Handle, "--thread", string(view.ThreadID)}
	default:
		return Launch{}, sdk.ErrProtocol
	}
	return Launch{Mode: LaunchArgv, Program: exe, Args: append(args, "--socket", attachment.Socket, "--pane-epoch", attachment.Epoch)}, nil
}

// Start a local interactive shell without executing model-supplied text. Use
// argv so even adapters without a native cwd parameter honor the pinned cwd.
func terminalLaunch() Launch {
	shell := os.Getenv("SHELL")
	if !filepath.IsAbs(shell) {
		shell = "/bin/sh"
	} else if _, err := exec.LookPath(shell); err != nil {
		shell = "/bin/sh"
	}
	return Launch{Mode: LaunchArgv, Program: shell, Args: []string{"-i"}}
}
