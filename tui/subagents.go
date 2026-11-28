package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const subagentsID x.ActionID = coreID + ".subagents"
const openSubagentsOp x.LocalOp = "subagents.open"
const subagentOpenTarget = -22

type subagentBackend interface {
	WatchSubagents(context.Context, sdk.ID) <-chan sdk.SubagentUpdate
}

type subagentFeed struct {
	feedLease
	update  sdk.SubagentUpdate
	probe   uint64
	running int
	failed  bool
	ready   bool
}

type subagentMsg struct {
	lease  uint64
	source <-chan sdk.SubagentUpdate
	update sdk.SubagentUpdate
	ok     bool
}

func (m *Model) stopSubagents() {
	if m.subagents.cancel != nil {
		m.subagents.cancel()
	}
	m.subagents = subagentFeed{}
}

func (m *Model) syncSubagents() tea.Cmd {
	backend, ok := m.backend.(subagentBackend)
	if !ok || !m.active.Valid() || !m.menuFeaturesFromBackend.Has(x.Subagents) {
		m.stopSubagents()
		return nil
	}
	if m.subagents.owner != m.active {
		m.stopSubagents()
	}
	if m.subagents.token != 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	lease := feedLease{m.token(), m.active, ctx, cancel}
	m.subagents.feedLease, m.subagents.probe = lease, m.probe.latest
	return func() tea.Msg { return awaitSubagents(lease, backend.WatchSubagents(ctx, lease.owner))() }
}

func awaitSubagents(lease feedLease, source <-chan sdk.SubagentUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-lease.ctx.Done():
			return nil
		case update, ok := <-source:
			return subagentMsg{lease.token, source, update, ok}
		}
	}
}

func (m *Model) receiveSubagents(msg subagentMsg) tea.Cmd {
	f := &m.subagents
	if msg.lease == 0 || msg.lease != f.token || f.owner != m.active {
		return nil
	}
	defer func() {
		for _, d := range m.overlays {
			if d.usage && m.foreground(d.route) {
				m.refreshUsage(d, m.threads[d.route.owner])
			}
		}
	}()
	if !msg.ok {
		f.cancel()
		f.feedLease = feedLease{owner: f.owner}
		f.failed = true
		if m.probe.known && m.probe.latest > f.probe {
			return m.syncSubagents()
		}
		return nil
	}
	if msg.update.Err != nil {
		f.failed = true
		return awaitSubagents(f.feedLease, msg.source)
	}
	previous, spent := f.update.Workers, f.update.Usage
	f.update, f.failed, f.ready = msg.update, false, true
	if f.update.Usage != spent {
		// Subagent spend is part of the thread's usage events, also when the
		// parent's own usage did not change.
		m.emit(f.owner, x.Event{Kind: x.UsageChanged})
	}
	f.running = 0
	for _, row := range f.update.Workers {
		if row.Active() {
			f.running++
		}
	}
	changed := !slices.EqualFunc(previous, f.update.Workers, func(a, b sdk.Subagent) bool { return a.ID == b.ID })
	for _, d := range m.overlays {
		if d.subagent == nil || !m.foreground(d.route) {
			continue
		}
		if d.subagent.id.Valid() {
			m.refreshSubagent(d)
		} else if changed {
			d.selected = max(0, min(d.selected, len(f.update.Workers)-1))
			d.token = m.token() // don't apply an old row hit to a replacement roster
		}
	}
	return awaitSubagents(f.feedLease, msg.source)
}

func (m *Model) subagentBadge(room int) displayRow {
	f := m.subagents
	if f.owner != m.active || len(f.update.Workers) == 0 || !m.menuFeaturesFromBackend.Has(x.Subagents) {
		return displayRow{}
	}
	short := fmt.Sprintf("Subagents %d", len(f.update.Workers))
	label := short
	if f.failed {
		label += " · offline"
	} else if f.running > 0 {
		label += fmt.Sprintf(" · %d running", f.running)
	}
	if stringWidth(label) > room {
		label = short
	}
	if w := stringWidth(label); w <= room {
		return displayRow{text: m.quiet(label), hits: []spanHit{{0, w, x.ActionRef{ID: subagentsID}, "inspect subagents"}}}
	}
	return displayRow{}
}

