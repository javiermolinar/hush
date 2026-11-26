package tui

import (
	"math"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const margin = "  "

type frameMemo struct {
	rows                                        []string
	hits                                        [][]spanHit
	owner                                       sdk.ID
	navigation, theme                           uint64
	width, height                               int
	transcriptHeight, ruleY                     int
	hasAbove, hasBelow, hasTranscript           bool
	thumbStart, thumbEnd                        int
	top                                         rowAnchor
	copyToken                                   uint64
	openActivity                                string
	dialogToken                                 uint64
	dialogPlanRev                               string
	dialogPlanSelectionRev                      uint64
	dialogX, dialogY, dialogWidth, dialogHeight int
	dialogChoices                               []int
	slashStart, slashEnd                        int
	hasHover                                    bool
	intro                                       introFrame
	imageWants                                  []imageDemand
}

func stringWidth(s string) int { return ansi.StringWidth(s) }
func ansiCut(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "")
}
func clipWidth(s string, w int) (string, int) {
	n := stringWidth(s)
	if n > w {
		s = ansiCut(s, w)
		n = stringWidth(s)
	}
	return s, n
}
func fit(s string, w int) string {
	s, n := clipWidth(s, w)
	return s + strings.Repeat(" ", max(0, w-n))
}

func (m *Model) scrollCell(f *frameMemo, y int) string {
	if y >= f.thumbStart && y < f.thumbEnd {
		return m.control("┃")
	}
	return m.paint(x.Muted, "│")
}

func (m *Model) currentFrame() bool {
	f := m.frame
	return f.width == m.width && f.height == m.height && f.owner == m.active && f.navigation == m.navigation
}

