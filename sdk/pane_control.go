package sdk

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	PaneControlVersion = 1
	ThreadViewVersion  = 2
	paneControlCap     = "paneControl"
	paneBind           = "ext.paneControl.bind"
	paneSubmit         = "ext.paneControl.submit"
	paneResult         = "ext.paneControl.result"
	paneUnbind         = "ext.paneControl.unbind"
	paneWorkspace      = "ext.paneControl.workspace"
	paneMaxCalls       = 4096
)

// PaneHandler is a client-local handler, bound to a verified interactive
// terminal. A new handler must use a fresh handle namespace. Verify and
// LocalBackend are read-only and may run concurrently with Handle. Handle
// calls are serialized.
type PaneHandler interface {
	Capabilities() PaneCapabilities
	LocalBackend(socket string, identity PaneFileIdentity) bool
	Verify(context.Context) error
	Handle(context.Context, PaneRequest) PaneReply
}

type PaneCapabilities struct {
	Left       bool `json:"left"`
	Above      bool `json:"above"`
	Tabs       bool `json:"tabs"`
	Read       bool `json:"read"`
	Views      bool `json:"views,omitempty"` // terminal and fresh Hush launches
	ThreadTabs bool `json:"threadTabs,omitempty"`
	NoPanes    bool `json:"noPanes,omitempty"` // tab-only route without a native adapter
}

// supports checks the negotiated matrix independently of wire-shape parsing
// and the handler. Custom handlers do not have to reimplement negotiation.
func (c PaneCapabilities) supports(r PaneRequest) bool {
	if r.Op == "thread_open" {
		// focus opens and switches to the thread; only a conversation move sends it.
		return c.ThreadTabs && (r.Where == "tab" || r.Where == "focus" || r.Where == "split" && !c.NoPanes && c.Views)
	}
	if c.NoPanes {
		return false
	}
	switch r.Op {
	case "list", "focus":
		return true
	case "read":
		return c.Read
	case "open":
		switch r.Launch.Mode {
		case "argv", "shell":
		case "terminal", "hush":
			if !c.Views {
				return false
			}
		default:
			return false
		}
		return r.Placement == "right" || r.Placement == "below" || r.Placement == "left" && c.Left ||
			r.Placement == "above" && c.Above || r.Placement == "tab" && c.Tabs
	}
	return false
}

func supportedPaneControl(version uint64) bool {
	return version == PaneControlVersion || version == ThreadViewVersion
}

var ErrPaneUnavailable = errors.New("pane control is unavailable on this connection")

// PaneLaunchPrompt recognizes an explicit command-pane input, not a shell command.
func PaneLaunchPrompt(text string) bool {
	tail, ok := strings.CutPrefix(text, "/pane")
	return ok && len(tail) != 0 && (tail[0] == ' ' || tail[0] == '\t') && strings.ContainsFunc(tail, func(r rune) bool {
		return !unicode.IsSpace(r) && r != '\uFEFF'
	})
}

// PaneFileIdentity is local filesystem evidence, not an authorization grant.
// Use decimal strings so full-width device and inode numbers survive JSON.
type PaneFileIdentity struct {
	Device string `json:"device"`
	Inode  string `json:"inode"`
	Birth  string `json:"birth"`
}

func (p PaneFileIdentity) valid() bool {
	for _, s := range []string{p.Device, p.Inode, p.Birth} {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != s {
			return false
		}
	}
	return p.Inode != "0"
}

type PaneDirectory struct {
	Dir       string           `json:"dir"`
	Directory PaneFileIdentity `json:"directory"`
}

func (w PaneDirectory) valid() bool {
	return filepath.IsAbs(w.Dir) && !strings.ContainsRune(w.Dir, 0) && w.Directory.valid()
}

// PaneAttachment identifies one already-connected backend. A child must
// attach only to Socket and verify Epoch before using any thread IDs.
type PaneAttachment struct{ Socket, Epoch string }

type localPaneBackend struct {
	handler    PaneHandler
	attachment PaneAttachment
	identity   PaneFileIdentity
}

func (b *localPaneBackend) verified() bool {
	return b != nil && b.handler.LocalBackend(b.attachment.Socket, b.identity)
}

// PaneCommandsAvailable reports observed capability state without connecting
// or probing. Effects still revalidate the exact origin at dispatch.
func (c *Client) PaneCommandsAvailable() bool {
	s := c.session.Load()
	if s == nil || c.life.Err() != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cause == nil && s.pane != nil && !s.pane.capabilities.NoPanes && s.pane.life.Err() == nil
}

