package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const openThreadPickerOp x.LocalOp = "thread.picker"
const threadSearchDelay = 120 * time.Millisecond

type threadPicker struct {
	all, searchable, failed bool
	cancel                  context.CancelFunc
	cursor                  string
	rows                    []threadPickerRow // aligned with the dialog choices
	creation                *workspaceThreadCreation
}

type threadPickerRow struct {
	workspaceID, workspaceName string
	status                     x.ThreadStatus
	destination                *workspaceThreadTarget
	directory                  string // a choice in the directory picker
}

func (p *threadPicker) selectedRow(d *dialog) threadPickerRow {
	if d.selected < len(d.filtered) {
		return p.rows[d.filtered[d.selected]]
	}
	return threadPickerRow{}
}

type threadPickerResult struct {
	token  uint64
	op     x.ListThreads
	result x.ClientResult
}

func (p *threadPicker) stop() {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}

func (m *Model) openThreadPicker(r ownerRoute, all bool, budget *int) tea.Cmd {
	if !m.foreground(r) {
		return nil
	}
	d := &dialog{route: r, budget: budget, placement: x.Sheet, title: "Switch thread",
		threadPicker: &threadPicker{all: all}, actions: []x.ActionID{newThreadID, forkPickerID, coreID + ".close", renameID}}
	d.done = func(_ x.Context, value any, accepted bool) x.Outcome {
		if !accepted {
			return nil
		}
		id, _ := value.(string)
		if id == "new" {
			return x.Outcome{x.Dispatch{Action: x.ActionRef{ID: newThreadID}}}
		}
		return x.Outcome{x.Client{Op: x.Switch{ThreadID: sdk.ID(id)}}}
	}
	return tea.Batch(m.pushDialog(d), m.loadThreadPicker(d, "", 0))
}

func (m *Model) loadThreadPicker(d *dialog, cursor string, delay time.Duration) tea.Cmd {
	p := d.threadPicker
	p.stop()
	ctx, cancel := context.WithCancel(m.ctx)
	p.cancel, p.failed, p.rows = cancel, false, nil
	d.choices, d.filtered, d.selected = nil, nil, 0
	d.token = m.token() // old rows and mouse targets must not be selectable
	token := d.token
	op := x.ListThreads{Limit: 50, Cursor: cursor, LaunchScope: true, BrowseAll: p.all}
	if p.searchable {
		op.Query = strings.TrimSpace(d.query)
	}
	var run func(context.Context) (any, error)
	if p.creation != nil {
		source, _ := m.backend.(workspaceDirectoryBackend)
		target := p.creation.target
		run = func(ctx context.Context) (any, error) {
			return workspaceThreadPage(ctx, source, target.id, target.scope, op.Cursor, op.Limit), nil
		}
	} else {
		run = m.clientJob(d.route, op)
	}
	return func() tea.Msg {
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		value, err := run(call)
		result, _ := value.(x.ClientResult)
		if err != nil {
			result.Err = err
		}
		return threadPickerResult{token: token, op: op, result: result}
	}
}

func (m *Model) receiveThreadPicker(v threadPickerResult) tea.Cmd {
	// Results can arrive under an overlay, but not after their picker was
	// closed or the active thread changed.
	d := m.sheet
	if d == nil || d.token != v.token {
		d = nil
		for _, overlay := range m.overlays {
			if overlay.token == v.token {
				d = overlay
				break
			}
		}
	}
	if d == nil || d.threadPicker == nil || !m.foreground(d.route) {
		return nil
	}
	p := d.threadPicker
	p.stop()
	d.token = m.token()
	r := v.result
	if p.creation != nil {
		return m.receiveWorkspaceDirectories(d, v.op.Cursor, r)
	}
	if r.Err != nil {
		p.failed = true
		p.addChoice(d, "refresh", "Retry thread list…", "", threadPickerRow{})
		if !p.all && m.options.WorkspaceLocal {
			p.addChoice(d, "all-workspaces", "Browse all workspaces…", "", threadPickerRow{})
		}
		d.filter()
		return nil
	}
	p.searchable = r.Workspaces != nil
	if p.searchable && strings.TrimSpace(d.query) != v.op.Query {
		// Input can arrive before the first list reports search support.
		return m.loadThreadPicker(d, "", threadSearchDelay)
	}
	// Open tabs can override display metadata. Use only the directory's
	// recorded paths, not the active tab's projected workspace, for creation.
	var directories map[sdk.ID]string
	if _, ok := m.backend.(workspaceDirectoryBackend); ok && p.all && p.searchable {
		directories = make(map[sdk.ID]string, len(r.Threads))
		for _, t := range r.Threads {
			if validWorkspaceDirectory(t.Workspace) {
				directories[t.ID] = t.Workspace
			}
		}
	}
	m.applyMenuResult(v.op, &r)
	p.setPage(d, m.active, r, directories)
	return nil
}

