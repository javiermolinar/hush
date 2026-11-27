package sdk

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

const (
	StoreIdentityVersion = 1
	storeIdentityCap     = "storeIdentity"
	storeIdentityGet     = "ext.storeIdentity.get"
)

var storeUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type storeIdentity struct {
	id, generation string
}

func parseStoreIdentity(v any) (storeIdentity, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 {
		return storeIdentity{}, ErrProtocol
	}
	id, _ := m["storeId"].(string)
	gen, _ := m["generation"].(string)
	if !storeUUID.MatchString(id) || !storeUUID.MatchString(gen) {
		return storeIdentity{}, ErrProtocol
	}
	return storeIdentity{id, gen}, nil
}

type storeProof struct {
	identity storeIdentity
	epoch    string // nonempty only for a legacy listener
}

func (c *Client) changedStore() bool {
	select {
	case <-c.storeInvalid:
		return true
	default:
		return false
	}
}

// A single publication fence for every kind of session. Handshakes can run in
// parallel, but competing first connections cannot establish independent pins.
func (c *Client) verifyStore(ctx, lifetime context.Context, s *session, h Hello, proof storeProof) error {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	if c.changedStore() || c.epoch != "" && c.epoch != h.Epoch {
		return ErrStoreChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if lifetime.Err() != nil || c.life.Err() != nil {
		return ErrClosed
	}
	if c.storePin != (storeProof{}) && c.storePin != proof {
		close(c.storeInvalid) // also wake calls waiting for a pool lease
		for other := range c.verified {
			other.fail(ErrStoreChanged)
		}
		return ErrStoreChanged
	}
	for name, version := range c.required {
		if h.Capabilities[name] != version {
			return &ServerError{Code: "unsupported_capability", Message: "Listener does not support the required runtime profile or store identity", Capability: name}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cause != nil {
		return &TransportError{Err: s.cause}
	}
	if c.verified == nil {
		c.verified = make(map[*session]struct{})
	}
	c.storePin = proof
	c.verified[s] = struct{}{}
	s.verified = true
	return nil
}

func (c *Client) untrack(s *session) {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	delete(c.verified, s)
}

func retryableTransport(err error) bool {
	var transport *TransportError
	var server *ServerError
	return errors.As(err, &transport) && !errors.As(err, &server) &&
		!errors.Is(err, ErrStoreChanged) && !errors.Is(err, ErrStoreIncompatible) &&
		!errors.Is(err, ErrProtocol) && !errors.Is(err, ErrClosed) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func storeEnvelope(f map[string]any, method string, version uint64, success bool) error {
	if method != storeIdentityGet || version != StoreIdentityVersion {
		return fmt.Errorf("%w: store identity negotiation or method", ErrProtocol)
	}
	if !success {
		return schemaCheck("failureReply", f)
	}
	id, _ := f["id"].(string)
	if !token(id) || len(f) != 5 || !keys(f, "type", "id", "method", "ok", "result") {
		return fmt.Errorf("%w: store identity envelope", ErrProtocol)
	}
	_, err := parseStoreIdentity(f["result"])
	return err
}