func (m *Model) View() tea.View {
	if m.introOnly {
		i := m.intro
		if i.memo.sampleID != i.motion.renderID {
			defer m.measureIntroFrame()
		}
		if v, ok := m.patchIntro(); ok {
			return v
		}
	}
	f := frameMemo{owner: m.active, navigation: m.navigation, theme: m.theme.Revision, width: m.width, height: m.height, rows: make([]string, m.height), hits: make([][]spanHit, m.height)}
	v := tea.NewView("")
	v.AltScreen = true
	pane := m.pane()
	if !pane.usable {
		if m.height > 0 && m.width > 0 {
			warning := "terminal too small"
			if m.width < len(warning) {
				warning = "too small"
			}
			f.rows[0] = ansiCut(warning, m.width)
		}
		v.SetContent(strings.Join(f.rows, "\n"))
		*m.frame = f
		return v
	}
	t := m.threads[m.active]
	w := pane.width
	var slash []registeredAction
	var below []displayRow
	f.slashStart, f.slashEnd = m.height, m.height
	continuation, filePicker := false, false
	var fileChoices int
	var chrome []displayRow
	if !pane.short {
		chrome = m.chromeRows(t, w)
	}
	listEnd := m.height - len(chrome)
	// Size completions against the maximum editor height. A completion can
	// use the one transcript row on a minimum-height terminal.
	listBudget := max(1, listEnd-2-pane.editorRows)
	d := m.focusedDialog()
	if d != nil {
		continuation = true // hide background completion lists
	} else if files, n := m.fileRows(w, listBudget); len(files) > 0 {
		below, filePicker, fileChoices = files, true, n
	} else if slash = m.slashMatches(); len(slash) > 0 {
		below = m.slashSheet(slash, w, listBudget)
		f.slashStart, f.slashEnd = listEnd-len(below), listEnd
	} else {
		continuation = true
	}
	below = append(below, chrome...)
	editorRows, cursor := m.promptRows(t, pane, slash)
	const fixedRows = 1 // live rule
	var notice displayRow
	free := m.height - fixedRows - len(editorRows) - len(below)
	var attachments []displayRow
	if free > 1 && (t.attaching || len(t.attachments) != 0) {
		attachments = []displayRow{m.attachmentRow(t, w)}
		free--
	}
	if free > 1 && m.notice.text != "" {
		notice.text = m.paint(m.notice.tone, ansi.Truncate(oneLine(m.notice.text), w, "…"))
		free--
	}
	noticeRows := 0
	if notice.text != "" {
		noticeRows = 1
	}
	room := max(0, free-1)
	queued := m.queueRows(t, w, room)
	room -= len(queued)
	widgetAbove := m.widgetRows(t, w, x.AboveEditor, min(3, room))
	if continuation {
		widgetBelow := m.widgetRows(t, w, x.BelowEditor, min(3, room-len(widgetAbove)))
		below = append(widgetBelow, below...)
	}
	f.transcriptHeight = max(0, m.height-fixedRows-len(editorRows)-len(below)-len(widgetAbove)-len(queued)-noticeRows-len(attachments))
	rows := m.transcript(t, w, f.transcriptHeight)
	origin := 0
	// Keep short transcripts under their intro. Once the viewport fills,
	// tail-following scrolls the intro away like any other block.
	if t.following && t.intro == nil {
		origin = f.transcriptHeight - len(rows)
	}
	if len(rows) > 0 {
		f.top, f.hasTranscript = rows[0].anchor, true
	}
	from, to, total := m.scrollSpan(t, rows, w)
	f.hasAbove, f.hasBelow = from > 0, total > to
	rail := f.hasAbove || f.hasBelow
	if rail {
		h := float64(f.transcriptHeight)
		f.thumbStart = min(f.transcriptHeight-1, int(from/total*h))
		f.thumbEnd = min(f.transcriptHeight, max(f.thumbStart+1, int(math.Ceil(to/total*h))))
	}
	if m.editor.value() == "" && len(t.attachments) == 0 && !t.attaching && m.focusedDialog() == nil && m.leader == "" {
		for i := len(rows) - 1; i >= 0; i-- {
			if target := moreActivityTarget(rows[i].displayRow); target != "" {
				if f.openActivity == "" || strings.HasPrefix(target, outputTargetPrefix) {
					f.openActivity = target
				}
				if strings.HasPrefix(target, outputTargetPrefix) {
					break
				}
			}
		}
	}
	var copyLink string
	copySweep := -1
	if m.copied.token != 0 && m.copied.owner == m.active {
		copyLink = codeCopyLink(m.copied.id)
		if m.focusedDialog() == nil {
			copySweep = m.copied.sweep
		}
	}
	padding := strings.Repeat(" ", m.width)
	margin, railGap := padding[:pane.edge], padding[:pane.edge-1]
	add := func(y int, r displayRow) {
		if y < 0 || y >= len(f.rows) {
			return
		}
		if copyLink != "" && y < f.transcriptHeight {
			var copied bool
			if r.text, copied = m.paintCopiedCode(r.text, copyLink, copySweep); copied {
				f.copyToken = m.copied.token
				copyLink = ""
			}
		}
		if target := moreActivityTarget(r); target != "" && target != f.openActivity {
			if lead, ok := strings.CutSuffix(r.text, " · enter open"); ok {
				const hint = " · click open"
				r.text = lead
				if stringWidth(lead)+stringWidth(hint) <= w {
					r.text += hint
				}
			}
		}
		text, cells := clipWidth(r.text, w)
		line := text + padding[:max(0, w-cells)] // fit, without a per-row allocation
		if r.card {
			line = surface(line, m.palette.softInk)
		}
		prefix := margin
		if r.user {
			prefix = m.paint(x.Accent, "│") + railGap
		}
		f.rows[y] = prefix + line
		if r.tabBand {
			f.rows[y] = surface(f.rows[y]+margin, m.palette.softInk)
		}
		if rail && y < f.transcriptHeight {
			f.rows[y] += railGap + m.scrollCell(&f, y)
		}
		if len(r.hits) > 0 {
			for _, hit := range r.hits {
				hit.first = pane.edge + max(0, hit.first)
				hit.last = pane.edge + min(cells, hit.last)
				if hit.last > hit.first {
					f.hits[y] = append(f.hits[y], hit)
				}
			}
			f.hasHover = true
		}
		if strings.Contains(r.text, "\x1b]8;") {
			f.hasHover = true
		}
	}
	canPatchIntro := m.focusedDialog() == nil && !m.selection.dragging && !m.selection.selected
	for i, row := range rows {
		y := origin + i
		if row.anchor.id == introEntry {
			if len(row.hits) > 0 {
				add(y, row.displayRow)
			} else {
				f.rows[y] = margin + row.text // intro rows are already fitted
				if rail {
					f.rows[y] += railGap + m.scrollCell(&f, y)
				}
			}
			if canPatchIntro {
				if f.intro.count == 0 {
					f.intro.at, f.intro.first = y, row.anchor.row
				}
				f.intro.count++
			}
			continue
		}
		add(y, row.displayRow)
		if d == nil && row.image != nil && len(f.imageWants) < previewCacheSize {
			found := false
			for _, want := range f.imageWants {
				found = found || want.ref == row.image.ref
			}
			if !found {
				f.imageWants = append(f.imageWants, *row.image)
			}
		}
	}
	if rail {
		for y := 0; y < f.transcriptHeight; y++ {
			if f.rows[y] == "" {
				f.rows[y] = padding[:m.width-1] + m.scrollCell(&f, y)
			}
		}
	}
	y := f.transcriptHeight
	for _, row := range widgetAbove {
		add(y, row)
		y++
	}
	if noticeRows != 0 {
		add(y, notice)
		y++
	}
	for _, row := range queued {
		add(y, row)
		y++
	}
	for _, row := range attachments {
		add(y, row)
		y++
	}
	paused := ""
	if !t.following && (t.intro == nil || t.intro.frozen || f.hasBelow || t.anchor != (rowAnchor{id: introEntry})) {
		paused = "scroll paused"
		if f.hasBelow {
			paused = "output below"
		}
	}
	f.ruleY = y
	var hints []string
	if !pane.short {
		hints = m.ruleHints(t, len(slash) > 0, filePicker, fileChoices)
	}
	add(y, m.promptRule(t, w, paused, hints))
	y++
	if cursor != nil && m.focused {
		cursor.Position.X += pane.edge
		cursor.Position.Y += y
		v.Cursor = cursor
	}
	for _, row := range editorRows {
		add(y, row)
		y++
	}
	for _, row := range below {
		add(y, row)
		y++
	}

	// A stationary press is not a selection. Highlighting it strips the
	// hyperlink under the click before mouse-up can resolve the target.
	if m.selection.selected || m.selection.dragging && m.selection.from != m.selection.to {
		for i := 0; i < f.transcriptHeight; i++ {
			if a, b, ok := m.selection.span(i, m.width-pane.edge); ok {
				line := fit(f.rows[i], m.width)
				f.rows[i] = ansi.Cut(line, 0, a) + ansiCut("\x1b[7m"+ansi.Strip(ansi.Cut(line, a, b))+"\x1b[27m", b-a) + ansi.Cut(line, b, m.width)
			}
		}
	}
	if d != nil {
		panel := m.menuFrame(d, f.ruleY)
		top := f.ruleY - len(panel.rows)
		for j, row := range panel.rows {
			y := top + j
			f.rows[y] = margin + fit(row, w) + margin
			f.hits[y] = nil // the focused panel owns this row
		}
		f.dialogToken, f.dialogX, f.dialogY = d.token, pane.edge, top
		f.dialogWidth, f.dialogHeight, f.dialogChoices = w, len(panel.rows), panel.targets
		if d.plan != nil {
			f.dialogPlanRev = m.planViewFor(d).snapshot.Revision
			f.dialogPlanSelectionRev = d.plan.selectionRev
		}
		if panel.cursor != nil && m.focused {
			panel.cursor.Position.X += pane.edge
			panel.cursor.Position.Y += top
			v.Cursor = panel.cursor
		}
	}

	if v.Cursor != nil {
		// Let the terminal blink the block without waking the render loop.
		v.Cursor.Shape = tea.CursorBlock
		v.Cursor.Blink = !m.options.ReducedMotion
	}
	v.SetContent(strings.Join(f.rows, "\n"))
	v.MouseMode = tea.MouseModeCellMotion
	if f.hasHover && m.focusedDialog() == nil {
		v.MouseMode = tea.MouseModeAllMotion
	}
	m.decorateView(&v, t)
	if f.intro.count != 0 {
		f.intro.view = v
	}
	*m.frame = f
	return v
}