func (p *threadPicker) addChoice(d *dialog, id, label, detail string, row threadPickerRow) {
	d.choices = append(d.choices, x.Choice{ID: id, Label: limitText(label, 512), Description: limitText(detail, 1024)})
	p.rows = append(p.rows, row)
}

func (p *threadPicker) setPage(d *dialog, active sdk.ID, r x.ClientResult, directories map[sdk.ID]string) {
	d.choices, p.rows = d.choices[:0], p.rows[:0]
	p.cursor = r.Cursor
	d.title = "Switch thread · workspace unverified"
	switch {
	case r.ScopeUnavailable:
		d.title = "Switch thread · launch workspace scope unavailable"
	case r.WorkspaceScoped:
		d.title = "Switch thread · launch workspace"
	case p.all || p.searchable:
		d.title = "Switch thread · all workspaces"
	}
	if r.Indexing {
		d.title += " · indexing"
	}

	// The directory returns rows in recency order. Preserve that order within
	// each group, with the launch workspace first and other groups ordered by
	// their most recent match. Group by identity, not by the display name.
	type group struct {
		row     threadPickerRow
		indices []int
	}
	groups := make([]group, 0, len(r.Threads))
	byID := make(map[string]int)
	launch := -1
	for i := range r.Threads {
		row := threadPickerRow{workspaceID: "unknown", workspaceName: "Workspace unknown"}
		match := ""
		if i < len(r.WorkspaceMatches) {
			match = r.WorkspaceMatches[i]
		}
		if i < len(r.Workspaces) && r.Workspaces[i] != nil {
			row.workspaceID, row.workspaceName = r.Workspaces[i].ID, r.Workspaces[i].Name
		} else if match == "inside" {
			row.workspaceID, row.workspaceName = "inside", "Launch workspace"
		} else if match == "outside" {
			row.workspaceID, row.workspaceName = "outside", "Other workspaces"
		}
		at, exists := byID[row.workspaceID]
		if !exists {
			at = len(groups)
			byID[row.workspaceID] = at
			groups = append(groups, group{row: row})
		}
		if match == "inside" {
			launch = at
		}
		groups[at].indices = append(groups[at].indices, i)
	}
	addGroup := func(g group) {
		g.row.destination = workspaceThreadDestination(g.row, g.indices, r, directories)
		if dest := g.row.destination; dest != nil {
			detail := "Choose directory…"
			if dest.preview != "" {
				detail = workspaceDirectoryLabel(dest.preview)
			}
			p.addChoice(d, newThreadHereChoice, "+ New thread here", detail, g.row)
		}
		for _, i := range g.indices {
			t := r.Threads[i]
			row := g.row
			row.status = t.Status
			label := threadTitle(t)
			if t.Forked {
				label = "⑂ " + label
			}
			if t.ID == active {
				label += " · current"
			}
			p.addChoice(d, string(t.ID), label, threadChoiceDetail(t, r.Directory), row)
		}
	}
	if launch >= 0 {
		addGroup(groups[launch])
	}
	for i, g := range groups {
		if i != launch {
			addGroup(g)
		}
	}
	// Keep navigation available while filtering, including when the current
	// workspace has no matches. It carries the query into the next request.
	if !p.all && (strings.TrimSpace(d.query) == "" || !p.searchable) {
		p.addChoice(d, "new", "+ New thread", "", threadPickerRow{})
	}
	if r.Indexing {
		p.addChoice(d, "refresh", "Refresh thread list…", "", threadPickerRow{})
	} else if r.Cursor != "" {
		label := "More threads…"
		if r.Directory {
			label = "Older threads…"
			if strings.TrimSpace(d.query) != "" && p.searchable {
				label = "Older matching threads…"
			}
		}
		p.addChoice(d, pageChoice, label, "", threadPickerRow{})
	}
	if !p.all && (r.WorkspaceScoped || r.ScopeUnavailable) {
		p.addChoice(d, "all-workspaces", "Browse all workspaces…", "", threadPickerRow{})
	}
	d.filter()
}

