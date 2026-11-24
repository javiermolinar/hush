package headless

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/javiermolinar/hush/app/internal/backendstart"
	"golang.org/x/term"
)

// Local auth deliberately skips the managed daemon resolver and every socket path.
func runAuth(ctx context.Context, o options, in, out, stderr *os.File) (result error) {
	profile, err := backendstart.SelectProfile(o.ui.Profile, false)
	if err != nil || profile != backendstart.Hush || os.Getenv("HUSH_SOCKET") != "" {
		return errors.New("auth is local to the Hush backend host; unset HUSH_SOCKET and select the Hush profile")
	}
	if o.authAction == "login" {
		fd := int(in.Fd())
		state, err := term.GetState(fd)
		if err != nil || !term.IsTerminal(int(out.Fd())) || !term.IsTerminal(int(stderr.Fd())) {
			return errors.New("auth login requires an interactive terminal on the backend host")
		}
		defer term.Restore(fd, state)
	}
	node, entry, cleanup, err := backendstart.LocalInvocation(ctx, o.bundle)
	if err != nil {
		return err
	}
	defer func() {
		if err := cleanup(); err != nil {
			result = errors.Join(result, err)
		}
	}()
	cmd := exec.CommandContext(ctx, node, entry, "auth", o.authAction)
	cmd.Env = backendstart.RuntimeEnv(backendstart.Hush)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, stderr
	// Give the bundle time to abort the provider flow and restore tty mode.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Run(); err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) && (exited.ExitCode() == 1 || exited.ExitCode() == 130) {
			return exited
		}
		return errors.New("local auth command did not complete")
	}
	return nil
}
