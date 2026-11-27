package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/javiermolinar/hush/app/internal/backendstart"
	"github.com/javiermolinar/hush/app/internal/filelist"
	"github.com/javiermolinar/hush/app/internal/headless"
	"github.com/javiermolinar/hush/app/internal/keyring"
	"github.com/javiermolinar/hush/app/internal/pane"
)

type signalExit struct{ syscall.Signal }

func (s signalExit) Error() string { return s.Signal.String() }

func main() {
	if len(os.Args) > 1 && os.Args[1] == pane.LaunchGuardArg {
		os.Exit(pane.RunLaunchGuard(os.Args[2:], os.Stderr))
	}
	if len(os.Args) == 2 && os.Args[1] == keyring.HelperArg {
		if !privatePipe(os.Stdin) || !privatePipe(os.Stdout) {
			os.Exit(2)
		}
		os.Exit(keyring.Run(os.Stdin, os.Stdout))
	}
	if len(os.Args) == 2 && (os.Args[1] == filelist.HelperArg || os.Args[1] == filelist.PlanFileArg) {
		if !privatePipe(os.Stdin) || !privatePipe(os.Stdout) {
			os.Exit(2)
		}
		if os.Args[1] == filelist.PlanFileArg {
			os.Exit(filelist.RunPlanFile(os.Stdin, os.Stdout))
		}
		os.Exit(filelist.Run(os.Stdin, os.Stdout))
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	go func() {
		select {
		case s := <-signals:
			cancel(signalExit{s.(syscall.Signal)})
		case <-ctx.Done():
		}
	}()
	if len(os.Args) > 1 && os.Args[1] == backendstart.ServiceRunnerArg {
		code := backendstart.RunSupervised(ctx, os.Args[2:], os.Stderr)
		cancel(nil)
		os.Exit(code) // A normal supervisor stop should not trigger restart.
	}
	code := headless.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, runInteractive)
	if sig, ok := context.Cause(ctx).(signalExit); ok {
		code = 128 + int(sig.Signal)
	}
	cancel(nil)
	os.Exit(code)
}

func privatePipe(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0
}
