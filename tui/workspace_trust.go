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
const workspaceTrustCopyTarget = -5
const workspaceTrustStartTarget = -6

// WorkspaceTrustOptions enables /trust. Review must be nil on attached clients,
// even when they share the backend's filesystem for links.
type WorkspaceTrustOptions struct {
	Review func(context.Context, string) (WorkspaceTrustPreview, error)
}

// WorkspaceTrustPreview describes the grant for one exact workspace directory.
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
	onGrant           func() tea.Cmd // a local plan trusts the checkout in this dialog
	command           string         // an attached plan needs the operator to grant trust
	onStart           func() tea.Cmd
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
	if w.onGrant == nil && w.cwd != m.workspaceTrustCWD() {
		w.notice = "Workspace changed · reopen /trust"
		return nil
	}
	if !msg.decision {
		if w.onGrant != nil && msg.err == nil && msg.preview.Path != w.cwd {
			w.notice = "Worktree path changed · no run started"
			return nil
		}
		w.apply(msg.preview, msg.err)
		if w.onGrant != nil && w.trusted {
			return tea.Batch(m.closeDialog(d, nil, false), w.onGrant())
		}
		return nil
	}
	if msg.err != nil {
		w.notice = "Could not save workspace trust · reopen /trust"
		return nil
	}
	closed := m.closeDialog(d, nil, false)
	if msg.granted {
		if w.onGrant != nil {
			return tea.Batch(closed, w.onGrant())
		}
		// Old hosts don't observe the grant themselves. Refresh their scope
		// now instead of waiting for the next poll.
		if !m.fileScope.pushed {
			m.stopFileScope()
		}
		if !m.gitDiffFeed.pushed {
			m.stopGitDiff()
		}
		m.stopSkills()
		return tea.Batch(closed, m.syncGitDiff(), m.notifyRoute(d.route, "Workspace trusted", x.Good))
	}
	return closed
}

func workspaceGrantCommand(cwd string) string {
	// Quote apostrophes and backslashes outside the single-quoted segments
	// so the command works in both POSIX shells and fish.
	return "hush trust grant --cwd '" + strings.NewReplacer("'", "'\\''", "\\", "'\\\\'").Replace(cwd) + "'"
}

func (m *Model) workspaceTrustKey(d *dialog, chord string) tea.Cmd {
	w := d.workTrust
	if w.onStart != nil && m.dialogVisible(d) && m.foreground(d.route) {
		switch {
		case chord == "y" && slices.Contains(m.frame.dialogChoices, workspaceTrustCopyTarget):
			return m.copyTextWithFeedback(d.route, w.command, nil, func() tea.Cmd {
				w.notice = "Grant command copied"
				return nil
			})
		case chord == "w" && slices.Contains(m.frame.dialogChoices, workspaceTrustStartTarget):
			return tea.Batch(m.closeDialog(d, nil, false), w.onStart())
		}
	}
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
	if w.onGrant == nil && w.cwd != m.workspaceTrustCWD() {
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

func (m *Model) workspaceTrustRows(d *dialog, w, h int) menuFrame {
	wt := d.workTrust
	c := menuChrome{title: "Workspace trust", keys: chromeLine(m.menuKeys(w, d.zoomKey()))}
	if wt.cancel != nil {
		status := "Checking workspace trust…"
		if wt.path != "" {
			status = "Saving workspace trust…"
		}
		return m.menuShell(w, h, c, func(int) menuFrame {
			return menuFrame{rows: []string{m.quiet(status)}, targets: []int{-1}}
		})
	}
	if wt.decide != nil || wt.trusted {
		c.title = "Trusted workspace"
		access := strings.Split(ansi.Wrap("Tools, file browsing, and Git (whole checkout)", w, ""), "\n")
		c.minBody = len(access) + 1
		if !wt.trusted {
			c.title = "Trust this workspace?"
			c.consequence = chromeLine(m.quiet("Resize or zoom to review the full path"))
			c.keys = menuLine{m.menuKeys(w, "n not now", d.zoomKey()), workspaceTrustDismissTarget}
		}
		return m.menuShell(w, h, c, func(budget int) menuFrame {
			var out menuFrame
			for _, line := range access[:min(budget, len(access))] {
				out.add(m.quiet(line), -1)
			}
			room := budget - len(out.rows)
			if room > 0 && wt.pathCells <= w*room {
				path := strings.Split(ansi.Hardwrap(wt.path, w, true), "\n")
				if len(path) <= room {
					for _, line := range path {
						out.add(line, -1)
					}
					if !wt.trusted {
						// The grant target is emitted only with the whole scope above it.
						out.consequence = menuLine{strongText("t trust") + "  " + m.quiet("Allow access to this checkout"), workspaceTrustGrantTarget}
					}
					return out
				}
			}
			if room > 0 {
				out.add(m.quiet("Full path does not fit"), -1)
			}
			return out
		})
	}
	if wt.onStart != nil {
		c.title = "Trust on the backend host"
	}
	return m.menuShell(w, h, c, func(budget int) menuFrame {
		var out menuFrame
		if wt.onStart != nil {
			label := "hush trust grant --cwd PATH"
			if wt.notice != "" {
				label = wt.notice
			}
			out.add(m.quiet(label), -1)
			path := strconv.QuoteToGraphic(wt.cwd)
			if space := budget - 3; space > 0 && stringWidth(path) <= space*w {
				lines := strings.Split(ansi.Hardwrap(path, w, true), "\n")
				if len(lines) <= space {
					for _, line := range lines {
						out.add(line, -1)
					}
				}
			}
			if len(out.rows) < budget {
				out.add(m.quiet("y copy grant command"), workspaceTrustCopyTarget)
			}
			if len(out.rows) < budget {
				out.add(strongText("w start after granting"), workspaceTrustStartTarget)
			}
			return out
		}
		if wt.notice != "" && budget >= 2 {
			out.add(m.paint(x.Warning, wt.notice), -1)
		}
		if budget-len(out.rows) >= 2 {
			out.add(m.quiet("On the backend host, run:"), -1)
		}
		for _, line := range wrapped("hush trust grant --cwd PATH", w) {
			if len(out.rows) >= budget {
				break
			}
			out.add(m.quiet(line), -1)
		}
		if wt.cwd != "" && len(out.rows) < budget {
			out.add(m.quiet("PATH = "+gitReviewScope(wt.cwd, "", w-7)), -1)
		}
		return out
	})
}