func (m *Model) widgetRows(t *threadState, w int, where x.WidgetPlacement, n int) []displayRow {
	if n <= 0 {
		return nil
	}
	var out []displayRow
	for _, widget := range m.host.widgets {
		if widget.Placement != where {
			continue
		}
		ctx := m.renderContext(t, w, n-len(out), false)
		out = append(out, m.layoutContent(ctx, renderKey{}, widget.Render(ctx))...)
		if len(out) >= n {
			break
		}
	}
	return out
}

func (m *Model) slashSheet(matches []registeredAction, w, budget int) []displayRow {
	// Keep the selected command in view without making the transcript jump
	// with every arrow press. All matching commands remain reachable.
	spacer := 0
	if budget > 2 {
		spacer = 1
	}
	limit := min(5, len(matches), budget-spacer)
	selected := min(m.slashIndex, len(matches)-1)
	start := min(max(0, selected-limit+1), len(matches)-limit)
	visible := matches[start : start+limit]
	nameWidth := 0
	keyWidth := 0
	for _, a := range visible {
		nameWidth = max(nameWidth, 1+stringWidth(a.action.Slash.Name))
		keyWidth = max(keyWidth, stringWidth(m.actionBinding(a)))
	}
	nameWidth = min(nameWidth, max(8, w/3))
	if w-nameWidth-keyWidth < 12 {
		keyWidth = 0
	}
	out := make([]displayRow, spacer, limit+spacer)
	for i, a := range visible {
		active := start+i == selected
		prefix := "  "
		tone := x.Plain
		if active {
			prefix = m.paint(x.Accent, "› ")
			tone = x.Accent
		}
		name := ansi.Truncate("/"+a.action.Slash.Name, nameWidth, "…")
		note := a.action.Description
		if note == "" {
			note = a.action.Title
		}
		labelEnd := w - keyWidth
		if keyWidth > 0 {
			labelEnd -= 2
		}
		noteWidth := max(0, labelEnd-nameWidth-4)
		line := prefix + m.paint(tone, fit(name, nameWidth))
		if noteWidth > 0 {
			line += "  " + m.control(ansi.Truncate(oneLine(note), noteWidth, "…"))
		}
		if keyWidth > 0 {
			hint := m.actionBinding(a)
			line = fit(line, w-stringWidth(hint)) + m.control(hint)
		}
		if active && m.theme.Fills {
			line = fill(fit(line, w), m.palette.selected)
		}
		ref := x.ActionRef{ID: a.action.ID}
		if ref.ID == stageSkillID {
			ref.Target = a.skill.name + "\n" + a.skill.token
		}
		out = append(out, displayRow{text: line, hits: []spanHit{{0, w, ref, a.action.Title}}})
	}
	return out
}

