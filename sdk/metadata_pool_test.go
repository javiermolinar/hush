package sdk

import (
	"bufio"
	"context"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

const scopeMethod = workspaceFilesPrefix + "scope"

var metadataFixtureCaps = map[string]uint64{workspaceFilesCapability: WorkspaceFilesVersion}
var openScope = map[string]bool{"workspace": true, "home": false}

// The fixture counts complete client requests on real Unix connections. For
// the forwarded case, hold each write and its reply for one half-RTT.
type metadataTraffic struct {
	dials, requests atomic.Int32
	oneWay          time.Duration
}

type measuredMetadataConn struct {
	net.Conn
	traffic *metadataTraffic
}

func (m *metadataTraffic) dial(ctx context.Context, path string) (net.Conn, error) {
	m.dials.Add(1)
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	return measuredMetadataConn{c, m}, nil
}

func (m measuredMetadataConn) Write(p []byte) (int, error) {
	time.Sleep(m.traffic.oneWay)
	n, err := m.Conn.Write(p)
	if err == nil && n == len(p) {
		m.traffic.requests.Add(1)
	}
	return n, err
}

func (m measuredMetadataConn) Read(p []byte) (int, error) {
	n, err := m.Conn.Read(p)
	if n != 0 {
		time.Sleep(m.traffic.oneWay)
	}
	return n, err
}

func newPoolClient(path string, m *metadataTraffic) *Client {
	return NewClient(path, ClientOptions{DialUnix: m.dial,
		Capabilities: map[string][]uint64{workspaceFilesCapability: {WorkspaceFilesVersion}}})
}

func TestMetadataPoolRoundTrips(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		oneWay time.Duration
	}{{"local", 0}, {"rtt-50ms", 25 * time.Millisecond}} {
		t.Run(scenario.name, func(t *testing.T) {
			if testing.Short() && scenario.oneWay != 0 {
				t.Skip("skip simulated network timing in short mode")
			}
			ctx, path := runContext(t)
			listener(t, path, func(c net.Conn, r *bufio.Reader) {
				if !storeGreeting(t, c, r, "metadata-fixture", fixtureStore, metadataFixtureCaps) {
					return
				}
				for {
					q, err := readReq(r)
					if err != nil {
						return
					}
					if q.Method != scopeMethod || q.Params["threadId"] != "1" {
						t.Errorf("unexpected metadata request %+v", q)
						return
					}
					reply(c, q, openScope)
				}
			})
			var cold, warm []time.Duration
			for range 12 {
				traffic := &metadataTraffic{oneWay: scenario.oneWay}
				c := newPoolClient(path, traffic)
				defer c.Close()
				read := func() time.Duration {
					start := time.Now()
					v, err := c.WorkspaceFileScope(ctx, WorkspaceFileTarget{ThreadID: "1"})
					if err != nil || !v.Workspace || v.Home {
						t.Fatalf("scope: %+v %v", v, err)
					}
					return time.Since(start)
				}
				cold = append(cold, read())
				if d, n := traffic.dials.Load(), traffic.requests.Load(); d != 1 || n != 3 {
					t.Fatalf("cold scope dials=%d requests=%d, want 1 and 3", d, n)
				}
				warm = append(warm, read())
				if d, n := traffic.dials.Load(), traffic.requests.Load(); d != 1 || n != 4 {
					t.Fatalf("warm scope dials=%d requests=%d, want 1 and 4 total", d, n)
				}
				c.Close()
			}
			slices.Sort(cold)
			slices.Sort(warm)
			t.Logf("%d samples: cold p50 %s (3 round trips); warm p50 %s (1 round trip, 0 extra dials)",
				len(cold), cold[len(cold)/2], warm[len(warm)/2])
		})
	}
}

func awaitPoolSignal(t *testing.T, ctx context.Context, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestMetadataPoolCancelClosesAndRefills(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, path := runContext(t)
			entered, released, replacement := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var peers atomic.Int32
			listener(t, path, func(c net.Conn, r *bufio.Reader) {
				peer := peers.Add(1)
				if !storeGreeting(t, c, r, "metadata-fixture", fixtureStore, metadataFixtureCaps) {
					return
				}
				if peer == 2 {
					close(replacement)
				}
				for {
					q, err := readReq(r)
					if err != nil {
						return
					}
					if q.Method != scopeMethod {
						t.Errorf("unexpected metadata request: %+v", q)
						return
					}
					if q.Params["threadId"] == "9" {
						if peer != 1 {
							t.Error("abandoned request was replayed")
							return
						}
						close(entered)
						if _, err := readReq(r); err == nil {
							t.Error("abandoned peer was reused, not closed")
						}
						close(released)
						return
					}
					reply(c, q, openScope)
				}
			})
			traffic := &metadataTraffic{}
			c := newPoolClient(path, traffic)
			defer c.Close()
			good := WorkspaceFileTarget{ThreadID: "1"}
			if _, err := c.WorkspaceFileScope(ctx, good); err != nil {
				t.Fatal(err)
			}
			var call context.Context
			var cancel context.CancelFunc
			want := context.Canceled
			if mode == "timeout" {
				call, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
				want = context.DeadlineExceeded
			} else {
				call, cancel = context.WithCancel(ctx)
			}
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := c.WorkspaceFileScope(call, WorkspaceFileTarget{ThreadID: "9"})
				done <- err
			}()
			awaitPoolSignal(t, ctx, entered, "pending metadata call")
			if mode == "cancel" {
				cancel()
			}
			if err := <-done; !errors.Is(err, want) {
				t.Fatalf("abandoned metadata call: %v; want %v", err, want)
			}
			awaitPoolSignal(t, ctx, released, "EOF releasing server wait")
			awaitPoolSignal(t, ctx, replacement, "background negotiation before another call")
			for len(c.metadata.idle) == 0 && ctx.Err() == nil {
				time.Sleep(time.Millisecond)
			}
			if _, err := c.WorkspaceFileScope(ctx, good); err != nil || traffic.dials.Load() != 2 {
				t.Fatalf("replacement was not reused: %v, %d dials", err, traffic.dials.Load())
			}
		})
	}
}

