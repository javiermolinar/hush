package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestPaneControlSharedFixtures(t *testing.T) {
	raw, err := os.ReadFile("../protocol/v1/fixtures/pane-control.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Intent          [][2]json.RawMessage
		CapabilityCases []struct {
			Caps                              PaneCapabilities
			Placements, Launches, ThreadViews []string
			Native, Read                      bool
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, row := range fixture.Intent {
		var text string
		var expected bool
		if json.Unmarshal(row[0], &text) != nil || json.Unmarshal(row[1], &expected) != nil {
			t.Fatal("invalid intent fixture")
		}
		if PaneLaunchPrompt(text) != expected {
			t.Errorf("explicit pane intent for %q: want %t", text, expected)
		}
	}
	for _, row := range fixture.CapabilityCases {
		check := func(r PaneRequest, expected bool) {
			t.Helper()
			if row.Caps.supports(r) != expected {
				t.Errorf("caps %+v, request %+v: expected availability %t", row.Caps, r, expected)
			}
		}
		for _, placement := range []string{"right", "below", "left", "above", "tab"} {
			for _, mode := range []string{"argv", "shell", "terminal", "hush", "review"} {
				check(PaneRequest{Op: "open", Placement: placement, Launch: PaneLaunch{Mode: mode}},
					slices.Contains(row.Placements, placement) && slices.Contains(row.Launches, mode))
			}
		}
		for _, where := range []string{"tab", "split"} {
			check(PaneRequest{Op: "thread_open", Where: where}, slices.Contains(row.ThreadViews, where))
		}
		check(PaneRequest{Op: "list"}, row.Native)
		check(PaneRequest{Op: "focus"}, row.Native)
		check(PaneRequest{Op: "read"}, row.Read)
	}
}

type paneTestHandler struct {
	foreign bool
	verify  func(context.Context) error
	handle  func(context.Context, PaneRequest) PaneReply
}

func (*paneTestHandler) Capabilities() PaneCapabilities               { return PaneCapabilities{Tabs: true} }
func (h *paneTestHandler) LocalBackend(string, PaneFileIdentity) bool { return !h.foreign }
func (h *paneTestHandler) Verify(ctx context.Context) error {
	if h.verify != nil {
		return h.verify(ctx)
	}
	return nil
}
func (h *paneTestHandler) Handle(ctx context.Context, r PaneRequest) PaneReply {
	return h.handle(ctx, r)
}

func paneSDKHello(t *testing.T, c net.Conn, rd *bufio.Reader, selected bool) bool {
	t.Helper()
	q, err := readReq(rd)
	if err != nil || q.Method != "hello" {
		t.Errorf("want hello, got %v: %v", q, err)
		return false
	}
	if _, offered := q.Params["capabilities"].(map[string]any)[paneControlCap]; !offered {
		t.Error("command connection did not offer pane control")
	}
	caps := map[string]int{}
	if selected {
		caps[paneControlCap] = PaneControlVersion
	}
	reply(c, q, map[string]any{"version": 1, "epoch": "pane-fixture", "capabilities": caps})
	return true
}

func paneSDKBind(t *testing.T, c net.Conn, rd *bufio.Reader, route string) bool {
	t.Helper()
	q, err := readReq(rd)
	if err != nil || q.Method != paneBind {
		t.Errorf("want bind, got %v: %v", q, err)
		return false
	}
	if !reflect.DeepEqual(q.Params["capabilities"], map[string]any{"left": false, "above": false, "tabs": true, "read": false}) {
		t.Errorf("bad capabilities: %v", q.Params)
	}
	reply(c, q, map[string]any{"route": route, "socket": PaneFileIdentity{Device: "1", Inode: "2", Birth: "3"}})
	return true
}

func TestPaneControlSDKFallbackAndOptIn(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		selected, optIn, stale, foreign bool
		want                            string
	}{
		{"legacy", false, true, false, false, "submit"},
		{"ordinary-on-bound-peer", true, false, false, false, "submit"},
		{"stale-origin", true, true, true, false, "submit"},
		{"remote-backend", true, true, false, true, "submit"},
		{"interactive", true, true, false, false, paneSubmit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, path := runContext(t)
			h := &paneTestHandler{foreign: tc.foreign, verify: func(context.Context) error {
				if tc.stale {
					return errors.New("stale")
				}
				return nil
			}}
			listener(t, path, func(c net.Conn, rd *bufio.Reader) {
				if !paneSDKHello(t, c, rd, tc.selected) || tc.selected && !paneSDKBind(t, c, rd, "route-first") {
					return
				}
				if tc.stale || tc.foreign {
					q, err := readReq(rd)
					if err != nil || q.Method != paneUnbind {
						t.Errorf("want unbind, got %v: %v", q, err)
						return
					}
					reply(c, q, map[string]any{})
				}
				q, err := readReq(rd)
				if err != nil || q.Method != tc.want {
					t.Errorf("want %s, got %v: %v", tc.want, q, err)
					return
				}
				if tc.want == paneSubmit && q.Params["route"] != "route-first" {
					t.Errorf("did not bind submit: %v", q.Params)
				}
				reply(c, q, map[string]string{"submissionId": "7"})
			})
			client := NewClient(path, ClientOptions{NewPaneHandler: func() PaneHandler { return h }})
			defer client.Close()
			if got, err := client.Submit(ctx, Input{ThreadID: "1", RequestID: "input", Text: "fixture", PaneControl: tc.optIn}); err != nil || got.SubmissionID != "7" {
				t.Fatalf("submit = %+v, %v", got, err)
			}
		})
	}
}

