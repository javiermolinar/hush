package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const gitDiffID x.ActionID = coreID + ".gitDiff"
const openGitDiffOp x.LocalOp = "gitDiff.open"
const gitRangeTarget = -2
const gitDiffTimeout = 5 * time.Second

type gitDiffBackend interface {
	GitDiffSummary(context.Context, sdk.ID, string) (sdk.GitDiffSummary, error)
	GitDiffFile(context.Context, sdk.ID, string, int) (sdk.GitDiffPatch, error)
}

type gitDiffState struct {
	summary sdk.GitDiffSummary
	err     string
	badge   string
}

func gitDiffBadge(summary sdk.GitDiffSummary) string {
	result := fmt.Sprintf("± %d files", len(summary.Files))
	if len(summary.Files) == 1 {
		result = "± 1 file"
	}
	if len(summary.Files) == 0 {
		return result
	}
	var add, remove int64
	var unknown bool
	for _, f := range summary.Files {
		if f.Added != nil && f.Removed != nil {
			add += *f.Added
			remove += *f.Removed
		} else {
			unknown = true
		}
	}
	result += fmt.Sprintf("  +%d −%d", add, remove)
	if unknown {
		result += "*"
	}
	return result
}

func (m *Model) gitDiffRule(t *threadState, left string, w int) displayRow {
	if !m.active.Valid() || !m.menuFeaturesFromBackend.Has(x.GitDiff) || m.connection != x.Connected {
		return displayRow{text: left + paint(x.Muted, strings.Repeat("─", max(0, w-stringWidth(left))))}
	}
	label := t.gitDiff.badge
	if label == "" {
		label = "± …"
		if t.gitDiff.err != "" {
			label = "± unavailable"
		}
	}
	if stringWidth(label)+stringWidth(left)+4 > w {
		if i := strings.Index(label, "  "); i >= 0 {
			label = label[:i]
		}
	}
	if stringWidth(label)+stringWidth(left)+4 > w {
		left = ansi.Truncate(left, max(0, w-stringWidth(label)-4), "…")
	}
	first := w - stringWidth(label) - 3
	return displayRow{text: left + paint(x.Muted, strings.Repeat("─", max(0, first-1-stringWidth(left)))) + " " +
		paint(x.Accent, label) + paint(x.Muted, " ──"),
		hits: []spanHit{{first, first + stringWidth(label), x.ActionRef{ID: gitDiffID}, "review worktree changes"}}}
}

type gitDiffPoll struct {
	feedLease
	tick    uint64
	pending bool
	last    time.Time
}

type gitDiffTick uint64
type gitDiffSummaryMsg struct {
	lease, dialog, request uint64
	value                  sdk.GitDiffSummary
	err                    error
}
type gitDiffPatchMsg struct {
	dialog, request uint64
	value           sdk.GitDiffPatch
	err             error
}

func (m *Model) stopGitDiff() {
	if m.gitDiffFeed.cancel != nil {
		m.gitDiffFeed.cancel()
	}
	m.gitDiffFeed = gitDiffPoll{}
}

func (m *Model) syncGitDiff() tea.Cmd {
	f := m.gitDiffFeed
	if !m.menuFeaturesFromBackend.Has(x.GitDiff) || !m.active.Valid() {
		m.stopGitDiff()
		return nil
	}
	if f.owner == m.active {
		return nil
	}
	m.stopGitDiff()
	ctx, cancel := context.WithCancel(m.ctx)
	m.gitDiffFeed = gitDiffPoll{feedLease: feedLease{token: m.token(), owner: m.active, ctx: ctx, cancel: cancel}}
	return m.scanGitDiff()
}

func (m *Model) scanGitDiff() tea.Cmd {
	backend, ok := m.backend.(gitDiffBackend)
	f := m.gitDiffFeed.feedLease
	if !ok || f.token == 0 || m.gitDiffFeed.pending {
		return nil
	}
	m.gitDiffFeed.pending, m.gitDiffFeed.tick, m.gitDiffFeed.last = true, 0, time.Now()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(f.ctx, gitDiffTimeout)
		defer cancel()
		if ctx.Err() != nil {
			return nil
		}
		s, err := backend.GitDiffSummary(ctx, f.owner, sdk.GitDiffHead)
		return gitDiffSummaryMsg{lease: f.token, value: s, err: err}
	}
}

// A commit observation can invalidate an idle poll; don't launch subprocesses
// for individual tokens or bursts of tool results.
func (m *Model) expediteGitDiff() tea.Cmd {
	if time.Since(m.gitDiffFeed.last) >= time.Second {
		return m.scanGitDiff()
	}
	return nil
}

