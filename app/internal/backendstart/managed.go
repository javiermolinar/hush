package backendstart

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/javiermolinar/hush/sdk"
)

const retryCooldown = 2 * time.Second

// NewClient selects one store without blocking on startup. Connections to the
// managed default can wait for a shared launch; explicit sockets are attach-only.
// Cancel life when the frontend exits. A started backend remains independent.
func NewClient(life context.Context, opts Options, clientOpts sdk.ClientOptions) (*sdk.Client, error) {
	t, err := resolveTarget(opts)
	if err != nil {
		return nil, err
	}
	if t.agent != "" {
		m := &managedSocket{life: life, start: func(ctx context.Context) error { return t.ensure(ctx, opts.Bundle) }}
		clientOpts.DialUnix = m.dial
		clientOpts = t.clientOptions(clientOpts)
		if t.profile == Hush && opts.RequireWorkTrust {
			clientOpts.RequiredCapabilities["workspaceWorkTrust"] = 1
		}
	}
	return sdk.NewClient(t.socket, clientOpts), nil
}

type startupAttempt struct {
	done       chan struct{}
	err        error // published by closing done
	retryAfter time.Time
}

type managedSocket struct {
	life  context.Context
	start func(context.Context) error

	mu      sync.Mutex
	attempt *startupAttempt // guarded by mu; retained briefly on failure
}

func (m *managedSocket) dial(ctx context.Context, path string) (net.Conn, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if errors.Is(err, syscall.ENOTSOCK) {
		return nil, fmt.Errorf("backend path is not a Unix socket: %s", path)
	}
	if err == nil || !unavailable(err) {
		return conn, err // never replace a listener or start after a permission/handshake error
	}
	if err := m.waitReady(ctx); err != nil {
		return nil, err
	}
	return dialer.DialContext(ctx, "unix", path)
}

func (m *managedSocket) waitReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.life.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	a := m.attempt
	if a == nil || !a.retryAfter.IsZero() && !time.Now().Before(a.retryAfter) {
		a = &startupAttempt{done: make(chan struct{})}
		m.attempt = a
		// A short probe or a thread switch can cancel its own wait, not the
		// shared boot on which the first prompt or another feed may depend.
		go m.boot(a)
	}
	m.mu.Unlock()
	select {
	case <-a.done:
		return a.err
	case <-ctx.Done():
		return ctx.Err()
	case <-m.life.Done():
		return m.life.Err()
	}
}

func (m *managedSocket) boot(a *startupAttempt) {
	err := m.start(m.life)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.attempt = nil
	} else {
		a.err = fmt.Errorf("start local runtime: %w", err)
		a.retryAfter = time.Now().Add(retryCooldown)
	}
	close(a.done)
}