func TestPaneControlSDKRunsOffReaderAndFencesDuplicate(t *testing.T) {
	ctx, path := runContext(t)
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	h := &paneTestHandler{handle: func(ctx context.Context, r PaneRequest) PaneReply {
		if calls.Add(1) == 1 {
			close(started)
		}
		if r.Op != "list" {
			t.Errorf("unexpected pane request: %+v", r)
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return PaneReply{Outcome: "confirmed", Panes: []string{"hp_ABCDEFGHIJKLMNOPQRSTUVWXYZ"}}
	}}
	acked, settled := make(chan struct{}), make(chan struct{})
	listener(t, path, func(c net.Conn, rd *bufio.Reader) {
		if !paneSDKHello(t, c, rd, true) || !paneSDKBind(t, c, rd, "route-first") {
			return
		}
		q, err := readReq(rd)
		if err != nil || q.Method != paneSubmit {
			t.Errorf("want submit, got %v: %v", q, err)
			return
		}
		reply(c, q, map[string]string{"submissionId": "7"})
		push := map[string]any{"type": "pane_call", "id": "one-effect", "deadlineMs": time.Now().Add(3 * time.Second).UnixMilli(), "request": map[string]string{"op": "list"}}
		encoder := json.NewEncoder(c)
		_ = encoder.Encode(push)
		_ = encoder.Encode(push)
		q, err = readReq(rd)
		if err != nil || q.Method != "thread.list" {
			t.Errorf("want concurrent read, got %v: %v", q, err)
			return
		}
		reply(c, q, map[string]any{"threads": []any{}, "nextCursor": nil})
		q, err = readReq(rd)
		if err != nil || q.Method != paneResult || q.Params["callId"] != "one-effect" {
			t.Errorf("want one pane result, got %v: %v", q, err)
			return
		}
		if !reflect.DeepEqual(q.Params["result"], map[string]any{"outcome": "confirmed", "panes": []any{"hp_ABCDEFGHIJKLMNOPQRSTUVWXYZ"}}) {
			t.Errorf("bad reply: %v", q.Params)
		}
		reply(c, q, map[string]any{})
		_ = encoder.Encode(push)
		close(acked)
		q, err = readReq(rd)
		if err != nil || q.Method != "thread.list" {
			t.Errorf("duplicate effect or lost peer: %v: %v", q, err)
			return
		}
		reply(c, q, map[string]any{"threads": []any{}, "nextCursor": nil})
		close(settled)
	})
	client := NewClient(path, ClientOptions{NewPaneHandler: func() PaneHandler { return h }})
	defer client.Close()
	if _, err := client.Submit(ctx, Input{ThreadID: "1", RequestID: "first", Text: "fixture", PaneControl: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := client.ListThreads(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-acked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := client.ListThreads(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("native dispatch count %d", got)
	}
}

func TestPaneControlSDKPinsAdmissionOnReconnect(t *testing.T) {
	ctx, path := runContext(t)
	var connections atomic.Int32
	listener(t, path, func(c net.Conn, rd *bufio.Reader) {
		n := connections.Add(1)
		current := "new-route"
		if n == 1 {
			current = "first-route"
		}
		if !paneSDKHello(t, c, rd, true) || !paneSDKBind(t, c, rd, current) {
			return
		}
		q, err := readReq(rd)
		if err != nil || q.Method != paneSubmit || q.Params["route"] != "first-route" {
			t.Errorf("retry changed route: %v: %v", q, err)
			return
		}
		if n == 2 {
			reply(c, q, map[string]string{"submissionId": "7"})
		}
	})
	client := NewClient(path, ClientOptions{NewPaneHandler: func() PaneHandler { return &paneTestHandler{} }})
	defer client.Close()
	if got, err := client.Submit(ctx, Input{ThreadID: "1", RequestID: "same-key", Text: "fixture", PaneControl: true}); err != nil || got.SubmissionID != "7" || connections.Load() != 2 {
		t.Fatalf("retry: %+v, %v", got, err)
	}
}

func TestPaneControlSDKStopsLocalCallOnDisconnect(t *testing.T) {
	ctx, path := runContext(t)
	started, stopped := make(chan struct{}), make(chan error, 1)
	var connections atomic.Int32
	listener(t, path, func(c net.Conn, rd *bufio.Reader) {
		connections.Add(1)
		if !paneSDKHello(t, c, rd, true) || !paneSDKBind(t, c, rd, "route-first") {
			return
		}
		q, err := readReq(rd)
		if err != nil || q.Method != paneSubmit {
			t.Errorf("want submit, got %v: %v", q, err)
			return
		}
		reply(c, q, map[string]string{"submissionId": "7"})
		_ = json.NewEncoder(c).Encode(map[string]any{"type": "pane_call", "id": "dispatch-before-loss",
			"deadlineMs": time.Now().Add(8 * time.Second).UnixMilli(), "request": map[string]any{
				"op": "open", "placement": "below", "dir": "/fixture", "directory": PaneFileIdentity{Device: "1", Inode: "2", Birth: "3"},
				"launch": map[string]any{"mode": "argv", "program": "example", "args": []string{}},
			}})
		select {
		case <-started: // drop the peer after the client began executing
		case <-ctx.Done():
			t.Error("client did not start the request")
		}
	})
	h := &paneTestHandler{handle: func(ctx context.Context, r PaneRequest) PaneReply {
		if r.Op != "open" || r.Launch.Program != "example" {
			t.Errorf("unexpected request: %+v", r)
		}
		close(started)
		<-ctx.Done()
		stopped <- ctx.Err()
		return PaneReply{Outcome: "unknown"}
	}}
	client := NewClient(path, ClientOptions{NewPaneHandler: func() PaneHandler { return h }})
	defer client.Close()
	if _, err := client.Submit(ctx, Input{ThreadID: "1", RequestID: "input", Text: "fixture", PaneControl: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) || connections.Load() != 1 {
			t.Fatalf("disconnected operation: %v, %d connections", err, connections.Load())
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestUnsupportedPaneCallNeverReachesCustomHandler(t *testing.T) {
	handle := "hp_ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	stamp := map[string]string{"device": "1", "inode": "2", "birth": "3"}
	for _, tc := range []struct {
		name    string
		request map[string]any
	}{
		{"read", map[string]any{"op": "read", "pane": handle, "lines": 1}},
		{"left", map[string]any{"op": "open", "placement": "left", "dir": "/fixture", "directory": stamp, "launch": map[string]any{"mode": "argv", "program": "example", "args": []any{}}}},
		{"view", map[string]any{"op": "open", "placement": "right", "dir": "/fixture", "directory": stamp, "launch": map[string]any{"mode": "terminal"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, path := runContext(t)
			var called atomic.Bool
			h := &paneTestHandler{handle: func(context.Context, PaneRequest) PaneReply {
				called.Store(true)
				return PaneReply{Outcome: "rejected"}
			}}
			listener(t, path, func(c net.Conn, rd *bufio.Reader) {
				if !paneSDKHello(t, c, rd, true) || !paneSDKBind(t, c, rd, "route-first") {
					return
				}
				q, err := readReq(rd)
				if err != nil || q.Method != "thread.list" {
					t.Errorf("want concurrent read: %v, %v", q, err)
					return
				}
				_ = json.NewEncoder(c).Encode(map[string]any{"type": "pane_call", "id": "unsupported", "deadlineMs": time.Now().Add(time.Second).UnixMilli(), "request": tc.request})
				reply(c, q, map[string]any{"threads": []any{}, "nextCursor": nil})
			})
			client := NewClient(path, ClientOptions{NewPaneHandler: func() PaneHandler { return h }})
			defer client.Close()
			if _, err := client.ListThreads(ctx, 4, ""); !errors.Is(err, ErrProtocol) || called.Load() {
				t.Fatalf("unsupported request accepted or dispatched: %v; called=%t", err, called.Load())
			}
		})
	}
}

func TestExplicitPanePromptDoesNotFallBackToLegacySubmit(t *testing.T) {
	ctx, path := runContext(t)
	complete := make(chan struct{})
	listener(t, path, func(c net.Conn, rd *bufio.Reader) {
		defer close(complete)
		if !paneSDKHello(t, c, rd, false) {
			return
		}
		if q, err := readReq(rd); err == nil {
			t.Errorf("explicit pane prompt reached a legacy handler as %s", q.Method)
		}
	})
	client := NewClient(path, ClientOptions{NewPaneHandler: func() PaneHandler { return &paneTestHandler{} }})
	defer client.Close()
	_, err := client.Submit(ctx, Input{ThreadID: "1", RequestID: "one", Text: "/pane run it on the right", PaneControl: true})
	if !errors.Is(err, ErrPaneUnavailable) {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-complete:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestPaneChildRejectsWrongBackendBeforeCreatingThread(t *testing.T) {
	ctx, path := runContext(t)
	complete := make(chan struct{})
	listener(t, path, func(c net.Conn, rd *bufio.Reader) {
		defer close(complete)
		if !greeting(c, rd, "other-backend-epoch") {
			return
		}
		if q, err := readReq(rd); err == nil {
			t.Errorf("replacement backend received %s", q.Method)
		}
	})
	client := NewClient(path, ClientOptions{ExpectedEpoch: "pinned-backend-epoch"})
	defer client.Close()
	if _, err := client.NewThread(ctx, NewThread{CWD: "/fixture"}); !errors.Is(err, ErrStoreChanged) {
		t.Fatal(err)
	}
	select {
	case <-complete:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestPaneViewWireRejectsExtraAuthority(t *testing.T) {
	request := map[string]any{"op": "open", "placement": "below", "dir": "/fixture",
		"directory": map[string]any{"device": "1", "inode": "2", "birth": "3"}}
	for _, launch := range []map[string]any{
		{"mode": "terminal"}, {"mode": "hush"}, {"mode": "review", "threadId": "7", "handle": "0123456789abcdef0123456789abcdef"},
	} {
		request["launch"] = launch
		got, err := paneRequest(request)
		if err != nil || got.Launch.Mode != launch["mode"] || got.Attachment != (PaneAttachment{}) {
			t.Fatalf("valid view: %+v, %v", got, err)
		}
		request["attachment"] = map[string]string{"socket": "/remote.sock", "epoch": "guessed"}
		if _, err := paneRequest(request); !errors.Is(err, ErrProtocol) {
			t.Fatal("accepted wire-supplied attachment", err)
		}
		delete(request, "attachment")
		launch["program"] = "/other"
		if _, err := paneRequest(request); !errors.Is(err, ErrProtocol) {
			t.Fatal("accepted extra launch fields", err)
		}
	}
}

type threadTabTestHandler struct{ paneTestHandler }

func (threadTabTestHandler) Capabilities() PaneCapabilities {
	return PaneCapabilities{ThreadTabs: true, NoPanes: true}
}

type nativeThreadPaneTestHandler struct{ paneTestHandler }

func (nativeThreadPaneTestHandler) Capabilities() PaneCapabilities {
	return PaneCapabilities{ThreadTabs: true, Views: true, Tabs: true}
}

func TestPaneProbeDowngradesToFreshTabRouteAndKeepsAttachment(t *testing.T) {
	ctx, path := runContext(t)
	var probes atomic.Int32
	var calls atomic.Int32
	h := &nativeThreadPaneTestHandler{paneTestHandler{verify: func(context.Context) error {
		if probes.Add(1) > 1 {
			return errors.New("native origin temporarily unavailable")
		}
		return nil
	}, handle: func(_ context.Context, r PaneRequest) PaneReply {
		calls.Add(1)
		if r.Op != "thread_open" || r.Where != "tab" || r.Attachment.Epoch != "route-fixture" {
			t.Errorf("unexpected tab request: %+v", r)
		}
		return PaneReply{Outcome: "confirmed", ThreadID: r.ThreadID, Where: "tab"}
	}}}
	verified := PaneFileIdentity{Device: "1", Inode: "2", Birth: "3"}
	first, acked := make(chan struct{}), make(chan struct{})
	listener(t, path, func(c net.Conn, rd *bufio.Reader) {
		read := func(method string) wireReq {
			t.Helper()
			q, err := readReq(rd)
			if err != nil || q.Method != method {
				t.Errorf("want %s, got %v: %v", method, q, err)
			}
			return q
		}
		q := read("hello")
		reply(c, q, map[string]any{"version": 1, "epoch": "route-fixture", "capabilities": map[string]int{paneControlCap: ThreadViewVersion}})
		q = read(paneBind)
		if q.Params["capabilities"].(map[string]any)["views"] != true {
			t.Error("initial probe is healthy, expected native views")
		}
		reply(c, q, map[string]any{"route": "old-native", "socket": verified})
		// Follow the bind with an ordinary request so no push races the read
		// of the binding response.
		reply(c, read("thread.list"), map[string]any{"threads": []any{}, "nextCursor": nil})
		push := func(id string) {
			_ = json.NewEncoder(c).Encode(map[string]any{"type": "pane_call", "id": id, "deadlineMs": time.Now().Add(5 * time.Second).UnixMilli(),
				"request": map[string]string{"op": "thread_open", "threadId": "9", "where": "tab"}})
		}
		push("old-effect")
		reply(c, read(paneResult), map[string]any{})
		close(first)
		reply(c, read(paneUnbind), map[string]any{})
		q = read(paneBind)
		if got := q.Params["capabilities"]; !reflect.DeepEqual(got, map[string]any{"left": false, "above": false, "tabs": false,
			"read": false, "noPanes": true, "threadTabs": true}) {
			t.Errorf("downgrade is not tab-only: %v", got)
		}
		reply(c, q, map[string]any{"route": "new-tabs", "socket": verified})
		q = read(paneSubmit)
		if q.Params["route"] != "new-tabs" {
			t.Errorf("stale route reused: %v", q)
		}
		reply(c, q, map[string]string{"submissionId": "7"})
		push("old-effect") // a delayed duplicate cannot get through a replacement binding
		push("tab-effect")
		q = read(paneResult)
		if q.Params["callId"] != "tab-effect" || !reflect.DeepEqual(q.Params["result"], map[string]any{"outcome": "confirmed", "threadId": "9", "where": "tab"}) {
			t.Errorf("tab not confirmed: %v", q)
		}
		reply(c, q, map[string]any{})
		close(acked)
		// The verified attachment remains available without consulting the mux.
		if q, err := readReq(rd); err == nil {
			t.Errorf("unexpected request after downgrade: %v", q)
		}
	})
	client := NewClient(path, ClientOptions{NewPaneHandler: func() PaneHandler { return h }})
	defer client.Close()
	if _, err := client.ListThreads(ctx, 4, ""); err != nil || !client.PaneCommandsAvailable() {
		t.Fatalf("expected native route: %v", err)
	}
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("initial tab request was not acknowledged")
	}
	before, err := client.PaneAttachment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{ThreadID: "1", RequestID: "input", Text: "Open thread 9 in a tab", PaneControl: true}
	if _, err := client.Submit(ctx, input); err != nil || client.PaneCommandsAvailable() {
		t.Fatalf("expected ordinary routed submit after downgrade: %v", err)
	}
	select {
	case <-acked:
	case <-ctx.Done():
		t.Fatal("tab dispatch was not acknowledged")
	}
	if after, err := client.PaneAttachment(ctx); err != nil || after != before || probes.Load() != 2 {
		t.Fatalf("attachment or mux retry changed: %+v %v (%d probes)", after, err, probes.Load())
	}
	if err := client.CheckPaneCommand(ctx); !errors.Is(err, ErrPaneUnavailable) || probes.Load() != 2 {
		t.Fatalf("tab-only connection accepted a native command: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected duplicate call across routes: %d", calls.Load())
	}
}

func TestThreadViewCancellationLeavesCommandConnectionUsable(t *testing.T) {
	ctx, path := runContext(t)
	started, canceled := make(chan struct{}), make(chan struct{})
	h := &threadTabTestHandler{paneTestHandler{handle: func(ctx context.Context, r PaneRequest) PaneReply {
		if r.Op != "thread_open" || r.ThreadID != "9" || r.Where != "tab" || r.Attachment.Epoch != "thread-view-fixture" {
			t.Errorf("unexpected view: %+v", r)
		}
		close(started)
		<-ctx.Done()
		close(canceled)
		return PaneReply{Outcome: "unknown"}
	}}}
	listener(t, path, func(c net.Conn, rd *bufio.Reader) {
		q, err := readReq(rd)
		if err != nil || q.Method != "hello" {
			t.Errorf("want hello: %v, %v", q, err)
			return
		}
		reply(c, q, map[string]any{"version": 1, "epoch": "thread-view-fixture", "capabilities": map[string]int{paneControlCap: ThreadViewVersion}})
		q, err = readReq(rd)
		if err != nil || q.Method != paneBind {
			t.Errorf("want view bind: %v, %v", q, err)
			return
		}
		if fields := q.Params["capabilities"].(map[string]any); fields["noPanes"] != true || fields["threadTabs"] != true {
			t.Errorf("wrong tab capabilities: %v", fields)
		}
		reply(c, q, map[string]any{"route": "tab-route", "socket": PaneFileIdentity{Device: "1", Inode: "2", Birth: "3"}})
		q, err = readReq(rd)
		if err != nil || q.Method != paneSubmit {
			t.Errorf("want routed submission: %v, %v", q, err)
			return
		}
		reply(c, q, map[string]string{"submissionId": "7"})
		wire := json.NewEncoder(c)
		_ = wire.Encode(map[string]any{"type": "pane_call", "id": "one-view", "deadlineMs": time.Now().Add(5 * time.Second).UnixMilli(),
			"request": map[string]string{"op": "thread_open", "threadId": "9", "where": "tab"}})
		select {
		case <-started:
		case <-ctx.Done():
			return
		}
		_ = wire.Encode(map[string]string{"type": "pane_cancel", "id": "one-view"})
		q, err = readReq(rd)
		if err != nil || q.Method != "submit" {
			t.Errorf("want plain submit after route canceled: %v, %v", q, err)
			return
		}
		reply(c, q, map[string]string{"submissionId": "8"})
	})
	client := NewClient(path, ClientOptions{NewPaneHandler: func() PaneHandler { return h }})
	defer client.Close()
	in := Input{ThreadID: "1", RequestID: "input", Text: "Open it as a tab.", PaneControl: true}
	if _, err := client.Submit(ctx, in); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("view not canceled")
	}
	in.RequestID, in.Text = "next", "Ordinary follow-up"
	if got, err := client.Submit(ctx, in); err != nil || got.SubmissionID != "8" {
		t.Fatal(got, err)
	}
}