func (m *Model) receivingGitDialog(token, request uint64) *dialog {
	for _, d := range m.overlays {
		if d.token == token && d.git != nil && d.git.request == request {
			return d
		}
	}
	return nil
}

func (m *Model) receiveGitSummary(msg gitDiffSummaryMsg) tea.Cmd {
	if msg.dialog != 0 {
		if d := m.receivingGitDialog(msg.dialog, msg.request); d != nil {
			d.git.stop()
			d.git.apply(msg.value, msg.err)
		}
		return nil
	}
	f := m.gitDiffFeed
	if f.token == 0 || msg.lease != f.token || f.owner != m.active {
		return nil
	}
	m.gitDiffFeed.pending = false
	t := m.threads[f.owner]
	if msg.err != nil {
		t.gitDiff = gitDiffState{err: msg.err.Error()}
	} else {
		t.gitDiff = gitDiffState{summary: msg.value, badge: gitDiffBadge(msg.value)}
	}
	if d := m.focusedDialog(); d != nil && d.git != nil && !d.git.patch && d.git.rangeName == sdk.GitDiffHead && d.git.cancel == nil {
		d.git.apply(msg.value, msg.err)
	}
	interval := 10 * time.Second
	if msg.err == nil && threadBusy(t) {
		interval = 1200 * time.Millisecond
	}
	tick := m.token()
	m.gitDiffFeed.tick = tick
	return tea.Tick(interval, func(time.Time) tea.Msg { return gitDiffTick(tick) })
}

type gitReview struct {
	rangeName string
	list      sdk.GitDiffSummary
	selected  int
	patch     bool
	view      *gitPatchView
	err       string
	request   uint64
	cancel    context.CancelFunc
}

func (g *gitReview) stop() {
	if g.cancel != nil {
		g.cancel()
		g.cancel = nil
	}
	g.request = 0
}

func (g *gitReview) apply(summary sdk.GitDiffSummary, err error) {
	g.err = ""
	if err != nil {
		g.err = err.Error()
		g.list, g.selected = sdk.GitDiffSummary{}, 0
		return
	}
	path := ""
	if g.selected < len(g.list.Files) {
		path = g.list.Files[g.selected].Path
	}
	g.list = summary
	if i := slices.IndexFunc(summary.Files, func(f sdk.GitDiffFile) bool { return f.Path == path }); i >= 0 {
		g.selected = i
	} else {
		g.selected = min(g.selected, max(0, len(summary.Files)-1))
	}
}

func (m *Model) openGitDiff(r ownerRoute) tea.Cmd {
	if !m.foreground(r) || !r.owner.Valid() {
		return nil
	}
	state := m.threads[r.owner].gitDiff
	d := &dialog{token: m.token(), route: r, git: &gitReview{rangeName: sdk.GitDiffHead, list: state.summary, err: state.err}}
	open := m.pushDialog(d)
	if m.gitDiffFeed.token == 0 {
		return tea.Batch(open, m.syncGitDiff())
	}
	return tea.Batch(open, m.expediteGitDiff())
}

func (m *Model) fetchGitReview(d *dialog, file bool) tea.Cmd {
	backend, ok := m.backend.(gitDiffBackend)
	if !ok {
		return nil
	}
	g, owner := d.git, d.route.owner
	g.stop()
	g.err = ""
	ctx, cancel := context.WithTimeout(m.ctx, gitDiffTimeout)
	req, token := m.token(), d.token
	g.request, g.cancel = req, cancel
	if !file {
		rangeName := g.rangeName
		return func() tea.Msg {
			defer cancel()
			v, err := backend.GitDiffSummary(ctx, owner, rangeName)
			return gitDiffSummaryMsg{dialog: token, request: req, value: v, err: err}
		}
	}
	g.patch, g.view = true, nil
	revision, selected := g.list.Revision, g.selected
	return func() tea.Msg {
		defer cancel()
		v, err := backend.GitDiffFile(ctx, owner, revision, selected)
		return gitDiffPatchMsg{dialog: token, request: req, value: v, err: err}
	}
}

func (m *Model) receiveGitPatch(msg gitDiffPatchMsg) {
	d := m.receivingGitDialog(msg.dialog, msg.request)
	if d == nil {
		return
	}
	g := d.git
	g.stop()
	if msg.err != nil {
		g.err = msg.err.Error()
		if e, ok := msg.err.(*sdk.ServerError); ok && e.Code == "invalid_cursor" {
			g.err = "File changed or list expired · esc back, u refresh"
		}
		return
	}
	g.view = newGitPatchView(msg.value)
}

