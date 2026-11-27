package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const workerActivityID x.ActionID = coreID + ".workers"
const openWorkersOp x.LocalOp = "workers.open"
const workerPrimaryTarget = -20
const workerKeysTarget = -21

type workerActivityBackend interface {
	WatchReviewActivity(context.Context, sdk.ID) <-chan sdk.ReviewActivityUpdate
	InspectReviewWorker(context.Context, sdk.ID, string, bool) (sdk.ReviewInspection, error)
	StopReviewWorker(context.Context, sdk.ID, string) (sdk.ReviewWorker, error)
}

type workerFeed struct {
	feedLease
	probe  uint64 // last successful capability probe when this watch started
	rows   []sdk.ReviewWorkerActivity
	ready  bool
	failed bool
}

type workerFeedMsg struct {
	lease  uint64
	source <-chan sdk.ReviewActivityUpdate
	update sdk.ReviewActivityUpdate
	ok     bool
}

func (m *Model) stopWorkerActivity() {
	if m.workers.cancel != nil {
		m.workers.cancel()
	}
	m.workers = workerFeed{}
}

func (m *Model) syncWorkerActivity() tea.Cmd {
	backend, ok := m.backend.(workerActivityBackend)
	if !ok || !m.active.Valid() || !m.menuFeaturesFromBackend.Has(x.WorkerActivity) {
		m.stopWorkerActivity()
		return nil
	}
	f := &m.workers
	if f.owner != m.active {
		m.stopWorkerActivity()
	}
	if f.token != 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	lease := feedLease{m.token(), m.active, ctx, cancel}
	f.feedLease, f.probe = lease, 0
	if m.probe.known {
		f.probe = m.probe.latest
	}
	return func() tea.Msg { return awaitWorkers(lease, backend.WatchReviewActivity(ctx, lease.owner))() }
}

func awaitWorkers(lease feedLease, source <-chan sdk.ReviewActivityUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-lease.ctx.Done():
			return nil
		case update, ok := <-source:
			return workerFeedMsg{lease.token, source, update, ok}
		}
	}
}

func (m *Model) receiveWorkers(msg workerFeedMsg) tea.Cmd {
	f := &m.workers
	if msg.lease == 0 || msg.lease != f.token || f.owner != m.active {
		return nil
	}
	if !msg.ok {
		f.cancel()
		f.feedLease = feedLease{owner: f.owner}
		f.failed = true // keep the last coherent view until the next baseline
		// The parent can renegotiate before EOF from this separate feed is
		// delivered. Allow one replacement for that newer probe, not a retry loop.
		if m.probe.known && m.probe.latest > f.probe {
			return m.syncWorkerActivity()
		}
		return nil
	}
	if msg.update.Err != nil {
		f.failed = true // the SDK still owns retries until the channel closes
	} else {
		next := msg.update.Workers
		if !slices.EqualFunc(f.rows, next, func(a, b sdk.ReviewWorkerActivity) bool { return a.Handle == b.Handle }) {
			for _, d := range m.overlays {
				if d.worker == nil || d.worker.handle != "" || !m.foreground(d.route) {
					continue
				}
				if d.selected >= 0 && d.selected < len(f.rows) {
					handle := f.rows[d.selected].Handle
					if i := slices.IndexFunc(next, func(w sdk.ReviewWorkerActivity) bool { return w.Handle == handle }); i >= 0 {
						d.selected = i
					}
				}
				d.selected = max(0, min(d.selected, len(next)-1))
				d.token = m.token() // old mouse rows address the prior roster
			}
		}
		f.rows, f.ready, f.failed = next, true, false
	}
	return awaitWorkers(f.feedLease, msg.source)
}

func (m *Model) workerBadgeRow(room int) displayRow {
	f := m.workers
	if f.owner != m.active || !f.ready || len(f.rows) == 0 {
		return displayRow{}
	}
	label := menuCount(len(f.rows), "worker", "workers")
	if w := stringWidth(label); w <= room {
		return displayRow{text: m.quiet(label), hits: []spanHit{{0, w, x.ActionRef{ID: workerActivityID}, "inspect review workers"}}}
	}
	return displayRow{}
}

