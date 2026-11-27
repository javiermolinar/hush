package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const footerID = "hush/footer"
const activateTabID x.ActionID = footerID + ".activate"
const usageID x.ActionID = footerID + ".usage"
const openUsageOp x.LocalOp = "usage.open"
const switcherID x.ActionID = "hush/menus.switch"
const maxOpenTabs = 24
const compactFooterWidth = 58

// ThreadMetadata is optional authoritative information supplied by the host.
// Empty fields mean unavailable; cumulative Usage is not context occupancy.
type ThreadMetadata struct {
	Title, Branch, Mode string
	ContextUse          x.ContextUse
	// ContextThreshold is the token count at which the context is compacted.
	// Zero leaves the physical context window as the denominator.
	ContextThreshold uint64
	// Status may override the coarse native projection, for example for a schedule.
	Status x.ThreadStatus
}

type footerMetadata struct {
	workspace string
	model     sdk.Model
	thinking  string
	context   x.ContextUse
}

func metadataFromReport(r IntroReport) footerMetadata {
	return footerMetadata{workspace: r.Workspace, model: deref(r.Model), thinking: r.Thinking, context: contextFromReport(r.Context)}
}

func contextFromReport(c *IntroContext) x.ContextUse {
	if c == nil || c.Window == 0 {
		return x.ContextUse{}
	}
	tokens := c.RenderedBytes / 4
	if c.RenderedBytes%4 != 0 {
		tokens++
	}
	if c.ReportedTokens != nil {
		tokens = *c.ReportedTokens
	}
	return x.ContextUse{Known: true, Window: c.Window, Tokens: tokens, Estimated: c.ReportedTokens == nil}
}

func (m *Model) footerThread(t *threadState) x.Thread {
	thread, last := t.info, t.footer
	if thread.Workspace == "" {
		thread.Workspace = last.workspace
		if thread.Workspace == "" && thread.ID == "" {
			thread.Workspace = m.options.NewThread.CWD
		}
	}
	if thread.Model.ModelID == "" {
		thread.Model, thread.Thinking = last.model, last.thinking
		if thread.Model.ModelID == "" && thread.ID == "" {
			thread.Model = deref(m.options.NewThread.Model)
		}
	}
	if !thread.ContextUse.Known && (last.model.ModelID == "" || thread.Model == last.model) &&
		(last.workspace == "" || filepath.Clean(thread.Workspace) == filepath.Clean(last.workspace)) {
		thread.ContextUse = last.context
	}
	if m.subagents.owner.Valid() && thread.ID == m.subagents.owner {
		s := m.subagents.update.Usage
		thread.SubagentUsage = s
		thread.Usage.InputTokens += s.InputTokens
		thread.Usage.OutputTokens += s.OutputTokens
		thread.Usage.TotalTokens += s.TotalTokens
		thread.Usage.CacheReadTokens += s.CacheReadTokens
		thread.Usage.CacheWriteTokens += s.CacheWriteTokens
		thread.Usage.CostUSD += s.CostUSD
	}
	return thread
}

func workspaceLabel(path string) string {
	if path == "" {
		return ""
	}
	if name := filepath.Base(path); name != "." && name != string(filepath.Separator) {
		return oneLine(name)
	}
	return ""
}