func (m *Model) promptRows(t *threadState, pane paneLayout, slash []registeredAction) ([]displayRow, *tea.Cursor) {
	w, label := pane.width, "you › "
	intro := t.intro != nil && !t.intro.frozen
	if intro {
		label = "main › "
	}
	if threadBusy(t) && len(slash) == 0 {
		label = "steer › "
	}
	if pane.narrow {
		label = "› "
	}
	prefix := stringWidth(label)
	output, editorCursor := m.editor.layout(pane.editorRows)
	var cursor *tea.Cursor
	if m.focusedDialog() == nil && editorCursor != nil {
		c := *editorCursor
		c.Position.X = prefix + min(c.Position.X, w-prefix-1)
		c.Position.Y = min(max(0, c.Position.Y), len(output)-1)
		cursor = &c
	}
	rows := make([]displayRow, 0, len(output))
	for i, text := range output {
		lead := ""
		if i == 0 {
			lead = label
		}
		text = ansiCut(text, w-prefix)
		if intro && i == 0 && m.editor.value() == "" && m.focusedDialog() == nil {
			text = m.control("ask anything")
		}
		if i == 0 && len(output) == 1 && m.focusedDialog() == nil {
			if len(slash) > 0 && m.editor.value() != "/" && m.editor.cursorByte() == len(m.editor.value()) {
				a := slash[min(m.slashIndex, len(slash)-1)]
				name := "/" + a.action.Slash.Name
				plain := m.editor.value()
				if tail, ok := strings.CutPrefix(name, plain); ok && tail != "" && stringWidth(name) < w-prefix {
					text = strings.TrimRight(text, " ") + m.paint(x.Muted, tail)
				}
			}
		}
		rows = append(rows, displayRow{text: m.control(fit(lead, prefix)) + text})
	}
	return rows, cursor
}

