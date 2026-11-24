package tui

import (
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

const openWorkspaceTrustOp x.LocalOp = "workspaceTrust.open"
const workspaceTrustTimeout = 30 * time.Second
const workspaceTrustGrantTarget = -3
const workspaceTrustDismissTarget = -4

// WorkspaceTrustOptions enables /trust. Review must be nil on attached clients,
// even when they share the backend's filesystem for links.
type WorkspaceTrustOptions struct {
	Review func(context.Context, string) (WorkspaceTrustPreview, error)
}

// WorkspaceTrustPreview describes permission to run tools in one exact directory.
// Decide must reject a changed directory, and persist either trust or Not now.
type WorkspaceTrustPreview struct {
	Path               string
	Trusted, Dismissed bool
	Decide             func(context.Context, bool) error
}

type workspaceTrustProbe struct {
	cwd     string
	request uint64
	cancel  context.CancelFunc
	pending *WorkspaceTrustPreview
}

type workspaceTrustDialog struct {
	cwd, path, notice string
	pathCells         int
	attached, trusted bool
	cancel            context.CancelFunc
	decide            func(context.Context, bool) error
}

type workspaceTrustMsg struct {
	request                 uint64
	preview                 WorkspaceTrustPreview
	err                     error
	auto, decision, granted bool
}

func (m *Model) workspaceTrustCWD() string {
	if !m.active.Valid() {
		return m.options.NewThread.CWD
	}
	t := m.threads[m.active]
	if t.observed && m.connection == x.Connected {
		return t.info.Workspace
	}
	return ""
}

// Only active workspaces are probed, and never from inside another overlay.
// Once a real path changes, discard any observation for the old workspace.
func (m *Model) syncWorkspaceTrust() tea.Cmd {
	opt := m.options.WorkspaceTrust
	if opt == nil || opt.Review == nil {
		return nil
	}
	cwd := m.workspaceTrustCWD()
	if cwd == "" {
		return nil // wait for the active thread's own watch snapshot
	}
	p := &m.workspaceTrust
	if p.cwd != cwd {
		if p.cancel != nil {
			p.cancel()
		}
		*p = workspaceTrustProbe{cwd: cwd}
	}
	if m.focusedDialog() != nil {
		return nil
	}
	if p.request == 0 && filepath.IsAbs(cwd) {
		ctx, cancel := context.WithTimeout(m.ctx, workspaceTrustTimeout)
		p.request, p.cancel = m.token(), cancel
		return reviewWorkspaceTrust(ctx, cancel, opt.Review, cwd, p.request, true)
	}
	if p.pending != nil {
		preview := p.pending
		p.pending = nil
		d := &dialog{token: m.token(), route: m.route(m.active, coreID), workTrust: &workspaceTrustDialog{cwd: cwd}}
		d.workTrust.apply(*preview, nil)
		return m.pushDialog(d)
	}
	return nil
}

func reviewWorkspaceTrust(ctx context.Context, cancel context.CancelFunc, review func(context.Context, string) (WorkspaceTrustPreview, error), cwd string, request uint64, auto bool) tea.Cmd {
	return func() tea.Msg {
		defer cancel()
		preview, err := review(ctx, cwd)
		return workspaceTrustMsg{request: request, preview: preview, err: err, auto: auto}
	}
}

func (m *Model) openWorkspaceTrust(r ownerRoute) tea.Cmd {
	opt := m.options.WorkspaceTrust
	if opt == nil || !m.foreground(r) {
		return nil
	}
	cwd := m.workspaceTrustCWD()
	if opt.Review == nil && !m.active.Valid() {
		cwd = "" // never suggest the client's cwd to a different backend
	}
	w := &workspaceTrustDialog{cwd: cwd, attached: opt.Review == nil}
	d := &dialog{token: m.token(), route: r, workTrust: w}
	open := m.pushDialog(d)
	if m.focusedDialog() != d || w.attached {
		return open
	}
	if !filepath.IsAbs(cwd) {
		w.notice = "Workspace path not available yet"
		return open
	}
	// The explicit dialog supersedes an automatic probe of the same workspace.
	p := &m.workspaceTrust
	if p.cancel != nil {
		p.cancel()
	}
	*p = workspaceTrustProbe{cwd: cwd, request: m.token()}
	ctx, cancel := context.WithTimeout(m.ctx, workspaceTrustTimeout)
	w.cancel = cancel
	return reviewWorkspaceTrust(ctx, cancel, opt.Review, cwd, d.token, false)
}

func validWorkspaceTrustPreview(p WorkspaceTrustPreview) bool {
	return filepath.IsAbs(p.Path) && len(p.Path) <= 16<<10 && (p.Trusted || p.Decide != nil)
}

func (w *workspaceTrustDialog) apply(p WorkspaceTrustPreview, err error) {
	if err != nil || !validWorkspaceTrustPreview(p) {
		w.notice = "Could not read local workspace trust"
		return
	}
	// Display the full scope, with escapes and directional controls visible.
	w.path = strconv.QuoteToGraphic(p.Path)
	w.pathCells = stringWidth(w.path)
	w.trusted = p.Trusted
	if !p.Trusted {
		w.decide = p.Decide
	}
}

func (m *Model) receiveWorkspaceTrust(msg workspaceTrustMsg) tea.Cmd {
	if msg.auto {
		p := &m.workspaceTrust
		if p.request == msg.request {
			p.cancel = nil
			if msg.err == nil && validWorkspaceTrustPreview(msg.preview) && !msg.preview.Trusted && !msg.preview.Dismissed {
				p.pending = &msg.preview
			}
		}
		return nil
	}
	d := m.focusedDialog()
	if d == nil || d.token != msg.request || d.workTrust == nil || !m.foreground(d.route) {
		return nil
	}
	w := d.workTrust
	w.cancel = nil
	if w.cwd != m.workspaceTrustCWD() {
		w.notice = "Workspace changed · reopen /trust"
		return nil
	}
	if !msg.decision {
		w.apply(msg.preview, msg.err)
		return nil
	}
	if msg.err != nil {
		w.notice = "Could not save workspace trust · reopen /trust"
		return nil
	}
	closed := m.closeDialog(d, nil, false)
	if msg.granted {
		return tea.Batch(closed, m.notifyRoute(d.route, "Workspace trusted for tools", x.Good))
	}
	return closed
}

func (m *Model) workspaceTrustKey(d *dialog, chord string) tea.Cmd {
	w := d.workTrust
	grant := chord == "t"
	if chord != "n" && chord != "esc" && !grant {
		return nil
	}
	if w.decide == nil || !m.foreground(d.route) {
		if chord == "esc" {
			return m.closeDialog(d, nil, false)
		}
		return nil
	}
	if w.cwd != m.workspaceTrustCWD() {
		if chord == "esc" {
			return m.closeDialog(d, nil, false)
		}
		w.decide, w.notice = nil, "Workspace changed · reopen /trust"
		return nil
	}
	if grant && (!m.dialogVisible(d) || !slices.Contains(m.frame.dialogChoices, workspaceTrustGrantTarget)) {
		return nil // the full scope and the grant target must be visible
	}
	decide := w.decide
	w.decide = nil
	ctx, cancel := context.WithTimeout(m.ctx, workspaceTrustTimeout)
	w.cancel = cancel
	token := d.token
	return func() tea.Msg {
		defer cancel()
		return workspaceTrustMsg{request: token, decision: true, granted: grant, err: decide(ctx, grant)}
	}
}

func (m *Model) workspaceTrustRows(wt *workspaceTrustDialog, w, h int) (rows []string, targets []int) {
	add := func(row string, target int) {
		if len(rows) < h {
			rows = append(rows, ansi.Truncate(row, w, "…"))
			targets = append(targets, target)
		}
	}
	switch {
	case wt.cancel != nil:
		status := "Checking workspace trust…"
		if wt.path != "" {
			status = "Saving workspace trust…"
		}
		add(m.control(status), -1)
	case wt.decide != nil || wt.trusted:
		label, tone, reserved := "Allow Hush to run tools in:", x.Warning, 3
		if wt.trusted {
			label, tone, reserved = "Hush may run tools in:", x.Good, 2
		}
		add(paint(tone, label), -1)
		var path []string
		if w > 0 && wt.pathCells <= w*(h-reserved) {
			path = strings.Split(ansi.Hardwrap(wt.path, w, true), "\n")
		}
		if len(path) != 0 && len(path)+reserved <= h {
			for _, line := range path {
				add(m.control(line), -1)
			}
			if !wt.trusted {
				add(m.keyHint("t", "trust"), workspaceTrustGrantTarget)
			}
		} else {
			add(m.control("Resize to review the full path."), -1)
		}
		if !wt.trusted {
			add(m.keyHint("n", "not now")+"  "+m.keyHint("esc", "dismiss"), workspaceTrustDismissTarget)
			return rows, targets
		}
	default:
		add(paint(x.Warning, "Workspace tool trust"), -1)
		if wt.notice != "" {
			add(paint(x.Warning, wt.notice), -1)
		}
		add(m.control("On the backend host, run:"), -1)
		for _, line := range strings.Split(ansi.Wrap("hush trust grant --scope work --cwd PATH", w, ""), "\n") {
			add(m.control(line), -1)
		}
		if wt.cwd != "" {
			add(paint(x.Muted, "PATH = "+gitReviewScope(wt.cwd, "", w-7)), -1)
		}
	}
	add(m.keyHint("esc", "close"), -1)
	return rows, targets
}