func footerRegistration() x.Registration {
	usageSpan := func(text string, tone x.Tone) x.Line {
		return x.Line{{Text: text, Tone: tone, Action: x.ActionRef{ID: usageID}, Hint: "show usage details"}}
	}
	return x.Registration{ID: footerID, APIVersion: x.APIVersion, Actions: []x.Action{
		{ID: activateTabID, Title: "Previous tab", Binding: x.Binding{Direct: "ctrl+[", Leader: "["}, Available: func(ctx x.Context) bool { return ctx.ActiveThreadID != "" }, Run: func(ctx x.Context, v x.Invocation) x.Outcome {
			id := sdk.ID(v.Target)
			if id == "" {
				id = ctx.PreviousThreadID
			}
			if !id.Valid() {
				return nil
			}
			return x.Outcome{x.Client{Op: x.Switch{ThreadID: id}}}
		}},
		{ID: usageID, Title: "Usage details", Binding: x.Binding{Leader: "i u"}, Slash: &x.Slash{Name: "usage"},
			Available: func(c x.Context) bool { return c.Thread.ID.Valid() }, Run: func(x.Context, x.Invocation) x.Outcome {
				return x.Outcome{x.Local{Op: openUsageOp}}
			}},
	}, Segments: []x.Segment{
		{Key: "context", Slot: x.StatusRight, Order: 10, Priority: 46, Render: func(c x.RenderContext) x.Line {
			v := c.Thread.ContextUse
			if !v.Known || v.Window == 0 {
				return nil
			}
			ratio := float64(v.Tokens) / float64(contextLimit(c.Thread))
			p := int(min(9.99, ratio) * 100)
			filled := min(6, max(0, int(ratio*6+0.5)))
			if v.Tokens != 0 {
				filled = max(1, filled)
			}
			tone := x.Muted
			if p >= 80 {
				tone = x.Danger
			} else if p >= 60 {
				tone = x.Warning
			}
			percent := fmt.Sprintf("%d%%", p)
			if v.Estimated {
				percent = "~" + percent
			}
			label := "ctx " + percent
			if c.Width >= compactFooterWidth {
				label = fmt.Sprintf("ctx %s%s %s", strings.Repeat("▰", filled), strings.Repeat("▱", 6-filled), percent)
			}
			return usageSpan(label, tone)
		}},
		{Key: "cost", Slot: x.StatusRight, Order: 30, Priority: 43, Render: func(c x.RenderContext) x.Line {
			usage := c.Thread.Usage
			if usage.CostUSD == 0 && usage.TotalTokens == 0 {
				return nil
			}
			if c.Thread.SubagentUsage != (sdk.Usage{}) && c.Width >= compactFooterWidth {
				return usageSpan(spendLabel(usage), x.Muted)
			}
			return usageSpan(fmt.Sprintf("$%.2f", usage.CostUSD), x.Muted)
		}},
	}}
}

func contextLimit(t x.Thread) uint64 {
	if t.ContextThreshold != 0 {
		return min(t.ContextThreshold, t.ContextUse.Window)
	}
	return t.ContextUse.Window
}

func (m *Model) openUsage(r ownerRoute, t *threadState, budget *int) tea.Cmd {
	if !m.foreground(r) {
		return nil
	}
	d := &dialog{route: r, token: m.token(), budget: budget, title: "Usage details", usage: true}
	m.refreshUsage(d, t)
	return m.pushDialog(d)
}

func (m *Model) refreshUsage(d *dialog, t *threadState) {
	if t == nil {
		return
	}
	thread := m.footerThread(t)
	usage := thread.Usage
	text := fmt.Sprintf("Total tokens: %d\nSpend: $%.2f", usage.TotalTokens, usage.CostUSD)
	if m.menuFeaturesFromBackend.Has(x.Subagents) {
		f := m.subagents
		switch {
		case f.owner != d.route.owner || !f.ready:
			text = "Parent only · subagent spend not yet available\n\n" + spendLabel(t.info.Usage)
		case f.failed:
			text = "Last observed subagent spend\n\n" + text
		}
	}
	if agents := thread.SubagentUsage; agents != (sdk.Usage{}) {
		text += fmt.Sprintf("\n\nParent: %s\nSubagents: %s", spendLabel(t.info.Usage), spendLabel(agents))
	}
	if v := thread.ContextUse; v.Known && v.Window > 0 {
		used := strconv.FormatUint(v.Tokens, 10)
		if v.Estimated {
			used = "~" + used
		}
		text += fmt.Sprintf("\n\nCurrent context: %s of %d tokens", used, contextLimit(thread))
	}
	if d.output == nil || d.output.raw != text {
		m.setReviewText(d, text)
	}
}