// The rule takes one row in every state. Its left side is live activity;
// optional hints and the worktree action share the right without taking
// space from the editor or wrapping the rule into the transcript.
func (m *Model) promptRule(t *threadState, w int, paused string, hints []string) displayRow {
	phase, tool, count := t.liveStatus()
	var right displayRow
	if paused != "" {
		key := shellKeys.follow.Help().Key
		resume := paused + " · " + key + " resume"
		if phase != liveIdle && stringWidth(resume) > w/3 {
			resume = key + " resume"
			if w < 52 {
				resume = key
			}
		}
		if stringWidth(resume) > w-3 {
			resume = paused
		}
		right.text = m.control(ansi.Truncate(resume, max(0, w-3), "…"))
	}
	const ruleSpace = 3 // a stroke and room on each side
	liveWidth := w - ruleSpace - stringWidth(right.text)
	if len(hints) > 0 && phase != liveFailed && w >= 56 {
		first := stringWidth(hints[0]) + 2
		if liveWidth-first >= 28 {
			liveWidth -= first // keep the most important hint beside long commands
		}
	}
	out := m.liveActivity(t, liveWidth, phase, tool, count)
	if phase == liveIdle {
		out = m.planActivity(t, liveWidth)
		if d := m.focusedDialog(); out.text == "" && d != nil && d.plan != nil && liveWidth >= 6 {
			out.text = m.control("○ idle")
		}
	}
	leftWidth := stringWidth(out.text)
	room := w - ruleSpace - leftWidth - stringWidth(right.text)
	for _, hint := range hints {
		n := stringWidth(hint)
		if right.text != "" {
			n += 2
		}
		if n > room {
			break
		}
		mergeRow(&right, displayRow{text: hint}, "  ")
		room -= n
	}
	if right.text != "" {
		room -= 2
	}
	mergeRow(&right, m.gitDiffBadgeRow(t, room), "  ")
	strokes := w - leftWidth - stringWidth(right.text)
	if out.text != "" {
		out.text += " "
		strokes--
	}
	if right.text != "" {
		strokes--
	}
	out.text += m.paint(x.Muted, strings.Repeat("─", max(0, strokes)))
	mergeRow(&out, right, " ")
	return out
}

