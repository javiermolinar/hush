package headless

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/javiermolinar/hush/app/internal/backendstart"
)

// Store operations select the default local data directory. They never
// connect to a peer or start a backend listener, even to inspect compatibility.
func runStore(ctx context.Context, o options, out, stderr *os.File) (result error) {
	profile, err := backendstart.SelectProfile(o.ui.Profile, false)
	if err != nil || profile != backendstart.Hush || os.Getenv("HUSH_SOCKET") != "" {
		return errors.New("store operations require the managed local Hush profile; unset HUSH_SOCKET")
	}
	node, entry, cleanup, err := backendstart.LocalInvocation(ctx, o.bundle)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, cleanup()) }()
	args := []string{entry, "store", o.storeAction}
	if o.storeFrom != "" {
		args = append(args, "--from", o.storeFrom)
	}
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Env = append(backendstart.RuntimeEnv(backendstart.Hush), "HUSH_PROFILE=hush")
	cmd.Stdout, cmd.Stderr = out, stderr
	return cmd.Run()
}
