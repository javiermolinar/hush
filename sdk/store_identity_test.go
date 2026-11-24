package sdk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
)

const fixtureStoreID = "50585983-c167-459d-9c4b-492d3da95284"
const fixtureGeneration = "50585983-c167-459d-9c4b-492d3da95285"

var fixtureStore = map[string]string{"storeId": fixtureStoreID, "generation": fixtureGeneration}

func storeGreeting(t *testing.T, c net.Conn, r *bufio.Reader, epoch string, id any, extra map[string]uint64) bool {
	t.Helper()
	q, err := readReq(r)
	if err != nil || q.Method != "hello" {
		return false
	}
	caps, _ := q.Params["capabilities"].(map[string]any)
	versions, _ := caps[storeIdentityCap].([]any)
	if len(versions) != 1 || versions[0] != float64(1) {
		t.Error("store identity was not offered")
		return false
	}
	selected := map[string]uint64{storeIdentityCap: 1}
	for k, v := range extra {
		selected[k] = v
	}
	reply(c, q, Hello{Version: 1, Epoch: epoch, Capabilities: selected})
	q, err = readReq(r)
	if err != nil {
		return false
	}
	if q.Method != storeIdentityGet || q.Params == nil || len(q.Params) != 0 {
		t.Errorf("command before store verification: %+v", q)
		return false
	}
	reply(c, q, id)
	return true
}

func TestConcurrentFirstConnectionsShareStorePin(t *testing.T) {
	ctx, path := runContext(t)
	var peers atomic.Int32
	commandAtIdentity, watchAtIdentity, releaseWatch := make(chan struct{}), make(chan struct{}), make(chan struct{})
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		first := peers.Add(1) == 1
		q, err := readReq(r)
		if err != nil || q.Method != "hello" {
			return
		}
		epoch := "boot-two"
		if first {
			epoch = "boot-one"
		}
		reply(c, q, Hello{Version: 1, Epoch: epoch, Capabilities: map[string]uint64{storeIdentityCap: 1}})
		q, err = readReq(r)
		if err != nil || q.Method != storeIdentityGet {
			t.Errorf("expected identity request, got %+v %v", q, err)
			return
		}
		if first {
			close(commandAtIdentity)
			select {
			case <-watchAtIdentity:
			case <-ctx.Done():
				return
			}
			reply(c, q, fixtureStore)
		} else {
			close(watchAtIdentity)
			select {
			case <-releaseWatch:
			case <-ctx.Done():
				return
			}
			reply(c, q, map[string]string{"storeId": fixtureStoreID, "generation": "50585983-c167-459d-9c4b-492d3da95286"})
		}
		if q, err := readReq(r); err == nil {
			t.Errorf("sent %s after attachment was invalidated", q.Method)
		}
	})
	client := NewClient(path, ClientOptions{})
	defer client.Close()
	hello := make(chan error, 1)
	go func() { _, err := client.Hello(ctx); hello <- err }()
	select {
	case <-commandAtIdentity:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	watch := client.Watch(ctx, "1", Window{})
	select {
	case <-watchAtIdentity:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-hello; err != nil {
		t.Fatal(err)
	}
	close(releaseWatch)
	if u := <-watch; !errors.Is(u.Err, ErrStoreChanged) {
		t.Fatalf("watch reached another generation: %+v", u)
	}
	if u, ok := <-watch; ok {
		t.Fatalf("terminal watch published another update: %+v", u)
	}
	if _, err := client.History(ctx, "1", Before(nil), 1); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("old cached command session remained usable: %v", err)
	}
	if err := client.WaitIdle(ctx, "1"); !errors.Is(err, ErrStoreChanged) || peers.Load() != 2 {
		t.Fatalf("invalidated client: %v (%d peers)", err, peers.Load())
	}
}

