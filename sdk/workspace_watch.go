package sdk

import (
	"context"
	"regexp"
	"slices"
	"time"
)

var workspaceRevision = regexp.MustCompile(`^[0-9a-f]{64}$`)

type workspaceWatch struct {
	method, capability string
	version            uint64
	base               string
	baseVersions       []uint64
	params             map[string]any
}

// Workspace watches hold a cancelable, verified connection of their own.
// On reconnect, resnapshot without sending a revision from the old peer.
func watchWorkspace[T any](ctx context.Context, c *Client, w workspaceWatch,
	decodeReport func(any) (T, string, error), failure func(error) T,
) <-chan T {
	out := make(chan T, 1)
	go func() {
		defer close(out)
		if w.params == nil {
			out <- failure(ErrProtocol)
			return
		}
		life, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(c.life, cancel)
		defer func() { stop(); cancel() }()
		publish := func(v T) {
			select {
			case <-out:
			default:
			}
			out <- v
		}
		for delay := 250 * time.Millisecond; life.Err() == nil; delay = min(delay*2, 4*time.Second) {
			ready, err := workspaceWatchConnection(life, c, w, decodeReport, publish)
			if life.Err() != nil {
				return
			}
			publish(failure(err))
			if !retryableTransport(err) && !IsCode(err, "limit") {
				return
			}
			if ready {
				delay = 250 * time.Millisecond
			}
			if !pause(life, c.life, delay) {
				return
			}
		}
	}()
	return out
}

func workspaceWatchConnection[T any](life context.Context, c *Client, w workspaceWatch,
	decodeReport func(any) (T, string, error), publish func(T),
) (ready bool, err error) {
	ctx, cancel := context.WithTimeout(life, 10*time.Second)
	defer cancel()
	s, err := c.dial(ctx, life, c.offered, nil)
	if err != nil {
		return false, err
	}
	defer s.fail(ErrClosed)
	caps := s.hello.Capabilities
	if caps[w.capability] != w.version || !slices.Contains(w.baseVersions, caps[w.base]) {
		return false, &ServerError{Code: "unsupported_capability", Capability: w.capability, Message: "Workspace watch was not negotiated for this target"}
	}
	delete(w.params, "after")
	var prior string
	for {
		raw, err := s.call(ctx, w.method, w.params, nil)
		if err != nil {
			return ready, err
		}
		v, rev, err := decodeReport(raw)
		if err != nil {
			return ready, err
		}
		if !workspaceRevision.MatchString(rev) || rev == prior {
			return ready, ErrProtocol
		}
		publish(v)
		cancel()
		ctx = life
		ready, prior, w.params["after"] = true, rev, rev
	}
}