func threadChoiceDetail(t x.Thread, directory bool) string {
	var parts []string
	if directory && !t.LastMessageAt.IsZero() {
		parts = append(parts, t.LastMessageAt.Format("2006-01-02"))
	}
	if directory && t.Title != "" {
		parts = append(parts, "#"+string(t.ID))
	}
	if t.ParentID != "" {
		parts = append(parts, "fork of #"+string(t.ParentID))
	}
	if directory && t.Branch != "" {
		parts = append(parts, "⎇ "+t.Branch)
	}
	if t.Preview != "" {
		parts = append(parts, t.Preview)
	}
	return strings.Join(parts, " · ")
}

func threadPickerStatus(status x.ThreadStatus) (icon, label string, tone x.Tone) {
	if status == "" {
		return "", "", x.Plain
	}
	icon, tone = tabStatus(status)
	switch status {
	case x.Running:
		return icon, "running", x.Accent
	case x.Parked, x.Failed:
		return icon, "needs attention", tone
	case x.Stopped:
		return icon, "stopped", x.Plain
	default:
		return icon, string(status), tone
	}
}

func (p *threadPicker) message(d *dialog) string {
	if p.creation != nil {
		switch {
		case p.cancel != nil:
			return "Loading working directories…"
		case p.failed:
			return "Could not load working directories"
		}
		return ""
	}
	query := strings.TrimSpace(d.query)
	switch {
	case p.failed:
		return "Could not load threads"
	case p.cancel != nil && query != "":
		return "Searching threads…"
	case p.cancel != nil:
		return "Loading threads…"
	case query == "" || len(d.filtered) > 0 && p.rows[d.filtered[0]].workspaceID != "":
		return ""
	case !p.searchable:
		return "No matches on this page"
	default:
		return "No matching threads"
	}
}

func (m *Model) threadPickerRows(d *dialog, w, budget int, add func(string, int)) (lo, hi int) {
	p := d.threadPicker
	count := len(d.filtered)
	if count == 0 || budget <= 0 {
		return 0, 0
	}
	// Fit the selection and its heading together. Work backwards only as far
	// as can fit, then draw forwards; chrome is never a selection target.
	selected := min(d.selected, count-1)
	grouped := budget > 1
	used, next := 0, ""
	lo = selected
	for i := selected; i >= 0; i-- {
		group := p.rows[d.filtered[i]].workspaceID
		cost := 1
		if grouped && group != "" && group != next {
			cost++
		}
		if used+cost > budget {
			break
		}
		lo, used, next = i, used+cost, group
	}
	previous := ""
	for hi = lo; hi < count && budget > 0; hi++ {
		at := d.filtered[hi]
		row := p.rows[at]
		if grouped && row.workspaceID != "" && row.workspaceID != previous {
			if budget < 2 {
				break // never leave an orphaned workspace heading
			}
			name := ansi.Truncate("  "+oneLine(row.workspaceName), w, "…")
			add("\x1b[1m"+m.control(name)+"\x1b[22m", -1)
			budget--
		}
		previous = row.workspaceID
		add(m.threadPickerLine(d.choices[at], row, hi == selected, w), hi)
		budget--
	}
	return lo, hi
}

func (m *Model) threadPickerLine(item x.Choice, row threadPickerRow, selected bool, w int) string {
	prefix, tone := "  ", x.Plain
	if selected {
		prefix, tone = "› ", x.Accent
	}
	if row.workspaceID != "" {
		prefix = "  " + prefix
	}
	icon, label, statusTone := threadPickerStatus(row.status)
	if icon != "" {
		prefix += icon + " "
	}
	room := w
	if label != "" {
		room = max(1, w-stringWidth(label)-2)
	}
	labelText := oneLine(item.Label)
	if row.directory != "" {
		labelText = shortenDirectory(labelText, room-stringWidth(prefix))
	}
	line := paint(tone, ansi.Truncate(prefix+labelText, room, "…"))
	if label != "" {
		line += "  " + paint(statusTone, label)
	}
	sep := "  "
	if label != "" {
		sep = " · "
	}
	if room = w - stringWidth(line) - stringWidth(sep); room > 1 && item.Description != "" {
		detail := oneLine(item.Description)
		if item.ID == newThreadHereChoice && row.destination.preview != "" {
			detail = shortenDirectory(detail, room)
		}
		line += m.control(sep + ansi.Truncate(detail, room, "…"))
	}
	return line
}