type workerPanel struct {
	handle, name   string // empty handle is the roster
	transcript     bool
	cancel         context.CancelFunc
	request        uint64
	loading, ready bool
	stopping       bool
	note           string
	value          sdk.ReviewInspection
	viewer         *outputViewer
}

type workerInspectionMsg struct {
	token, request uint64
	value          sdk.ReviewInspection
	err            error
}

func (m *Model) openWorkers(r ownerRoute) tea.Cmd {
	if !m.foreground(r) || !r.owner.Valid() || !m.menuFeaturesFromBackend.Has(x.WorkerActivity) {
		return nil
	}
	d := &dialog{route: r, token: m.token(), worker: &workerPanel{}}
	rows := m.workers.rows
	d.selected = slices.IndexFunc(rows, func(w sdk.ReviewWorkerActivity) bool {
		return workerCanStop(w.State) || w.State == "stopping"
	})
	if d.selected < 0 {
		d.selected = max(0, len(rows)-1)
	}
	return m.pushDialog(d)
}

func (m *Model) openWorkerInspection(d *dialog, handle, name string, transcript bool) tea.Cmd {
	if len(m.overlays) >= maxDialogs {
		return nil
	}
	next := &dialog{route: d.route, token: m.token(), worker: &workerPanel{handle: handle, name: name, transcript: transcript}}
	return tea.Batch(m.pushDialog(next), m.fetchWorkerInspection(next))
}