func TestMetadataPoolBoundAndNativeWaitIsolation(t *testing.T) {
	ctx, path := runContext(t)
	entered, release := make(chan struct{}, metadataPoolSize), make(chan struct{})
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		if !storeGreeting(t, c, r, "metadata-fixture", fixtureStore, metadataFixtureCaps) {
			return
		}
		q, err := readReq(r)
		if err != nil {
			return
		}
		switch q.Method {
		case scopeMethod:
			select {
			case entered <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			reply(c, q, openScope)
		case "thread.wait":
			reply(c, q, struct{}{})
		case "submission.wait":
			placed, answer := ID("8"), ID("10")
			reply(c, q, Submission{ThreadID: "1", SubmissionID: "7", Status: "done", EntryID: &placed, AnswerEntryID: &answer})
		default:
			t.Errorf("unexpected method %q", q.Method)
		}
		if next, err := readReq(r); err == nil {
			t.Errorf("unexpected %q on %q connection", next.Method, q.Method)
		}
	})
	t.Cleanup(func() { close(release) }) // unblock fixture peers even on a failed assertion
	traffic := &metadataTraffic{}
	c := newPoolClient(path, traffic)
	defer c.Close()
	target := WorkspaceFileTarget{ThreadID: "1"}
	done := make(chan error, metadataPoolSize)
	for range metadataPoolSize {
		go func() { _, err := c.WorkspaceFileScope(ctx, target); done <- err }()
	}
	for range metadataPoolSize {
		awaitPoolSignal(t, ctx, entered, "exclusive metadata peer")
	}
	queued, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	if _, err := c.WorkspaceFileScope(queued, target); !errors.Is(err, context.DeadlineExceeded) || traffic.dials.Load() != metadataPoolSize {
		t.Fatalf("saturated pool opened an extra peer or didn't honor deadline: %v, %d dials", err, traffic.dials.Load())
	}
	if err := c.WaitIdle(ctx, "1"); err != nil {
		t.Fatalf("thread.wait shared blocked metadata peers: %v", err)
	}
	if _, err := c.WaitSubmission(ctx, "1", "7"); err != nil {
		t.Fatalf("submission.wait shared blocked metadata peers: %v", err)
	}
	if got := traffic.dials.Load(); got != metadataPoolSize+2 {
		t.Fatalf("dedicated waits: got %d total peers", got)
	}
	// Close before releasing the stalled fixture calls; in-flight reads must
	// fail instead of retaining the pool (or dialing replacements).
	c.Close()
	for range metadataPoolSize {
		if err := <-done; !errors.Is(err, ErrClosed) {
			t.Fatalf("closed pool released an active call with %v", err)
		}
	}
}

func TestMetadataPoolStoreReplacementFailsBusyAndIdlePeers(t *testing.T) {
	ctx, path := runContext(t)
	var replace atomic.Bool
	var peers atomic.Int32
	closed, entered := make(chan int32, 4), make(chan struct{})
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		peer := peers.Add(1)
		defer func() { closed <- peer }()
		if replace.Load() {
			next := map[string]string{"storeId": fixtureStoreID, "generation": "50585983-c167-459d-9c4b-492d3da95286"}
			if storeGreeting(t, c, r, "replacement-boot", next, metadataFixtureCaps) {
				if q, err := readReq(r); err == nil {
					t.Errorf("command crossed the store fence: %+v", q)
				}
			}
			return
		}
		if !storeGreeting(t, c, r, "original-boot", fixtureStore, metadataFixtureCaps) {
			return
		}
		q, err := readReq(r)
		if err != nil {
			return
		}
		if q.Method != scopeMethod || (peer == 1) != (q.Params["threadId"] == "9") {
			t.Errorf("unexpected metadata request %+v (peer %d)", q, peer)
			return
		}
		if peer == 1 {
			close(entered)
		} else {
			reply(c, q, openScope)
		}
		if extra, err := readReq(r); err == nil {
			t.Errorf("original peer sent a metadata request after replacement: %+v", extra)
		}
	})
	traffic := &metadataTraffic{}
	c := newPoolClient(path, traffic)
	defer c.Close()
	blocked := make(chan error, 1)
	go func() {
		_, err := c.WorkspaceFileScope(ctx, WorkspaceFileTarget{ThreadID: "9"})
		blocked <- err
	}()
	awaitPoolSignal(t, ctx, entered, "first pooled peer")
	target := WorkspaceFileTarget{ThreadID: "1"}
	if _, err := c.WorkspaceFileScope(ctx, target); err != nil {
		t.Fatal(err)
	}
	replace.Store(true)
	if err := c.WaitIdle(ctx, "1"); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("replacement handshake was accepted: %v", err)
	}
	if err := <-blocked; !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("active lease did not fail with store change: %v", err)
	}
	seen := make(map[int32]bool)
	for !seen[1] || !seen[2] {
		select {
		case n := <-closed:
			seen[n] = true
		case <-ctx.Done():
			t.Fatal("store change did not close both pooled peers")
		}
	}
	if _, err := c.WorkspaceFileScope(ctx, target); !errors.Is(err, ErrStoreChanged) || traffic.dials.Load() != 3 {
		t.Fatalf("pool reused or replaced a fenced session: %v (%d dials)", err, traffic.dials.Load())
	}
}