type subagentPanel struct {
	id     sdk.ID // empty for the roster
	row    sdk.Subagent
	viewer *outputViewer
	follow bool
	gone   bool
}

func (m *Model) openSubagents(r ownerRoute) tea.Cmd {
	if !m.foreground(r) || !r.owner.Valid() || !m.menuFeaturesFromBackend.Has(x.Subagents) {
		return nil
	}
	d := &dialog{route: r, token: m.token(), subagent: &subagentPanel{}}
	d.selected = max(0, slices.IndexFunc(m.subagents.update.Workers, sdk.Subagent.Active))
	return m.pushDialog(d)
}

func (m *Model) refreshSubagent(d *dialog) {
	p := d.subagent
	if i := slices.IndexFunc(m.subagents.update.Workers, func(w sdk.Subagent) bool { return w.ID == p.id }); i >= 0 {
		p.gone = false
		next := m.subagents.update.Workers[i]
		if p.viewer == nil || p.row.Text != next.Text || p.row.Truncated != next.Truncated {
			text := next.Text
			if next.Truncated {
				text = "Earlier output omitted\n\n" + text
			}
			old := p.viewer
			p.viewer = newOutputViewer(text)
			if old != nil && !p.follow {
				anchor := old.reviewAnchor()
				if !strings.HasPrefix(text, old.raw) {
					// A bounded tail may have dropped earlier lines, or a
					// provider retry may have replaced them altogether.
					fragment := old.lines[min(old.top, len(old.lines)-1)].text
					anchor = 0
					if fragment != "" {
						anchor = max(0, strings.Index(safeText(text), fragment))
					}
				}
				p.viewer.reflowReview(old.reviewWidth)
				p.viewer.seekReview(anchor)
			}
		}
		p.row = next
	} else {
		p.gone = true
	}
}

// Glyphs and tones follow docs/ui-style.md. A finished worker's result is
// input to its parent, not an outcome of record, so its ✓ is quiet.
func subagentStatus(s string) (icon, label string, tone x.Tone) {
	switch s {
	case "starting":
		return "○", "Starting", x.Muted
	case "reading":
		return "◐", "Reading files", x.Warning
	case "working":
		return "◐", "Running tools", x.Warning
	case "responding":
		return "◐", "Responding", x.Warning
	case "waiting":
		return "◐", "Waiting for model", x.Warning
	case "retrying":
		return "⏸", "Retrying", x.Warning
	case "ok":
		return "✓", "Done", x.Muted
	case "stopping":
		return "⏸", "Stopping", x.Warning
	case "cancelled":
		return "✗", "Cancelled", x.Muted
	case "timed_out":
		return "✗", "Timed out", x.Danger
	case "failed":
		return "✗", "Failed", x.Danger
	default:
		return "⏸", "Unresolved", x.Warning
	}
}

func spendLabel(u sdk.Usage) string {
	return fmt.Sprintf("%s / $%.2f", tokenLabel(u.TotalTokens), u.CostUSD)
}