func (c *Client) paneAttachment(ctx context.Context) (*session, PaneAttachment, error) {
	s, err := c.connect(ctx)
	if err != nil {
		return nil, PaneAttachment{}, err
	}
	s.mu.Lock()
	b := s.paneBackend
	s.mu.Unlock()
	if !b.verified() {
		return nil, PaneAttachment{}, ErrPaneUnavailable
	}
	return s, b.attachment, nil
}

// CheckPaneCommand verifies command-pane routing before a client allocates a
// new durable thread for an explicit /pane input. Submit revalidates again.
func (c *Client) CheckPaneCommand(ctx context.Context) error {
	s, err := c.connect(ctx)
	if err != nil {
		return err
	}
	if s.paneForInput(ctx, true) == "" {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrPaneUnavailable
	}
	return nil
}

func (c *Client) PaneAttachment(ctx context.Context) (PaneAttachment, error) {
	_, attach, err := c.paneAttachment(ctx)
	return attach, err
}

func (c *Client) PaneWorkspace(ctx context.Context, thread ID) (PaneAttachment, PaneDirectory, error) {
	if !thread.Valid() {
		return PaneAttachment{}, PaneDirectory{}, ErrProtocol
	}
	s, attach, err := c.paneAttachment(ctx)
	if err != nil {
		return PaneAttachment{}, PaneDirectory{}, err
	}
	if s.hello.Capabilities[paneControlCap] == PaneControlVersion {
		s.mu.Lock()
		live := s.pane != nil && s.pane.life.Err() == nil
		s.mu.Unlock()
		if !live {
			return PaneAttachment{}, PaneDirectory{}, ErrPaneUnavailable
		}
	}
	v, err := s.call(ctx, paneWorkspace, threadParam{thread}, nil)
	if err != nil {
		return PaneAttachment{}, PaneDirectory{}, err
	}
	workspace, err := decode[PaneDirectory](v)
	if err != nil || !workspace.valid() {
		return PaneAttachment{}, PaneDirectory{}, ErrProtocol
	}
	return attach, workspace, nil
}

type PaneLaunch struct {
	Mode     string   `json:"mode"`
	Program  string   `json:"program,omitempty"`
	Args     []string `json:"args,omitempty"`
	Text     string   `json:"text,omitempty"`
	ThreadID ID       `json:"threadId,omitempty"` // existing-thread view, derived locally
}

type PaneRequest struct {
	Op        string           `json:"op"`
	Placement string           `json:"placement,omitempty"`
	Dir       string           `json:"dir,omitempty"`
	Directory PaneFileIdentity `json:"directory,omitempty"`
	Launch    PaneLaunch       `json:"launch,omitempty"`
	Pane      string           `json:"pane,omitempty"`
	Lines     int              `json:"lines,omitempty"`
	ThreadID  ID               `json:"threadId,omitempty"`
	Where     string           `json:"where,omitempty"`
	// Attachment is supplied locally for view launches, never decoded from wire data.
	Attachment PaneAttachment `json:"-"`
}

type PaneReply struct {
	Outcome   string
	Pane      string
	Panes     []string
	Text      string
	Truncated bool
	ThreadID  ID
	Where     string
}

func (r PaneReply) wire(op string) map[string]any {
	if r.Outcome == "confirmed" {
		v := map[string]any{"outcome": "confirmed"}
		switch op {
		case "open", "focus":
			v["pane"] = r.Pane
		case "list":
			v["panes"] = r.Panes
			if r.Panes == nil {
				v["panes"] = []string{}
			}
		case "read":
			v["text"], v["truncated"] = r.Text, r.Truncated
		case "thread_open":
			v["threadId"], v["where"] = r.ThreadID, r.Where
		}
		return v
	}
	if r.Outcome == "rejected" {
		return map[string]any{"outcome": "rejected"}
	}
	return map[string]any{"outcome": "unknown"}
}

