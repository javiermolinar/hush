package backendstart

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/javiermolinar/hush/sdk"
)

// Stop asks the selected backend to shut down without starting a missing one.
// It waits until the original server epoch is gone, so a subsequent managed
// command cannot accidentally attach to the old process. The boolean is false
// when no listener was present.
func Stop(ctx context.Context, opts Options) (bool, error) {
	opts.AttachOnly = true
	t, err := resolveTarget(opts)
	if err != nil {
		return false, err
	}
	bounded, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	options := t.clientOptions(sdk.ClientOptions{Capabilities: map[string][]uint64{"runtime": {sdk.RuntimeVersion}}})
	client := sdk.NewClient(t.socket, options)
	defer client.Close()
	first, err := client.Hello(bounded)
	if unavailable(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := client.StopRuntime(bounded); err != nil {
		var transport *sdk.TransportError
		if !errors.As(err, &transport) || bounded.Err() != nil {
			return false, err
		}
		// The listener may already be closing, or its acknowledgement may be
		// lost. Confirm the old epoch instead of replaying stop against a
		// possible replacement.
	}
	client.Close()

	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		probe := sdk.NewClient(t.socket, t.clientOptions(sdk.ClientOptions{}))
		next, probeErr := probe.Hello(bounded)
		probe.Close()
		switch {
		case probeErr == nil && next.Epoch != first.Epoch:
			return true, nil
		case unavailable(probeErr):
			return true, nil
		case probeErr != nil:
			var transport *sdk.TransportError
			if !errors.As(probeErr, &transport) {
				return false, probeErr
			}
			// A listener can refuse or cut a probe while shutdown is underway.
		}
		select {
		case <-tick.C:
		case <-bounded.Done():
			return false, fmt.Errorf("backend at %s did not stop: %w", t.socket, bounded.Err())
		}
	}
}