func (m *Model) ruleHints(t *threadState, slashes, files bool, fileChoices int) []string {
	if m.focusedDialog() != nil {
		return nil
	}
	if m.leader != "" {
		var labels []string
		for _, a := range m.host.actions {
			prefix := strings.TrimSpace(m.leader)
			if m.eligible(a, t.info.ID) && (prefix == "" || strings.HasPrefix(a.action.Binding.Leader, prefix+" ")) {
				labels = append(labels, m.keyHint(a.action.Binding.Leader, oneLine(a.action.Title)))
			}
		}
		return labels
	}
	if slashes {
		return []string{m.keyHint("enter", "run"), m.keyHint("tab", "complete"), m.keyHint("esc", "close")}
	}
	if files {
		hints := []string{m.keyHint("esc", "close")}
		if (m.files.kind != fileMention || m.files.query != "") && fileChoices > 0 {
			if m.files.kind == fileRoots {
				hints = append(hints, m.keyHint("enter", "open"))
			} else {
				hints = append(hints, m.keyHint("tab", "complete"))
			}
			if fileChoices > 1 {
				hints = append(hints, m.keyHint("↑↓", "move"))
			}
		}
		return hints
	}
	if m.hover != "" {
		return []string{m.control(oneLine(m.hover))}
	}
	if threadBusy(t) {
		hints := []string{m.keyHint("esc", "stop")}
		if m.editor.value() != "" || len(t.attachments) != 0 {
			hints = append(hints, m.keyHint("enter", "steer"), m.keyHint("alt+enter", "follow-up"))
		}
		return hints
	}
	if canStopPlan(t) {
		return []string{m.keyHint("esc", "stop")}
	}
	if len(t.attachments) != 0 {
		return []string{m.keyHint("enter", "send"), m.keyHint("ctrl+c", "clear")}
	}
	if m.editor.value() != "" {
		return []string{m.keyHint("enter", "send")}
	}
	hints := []string{m.keyHint("/", "commands")}
	model, modelOK := m.host.byID[modelPickerID]
	thinking, thinkingOK := m.host.byID[x.ActionID(menusID+".thinking")]
	if modelOK && thinkingOK && m.eligible(m.host.actions[model], t.info.ID) && m.eligible(m.host.actions[thinking], t.info.ID) {
		hints = append(hints,
			m.keyHint(m.actionBinding(m.host.actions[model]), "model"),
			m.keyHint(m.actionBinding(m.host.actions[thinking]), "thinking"))
	}
	if t.intro != nil && !t.intro.frozen && len(m.tabs) < 2 {
		hints = append(hints, m.keyHint(m.actionBinding(m.host.actions[m.host.byID[switcherID]]), "threads"))
	}
	hints = append(hints, m.keyHint("?", "all keys"))
	if m.fileAvailable("") {
		hints = append(hints, m.keyHint("@", "files"))
	}
	return hints
}

type cell struct{ x, y int }
type selection struct {
	from, to           cell
	dragging, selected bool
}

func (s selection) bounds() (cell, cell) {
	a, b := s.from, s.to
	if a.y > b.y || a.y == b.y && a.x > b.x {
		a, b = b, a
	}
	return a, b
}

func (s selection) span(y, w int) (int, int, bool) {
	a, b := s.bounds()
	if y < a.y || y > b.y {
		return 0, 0, false
	}
	lo, hi := 0, w
	if a.y == y {
		lo = min(a.x, w)
	}
	if b.y == y {
		hi = min(b.x+1, w)
	}
	return lo, hi, lo < hi
}

func (m *Model) picked() string {
	var rows []string
	a, b := m.selection.bounds()
	for y := max(0, a.y); y <= min(b.y, m.frame.transcriptHeight-1); y++ {
		lo, hi, ok := m.selection.span(y, m.width-m.pane().edge)
		if ok {
			rows = append(rows, strings.TrimRight(ansi.Strip(ansi.Cut(m.frame.rows[y], lo, hi)), " "))
		}
	}
	return strings.Join(rows, "\n")
}

func (m *Model) hitAt(x, y int) (spanHit, bool) {
	if y < 0 || y >= len(m.frame.hits) {
		return spanHit{}, false
	}
	for _, hit := range m.frame.hits[y] {
		if x >= hit.first && x < hit.last {
			return hit, true
		}
	}
	return spanHit{}, false
}

func (m *Model) hoverAt(x, y int) string {
	if !m.currentFrame() || m.focusedDialog() != nil {
		return ""
	}
	if hit, ok := m.hitAt(x, y); ok {
		return hit.hint
	}
	if y >= 0 && y < m.frame.transcriptHeight {
		return linkHint(linkAt(m.frame.rows[y], x))
	}
	return ""
}

func motionFilter(model tea.Model, msg tea.Msg) tea.Msg {
	m, ok := model.(*Model)
	if !ok {
		return msg
	}
	motion, ok := msg.(tea.MouseMotionMsg)
	if !ok {
		return msg
	}
	p := motion.Mouse()
	if d := m.focusedDialog(); d != nil {
		if d.custom == nil {
			return nil
		}
		return msg
	}
	if m.selection.dragging {
		if m.selection.to == (cell{p.X, p.Y}) {
			return nil
		}
		return msg
	}
	if m.hoverAt(p.X, p.Y) == m.hover {
		return nil
	}
	return msg
}