func tokenLabel(n uint64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM tok", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk tok", float64(n)/1000)
	default:
		return strconv.FormatUint(n, 10) + " tok"
	}
}

func threadTitle(t x.Thread) string {
	if t.Title != "" {
		return oneLine(t.Title)
	}
	if name := workspaceLabel(t.Workspace); name != "" && t.ID != "" {
		return name + " #" + string(t.ID)
	}
	return threadLabel(t.ID)
}

// Titles are chrome, not prose. Reserve the ellipsis and keep only whole
// words rather than cutting through a word or a double-width character.
func fitTitle(title string, width int) string {
	if width <= 0 {
		return ""
	}
	if stringWidth(title) <= width {
		return title
	}
	var out strings.Builder
	room := width - stringWidth("…")
	for word := range strings.FieldsSeq(title) {
		n := stringWidth(word)
		if out.Len() != 0 {
			n++
		}
		if n > room {
			break
		}
		if out.Len() != 0 {
			out.WriteByte(' ')
		}
		out.WriteString(word)
		room -= n
	}
	return out.String() + "…"
}

func mergeRow(dst *displayRow, src displayRow, between string) {
	if dst.text != "" && src.text != "" {
		dst.text += between
	}
	at := stringWidth(dst.text)
	dst.text += src.text
	for _, hit := range src.hits {
		hit.first += at
		hit.last += at
		dst.hits = append(dst.hits, hit)
	}
}

type segmentPiece struct {
	rank  segmentRank
	row   displayRow
	width int
}

func (m *Model) segmentRow(t *threadState, w int, left x.SegmentSlot) displayRow {
	ctx := m.renderContext(t, w, 1, false)
	registered := m.host.tabSegments
	var dynamic []segmentPiece
	add := func(key string, priority, order int, text string) {
		if text != "" {
			dynamic = append(dynamic, segmentPiece{rank: segmentRank{key, priority, order, left}, row: displayRow{text: text}})
		}
	}
	if left == x.StatusLeft {
		registered = m.host.statusSegments
		ctx.Thread = m.footerThread(t)
		dir := workspaceLabel(ctx.Thread.Workspace)
		if dir == "" && ctx.Thread.Workspace != "" {
			dir = oneLine(filepath.Clean(ctx.Thread.Workspace))
		}
		if dir != "" {
			prefix := "Workspace "
			if ctx.Thread.Branch != "" {
				prefix = "Repo "
			}
			if w < compactFooterWidth {
				prefix = ""
			}
			room := min(w, max(12, w/3+stringWidth(prefix)))
			add("hush/workspace", 45, -90, m.quiet(ansi.Truncate(prefix+dir, room, "…")))
		}
		if t.info.Archived {
			add("hush/archive", 25, -85, m.paint(x.Muted, "archived"))
		}
		if m.connection != x.Connected && t.info.ID != "" {
			label := "connecting"
			if t.observed {
				label = "reconnecting"
			}
			if m.connection == x.Disconnected {
				label = "disconnected"
			}
			warning := []string{"◌", "◔", "◑", "◕"}[m.phase] + " " + label
			add("hush/connection", 35, -80, m.paint(x.Warning, warning))
		}
		for key, a := range t.status {
			add(key, a.Priority, a.Order, m.paint(a.Tone, oneLine(a.Text)))
		}
	}
	slices.SortFunc(dynamic, func(a, b segmentPiece) int { return compareSegmentPriority(a.rank, b.rank) })

	// Choose the longest fitting prefix in priority order. Registered
	// renderers are pure and already ranked, so once a piece overflows there
	// is no need to call any renderer that would have been evicted before it.
	const sep, sepWidth = " · ", 3
	var survivors []segmentPiece
	var widths [2]int
	var hasPiece [2]bool
	for len(registered) > 0 || len(dynamic) > 0 {
		var p segmentPiece
		if len(dynamic) > 0 && (len(registered) == 0 || compareSegmentPriority(dynamic[0].rank, registered[0].rank) < 0) {
			p, dynamic = dynamic[0], dynamic[1:]
		} else {
			a := registered[0]
			registered = registered[1:]
			n := 1024
			rows := m.layoutLine(ctx, a.render(ctx), &n, 1)
			if len(rows) == 0 || rows[0].text == "" {
				continue
			}
			p = segmentPiece{rank: a.rank, row: rows[0]}
		}
		p.width = stringWidth(p.row.text)
		side := 0
		if p.rank.slot != left {
			side = 1
		}
		next := widths
		next[side] += p.width
		if hasPiece[side] {
			next[side] += sepWidth
		}
		gap := 0
		if next[0] > 0 && next[1] > 0 {
			gap = 2
		}
		if next[0]+next[1]+gap > w {
			break
		}
		widths, hasPiece[side] = next, true
		survivors = append(survivors, p)
	}

	slices.SortFunc(survivors, func(a, b segmentPiece) int { return compareSegmentLayout(a.rank, b.rank) })
	var out displayRow
	var text strings.Builder
	at := 0
	for i, p := range survivors {
		if p.rank.slot != left && widths[1] == 0 {
			break
		}
		if p.rank.slot != left && (i == 0 || survivors[i-1].rank.slot == left) {
			pad := w - widths[1] - at
			text.WriteString(strings.Repeat(" ", pad))
			at += pad
		} else if i > 0 {
			text.WriteString(sep)
			at += sepWidth
		}
		text.WriteString(p.row.text)
		for _, hit := range p.row.hits {
			hit.first += at
			hit.last += at
			out.hits = append(out.hits, hit)
		}
		at += p.width
	}
	out.text = text.String()
	return out
}

