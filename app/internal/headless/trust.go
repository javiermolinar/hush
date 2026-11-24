package headless

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/javiermolinar/hush/app/internal/backendstart"
)

// Workspace grants belong to the host operator, never a socket peer. Use
// the same bundle and private Hush profile as the managed runtime.
func runTrust(ctx context.Context, o options, out, stderr *os.File) (result error) {
	profile, err := backendstart.SelectProfile(o.ui.Profile, false)
	if err != nil || profile != backendstart.Hush || os.Getenv("HUSH_SOCKET") != "" {
		return errors.New("trust is local to the Hush backend host; unset HUSH_SOCKET and select the Hush profile")
	}
	node, entry, cleanup, err := backendstart.LocalInvocation(ctx, o.bundle)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, cleanup()) }()
	args := []string{entry, "trust", o.trustAction}
	if o.trustScope != "" {
		args = append(args, "--scope="+o.trustScope)
	}
	if o.create.CWD != "" {
		args = append(args, "--cwd="+o.create.CWD)
	}
	if o.json {
		args = append(args, "--json")
	}
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Env = backendstart.RuntimeEnv(backendstart.Hush)
	cmd.Stdout, cmd.Stderr = out, stderr
	return cmd.Run()
}