func TestRequiredHushProfileRejectsListenerWithoutStoreIdentity(t *testing.T) {
	ctx, path := runContext(t)
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		q, err := readReq(r)
		if err != nil || q.Method != "hello" {
			return
		}
		reply(c, q, Hello{Version: 1, Epoch: "legacy-hush", Capabilities: map[string]uint64{"hushProfile": 1}})
		if q, err := readReq(r); err == nil {
			t.Errorf("managed client sent %s without a store identity", q.Method)
		}
	})
	client := NewClient(path, ClientOptions{RequiredCapabilities: map[string]uint64{"hushProfile": 1}})
	defer client.Close()
	_, err := client.Hello(ctx)
	var server *ServerError
	if !errors.As(err, &server) || server.Code != "unsupported_capability" || server.Capability != storeIdentityCap {
		t.Fatalf("managed profile did not require store identity: %v", err)
	}
}

func TestLegacyAndDowngradedListenersCannotRetryAcrossBoots(t *testing.T) {
	for _, mode := range []string{"legacy", "lost-capability"} {
		t.Run(mode, func(t *testing.T) {
			ctx, path := runContext(t)
			var peers, submits atomic.Int32
			listener(t, path, func(c net.Conn, r *bufio.Reader) {
				first := peers.Add(1) == 1
				if first && mode == "lost-capability" {
					if !storeGreeting(t, c, r, "boot-one", fixtureStore, nil) {
						return
					}
				} else {
					epoch := "boot-one"
					if mode == "legacy" && !first {
						epoch = "boot-two"
					}
					if !greeting(c, r, epoch) {
						return
					}
				}
				if q, err := readReq(r); err == nil && q.Method == "submit" {
					submits.Add(1) // admitted without a reply; second host must never see it
				}
			})
			client := NewClient(path, ClientOptions{})
			defer client.Close()
			_, err := client.Submit(ctx, Input{ThreadID: "1", RequestID: "lost", Text: "hello"})
			var tx *TransportError
			if !errors.Is(err, ErrStoreChanged) || !errors.As(err, &tx) || !tx.Indeterminate || submits.Load() != 1 || peers.Load() != 2 {
				t.Fatalf("submit=%v, peers=%d, submits=%d", err, peers.Load(), submits.Load())
			}
		})
	}
}

func TestStoreHandshakeProtocolFailuresDoNotRetry(t *testing.T) {
	for _, mode := range []string{"extra-field", "wrong-type", "unsolicited", "unnegotiated"} {
		t.Run(mode, func(t *testing.T) {
			ctx, path := runContext(t)
			var peers atomic.Int32
			listener(t, path, func(c net.Conn, r *bufio.Reader) {
				peers.Add(1)
				q, err := readReq(r)
				if err != nil || q.Method != "hello" {
					return
				}
				if mode == "unnegotiated" {
					// No hello has completed yet and this ID is for hello.
					q.Method = storeIdentityGet
					reply(c, q, fixtureStore)
					return
				}
				reply(c, q, Hello{Version: 1, Epoch: "one", Capabilities: map[string]uint64{storeIdentityCap: 1}})
				q, err = readReq(r)
				if err != nil || q.Method != storeIdentityGet {
					t.Errorf("expected store request: %+v %v", q, err)
					return
				}
				switch mode {
				case "extra-field":
					reply(c, q, map[string]any{"storeId": fixtureStoreID, "generation": fixtureGeneration, "extra": true})
				case "wrong-type":
					reply(c, q, map[string]any{"storeId": fixtureStoreID, "generation": 17})
				case "unsolicited":
					q.ID = "1" // the already completed hello
					reply(c, q, fixtureStore)
				}
				if q, err := readReq(r); err == nil {
					t.Errorf("sent command after invalid identity response: %+v", q)
				}
			})
			client := NewClient(path, ClientOptions{})
			defer client.Close()
			_, err := client.Submit(ctx, Input{ThreadID: "1", RequestID: "key", Text: "hello"})
			if !errors.Is(err, ErrProtocol) || peers.Load() != 1 {
				t.Fatalf("protocol failure was not terminal: %v (%d peers)", err, peers.Load())
			}
		})
	}
}