func (m *Model) chromeRows(t *threadState, w int) []displayRow {
	meta := m.segmentRow(t, w, x.StatusLeft)
	if len(m.tabs) == 0 && !t.info.ID.Valid() {
		return []displayRow{meta}
	}
	return []displayRow{meta, m.tabLayout(w)}
}

func (m *Model) tabItem(id sdk.ID, w int) displayRow {
	state := m.threads[id]
	t := state.info
	glyph, _, tone := threadPickerStatus(t.Status)
	if glyph == "" {
		glyph, tone = "○", x.Muted
	}
	if id == "" {
		glyph = "+"
	}
	fork := ""
	if t.Forked {
		fork = "⑂ "
	}
	room := max(1, w-5-stringWidth(fork)) // focus, status, and trailing space
	branch := ""
	if t.Branch != "" && room >= 10 {
		branch = ansi.Truncate("⎇ "+oneLine(t.Branch), min(room-5, room/2), "…")
		room -= stringWidth(branch) + 1
	}
	label := fork + fitTitle(threadDisplayTitle(t), room)
	focus := m.cursorMark(id == m.active)
	if id == m.active {
		label = strongText(label)
	}
	row := displayRow{text: focus + m.paint(tone, glyph) + " " + label}
	if branch != "" {
		mergeRow(&row, displayRow{text: m.quiet(branch)}, " ")
	}
	if room := w - stringWidth(row.text) - 1; room > 5 {
		mergeRow(&row, m.segmentRow(state, room-1, x.Tab), " ")
	}
	row.text += " "
	if id == m.active {
		row.text = surface(row.text, m.palette.activeTab)
	}
	if id != "" && id != m.active {
		row.hits = append(row.hits, spanHit{0, stringWidth(row.text), x.ActionRef{ID: activateTabID, Target: string(id)}, "Switch to " + threadDisplayTitle(t)})
	}
	return row
}