func (m *Model) fetchWorkerInspection(d *dialog) tea.Cmd {
	backend, ok := m.backend.(workerActivityBackend)
	if !ok || !m.menuFeaturesFromBackend.Has(x.WorkerActivity) {
		return nil
	}
	p := d.worker
	if p.cancel != nil {
		p.cancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	p.cancel, p.request, p.loading, p.note = cancel, m.token(), true, ""
	token, request, owner, handle, transcript := d.token, p.request, d.route.owner, p.handle, p.transcript
	return func() tea.Msg {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		v, err := backend.InspectReviewWorker(call, owner, handle, transcript)
		return workerInspectionMsg{token, request, v, err}
	}
}

func (m *Model) receiveWorkerInspection(msg workerInspectionMsg) {
	for _, d := range m.overlays {
		p := d.worker
		if d.token != msg.token || p == nil || p.request != msg.request || !m.foreground(d.route) {
			continue
		}
		p.loading = false
		if msg.err != nil {
			p.note = "Inspection unavailable: " + oneLine(msg.err.Error())
			return
		}
		p.value, p.ready = msg.value, true
		p.viewer = newOutputViewer(workerInspectionText(msg.value))
		return
	}
}

func workerInspectionText(v sdk.ReviewInspection) string {
	var out strings.Builder
	_, state, _ := workerStateLabel(v.Worker.State)
	fmt.Fprintf(&out, "%s · advisory only\n", state)
	if v.Worker.Detail != "" {
		fmt.Fprintln(&out, v.Worker.Detail)
	}
	if v.Transcript != nil {
		if v.Transcript.Truncated {
			fmt.Fprintln(&out, "Earlier entries omitted.")
		}
		if len(v.Transcript.Entries) == 0 {
			fmt.Fprintln(&out, "No public transcript entries.")
		}
		for _, e := range v.Transcript.Entries {
			label := "Reviewer"
			if e.Kind == "activity" {
				label = "Activity"
			}
			fmt.Fprintf(&out, "\n%s\n%s\n", label, e.Text)
		}
		return out.String()
	}
	fmt.Fprintf(&out, "\nBrief: %s\n", v.Brief)
	if v.Worker.Report != "" {
		fmt.Fprintf(&out, "\nReport · advisory\n%s\n", v.Worker.Report)
	} else {
		fmt.Fprintln(&out, "No report committed.")
	}
	r := v.Receipt
	fmt.Fprintf(&out, "\nPinned evidence\nSnapshot: %s\nBase: %s\nModel: %s/%s · thinking %s\nRequest: %s · %s\n",
		r.SnapshotID, r.Base, r.Selection.Model.Provider, r.Selection.Model.ModelID, r.Selection.Thinking, r.RequestID, r.Mode)
	for _, p := range v.Scope.Tracked {
		fmt.Fprintf(&out, "Tracked: %s\n", p)
	}
	for _, p := range v.Scope.Untracked {
		fmt.Fprintf(&out, "Untracked: %s\n", p)
	}
	if a := v.Assessment; a != nil {
		for _, c := range a.Checks {
			fmt.Fprintf(&out, "\nCheck [%s]: %s\n", c.ID, c.Description)
		}
		for _, e := range a.Evidence {
			fmt.Fprintf(&out, "\nSupplied [%s]: %s (not rerun)\n%s\n", e.ID, e.Label, e.Text)
		}
	}
	return out.String()
}

func workerCanStop(state string) bool {
	// The backend checks identity even when the roster cannot resolve the outcome.
	return state == "running" || state == "reserved" || state == "unresolved"
}

func (m *Model) canRequestWorkerStop(d *dialog, state string) bool {
	return m.workers.owner == d.route.owner && m.workers.ready && !m.workers.failed && !d.worker.transcript &&
		!d.worker.stopping && workerCanStop(state)
}

func workerStateLabel(state string) (icon, label string, tone x.Tone) {
	switch state {
	case "reserved":
		return "○", "Reserved", x.Muted
	case "running":
		return "◐", "Running", x.Muted
	case "stopping":
		return "⏸", "Stopping", x.Warning
	case "succeeded":
		return "•", "Reported", x.Muted // not verified
	case "cancelled":
		return "○", "Cancelled", x.Muted
	case "timed_out":
		return "✗", "Timed out", x.Warning
	case "failed":
		return "✗", "Failed", x.Danger
	default:
		return "⏸", "Unresolved", x.Warning
	}
}

func (m *Model) workerCurrentState(p *workerPanel) string {
	for _, w := range m.workers.rows {
		if w.Handle == p.handle {
			return w.State
		}
	}
	return p.value.Worker.State
}

func (m *Model) workerKey(d *dialog, chord string) tea.Cmd {
	if !m.menuFeaturesFromBackend.Has(x.WorkerActivity) {
		return nil
	}
	p := d.worker
	if p.handle == "" {
		rows := m.workers.rows
		switch chord {
		case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
			delta := map[string]int{"up": -1, "k": -1, "down": 1, "j": 1, "pgup": -5, "pgdown": 5, "home": -d.selected, "end": len(rows)}[chord]
			d.selected = max(0, min(len(rows)-1, d.selected+delta))
		case "enter", "tab", "x":
			if d.selected >= 0 && d.selected < len(rows) {
				w := rows[d.selected]
				if chord != "x" {
					return m.openWorkerInspection(d, w.Handle, w.Name, false)
				}
				if m.canRequestWorkerStop(d, w.State) {
					return m.stopOneWorker(d, w.Handle)
				}
			}
		}
		return nil
	}
	switch chord {
	case "r":
		return m.fetchWorkerInspection(d)
	case "enter":
		if p.ready && !p.transcript && m.workerCurrentState(p) != "unresolved" {
			return m.openWorkerInspection(d, p.handle, p.name, true)
		}
	case "x":
		if p.ready && m.canRequestWorkerStop(d, m.workerCurrentState(p)) {
			return m.stopOneWorker(d, p.handle)
		}
	case "o":
		if p.ready && p.transcript && m.options.OpenReviewPane != nil && m.workerCurrentState(p) != "unresolved" {
			return m.openWorkerRexPane(d)
		}
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		if v := p.viewer; v != nil {
			_, h := m.outputSize(d)
			delta := map[string]int{"up": -1, "k": -1, "down": 1, "j": 1, "pgup": -h, "pgdown": h, "home": -v.top, "end": len(v.lines)}[chord]
			v.top = min(max(0, v.top+delta), max(0, len(v.lines)-h))
		}
	}
	return nil
}

func (m *Model) workerClick(d *dialog, target, col, row int) tea.Cmd {
	f := m.frame
	switch target {
	case workerPrimaryTarget:
		return m.workerKey(d, "enter")
	case workerKeysTarget:
		// Match only the hints actually drawn in this frame.
		keys := ansi.Strip(ansi.Cut(f.rows[row], f.dialogX, f.dialogX+f.dialogWidth))
		for _, hint := range []string{"r refresh", "x stop worker", "o Rex pane", d.zoomKey()} {
			if at := strings.Index(keys, hint); at >= 0 {
				left := f.dialogX + stringWidth(keys[:at])
				if col >= left && col < left+stringWidth(hint) {
					if hint == d.zoomKey() {
						d.full = !d.full
						return nil
					}
					return m.workerKey(d, hint[:1])
				}
			}
		}
	default:
		if target >= 0 && target < len(m.workers.rows) && (f.dialogListWidth == 0 || col < f.dialogX+f.dialogListWidth) {
			d.selected = target
		}
	}
	return nil
}

func (m *Model) stopOneWorker(d *dialog, handle string) tea.Cmd {
	backend, ok := m.backend.(workerActivityBackend)
	if !ok || !m.menuFeaturesFromBackend.Has(x.WorkerActivity) {
		return nil
	}
	p := d.worker
	p.stopping, p.note = true, ""
	owner := d.route.owner
	budget := maxEffects
	return m.startWork(d.route, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return backend.StopReviewWorker(call, owner, handle)
	}, func(value any, err error, _ *int) tea.Cmd {
		if !slices.Contains(m.overlays, d) {
			return nil
		}
		p.stopping = false
		result, ok := value.(sdk.ReviewWorker)
		if err != nil || !ok || workerCanStop(result.State) {
			p.note = "Stop was not confirmed; check worker activity"
			return nil
		}
		if result.State == "stopping" {
			p.note = "Stop requested; check worker activity"
			return nil
		}
		if p.handle != "" {
			return m.fetchWorkerInspection(d)
		}
		return nil
	}, &budget)
}