func paneRequest(raw any) (PaneRequest, error) {
	fields, ok := raw.(map[string]any)
	if !ok {
		return PaneRequest{}, ErrProtocol
	}
	r, err := decode[PaneRequest](fields)
	if err != nil {
		return r, err
	}
	switch r.Op {
	case "open":
		launch, ok := fields["launch"].(map[string]any)
		if !ok || len(fields) != 5 || !keys(fields, "op", "placement", "dir", "directory", "launch") ||
			!(PaneDirectory{Dir: r.Dir, Directory: r.Directory}).valid() ||
			!oneOf(r.Placement, "right", "below", "left", "above", "tab") {
			break
		}
		switch r.Launch.Mode {
		case "argv":
			if len(launch) == 3 && keys(launch, "mode", "program", "args") && r.Launch.Program != "" && r.Launch.Args != nil {
				return r, nil
			}
		case "shell":
			if len(launch) == 2 && keys(launch, "mode", "text") && r.Launch.Text != "" {
				return r, nil
			}
		case "terminal", "hush":
			if len(launch) == 1 && keys(launch, "mode") {
				return r, nil
			}
		}
	case "thread_open":
		if !r.ThreadID.Valid() {
			break
		}
		if (r.Where == "tab" || r.Where == "focus") && len(fields) == 3 && keys(fields, "op", "threadId", "where") {
			return r, nil
		}
		if r.Where == "split" && len(fields) == 5 && keys(fields, "op", "threadId", "where", "dir", "directory") &&
			(PaneDirectory{Dir: r.Dir, Directory: r.Directory}).valid() {
			return r, nil
		}
	case "list":
		if len(fields) == 1 && keys(fields, "op") {
			return r, nil
		}
	case "focus":
		if len(fields) == 2 && keys(fields, "op", "pane") && paneHandle(r.Pane) {
			return r, nil
		}
	case "read":
		if len(fields) == 3 && keys(fields, "op", "pane", "lines") && paneHandle(r.Pane) && r.Lines >= 1 && r.Lines <= 200 {
			return r, nil
		}
	}
	return PaneRequest{}, ErrProtocol
}

