package sdk

import (
	"context"
	"slices"
	"time"
)

const metadataPoolSize = 3
const metadataRedialTimeout = 5 * time.Second

// Each lease owns the whole peer, not just one capability. Calls for any
// capability may run concurrently on other leases without hitting a peer's
// pending limit. Connections are created lazily, and an idle one is preferred.
type metadataPool struct {
	leases chan struct{}
	idle   chan *session
}

func newMetadataPool() metadataPool {
	return metadataPool{make(chan struct{}, metadataPoolSize), make(chan *session, metadataPoolSize)}
}

func (c *Client) checkoutMetadata(ctx context.Context) (_ *session, err error) {
	p := c.metadata
	select {
	case p.leases <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.life.Done():
		return nil, ErrClosed
	case <-c.storeInvalid:
		return nil, ErrStoreChanged
	}
	defer func() {
		if err != nil {
			<-p.leases
		}
	}()
	if c.changedStore() {
		return nil, ErrStoreChanged
	}
	if c.life.Err() != nil {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for {
		select {
		case s := <-p.idle:
			select {
			case <-s.done:
				continue
			default:
				return s, nil
			}
		default:
			return c.dial(ctx, c.life, c.offered, nil)
		}
	}
}

func (c *Client) returnMetadata(s *session, reusable bool) {
	p := c.metadata
	if reusable {
		select {
		case <-s.done:
		default:
			p.idle <- s
			<-p.leases
			return
		}
	}

	// A canceled passive read must close the transport, so EOF releases
	// server-side work. Never return a connection with an abandoned request.
	s.fail(ErrClosed)
	if c.changedStore() || c.life.Err() != nil {
		<-p.leases
		return
	}

	// Give the replacement this lease until its handshake settles to keep the
	// pool bounded. Try once, independently of the canceled call; in case of
	// failure the next borrower will dial on demand. Nothing is replayed.
	go func() {
		defer func() { <-p.leases }()
		ctx, cancel := context.WithTimeout(c.life, metadataRedialTimeout)
		defer cancel()
		if restored, err := c.dial(ctx, c.life, c.offered, nil); err == nil {
			p.idle <- restored
		}
	}()
}

func metadataRead[T any](ctx context.Context, c *Client, method string, params any, capability string, versions ...uint64) (T, error) {
	out, _, err := versionedMetadataRead[T](ctx, c, method, params, capability, versions...)
	return out, err
}

// Version selection and store verification belong to the checked-out peer,
// not the client's command connection or a previous checkout.
func versionedMetadataRead[T any](ctx context.Context, c *Client, method string, params any, capability string, versions ...uint64) (T, uint64, error) {
	var zero T
	s, err := c.checkoutMetadata(ctx)
	if err != nil {
		return zero, 0, err
	}
	reuse := false
	defer func() { c.returnMetadata(s, reuse) }()
	version := s.hello.Capabilities[capability]
	if !slices.Contains(versions, version) {
		reuse = true // no request was sent
		return zero, 0, &ServerError{Code: "unsupported_capability", Message: "Capability was not negotiated", Capability: capability}
	}
	v, err := s.call(ctx, method, params, nil)
	var out T
	if err == nil {
		out, err = decode[T](v)
	}
	if c.changedStore() {
		return zero, version, ErrStoreChanged
	}
	if ctx.Err() != nil {
		return zero, version, ctx.Err()
	}
	if c.life.Err() != nil {
		return zero, version, ErrClosed
	}
	reuse = err == nil
	return out, version, err
}