func (m *Model) openWorkerRexPane(d *dialog) tea.Cmd {
	open, owner, handle := m.options.OpenReviewPane, d.route.owner, d.worker.handle
	budget := maxEffects
	return m.startWork(d.route, func(ctx context.Context) (any, error) { return nil, open(ctx, owner, handle) }, func(_ any, err error, _ *int) tea.Cmd {
		if err != nil && slices.Contains(m.overlays, d) {
			d.worker.note = "Could not open the Rex pane; the worker is unchanged"
		}
		return nil
	}, &budget)
}

func (m *Model) workerRows(d *dialog, w, n int) menuFrame {
	p := d.worker
	if p.handle == "" {
		return m.workerRosterRows(d, w, n)
	}
	c := menuChrome{title: "Review evidence", scope: p.name}
	if p.transcript {
		c.title = "Worker transcript"
	}
	c.consequence = chromeLine(m.quiet("Read-only · advisory"))
	if p.ready {
		switch p.value.Target {
		case "current":
			c.count = "Current at inspection"
		case "stale":
			c.count = "Stale snapshot"
		default:
			c.count = "Target unavailable"
		}
		if !p.transcript && m.workerCurrentState(p) != "unresolved" {
			c.consequence = menuLine{m.menuAction("Open transcript", "Read public activity only"), workerPrimaryTarget}
		}
	}
	c.keys = menuLine{m.menuKeys(w, m.workerDetailHints(d)...), workerKeysTarget}
	return m.menuShell(w, n, c, func(h int) menuFrame {
		var out menuFrame
		if p.note != "" {
			out.add(m.paint(x.Warning, p.note), -1)
			h--
		} else if p.loading || p.stopping {
			label := "Reading worker evidence…"
			if p.stopping {
				label = "Requesting Stop for this worker…"
			}
			out.add(m.quiet(label), -1)
			h--
		}
		if p.ready && h > 0 {
			if state := m.workerCurrentState(p); state != p.value.Worker.State {
				_, label, _ := workerStateLabel(state)
				out.add(m.quiet("Now "+strings.ToLower(label)+" · r refresh inspection"), -1)
				h--
			}
			v := p.viewer
			v.reflowReview(w)
			top := min(v.top, max(0, len(v.lines)-h))
			for i := top; i < min(len(v.lines), top+h); i++ {
				out.add(v.window(i, 0, w), -1)
			}
		}
		return out
	})
}