// tabLayout generates both visible labels and their hit regions. Keep the active
// tab visible even when all preceding tabs must be dropped.
func (m *Model) tabLayout(w int) displayRow {
	ids := m.tabs
	if len(ids) == 0 {
		ids = []sdk.ID{m.active}
	}
	var rows []displayRow
	for _, id := range ids {
		rows = append(rows, m.tabItem(id, max(8, min(40, w-6))))
	}
	chosen := make([]bool, len(ids))
	active := slices.Index(ids, m.active)
	if active < 0 {
		active = 0
	}
	chosen[active] = true
	remaining := w - stringWidth(rows[active].text) - 6
	for i, row := range rows {
		if i != active && remaining >= stringWidth(row.text)+1 {
			remaining -= stringWidth(row.text) + 1
			chosen[i] = true
		}
	}
	out := displayRow{tabBand: true}
	hidden := 0
	for i, row := range rows {
		if chosen[i] {
			mergeRow(&out, row, " ")
		} else {
			hidden++
		}
	}
	if hidden > 0 {
		s := "+" + strconv.Itoa(hidden)
		mergeRow(&out, displayRow{text: m.control(s), hits: []spanHit{{0, stringWidth(s), x.ActionRef{ID: switcherID}, "show threads"}}}, " ")
	}
	return out
}

func (m *Model) actionBinding(a registeredAction) string {
	if a.action.Binding.Direct == "ctrl+[" && !m.disambiguated {
		return "ctrl+x " + a.action.Binding.Leader
	}
	if m.taken[a.action.Binding.Direct] {
		return "ctrl+x " + a.action.Binding.Leader
	}
	return a.bindingLabel
}

func (m *Model) decorateView(v *tea.View, t *threadState) {
	v.ReportFocus = true
	needed := 0
	for _, id := range m.tabs {
		switch m.threads[id].info.Status {
		case x.Parked, x.Failed:
			needed++
		}
	}
	v.WindowTitle = "hush · " + threadTitle(t.info)
	if needed > 0 {
		v.WindowTitle = fmt.Sprintf("(%d) %s", needed, v.WindowTitle)
	}
	if m.quitting {
		v.ProgramStatus = &tea.ProgramStatus{State: tea.ProgramStateIdle}
		return
	}
	switch t.info.Status {
	case x.Running:
		v.ProgressBar = tea.NewProgressBar(tea.ProgressBarIndeterminate, 0)
	case x.Parked:
		v.ProgramStatus = &tea.ProgramStatus{State: tea.ProgramStateBlocked, Message: "Waiting"}
	}
}

func (m *Model) desktopStatus(id sdk.ID, before, after x.ThreadStatus) tea.Cmd {
	t := m.threads[id]
	if t.directoryBase.beforeFallback != "" {
		before, t.directoryBase.beforeFallback = t.directoryBase.beforeFallback, ""
	}
	if before == "" || before == after || m.focused && id == m.active {
		return nil
	}
	switch after {
	case x.Parked, x.Done, x.Stopped, x.Failed:
		return tea.Raw("\x1b]9;" + oneLine(threadTitle(t.info)+" · "+string(after)) + "\x07")
	}
	return nil
}

func (m *Model) openedTab(id sdk.ID) {
	if id == "" || slices.Contains(m.tabs, id) {
		return
	}
	m.tabs = append(m.tabs, id)
	if len(m.tabs) > maxOpenTabs {
		m.tabs = m.tabs[1:]
	}
}

func (m *Model) tabsWithout(id sdk.ID) {
	if i := slices.Index(m.tabs, id); i >= 0 {
		m.tabs = slices.Delete(m.tabs, i, i+1)
	}
}

// On legacy hosts, other open tabs use a small native subscription. The
// active thread always has its own full feed.
type tabFeed struct {
	lease  feedLease
	source <-chan sdk.StateUpdate
	update sdk.StateUpdate
	ok     bool
}

func nextTab(f feedLease, src <-chan sdk.StateUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-f.ctx.Done():
			return nil
		case v, ok := <-src:
			return tabFeed{f, src, v, ok}
		}
	}
}