func paneHandle(s string) bool {
	if !strings.HasPrefix(s, "hp_") || len(s) != 29 {
		return false
	}
	for _, r := range s[3:] {
		if (r < 'A' || r > 'Z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}

type paneSession struct {
	route        string
	handler      PaneHandler
	life         context.Context
	cancel       context.CancelFunc
	backend      *localPaneBackend
	capabilities PaneCapabilities

	// Guarded by session.paneGate after the initial bind.
	unbound, triedDowngrade bool

	// Guarded by session.mu. Seen is not evicted; duplicate calls cannot
	// dispatch again even after the first one returned or lost its reply.
	seen         map[string]struct{}
	retired      map[string]struct{} // call IDs fenced by the revoked native binding
	busy         bool
	serverClosed bool
}

func (s *session) bindPane(ctx context.Context, socket string, handler PaneHandler) error {
	caps := handler.Capabilities()
	if s.hello.Capabilities[paneControlCap] == PaneControlVersion {
		if caps.NoPanes {
			return nil
		}
		caps.ThreadTabs = false
	} else if caps.ThreadTabs && !caps.NoPanes && verifyPaneOrigin(ctx, handler) != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		caps = PaneCapabilities{ThreadTabs: true, NoPanes: true}
	}
	return s.bindPaneRoute(ctx, socket, handler, caps, nil)
}

func (s *session) bindPaneRoute(ctx context.Context, socket string, handler PaneHandler, caps PaneCapabilities, previous *paneSession) error {
	v, err := s.call(ctx, paneBind, struct {
		Capabilities PaneCapabilities `json:"capabilities"`
	}{caps}, nil)
	if err != nil {
		return err
	}
	out, err := decode[struct {
		Route  string           `json:"route"`
		Socket PaneFileIdentity `json:"socket"`
	}](v)
	if err != nil || !token(out.Route) || !out.Socket.valid() {
		return ErrProtocol
	}
	path, err := filepath.Abs(socket)
	if err != nil {
		return err
	}
	if previous != nil && (previous.route == out.Route || previous.backend.identity != out.Socket) {
		return ErrProtocol
	}
	backend := &localPaneBackend{handler: handler, attachment: PaneAttachment{Socket: path, Epoch: s.hello.Epoch}, identity: out.Socket}
	if !backend.verified() {
		_, err = s.call(ctx, paneUnbind, nil, nil)
		return err // ordinary chat remains available on a nonlocal backend
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cause != nil {
		return &TransportError{Err: s.cause}
	}
	life, cancel := context.WithCancel(context.Background())
	s.paneBackend = backend
	s.pane = &paneSession{route: out.Route, handler: handler, life: life, cancel: cancel, seen: map[string]struct{}{},
		backend: backend, capabilities: caps}
	if previous != nil {
		s.pane.retired = previous.seen
	}
	return nil
}

func verifyPaneOrigin(ctx context.Context, handler PaneHandler) error {
	probe, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	return handler.Verify(probe)
}

func (s *session) paneForInput(ctx context.Context, requiresPanes bool) string {
	select {
	case s.paneGate <- struct{}{}:
		defer func() { <-s.paneGate }()
	case <-ctx.Done():
		return ""
	case <-s.done:
		return ""
	}
	s.mu.Lock()
	p := s.pane
	s.mu.Unlock()
	if p == nil {
		return ""
	}
	local := p.backend.verified()
	if local && p.life.Err() == nil {
		if p.capabilities.NoPanes {
			if requiresPanes {
				return ""
			}
			return p.route // tabs do not depend on the native adapter
		}
		probe, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(p.life, cancel)
		err := verifyPaneOrigin(probe, p.handler)
		stop()
		cancel()
		if ctx.Err() != nil {
			return ""
		}
		if err == nil && p.life.Err() == nil {
			return p.route
		}
	}
	// Revoke the old label before offering a fresh, strictly tab-only label.
	// Earlier inputs and handles never acquire the replacement route.
	p.cancel()
	change, end := context.WithTimeout(ctx, time.Second)
	defer end()
	s.mu.Lock()
	serverClosed := p.serverClosed
	s.mu.Unlock()
	if !p.unbound && !serverClosed {
		if _, err := s.call(change, paneUnbind, nil, nil); err != nil {
			s.fail(err)
			return ""
		}
		p.unbound = true
	}
	s.mu.Lock()
	busy := p.busy
	s.mu.Unlock()
	if !local || busy || p.triedDowngrade || p.capabilities.NoPanes || !p.capabilities.ThreadTabs {
		return ""
	}
	p.triedDowngrade = true
	caps := PaneCapabilities{ThreadTabs: true, NoPanes: true}
	if err := s.bindPaneRoute(change, p.backend.attachment.Socket, p.handler, caps, p); err != nil {
		var rejected *ServerError
		if !errors.As(err, &rejected) || rejected.Code != "invalid_request" {
			s.fail(err)
		}
		return ""
	}
	s.mu.Lock()
	next := s.pane
	s.mu.Unlock()
	if !requiresPanes && next != p && next.life.Err() == nil {
		return next.route
	}
	return ""
}

func (s *session) receivePaneCall(f map[string]any) error {
	id, _ := f["id"].(string)
	at, ok := asUint(f["deadlineMs"])
	if !ok || !token(id) || len(f) != 4 || !keys(f, "type", "id", "deadlineMs", "request") {
		return ErrProtocol
	}
	r, err := paneRequest(f["request"])
	if err != nil {
		return err
	}
	deadline := time.UnixMilli(int64(at))
	if time.Until(deadline) > 10*time.Second {
		return ErrProtocol
	}
	s.mu.Lock()
	p := s.pane
	if !s.verified || p == nil {
		s.mu.Unlock()
		return ErrProtocol
	}
	if _, retired := p.retired[id]; retired {
		s.mu.Unlock()
		return nil
	}
	if !p.capabilities.supports(r) {
		s.mu.Unlock()
		return ErrProtocol
	}
	if _, seen := p.seen[id]; seen || p.life.Err() != nil {
		s.mu.Unlock()
		return nil
	}
	if p.busy || len(p.seen) >= paneMaxCalls {
		s.mu.Unlock()
		return ErrProtocol
	}
	p.seen[id], p.busy = struct{}{}, true
	s.mu.Unlock()
	// Never execute or wait for local I/O on the protocol reader.
	go s.handlePaneCall(p, id, deadline, r)
	return nil
}

func (s *session) receivePaneCancel(f map[string]any) error {
	id, _ := f["id"].(string)
	if len(f) != 2 || !token(id) {
		return ErrProtocol
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pane
	if !s.verified || p == nil || !p.capabilities.ThreadTabs {
		return ErrProtocol
	}
	if _, seen := p.seen[id]; seen {
		p.serverClosed = true
		p.cancel() // the server closed this route, not the command connection
	}
	return nil
}

func (s *session) handlePaneCall(p *paneSession, id string, deadline time.Time, request PaneRequest) {
	ctx, cancel := context.WithDeadline(p.life, deadline)
	result := PaneReply{Outcome: "rejected"}
	view := request.Op == "thread_open" || request.Launch.Mode == "hush"
	if ctx.Err() == nil && p.backend.verified() {
		if view {
			request.Attachment = p.backend.attachment
		}
		result = p.handler.Handle(ctx, request)
	}
	cancel()
	s.mu.Lock()
	p.busy = false
	s.mu.Unlock()
	if p.life.Err() != nil {
		return
	}
	replyCtx, end := context.WithTimeout(p.life, time.Second)
	defer end()
	_, _ = s.call(replyCtx, paneResult, struct {
		CallID string         `json:"callId"`
		Result map[string]any `json:"result"`
	}{id, result.wire(request.Op)}, nil)
}
