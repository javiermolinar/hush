package pane

import (
	"context"
	"crypto/rand"
	"io/fs"
	"path/filepath"

	"github.com/javiermolinar/hush/sdk"
)

// A handler is issued per backend connection. Listing a native terminal on a
// replacement connection cannot resurrect a prior command's output grant.
func NewHandler(adapter Adapter, hushExecutable string) sdk.PaneHandler {
	return NewThreadHandler(adapter, hushExecutable, nil)
}

// openTab registers a TUI tab for the thread and, with focus, switches to it.
func NewThreadHandler(adapter Adapter, hushExecutable string, openTab func(ctx context.Context, id sdk.ID, focus bool) Outcome) sdk.PaneHandler {
	return &handler{adapter: adapter, exe: hushExecutable, openTab: openTab,
		refs: map[string]Ref{}, handles: map[Ref]string{}, commandPanes: map[Ref]bool{}}
}

type handler struct {
	adapter      Adapter
	exe          string
	openTab      func(context.Context, sdk.ID, bool) Outcome
	refs         map[string]Ref
	handles      map[Ref]string
	commandPanes map[Ref]bool
}

const maxHandles = 2048

func (h *handler) Capabilities() sdk.PaneCapabilities {
	if h.adapter == nil {
		return sdk.PaneCapabilities{ThreadTabs: h.openTab != nil, NoPanes: true}
	}
	c := h.adapter.Capabilities()
	return sdk.PaneCapabilities{Left: c.Left, Above: c.Above, Tabs: c.Tabs, Read: c.Read, Views: filepath.IsAbs(h.exe), ThreadTabs: h.openTab != nil}
}

func (h *handler) LocalBackend(socket string, identity sdk.PaneFileIdentity) bool {
	actual, ok := localIdentity(socket, fs.ModeSocket)
	return ok && actual == identity
}

func (h *handler) Verify(ctx context.Context) error {
	if h.adapter == nil {
		return ctx.Err()
	}
	return h.adapter.Verify(ctx)
}

func (h *handler) handle(ref Ref) string {
	if ref == (Ref{}) {
		return ""
	}
	if id := h.handles[ref]; id != "" {
		return id
	}
	if len(h.handles) >= maxHandles {
		return ""
	}
	for {
		id := "hp_" + rand.Text()
		if _, used := h.refs[id]; !used {
			h.handles[ref], h.refs[id] = id, ref
			return id
		}
	}
}

func threadViewReply(request sdk.PaneRequest, outcome Outcome) sdk.PaneReply {
	reply := sdk.PaneReply{Outcome: string(outcome)}
	if outcome == Confirmed {
		reply.ThreadID, reply.Where = request.ThreadID, request.Where
	}
	return reply
}

func (h *handler) Handle(ctx context.Context, request sdk.PaneRequest) sdk.PaneReply {
	reject, unknown := sdk.PaneReply{Outcome: string(Rejected)}, sdk.PaneReply{Outcome: string(Unknown)}
	if ctx.Err() != nil {
		return reject
	}
	if request.Op == "thread_open" {
		if h.openTab == nil || !request.ThreadID.Valid() {
			return reject
		}
		if request.Where == "tab" || request.Where == "focus" {
			return threadViewReply(request, h.openTab(ctx, request.ThreadID, request.Where == "focus"))
		}
		if request.Where != "split" || h.adapter == nil {
			return reject
		}
		launch, err := ViewLaunch(h.exe, request.Attachment, sdk.PaneLaunch{Mode: "thread", ThreadID: request.ThreadID})
		if err != nil {
			return reject
		}
		result := h.adapter.Open(ctx, OpenRequest{Placement: Right, Dir: request.Dir, Directory: &request.Directory, Launch: launch})
		return threadViewReply(request, result.Outcome())
	}
	if h.adapter == nil {
		return reject
	}
	switch request.Op {
	case "open":
		if len(h.handles) >= maxHandles {
			return reject
		}
		l := request.Launch
		var launch Launch
		command := l.Mode == "argv" || l.Mode == "shell"
		switch {
		case command:
			launch = Launch{Mode: LaunchMode(l.Mode), Program: l.Program, Args: l.Args, Text: l.Text}
		case !filepath.IsAbs(h.exe):
			return reject
		case l.Mode == "terminal":
			launch = terminalLaunch()
		default:
			var err error
			if launch, err = ViewLaunch(h.exe, request.Attachment, l); err != nil {
				return reject
			}
		}
		result := h.adapter.Open(ctx, OpenRequest{Placement: Placement(request.Placement), Dir: request.Dir, Directory: &request.Directory, Launch: launch})
		if ref, ok := result.Value(); ok {
			id := h.handle(ref)
			if id == "" {
				return unknown
			}
			if command {
				h.commandPanes[ref] = true
			}
			return sdk.PaneReply{Outcome: string(Confirmed), Pane: id}
		}
		return sdk.PaneReply{Outcome: string(result.Outcome())}
	case "list":
		result := h.adapter.List(ctx, ListRequest{})
		if refs, ok := result.Value(); ok {
			if len(refs) > 512 {
				return unknown
			}
			ids := make([]string, 0, len(refs))
			for _, ref := range refs {
				id := h.handle(ref)
				if id == "" {
					return unknown
				}
				ids = append(ids, id)
			}
			return sdk.PaneReply{Outcome: string(Confirmed), Panes: ids}
		}
		return sdk.PaneReply{Outcome: string(result.Outcome())}
	case "focus":
		ref, ok := h.refs[request.Pane]
		if !ok {
			return reject
		}
		result := h.adapter.Focus(ctx, FocusRequest{Pane: ref})
		if observed, ok := result.Value(); ok {
			if observed != ref {
				return unknown
			}
			return sdk.PaneReply{Outcome: string(Confirmed), Pane: request.Pane}
		}
		return sdk.PaneReply{Outcome: string(result.Outcome())}
	case "read":
		ref, ok := h.refs[request.Pane]
		if !ok || !h.commandPanes[ref] || !h.adapter.Capabilities().Read {
			return reject
		}
		result := h.adapter.Read(ctx, ReadRequest{Pane: ref, Lines: request.Lines})
		if snapshot, ok := result.Value(); ok {
			return sdk.PaneReply{Outcome: string(Confirmed), Text: snapshot.Text, Truncated: snapshot.Truncated}
		}
		return sdk.PaneReply{Outcome: string(result.Outcome())}
	default:
		return reject
	}
}