func (m *Model) workerDetailHints(d *dialog) []string {
	p := d.worker
	keys := []string{"r refresh"}
	if p.ready {
		keys = append(keys, "↑↓ scroll")
		if p.transcript && m.options.OpenReviewPane != nil && m.workerCurrentState(p) != "unresolved" {
			keys = append(keys, "o Rex pane")
		}
		if m.canRequestWorkerStop(d, m.workerCurrentState(p)) {
			keys = append(keys, "x stop worker")
		}
	}
	return append(keys, d.zoomKey())
}

func (m *Model) workerRosterRows(d *dialog, w, n int) menuFrame {
	f := m.workers
	c := menuChrome{title: "Review workers", scope: "This thread", count: menuCount(len(f.rows), "worker", "workers"),
		keys: chromeLine(m.menuKeys(w, "↑↓ move", "tab details", d.zoomKey())), consequence: chromeLine(m.quiet("No workers to inspect"))}
	if len(f.rows) == 0 {
		return m.menuShell(w, n, c, func(int) menuFrame {
			label := "No review assignments for this thread."
			if f.failed {
				label = "Worker activity unavailable."
			} else if !f.ready {
				label = "Loading worker activity…"
			}
			var out menuFrame
			out.add(m.quiet(label), -1)
			return out
		})
	}
	sel := max(0, min(len(f.rows)-1, d.selected))
	c.consequence = menuLine{m.menuAction("Inspect worker", "Read brief, evidence, and report"), workerPrimaryTarget}
	if m.canRequestWorkerStop(d, f.rows[sel].State) {
		c.keys = menuLine{m.menuKeys(w, "↑↓ move", "x stop worker", "tab details", d.zoomKey()), workerKeysTarget}
	}
	if f.failed {
		c.scope = "Last observed · disconnected"
	}
	return m.menuShell(w, n, c, func(h int) menuFrame {
		var out menuFrame
		if p := d.worker; h > 1 && (p.stopping || p.note != "") {
			if p.stopping {
				out.add(m.quiet("Requesting Stop for this worker…"), -1)
			} else {
				out.add(m.paint(x.Warning, ansi.Truncate(p.note, w, "…")), -1)
			}
			h--
		}
		out.appendBody(m.menuColumns(w, h, func(width, height int) []string {
			row := f.rows[sel]
			_, label, _ := workerStateLabel(row.State)
			if row.Activity != "" {
				label += " · " + row.Activity
			}
			if row.State == "unresolved" {
				label += " · Original evidence could not be correlated"
			}
			return m.menuDetail(row.Name, label, width, height)
		}, func(width, height int) menuFrame {
			start, end := menuWindow(sel, len(f.rows), height)
			var list menuFrame
			for i := start; i < end; i++ {
				row := f.rows[i]
				icon, label, tone := workerStateLabel(row.State)
				list.add(m.menuRow(width, 10, menuItem{title: row.Name, icon: icon, state: label, tone: tone, focus: i == sel}), i)
			}
			return list
		}))
		return out
	})
}