func (m *Model) subagentRows(d *dialog, w, n int) menuFrame {
	p, f := d.subagent, m.subagents
	keys := chromeLine(m.menuKeys(w, "↑↓ move", "tab log", d.zoomKey()))
	c := menuChrome{title: "Subagents", scope: "Latest delegated turn", count: fmt.Sprintf("%d running", f.running), keys: keys,
		consequence: chromeLine(m.quiet("No subagent log to open"))}
	if f.failed {
		c.scope = "Last observed · disconnected"
	}
	if p.id.Valid() {
		row := p.row
		icon, status, tone := subagentStatus(row.Status)
		if p.gone {
			icon, status, tone = "○", "No longer followed", x.Muted
		}
		c.title, c.scope, c.count = "Subagent log", "", spendLabel(row.Usage)
		if f.failed {
			c.scope = "Disconnected"
		}
		c.consequence = chromeLine(m.quiet("Public output only"))
		c.keys = chromeLine(m.menuKeys(w, "↑↓ scroll", "end follow", d.zoomKey()))
		return m.menuShell(w, n, c, func(h int) menuFrame {
			var out menuFrame
			out.add(strongText(oneLine(row.Title)), -1)
			if h > 1 {
				out.add(m.paint(tone, icon+" "+status), -1)
			}
			if h > 2 && row.Detail != "" {
				out.add(m.quiet(oneLine(row.Detail)), -1)
			}
			h -= len(out.rows)
			if h <= 0 {
				return out
			}
			if row.Text == "" {
				out.add(m.quiet("No public output yet"), -1)
				return out
			}
			v := p.viewer
			if v == nil {
				return out
			}
			if v.reviewWidth != w {
				anchor := v.reviewAnchor()
				v.reflowReview(w)
				v.seekReview(anchor)
			}
			end := max(0, len(v.lines)-h)
			top := min(v.top, end)
			if p.follow {
				top = end
			}
			for i := top; i < min(len(v.lines), top+h); i++ {
				out.add(v.window(i, 0, w), -1)
			}
			return out
		})
	}
	rows := f.update.Workers
	if len(rows) == 0 {
		return m.menuShell(w, n, c, func(int) menuFrame {
			var out menuFrame
			label := "No subagents for this thread"
			if f.failed {
				label = "Subagent activity unavailable"
			}
			out.add(m.quiet(label), -1)
			return out
		})
	}
	sel := max(0, min(len(rows)-1, d.selected))
	c.consequence = menuLine{m.menuAction("Open log", "Follow public output"), subagentOpenTarget}
	return m.menuShell(w, n, c, func(h int) menuFrame {
		return m.menuColumns(w, h, func(width, height int) []string {
			row := rows[sel]
			_, status, _ := subagentStatus(row.Status)
			mode := "Can edit"
			if row.ReadOnly {
				mode = "Read-only"
			}
			return m.menuDetail(oneLine(row.Title), strings.TrimSpace(strings.Join([]string{status + " · " + mode + " · " + spendLabel(row.Usage), oneLine(row.CWD), oneLine(row.Detail)}, "\n")), width, height)
		}, func(width, height int) menuFrame {
			start, end := menuWindow(sel, len(rows), height)
			var list menuFrame
			for i := start; i < end; i++ {
				row := rows[i]
				icon, label, tone := subagentStatus(row.Status)
				switch row.Status {
				case "waiting":
					label = "Waiting"
				case "reading":
					label = "Reading"
				case "working":
					label = "Tools"
				}
				list.add(m.menuRow(width, 10, menuItem{title: oneLine(row.Title), icon: icon, state: label, tone: tone, focus: i == sel}), i)
			}
			return list
		})
	})
}

func (m *Model) subagentKey(d *dialog, chord string) tea.Cmd {
	p := d.subagent
	if p.id.Valid() {
		v := p.viewer
		if v == nil {
			return nil
		}
		_, h := m.outputSize(d)
		h = max(1, h-2)
		if p.row.Detail != "" {
			h = max(1, h-1)
		}
		end := max(0, len(v.lines)-h)
		switch chord {
		case "end":
			p.follow = true
		case "up", "down", "pgup", "pgdown", "home":
			if p.follow {
				v.top = end
			}
			p.follow = false
			delta := map[string]int{"up": -1, "down": 1, "pgup": -h, "pgdown": h, "home": -v.top}[chord]
			v.top = max(0, min(end, v.top+delta))
		}
		return nil
	}
	rows := m.subagents.update.Workers
	if len(rows) == 0 {
		return nil
	}
	switch chord {
	case "up", "down", "pgup", "pgdown", "home", "end":
		delta := map[string]int{"up": -1, "down": 1, "pgup": -5, "pgdown": 5, "home": -d.selected, "end": len(rows)}[chord]
		d.selected = max(0, min(len(rows)-1, d.selected+delta))
	case "enter", "tab":
		next := &dialog{route: d.route, token: m.token(), subagent: &subagentPanel{id: rows[d.selected].ID, follow: true}}
		m.refreshSubagent(next)
		return m.pushDialog(next)
	}
	return nil
}
