package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/javiermolinar/hush/app/internal/pane"
	"github.com/javiermolinar/hush/sdk"
	"github.com/javiermolinar/hush/tui"
)

type paneHost interface {
	PaneWorkspace(context.Context, sdk.ID) (sdk.PaneAttachment, sdk.PaneDirectory, error)
}

type paneWorkflows struct {
	adapter pane.Adapter
	host    paneHost
	exe     string
}

func newPaneWorkflows(adapter pane.Adapter, host paneHost, exe string) *paneWorkflows {
	if adapter == nil || !filepath.IsAbs(exe) {
		return nil
	}
	return &paneWorkflows{adapter: adapter, host: host, exe: exe}
}

func (p *paneWorkflows) openThread(ctx context.Context, parent sdk.ID) error {
	if !parent.Valid() {
		return sdk.ErrProtocol
	}
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	attach, w, err := p.host.PaneWorkspace(call, parent)
	if err != nil {
		return err
	}
	// The child creates its own new thread after verifying hello. Failure to
	// place the pane cannot leave an unused thread on the parent backend.
	return p.open(call, attach, w.Dir, &w.Directory, sdk.PaneLaunch{Mode: "hush"})
}

func (p *paneWorkflows) open(ctx context.Context, attach sdk.PaneAttachment, dir string, identity *sdk.PaneFileIdentity, view sdk.PaneLaunch) error {
	launch, err := pane.ViewLaunch(p.exe, attach, view)
	if err != nil {
		return err
	}
	result := p.adapter.Open(ctx, pane.OpenRequest{Placement: pane.Right, Dir: dir, Directory: identity, Launch: launch})
	switch result.Outcome() {
	case pane.Confirmed:
		return nil
	case pane.Rejected:
		return errors.New("the terminal refused the pane request before dispatch")
	default:
		return tui.ErrPaneOpenUnknown
	}
}