func (m *Model) mouse(msg tea.Msg) tea.Cmd {
	if !m.currentFrame() {
		return nil
	}
	event := msg.(tea.MouseMsg)
	p := event.Mouse()
	f := m.frame
	// Preserve tab switching through sheets. Stacked dialogs own all clicks.
	if _, click := msg.(tea.MouseClickMsg); click && p.Button == tea.MouseLeft && len(m.overlays) == 0 && p.Y >= f.transcriptHeight {
		if hit, ok := m.hitAt(p.X, p.Y); ok && hit.action.ID == activateTabID {
			if id := sdk.ID(hit.action.Target); id.Valid() {
				return m.switchThread(id)
			}
		}
	}
	if d := m.focusedDialog(); d != nil {
		if f.dialogToken != d.token {
			return nil
		}
		inside := p.X >= f.dialogX && p.X < f.dialogX+f.dialogWidth && p.Y >= f.dialogY && p.Y < f.dialogY+f.dialogHeight
		if inside && d.git != nil {
			return m.gitDiffMouse(d, msg, p.Y-f.dialogY)
		}
		if inside && d.config != nil {
			return m.configMouse(d, msg, p.Y-f.dialogY)
		}
		switch msg.(type) {
		case tea.MouseClickMsg:
			if !inside {
				if p.Button == tea.MouseLeft && (d.themePicker == nil || !d.themePicker.saving) {
					return m.closeDialog(d, nil, false)
				}
			} else if d.custom == nil && p.Button == tea.MouseLeft {
				i := p.Y - f.dialogY
				if i >= 0 && i < len(f.dialogChoices) {
					target := f.dialogChoices[i]
					if target >= 0 {
						if d.planWorkspace != nil {
							d.selected = target
							return m.planWorkspaceKey(d, "enter")
						}
						if d.plan != nil {
							v := m.planViewFor(d)
							left, width := f.dialogX, f.dialogWidth
							if v.snapshot.Plan != nil && f.dialogPlanRev == v.snapshot.Revision && target < len(v.snapshot.Plan.Steps) &&
								p.X >= left && p.X < left+planListWidth(width) {
								d.plan.focus, d.plan.details, d.plan.scroll = target, false, 0
								m.togglePlanStep(v, d.plan)
							}
							return nil
						}
						if p := d.themePicker; p != nil {
							if p.loading || p.saving {
								return nil
							}
							// A click previews a theme; clicking the previewed row again
							// applies it, like Enter. The starter row copies at once.
							again := target == d.selected
							d.selected = target
							if c, ok := d.choice(); ok && (again || c.ID == themeTemplateChoice) {
								return m.acceptTheme(d)
							}
							m.previewTheme(d)
							return nil
						}
						d.selected = target
						return m.acceptChoice(d)
					}
					if target == themeFillsTarget && d.themePicker != nil {
						m.moveFills(d, 1)
						return nil
					}
					if d.workTrust != nil {
						switch target {
						case workspaceTrustGrantTarget:
							return m.workspaceTrustKey(d, "t")
						case workspaceTrustDismissTarget:
							return m.workspaceTrustKey(d, "n")
						case workspaceTrustCopyTarget:
							return m.workspaceTrustKey(d, "y")
						case workspaceTrustStartTarget:
							return m.workspaceTrustKey(d, "w")
						}
					}
					if target == outputSkillsTarget && d.loadedContext != nil {
						left := f.dialogX
						if p.X >= left && p.X < left+stringWidth(m.contextSkillsHint()) {
							return m.openContextSkills(d)
						}
					}
					if target == outputCopyTarget && d.output != nil {
						left := f.dialogX
						if p.X >= left && p.X < left+stringWidth(m.outputCopyHint()) {
							return m.copyToolOutput(d)
						}
					}
				}
			}
		case tea.MouseWheelMsg:
			if inside && d.themePicker != nil {
				switch p.Button {
				case tea.MouseWheelUp:
					m.moveTheme(d, -1)
				case tea.MouseWheelDown:
					m.moveTheme(d, 1)
				}
			} else if inside && d.output != nil {
				if p.Button == tea.MouseWheelUp {
					m.outputMove(d, -3, 0)
				} else if p.Button == tea.MouseWheelDown {
					m.outputMove(d, 3, 0)
				}
			} else if inside && (d.planWorkspace != nil || d.planHistory != nil || d.planReview != nil || d.plan != nil) {
				move := m.planKey
				if d.planWorkspace != nil {
					move = m.planWorkspaceKey
				} else if d.planHistory != nil {
					move = m.planHistoryKey
				} else if d.planReview != nil {
					move = m.planReviewKey
				}
				if p.Button == tea.MouseWheelUp {
					return move(d, "up")
				}
				if p.Button == tea.MouseWheelDown {
					return move(d, "down")
				}
			} else if inside && d.custom == nil {
				if p.Button == tea.MouseWheelUp {
					d.move(-1)
				} else if p.Button == tea.MouseWheelDown {
					d.move(1)
				}
			}
		}
		if inside && d.custom != nil {
			x, y := f.dialogX, f.dialogY+1 // skip the title row
			return m.customUpdate(d, relativeMouse(event, x, y))
		}
		return nil
	}
	switch msg.(type) {
	case tea.MouseWheelMsg:
		if p.Button == tea.MouseWheelUp {
			return m.scroll(-3)
		}
		if p.Button == tea.MouseWheelDown {
			return m.scroll(3)
		}
	case tea.MouseMotionMsg:
		if m.selection.dragging {
			m.selection.to = cell{max(0, p.X), max(0, min(f.transcriptHeight-1, p.Y))}
			return nil
		}
		m.hover = m.hoverAt(p.X, p.Y)
	case tea.MouseClickMsg:
		m.selection = selection{}
		if p.Button == tea.MouseMiddle {
			if h, ok := m.hitAt(p.X, p.Y); ok && h.action.ID == activateTabID {
				b := maxEffects
				return m.invoke(m.route(m.active, coreID), x.ActionRef{ID: coreID + ".close", Target: h.action.Target}, &b)
			}
		}
		if p.Button != tea.MouseLeft {
			return nil
		}
		if p.Y >= 0 && p.Y < f.transcriptHeight {
			m.selection = selection{cell{p.X, p.Y}, cell{p.X, p.Y}, true, false}
		}
		if p.Y >= f.transcriptHeight {
			if h, ok := m.hitAt(p.X, p.Y); ok {
				if p.Y >= f.slashStart && p.Y < f.slashEnd {
					if h.action.ID == browsePathsID {
						m.openPathRoots()
						return nil
					}
					if i, known := m.host.byID[h.action.ID]; known && m.host.actions[i].action.Slash != nil {
						return m.runSlash(m.host.actions[i])
					}
				}
				b := maxEffects
				return m.invoke(m.route(m.active, coreID), h.action, &b)
			}
		}
	case tea.MouseReleaseMsg:
		if !m.selection.dragging || p.Button != tea.MouseLeft {
			return nil
		}
		m.selection.dragging = false
		m.selection.to = cell{max(0, p.X), max(0, min(f.transcriptHeight-1, p.Y))}
		b := maxEffects
		r := m.route(m.active, coreID)
		if m.selection.from != m.selection.to {
			m.selection.selected = true
			return m.copyText(r, m.picked(), &b)
		}
		if p.Y < 0 || p.Y >= f.transcriptHeight {
			return nil
		}
		if host := linkAt(f.rows[p.Y], p.X); host != "" {
			return m.openLink(r, host, &b)
		}
		if hit, ok := m.hitAt(p.X, p.Y); ok {
			return m.invoke(r, hit.action, &b)
		}
	}
	return nil
}

func relativeMouse(event tea.MouseMsg, x, y int) tea.Msg {
	p := event.Mouse()
	p.X, p.Y = p.X-x, p.Y-y
	switch event.(type) {
	case tea.MouseClickMsg:
		return tea.MouseClickMsg(p)
	case tea.MouseReleaseMsg:
		return tea.MouseReleaseMsg(p)
	case tea.MouseMotionMsg:
		return tea.MouseMotionMsg(p)
	default:
		return tea.MouseWheelMsg(p)
	}
}
