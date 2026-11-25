package backendstart

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var errRestartSupervised = errors.New("the Hush user service owns this backend; use 'hush service restart' or check 'hush service status'")

// Restart replaces the managed local backend, or starts it if absent. It returns
// whether an existing backend was stopped and waits for a compatible hello.
func Restart(ctx context.Context, opts Options) (bool, error) {
	t, err := resolveTarget(opts)
	if err != nil {
		return false, err
	}
	if t.agent == "" {
		return false, errors.New("restart requires a managed local backend; --socket and HUSH_SOCKET are attach-only")
	}
	if installed, err := serviceInstalled(t.agent); err != nil {
		return false, err
	} else if installed {
		return false, errRestartSupervised
	}
	// Check the replacement's bundle and Node before stopping a healthy backend.
	node, bundle, version, err := localRuntime(ctx, opts.Bundle)
	if err != nil {
		return false, err
	}
	stopped, err := t.stop(ctx)
	if err != nil {
		return false, fmt.Errorf("stop backend: %w", err)
	}
	startup, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	err = t.startReplacement(startup, node, bundle, version)
	if stopped && err != nil {
		err = fmt.Errorf("previous backend stopped; replacement not ready: %w", err)
	}
	return stopped, err
}

func (t target) startReplacement(ctx context.Context, node, bundle, version string) error {
	for {
		if needed, err := t.needsStart(ctx); err != nil || !needed {
			return err // Another managed caller may have started a replacement.
		}
		switch err := launch(ctx, node, bundle, version, t); {
		case err == nil:
			return t.waitHello(ctx)
		case errors.Is(err, errSupervised):
			return errRestartSupervised
		case !errors.Is(err, errContended):
			return err
		}
		// The old backend closes its listener before releasing the writer lock.
		// Check for a ready listener before retrying under the same deadline.
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return fmt.Errorf("waiting for the backend writer lock: %w", ctx.Err())
		}
	}
}
