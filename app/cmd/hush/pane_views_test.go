package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/javiermolinar/hush/app/internal/pane"
	"github.com/javiermolinar/hush/sdk"
	"github.com/javiermolinar/hush/tui"
)

type paneViewHost struct {
	id        sdk.ID
	attach    sdk.PaneAttachment
	workspace sdk.PaneDirectory
	err       error
}

func (h *paneViewHost) PaneWorkspace(_ context.Context, id sdk.ID) (sdk.PaneAttachment, sdk.PaneDirectory, error) {
	h.id = id
	return h.attach, h.workspace, h.err
}

type paneViewAdapter struct {
	pane.Adapter
	requests []pane.OpenRequest
	result   pane.Result[pane.Ref]
}

func (a *paneViewAdapter) Open(_ context.Context, r pane.OpenRequest) pane.Result[pane.Ref] {
	a.requests = append(a.requests, r)
	return a.result
}

func TestPaneViewsUseExactAttachOnlyTargets(t *testing.T) {
	host := &paneViewHost{attach: sdk.PaneAttachment{Socket: "/fixture/sock with spaces", Epoch: "pinned-backend"},
		workspace: sdk.PaneDirectory{Dir: "/repo/worktree", Directory: sdk.PaneFileIdentity{Device: "1", Inode: "2", Birth: "3"}}}
	adapter := &paneViewAdapter{result: pane.Confirm(pane.NewScope().Ref("new"))}
	p := &paneWorkflows{host: host, adapter: adapter, exe: "/local/hush"}
	if err := p.openThread(t.Context(), "7"); err != nil {
		t.Fatal(err)
	}
	if got := adapter.requests[0]; got.Dir != host.workspace.Dir || got.Directory == nil || *got.Directory != host.workspace.Directory || host.id != "7" {
		t.Fatalf("fresh pane used wrong directory or source: %+v, %s", got, host.id)
	}
	got := adapter.requests[0]
	args := []string{"--local", "--socket", host.attach.Socket, "--pane-epoch", host.attach.Epoch}
	if got.Placement != pane.Right || got.Launch.Mode != pane.LaunchArgv || got.Launch.Program != p.exe || !slices.Equal(got.Launch.Args, args) {
		t.Fatalf("unexpected view launch: %+v", got)
	}
	adapter.result = pane.Uncertain[pane.Ref](errors.New("lost native reply"))
	if err := p.openThread(t.Context(), "7"); !errors.Is(err, tui.ErrPaneOpenUnknown) || len(adapter.requests) != 2 {
		t.Fatalf("placement was retried or reported as definite: %v, %d", err, len(adapter.requests))
	}
	host.err = sdk.ErrPaneUnavailable
	if err := p.openThread(t.Context(), "7"); !errors.Is(err, sdk.ErrPaneUnavailable) || len(adapter.requests) != 2 {
		t.Fatal("unavailable backend caused an effect")
	}
}
