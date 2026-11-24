package sdk

import (
	"context"
	"errors"
	"time"
)

type StateUpdate struct {
	Snapshot Snapshot
	Err      error
}

// Watch opens an independent connection so mutations and other watches are not
// held by this feed. It publishes latest snapshots, not an event log. On a lost
// feed it reconnects with bounded backoff and installs an entirely new snapshot.
// Cancel the previous Watch when switching threads; use Transcript separately.
func (c *Client) Watch(ctx context.Context, thread ID, window Window) <-chan StateUpdate {
	out := make(chan StateUpdate, 1)
	go c.watch(ctx, out, thread, window)
	return out
}

func latest(out chan StateUpdate, msg StateUpdate) {
	select {
	case <-out:
	default:
	}
	out <- msg
}

func (c *Client) watch(ctx context.Context, out chan StateUpdate, thread ID, window Window) {
	defer close(out)
	life, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.life, cancel)
	defer func() { stop(); cancel() }()
	if !thread.Valid() || window != (Window{}) &&
		(window.Min < 1 || window.Min > window.Max || window.Max > 100) {
		latest(out, StateUpdate{Err: ErrProtocol})
		return
	}
	for delay := 25 * time.Millisecond; life.Err() == nil; delay = min(delay*2, time.Second) {
		snapped, err := c.watchOnce(life, out, thread, window)
		if life.Err() != nil {
			return
		}
		latest(out, StateUpdate{Err: err})
		if !retryableTransport(err) && !errors.Is(err, ErrStale) {
			return
		}
		if snapped {
			delay = 25 * time.Millisecond
		}
		if !pause(life, c.life, delay) {
			return
		}
	}
}

func (c *Client) watchOnce(life context.Context, out chan StateUpdate, thread ID, window Window) (bool, error) {
	var r Replica // owned by the connection's read goroutine
	var installed bool
	buf := make(chan StateUpdate, 1)
	ctx, cancel := context.WithTimeout(life, 10*time.Second)
	defer cancel()
	s, err := c.dial(ctx, life, c.offered, func(frame map[string]any) error {
		if !installed {
			return ErrProtocol
		}
		changed, err := r.apply(frame)
		if err != nil {
			// Discard a broken replica and resnapshot, unlike a protocol
			// failure during hello or store verification.
			if errors.Is(err, ErrStale) {
				return err
			}
			return errors.Join(ErrStale, err)
		}
		if changed {
			got, _ := r.Current()
			latest(buf, StateUpdate{Snapshot: got})
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	defer s.fail(ErrClosed)
	p := struct {
		threadParam
		Window *Window `json:"window,omitempty"`
	}{threadParam: threadParam{thread}}
	if window != (Window{}) {
		p.Window = &window
	}
	_, err = s.call(ctx, "subscribe", p, func(v any) error {
		if err := r.install(v, s.hello.Epoch, thread); err != nil {
			return err
		}
		installed = true
		got, _ := r.Current()
		latest(buf, StateUpdate{Snapshot: got})
		return nil
	})
	cancel()
	if err != nil {
		return false, err
	}
	for {
		select {
		case msg := <-buf:
			latest(out, msg)
		case <-life.Done():
			return true, life.Err()
		case <-s.done:
			return true, s.lost(false)
		}
	}
}