func (m *Model) gitDiffKey(d *dialog, chord string) tea.Cmd {
	g := d.git
	if chord == "esc" && g.patch {
		g.stop()
		g.patch, g.view, g.err = false, nil, ""
		if g.rangeName == sdk.GitDiffHead {
			state := m.threads[d.route.owner].gitDiff
			if state.summary.Revision != "" {
				g.apply(state.summary, nil)
			}
		}
		return nil
	}
	if chord == "u" && !g.patch {
		return m.fetchGitReview(d, false)
	}
	if chord == "tab" && !g.patch && (g.list.Checkpoint || g.rangeName == sdk.GitDiffCheckpoint) {
		if g.rangeName == sdk.GitDiffHead {
			g.rangeName = sdk.GitDiffCheckpoint
		} else {
			g.rangeName = sdk.GitDiffHead
		}
		return m.fetchGitReview(d, false)
	}
	if g.patch {
		switch chord {
		case "n", "p":
			delta := 1
			if chord == "p" {
				delta = -1
			}
			if i := g.selected + delta; i >= 0 && i < len(g.list.Files) {
				g.selected = i
				return m.fetchGitReview(d, true)
			}
		case "up", "down", "pgup", "pgdown":
			step := 1
			if chord == "pgup" || chord == "pgdown" {
				step = max(1, m.frame.dialogHeight-4)
			}
			if chord == "pgup" || chord == "up" {
				step = -step
			}
			m.scrollGitDiff(d, step)
		}
		return nil
	}
	switch chord {
	case "up", "down":
		delta := 1
		if chord == "up" {
			delta = -1
		}
		g.selected = max(0, min(len(g.list.Files)-1, g.selected+delta))
	case "enter":
		if g.cancel == nil && g.selected < len(g.list.Files) {
			return m.fetchGitReview(d, true)
		}
	}
	return nil
}

func (m *Model) gitDiffMouse(d *dialog, msg tea.Msg, row int) tea.Cmd {
	switch v := msg.(type) {
	case tea.MouseClickMsg:
		if v.Button != tea.MouseLeft || row >= len(m.frame.dialogChoices) {
			return nil
		}
		target := m.frame.dialogChoices[row]
		if target == gitRangeTarget {
			return m.gitDiffKey(d, "tab")
		}
		if target >= 0 && !d.git.patch && d.git.cancel == nil && target < len(d.git.list.Files) {
			d.git.selected = target
			return m.gitDiffKey(d, "enter")
		}
	case tea.MouseWheelMsg:
		if v.Button == tea.MouseWheelUp {
			m.scrollGitDiff(d, -3)
		} else if v.Button == tea.MouseWheelDown {
			m.scrollGitDiff(d, 3)
		}
	}
	return nil
}

func (m *Model) scrollGitDiff(d *dialog, delta int) {
	if !d.git.patch {
		d.git.selected = max(0, min(len(d.git.list.Files)-1, d.git.selected+delta))
		return
	}
	if v := d.git.view; v != nil && m.dialogVisible(d) {
		w := m.frame.dialogWidth
		if m.frame.dialogFramed {
			w -= 4
		}
		v.move(w, delta)
	}
}

func (m *Model) gitReviewRows(g *gitReview, w, h int) ([]string, []int) {
	if h <= 0 {
		return nil, nil
	}
	var rows []string
	var targets []int
	add := func(s string, target int) {
		rows = append(rows, ansi.Truncate(s, w, "…"))
		targets = append(targets, target)
	}
	base := "HEAD"
	if g.rangeName == sdk.GitDiffCheckpoint {
		base = "last checkpoint"
	}
	target := -1
	title := "Worktree changes · " + base
	footer := m.keyHint("enter", "open") + "  " + m.keyHint("esc", "close")
	if g.patch {
		file := g.list.Files[g.selected]
		title = fmt.Sprintf("%s · %d/%d · %s", oneLine(file.Path), g.selected+1, len(g.list.Files), base)
		footer = m.keyHint("n/p", "next/previous file") + "  " + m.keyHint("esc", "back")
	} else if g.list.Checkpoint || g.rangeName == sdk.GitDiffCheckpoint {
		title += " ▾"
		target = gitRangeTarget
		footer += "  " + m.keyHint("tab", "range")
	}
	if !g.patch {
		footer += "  " + m.keyHint("u", "refresh")
	}
	add(paint(x.Accent, title), target)
	body := h - 1
	if body >= 2 {
		body--
	} // leave the smallest views one useful body row
	if body > 0 {
		switch {
		case g.cancel != nil || !g.patch && g.err == "" && g.list.Revision == "":
			add(m.control("Loading…"), -1)
		case g.err != "":
			add(paint(x.Warning, oneLine(g.err)), -1)
		case g.patch && g.view != nil:
			for _, row := range m.gitPatchRows(g.view, w, body) {
				add(row, -1)
			}
			if g.view.truncated {
				footer += "  " + paint(x.Warning, "preview clipped")
			}
		case len(g.list.Files) == 0:
			add(paint(x.Muted, "No changes"), -1)
		case !g.patch:
			lo := min(max(0, g.selected-body+1), max(0, len(g.list.Files)-body))
			for i := lo; i < min(lo+body, len(g.list.Files)); i++ {
				f := g.list.Files[i]
				prefix := "  "
				if i == g.selected {
					prefix = paint(x.Accent, "› ")
				}
				status, tone := f.Status, x.Warning
				switch status {
				case "A", "?":
					tone = x.Good
				case "D":
					tone = x.Danger
				}
				var stats string
				if f.Added != nil && f.Removed != nil {
					stats = paint(x.Good, "+"+strconv.FormatInt(*f.Added, 10)) + " " + paint(x.Danger, "−"+strconv.FormatInt(*f.Removed, 10))
				} else {
					stats = paint(x.Muted, "—")
				}
				if f.Staged {
					stats = m.control("index") + "  " + stats
				}
				left := prefix + paint(tone, status) + "  " + oneLine(f.Path)
				room := max(5, w-stringWidth(stats)-2)
				add(fit(ansi.Truncate(left, room, "…"), room)+"  "+stats, i)
			}
		}
	}
	if g.patch {
		for len(rows) < h-1 {
			add("", -1)
		}
	}
	if len(rows) < h && h >= 3 {
		add(m.hintRow(w, footer, m.keyHint("esc", "back"), ""), -1)
	} else if g.patch && len(rows) < h {
		add("", -1)
	}
	return rows, targets
}

