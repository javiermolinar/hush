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

const gitTrustTarget = -3
const gitTrustTimeout = 30 * time.Second

// GitTrustPreview identifies the Git common directory whose linked checkouts
// will share the grant. Grant must reject a different checkout or repository.
type GitTrustPreview struct {
	GitDirectory string
	Grant        func(context.Context) error
}

type gitTrustReview struct {
	cwd, notice, path string
	pathCells         int
	grant             func(context.Context) error
}

type gitTrustMsg struct {
	dialog, request uint64
	preview         GitTrustPreview
	grant           bool
	err             error
}

func (m *Model) offerGitTrust(d *dialog) tea.Cmd {
	if !m.foreground(d.route) || m.focusedDialog() != d {
		return nil
	}
	g, cwd := d.git, m.gitReviewCWD(d)
	g.patch, g.view, g.trust = false, nil, &gitTrustReview{cwd: cwd}
	prepare := m.options.PrepareGitTrust
	if prepare == nil || !filepath.IsAbs(cwd) || m.connection != x.Connected {
		return nil
	}
	ctx, cancel, req := m.gitReviewRequest(d, gitTrustTimeout)
	token := d.token
	return func() tea.Msg {
		defer cancel()
		preview, err := prepare(ctx, cwd)
		return gitTrustMsg{dialog: token, request: req, preview: preview, err: err}
	}
}

func (m *Model) receiveGitTrust(msg gitTrustMsg) tea.Cmd {
	d := m.receivingGitDialog(msg.dialog, msg.request)
	if d == nil || d.git.trust == nil {
		return nil
	}
	d.git.stop()
	t := d.git.trust
	switch {
	case m.gitReviewCWD(d) != t.cwd:
		t.notice = "Workspace changed · refresh"
	case msg.grant:
		if msg.err == nil {
			return m.fetchGitSummary(d, true)
		}
		t.notice = "Could not save trust · refresh"
	case msg.err != nil || !filepath.IsAbs(msg.preview.GitDirectory) || msg.preview.Grant == nil:
		t.notice = "Local trust check failed"
	default:
		// Keep escapes and directional characters visible in the exact path.
		t.path = strconv.QuoteToGraphic(msg.preview.GitDirectory)
		t.pathCells = stringWidth(t.path)
		t.grant = msg.preview.Grant
	}
	return nil
}

func (m *Model) gitTrustKey(d *dialog, chord string) tea.Cmd {
	g, t := d.git, d.git.trust
	if g.cancel != nil {
		return nil
	}
	if chord == "u" && t.grant == nil {
		return m.fetchGitSummary(d, false)
	}
	grant := t.grant
	if chord != "t" || grant == nil || !m.foreground(d.route) || m.connection != x.Connected ||
		!m.dialogVisible(d) || !slices.Contains(m.frame.dialogChoices, gitTrustTarget) {
		return nil
	}
	t.grant = nil
	if m.gitReviewCWD(d) != t.cwd {
		t.notice = "Workspace changed · refresh"
		return nil
	}
	ctx, cancel, req := m.gitReviewRequest(d, gitTrustTimeout)
	token := d.token
	return func() tea.Msg {
		defer cancel()
		err := grant(ctx)
		return gitTrustMsg{dialog: token, request: req, grant: true, err: err}
	}
}

func (m *Model) gitTrustRows(d *dialog, w, h int) (rows []string, targets []int) {
	g, t := d.git, d.git.trust
	add := func(row string, target int) {
		if len(rows) < h {
			rows = append(rows, ansi.Truncate(row, w, "…"))
			targets = append(targets, target)
		}
	}
	if t.grant != nil {
		add(paint(x.Warning, "Trust this repository?"), -1)
		// Only draw a grant target if the full scope is on screen.
		var path []string
		if t.pathCells <= w*(h-3) {
			path = strings.Split(ansi.Hardwrap(t.path, w, true), "\n")
		}
		if len(path) > 0 && len(path)+3 <= h {
			for _, line := range path {
				add(m.control(line), -1)
			}
			add(paint(x.Muted, "Also trusts linked worktrees."), -1)
			add(m.keyHint("t", "trust repository"), gitTrustTarget)
		} else {
			add(m.control("Resize to review the full path."), -1)
		}
		add(m.keyHint("esc", "cancel"), -1)
		return rows, targets
	}
	add(paint(x.Warning, "Workspace trust required"), -1)
	if g.cancel != nil {
		label := "Checking local repository…"
		if t.path != "" {
			label = "Saving trust…"
		}
		add(m.control(label), -1)
		add(m.keyHint("esc", "close"), -1)
		return rows, targets
	}
	if t.notice != "" {
		add(paint(x.Warning, t.notice), -1)
	}
	add(m.control("On the backend host, run:"), -1)
	add(m.control("hush trust grant --cwd PATH"), -1)
	if t.cwd != "" {
		add(paint(x.Muted, "PATH = "+gitReviewScope(t.cwd, "", w-7)), -1)
	}
	add(m.keyHint("u", "refresh")+"  "+m.keyHint("esc", "close"), -1)
	return rows, targets
}