func (m *Model) syncTabFeeds() tea.Cmd {
	for id, f := range m.tabFeeds {
		if m.directoryEnabled || id == m.active || !slices.Contains(m.tabs, id) {
			f.cancel()
			delete(m.tabFeeds, id)
		}
	}
	if m.directoryEnabled {
		return nil
	}
	var cmds []tea.Cmd
	for _, id := range m.tabs {
		if id == m.active || m.tabFeeds[id].token != 0 {
			continue
		}
		ctx, cancel := context.WithCancel(m.ctx)
		f := feedLease{m.token(), id, ctx, cancel}
		m.tabFeeds[id] = f
		backend := m.backend
		cmds = append(cmds, func() tea.Msg {
			if ctx.Err() != nil {
				return nil
			}
			return nextTab(f, backend.Watch(ctx, id, sdk.Window{Min: 1, Max: 1}))()
		})
	}
	return tea.Batch(cmds...)
}

func (m *Model) receiveTab(v tabFeed) tea.Cmd {
	id := v.lease.owner
	f := m.tabFeeds[id]
	if m.directoryEnabled || id == m.active || f.token == 0 || f.token != v.lease.token {
		return nil
	}
	if !v.ok {
		f.cancel()
		delete(m.tabFeeds, id)
		return nil
	}
	next := nextTab(f, v.source)
	if v.update.Err != nil || v.update.Snapshot.State.Thread.ID != id {
		return next
	}
	t := m.threads[id]
	prior := t.info
	t.info = m.threadInfo(t, v.update.Snapshot)
	if prior != t.info {
		t.revision++
		m.emit(id, x.Event{Kind: x.StatusChanged})
	}
	if prior.Usage != t.info.Usage {
		m.emit(id, x.Event{Kind: x.UsageChanged})
	}
	return tea.Batch(next, m.desktopStatus(id, prior.Status, t.info.Status))
}

func (m *Model) threadInfo(t *threadState, snap sdk.Snapshot) x.Thread {
	s := snap.State
	out := x.Thread{ID: s.Thread.ID, LastEntryID: deref(s.Window.LastEntryID), Workspace: s.Agent.CWD, Thinking: s.Agent.ThinkingLevel,
		Usage: s.Usage, Queued: len(s.Queue), Model: deref(s.Agent.Model), Status: x.Idle, Forked: s.Thread.Parent != nil}
	if s.Thread.Parent != nil {
		out.ParentID = s.Thread.Parent.ThreadID
	}
	prior := m.withoutDirectory(t)
	out.Title, out.Mode, out.Archived = prior.Title, prior.Mode, prior.Archived
	t.activeRun = s.Run != nil || s.Streaming != nil || len(s.Compactions) != 0
	if t.activeRun {
		out.Status = x.Running
		for _, tool := range s.Tools {
			if tool.Status == "sleeping" || tool.Status == "waiting" {
				out.Status = x.Parked
				break
			}
		}
	} else if s.CompactionFailure != nil || s.Failure != nil && failedRunCause(s.Failure.Reason) != "" && !t.dismissedFailure(s.Failure.TaskID) {
		out.Status = x.Failed
	} else if s.Failure != nil && s.Failure.Reason == "aborted" {
		out.Status = x.Stopped
	} else if prior.Status == x.Running || prior.Status == x.Parked || prior.Status == x.Done {
		out.Status = x.Done
	}
	hostStatus := false
	if read := m.options.ProjectThread; read != nil {
		meta := read(snap)
		out.Title, out.Branch, out.Mode = limitText(meta.Title, 256), limitText(meta.Branch, 256), limitText(meta.Mode, 64)
		out.ContextUse, out.ContextThreshold = meta.ContextUse, meta.ContextThreshold
		if meta.Status != "" {
			out.Status, hostStatus = meta.Status, true
		}
	}
	t.saveDirectoryBase(out, hostStatus)
	m.mergeDirectory(&out, t, t.activeRun)
	t.observed = true
	return out
}

func threadBusy(t *threadState) bool { return t.activeRun || t.info.Status == x.Running }