type gitPatchView struct {
	lines     []sdk.GitDiffLine
	truncated bool
	top, part int
	cache     *gitPatchLayout
}

type gitPatchLayout struct {
	width   int
	wrapped map[int][]string
}

func newGitPatchView(p sdk.GitDiffPatch) *gitPatchView {
	lines := slices.Clone(p.Lines)
	for i := range lines {
		lines[i].Text = oneLine(lines[i].Text)
	}
	return &gitPatchView{lines: lines, truncated: p.Truncated, cache: &gitPatchLayout{}}
}

const gitLineGutter = 9

func (v *gitPatchView) parts(i, width int) []string {
	cache := v.cache
	if cache.width != width || cache.wrapped == nil {
		cache.width, cache.wrapped = width, make(map[int][]string)
	}
	p := cache.wrapped[i]
	if p == nil {
		l := v.lines[i]
		if l.Kind != "hunk" && l.Kind != "note" {
			width -= gitLineGutter
		}
		p = strings.Split(ansi.Hardwrap(l.Text, max(1, width), true), "\n")
		cache.wrapped[i] = p
	}
	return p
}

func (v *gitPatchView) move(width, delta int) {
	if len(v.lines) == 0 {
		return
	}
	v.part = min(v.part, len(v.parts(v.top, width))-1)
	for ; delta > 0; delta-- {
		if v.part+1 < len(v.parts(v.top, width)) {
			v.part++
		} else if v.top+1 < len(v.lines) {
			v.top++
			v.part = 0
		}
	}
	for ; delta < 0; delta++ {
		if v.part > 0 {
			v.part--
		} else if v.top > 0 {
			v.top--
			v.part = len(v.parts(v.top, width)) - 1
		}
	}
}

func (m *Model) gitPatchRows(v *gitPatchView, width, height int) []string {
	rows := make([]string, 0, height)
	for i := v.top; i < len(v.lines) && len(rows) < height; i++ {
		line, parts := v.lines[i], v.parts(i, width)
		part := 0
		if i == v.top {
			part = min(v.part, len(parts)-1)
		}
		for ; part < len(parts) && len(rows) < height; part++ {
			text := parts[part]
			if line.Kind == "hunk" || line.Kind == "note" {
				rows = append(rows, paint(x.Muted, text))
				continue
			}
			n, sign, tone, color := line.NewLine, " ", x.Plain, 0
			switch line.Kind {
			case "add":
				sign, tone, color = "+", x.Good, 2
			case "remove":
				n, sign, tone, color = line.OldLine, "−", x.Danger, 1
			}
			prefix := strings.Repeat(" ", gitLineGutter)
			if part == 0 {
				number := strconv.Itoa(n)
				if len(number) > 6 {
					number = "…" + number[len(number)-5:]
				}
				prefix = paint(x.Muted, fmt.Sprintf("%6s", number)) + " " + paint(tone, sign+" ")
			}
			row := prefix + paint(tone, text)
			if color != 0 && m.theme.Fills {
				row = fill(fit(row, width), blendPalette(m.palette.background, m.palette.colors[color], 0.20))
			}
			rows = append(rows, row)
		}
	}
	return rows
}