func TestUnsolicitedIdentityAfterAdmissionDoesNotReplay(t *testing.T) {
	ctx, path := runContext(t)
	var submits, peers atomic.Int32
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		peers.Add(1)
		if !storeGreeting(t, c, r, "boot", fixtureStore, nil) {
			return
		}
		if q, err := readReq(r); err == nil && q.Method == "submit" {
			submits.Add(1)
			reply(c, wireReq{ID: "2", Method: storeIdentityGet}, fixtureStore)
		}
	})
	client := NewClient(path, ClientOptions{})
	defer client.Close()
	_, err := client.Submit(ctx, Input{ThreadID: "1", RequestID: "key", Text: "hello"})
	var tx *TransportError
	if !errors.Is(err, ErrProtocol) || !errors.As(err, &tx) || !tx.Indeterminate || peers.Load() != 1 || submits.Load() != 1 {
		t.Fatalf("protocol error replayed an admission: %v, peers=%d submits=%d", err, peers.Load(), submits.Load())
	}
}

func TestCancelStoreHandshakeDoesNotPinOrRetry(t *testing.T) {
	ctx, path := runContext(t)
	atIdentity, disconnected := make(chan struct{}), make(chan error, 1)
	var peers atomic.Int32
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		if peers.Add(1) != 1 {
			if greeting(c, r, "already-restarted") {
				_, _ = readReq(r)
			}
			return
		}
		q, err := readReq(r)
		if err != nil || q.Method != "hello" {
			return
		}
		reply(c, q, Hello{Version: 1, Epoch: "old", Capabilities: map[string]uint64{storeIdentityCap: 1}})
		q, err = readReq(r)
		if err != nil || q.Method != storeIdentityGet {
			return
		}
		close(atIdentity)
		_, err = readReq(r)
		disconnected <- err
	})
	client := NewClient(path, ClientOptions{})
	defer client.Close()
	ask, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := client.Submit(ask, Input{ThreadID: "1", RequestID: "key", Text: "hello"})
		finished <- err
	}()
	select {
	case <-atIdentity:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) || peers.Load() != 1 {
		t.Fatalf("cancelled handshake: %v, peers=%d", err, peers.Load())
	}
	select {
	case err := <-disconnected:
		if !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := client.Hello(ctx); err != nil {
		t.Fatalf("unverified first connection pinned the client: %v", err)
	}
}

func TestLegacyOrderedPathDoesNotReconnectOrReopenAnotherBoot(t *testing.T) {
	ctx, path := runContext(t)
	var peers, opens atomic.Int32
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		n := peers.Add(1)
		q, err := readReq(r)
		if err != nil || q.Method != "hello" {
			return
		}
		epoch := "boot-one"
		if n > 1 {
			epoch = "boot-two"
		}
		reply(c, q, Hello{Version: 1, Epoch: epoch, Capabilities: map[string]uint64{"piEvents": 1}})
		if q, err = readReq(r); err == nil && q.Method == piOpen {
			opens.Add(1)
			reply(c, q, piReady("1", streamOne))
			event(c, "1", streamOne, 1, "agent_start")
		}
	})
	client := NewClient(path, ClientOptions{})
	defer client.Close()
	// The ordered open itself pins the client; a second open is explicit
	// and cannot carry that legacy attachment into the next boot.
	p, _, err := client.OpenPi(ctx, PiOptions{CWD: "/fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if row, err := p.Next(ctx); err != nil || row.Event == nil || row.Event.Seq != 1 {
		t.Fatalf("verified prefix: %+v %v", row, err)
	}
	if _, err := p.Next(ctx); err == nil || peers.Load() != 1 {
		t.Fatalf("ordered stream did not end on loss: %v (%d peers)", err, peers.Load())
	}
	if _, _, err := client.OpenPi(ctx, PiOptions{CWD: "/fixture"}); !errors.Is(err, ErrStoreChanged) || opens.Load() != 1 {
		t.Fatalf("ordered open crossed a legacy boot: %v (%d opens)", err, opens.Load())
	}
}
