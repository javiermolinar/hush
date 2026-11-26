package tui

import (
	"context"
	"io"
	"path"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const loadedContextID x.ActionID = menusID + ".loaded-context"
const openLoadedContextOp x.LocalOp = "loadedContext.open"
const outputSkillsTarget = -3

type loadedContextBackend interface {
	LoadedContext(context.Context, sdk.ID) ([]sdk.InstructionSource, error)
}

type contextDialog struct {
	cancel context.CancelFunc
	ready  bool
}

func (m *Model) contextAvailable() bool {
	return m.nativeContext && m.active.Valid() && m.connection == x.Connected
}

func (m *Model) openLoadedContext(r ownerRoute, budget *int) tea.Cmd {
	backend, ok := m.backend.(loadedContextBackend)
	if !ok || !m.foreground(r) || !m.contextAvailable() {
		return nil
	}
	ctx, cancel := context.WithTimeout(m.ctx, menuTimeout+time.Second)
	d := &dialog{route: r, token: m.token(), title: "Loaded context · next submission", loadedContext: &contextDialog{cancel: cancel}}
	m.setContextOutput(d, "Loading source paths…")
	open := m.pushDialog(d)
	load := m.startWork(r, func(context.Context) (any, error) {
		defer cancel()
		return backend.LoadedContext(ctx, r.owner)
	}, func(v any, err error, _ *int) tea.Cmd {
		if !m.foreground(r) || m.focusedDialog() != d || !m.contextAvailable() {
			return nil
		}
		if err != nil {
			m.setContextOutput(d, "Could not read instruction sources from this backend.")
			return nil
		}
		sources, _ := v.([]sdk.InstructionSource)
		d.loadedContext.ready = true
		if len(sources) == 0 {
			m.setContextOutput(d, "No instruction files loaded for this thread.")
			return nil
		}
		var text strings.Builder
		for _, source := range sources {
			label := "Workspace"
			if source.Kind == "personal" {
				label = "Personal"
			}
			io.WriteString(&text, label+" · "+path.Base(source.Path)+"\n  "+source.Path+"\n")
		}
		m.setContextOutput(d, text.String())
		return nil
	}, budget)
	return tea.Batch(open, load)
}

func (m *Model) setContextOutput(d *dialog, text string) {
	d.output = newOutputViewer(text)
	d.output.reflowReview(m.pane().width)
}

func (m *Model) contextCanOpenSkills(d *dialog) bool {
	return d.loadedContext != nil && d.loadedContext.ready && m.foreground(d.route) && len(m.currentSkills()) != 0
}

func (m *Model) contextSkillsHint() string { return "s skills" }

func (m *Model) openContextSkills(d *dialog) tea.Cmd {
	if !m.contextCanOpenSkills(d) {
		return nil
	}
	close := m.closeDialog(d, nil, false)
	// Close an underlying command palette so the skills sheet takes focus.
	if palette := m.focusedDialog(); palette != nil && palette.palette {
		close = tea.Batch(close, m.closeDialog(palette, nil, false))
	}
	return tea.Batch(close, m.runAction(m.host.byID[skillsPickerID], ""))
}
