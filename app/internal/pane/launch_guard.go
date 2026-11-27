package pane

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/javiermolinar/hush/sdk"
	"golang.org/x/sys/unix"
)

// LaunchGuardArg is an internal subprocess entry point. It checks the cwd
// selected by the multiplexer, not a pathname that can be replaced later.
const LaunchGuardArg = "--hush-pane-exec"

func launchArgv(launch Launch) []string {
	if launch.Mode == LaunchShell {
		return []string{"/bin/sh", "-c", launch.Text}
	}
	return append([]string{launch.Program}, launch.Args...)
}

func guardedLaunch(req OpenRequest) (Launch, error) {
	if req.Directory == nil {
		return req.Launch, nil
	}
	exe, err := os.Executable()
	if err != nil || !filepath.IsAbs(exe) {
		return Launch{}, errNative
	}
	d := req.Directory
	args := append([]string{LaunchGuardArg, d.Device, d.Inode, d.Birth}, launchArgv(req.Launch)...)
	return Launch{Mode: LaunchArgv, Program: exe, Args: args}, nil
}

func RunLaunchGuard(args []string, stderr io.Writer) int {
	if len(args) < 4 {
		return 125
	}
	want := sdk.PaneFileIdentity{Device: args[0], Inode: args[1], Birth: args[2]}
	if err := checkDirectory(".", want); err != nil {
		fmt.Fprintln(stderr, "hush: pane working directory changed; command was not started")
		return 125
	}
	argv := args[3:]
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(stderr, "hush: pane command was not found")
		return 127
	}
	_ = unix.Exec(path, argv, os.Environ()) // returns only on error
	fmt.Fprintln(stderr, "hush: could not start the pane command")
	return 126
}
