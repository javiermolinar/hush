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
	navigation                                  uint64
	width, height                               int
	transcriptHeight, ruleY                     int
	hasAbove, hasBelow, hasTranscript           bool
	thumbStart, thumbEnd                        int
	top                                         rowAnchor
	openActivity                                string
	dialogToken                                 uint64
	dialogX, dialogY, dialogWidth, dialogHeight int
	dialogFramed                                bool
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
func fit(s string, w int) string {
	n := stringWidth(s)
	if n > w {
		s = ansiCut(s, w)
		n = stringWidth(s)
	}
	return s + strings.Repeat(" ", max(0, w-n))
}

func (m *Model) scrollCell(f *frameMemo, y int) string {
	if y >= f.thumbStart && y < f.thumbEnd {
		return m.control("┃")
	}
	return paint(x.Muted, "│")
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
	f := frameMemo{owner: m.active, navigation: m.navigation, width: m.width, height: m.height, rows: make([]string, m.height), hits: make([][]spanHit, m.height)}
	v := tea.NewView("")
	v.AltScreen = true
	if m.width < 40 || m.height < 12 {
		if m.height > 0 && m.width > 0 {
			f.rows[0] = ansiCut("terminal too small", m.width)
		}
		v.SetContent(strings.Join(f.rows, "\n"))
		*m.frame = f
		return v
	}
	t := m.threads[m.active]
	w := m.width - 4
	var slash []registeredAction
	var below []displayRow
	f.slashStart, f.slashEnd = m.height, m.height
	continuation, filePicker := false, false
	chrome := m.chromeRows(t, w)
	listEnd := m.height - len(chrome)
	var overlayRoom int
	if len(m.overlays) != 0 {
		overlayRoom = minOverlayBodyRows
	}
	if m.sheet != nil {
		limit := m.dialogLimit(m.sheet)
		if overlayRoom != 0 {
			limit = min(limit, max(1, listEnd-1-max(1, m.height/4)-overlayRoom))
		}
		rows, choices := m.dialogRows(m.sheet, w, limit)
		for _, row := range rows {
			below = append(below, displayRow{text: row})
		}
		f.dialogToken, f.dialogX, f.dialogWidth = m.sheet.token, 2, w
		f.dialogY, f.dialogHeight = listEnd-len(rows), len(rows)
		f.dialogChoices = choices
	} else if overlayRoom > 0 {
		continuation = true // an overlay owns focus; hide background completion lists
	} else if files := m.fileRows(w); len(files) > 0 {
		below, filePicker = files, true
	} else if slash = m.slashMatches(); len(slash) > 0 {
		below = m.slashSheet(slash, w)
		f.slashStart, f.slashEnd = listEnd-len(below)+1, listEnd
	} else {
		continuation = true
	}
	below = append(below, chrome...)

	editorRows, cursor := m.promptRows(t, w, slash)
	var notice displayRow
	free := m.height - 1 - len(editorRows) - len(below)
	var attachments []displayRow
	if free > overlayRoom+1 && m.focusedDialog() == nil && (t.attaching || len(t.attachments) != 0) {
		attachments = []displayRow{m.attachmentRow(t, w)}
		free--
	}
	if free > overlayRoom && m.notice.text != "" {
		notice.text = paint(m.notice.tone, ansi.Truncate(oneLine(m.notice.text), w, "…"))
		free--
	}
	noticeRows := 0
	if notice.text != "" {
		noticeRows = 1
	}
	room := max(0, free-max(1, overlayRoom))
	queued := t.queueRows(w, room)
	room -= len(queued)
	widgetAbove := m.widgetRows(t, w, x.AboveEditor, min(3, room))
	if continuation {
		widgetBelow := m.widgetRows(t, w, x.BelowEditor, min(3, room-len(widgetAbove)))
		below = append(widgetBelow, below...)
	}
	f.transcriptHeight = max(0, m.height-1-len(editorRows)-len(below)-len(widgetAbove)-len(queued)-noticeRows-len(attachments))
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
	add := func(y int, r displayRow) {
		if y < 0 || y >= len(f.rows) {
			return
		}
		if m.copied.token != 0 && m.copied.owner == m.active {
			old := hyperlink("copy", "", "hush-copy:"+m.copied.id) + "    "
			r.text = strings.Replace(r.text, old, paint(x.Good, "✓ copied"), 1)
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
		line := fit(r.text, w)
		if r.card {
			line = surface(line, m.palette.softInk)
		}
		prefix := margin
		if r.user {
			prefix = paint(x.Accent, "│") + " "
		}
		f.rows[y] = prefix + line
		if r.tabBand {
			f.rows[y] = surface(f.rows[y]+margin, m.palette.softInk)
		}
		if rail && y < f.transcriptHeight {
			f.rows[y] += " " + m.scrollCell(&f, y)
		}
		if len(r.hits) > 0 {
			for _, hit := range r.hits {
				hit.first += 2
				hit.last = min(2+w, hit.last+2)
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
					f.rows[y] += " " + m.scrollCell(&f, y)
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
		if len(m.overlays) == 0 && row.image != nil && len(f.imageWants) < previewCacheSize {
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
		spaces := strings.Repeat(" ", m.width-1)
		for y := 0; y < f.transcriptHeight; y++ {
			if f.rows[y] == "" {
				f.rows[y] = spaces + m.scrollCell(&f, y)
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
	add(y, m.promptRule(t, w, paused, m.ruleHints(t, len(slash) > 0, filePicker)))
	y++
	if cursor != nil && m.focused {
		cursor.Position.X += len(margin)
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

	if m.selection.dragging || m.selection.selected {
		for i := 0; i < f.transcriptHeight; i++ {
			if a, b, ok := m.selection.span(i, m.width-2); ok {
				line := fit(f.rows[i], m.width)
				f.rows[i] = ansi.Cut(line, 0, a) + ansiCut("\x1b[7m"+ansi.Strip(ansi.Cut(line, a, b))+"\x1b[27m", b-a) + ansi.Cut(line, b, m.width)
			}
		}
	}
	for i, d := range m.overlays {
		rows, choices := m.overlayRows(d, f.transcriptHeight)
		width := m.dialogWidth(d)
		left := max(0, (m.width-width)/2)
		space := max(0, f.transcriptHeight-len(rows))
		anchorSpace := space
		if d.palette {
			anchorSpace = max(0, f.transcriptHeight-paletteMaxHeight)
		}
		top := min(space, anchorSpace/3+2*i)
		if d.git != nil && !d.git.patch {
			top = space
		}
		for j, row := range rows {
			y := top + j
			base := fit(f.rows[y], m.width)
			f.rows[y] = ansi.Cut(base, 0, left) + fit(row, width) + ansi.Cut(base, left+width, m.width)
		}
		if i == len(m.overlays)-1 {
			f.dialogToken, f.dialogX, f.dialogY = d.token, left, top
			f.dialogWidth, f.dialogHeight = width, len(rows)
			f.dialogFramed = f.transcriptHeight >= minOverlayFrameRows
			f.dialogChoices = choices
			if d.git != nil {
				v.Cursor = nil
			}
			if d.palette && m.focused {
				v.Cursor = m.paletteCursor(d, left, top, width, len(rows), f.dialogFramed)
			}
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

func (m *Model) slashSheet(matches []registeredAction, w int) []displayRow {
	// Keep the selected command in view without making the transcript jump
	// with every arrow press. All matching commands remain reachable.
	limit := min(5, len(matches))
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
	out := make([]displayRow, 1, limit+1) // one blank row separates input and list
	for i, a := range visible {
		active := start+i == selected
		prefix := "  "
		tone := x.Plain
		if active {
			prefix = paint(x.Accent, "› ")
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
		line := prefix + paint(tone, fit(name, nameWidth))
		if noteWidth > 0 {
			line += "  " + m.control(ansi.Truncate(oneLine(note), noteWidth, "…"))
		}
		if keyWidth > 0 {
			hint := m.actionBinding(a)
			line = fit(line, w-stringWidth(hint)) + m.control(hint)
		}
		if active && m.theme.Fills {
			line = fill(fit(line, w), blendPalette(m.palette.background, m.palette.colors[4], .15))
		}
		ref := x.ActionRef{ID: a.action.ID}
		if ref.ID == stageSkillID {
			ref.Target = a.skill.name + "\n" + a.skill.token
		}
		out = append(out, displayRow{text: line, hits: []spanHit{{0, w, ref, a.action.Title}}})
	}
	return out
}

func (m *Model) promptRows(t *threadState, w int, slash []registeredAction) ([]displayRow, *tea.Cursor) {
	label := "you › "
	intro := t.intro != nil && !t.intro.frozen
	if intro {
		label = "main › "
	}
	if threadBusy(t) && m.focusedDialog() == nil && len(slash) == 0 {
		label = "steer › "
	}
	prefix := stringWidth(label)
	maxRows := max(1, m.height/4)
	output := m.editor.rows(m.editor.area.Width(), maxRows)
	var cursor *tea.Cursor
	if m.focusedDialog() == nil {
		if c := m.editor.area.Cursor(); c != nil {
			c.Position.X = prefix + min(c.Position.X, w-prefix-1)
			c.Position.Y = min(max(0, c.Position.Y), len(output)-1)
			cursor = c
		}
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
					text = strings.TrimRight(text, " ") + paint(x.Muted, tail)
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
		right.text = m.control(resume)
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
	out.text += paint(x.Muted, strings.Repeat("─", max(0, strokes)))
	mergeRow(&out, right, " ")
	return out
}

func (m *Model) ruleHints(t *threadState, slashes, files bool) []string {
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
		return []string{m.keyHint("esc", "close")}
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
	if len(t.attachments) != 0 {
		return []string{m.keyHint("enter", "send"), m.keyHint("ctrl+c", "clear")}
	}
	if m.editor.value() != "" {
		return []string{m.keyHint("enter", "send")}
	}
	hints := []string{m.keyHint("/", "commands")}
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
		lo, hi, ok := m.selection.span(y, m.width-2)
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
	// Switching tabs dismisses an under-prompt sheet, but a modal overlay
	// must consume the click without letting it through to the footer.
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
		switch msg.(type) {
		case tea.MouseClickMsg:
			if !inside {
				if p.Button == tea.MouseLeft {
					return m.closeDialog(d, nil, false)
				}
			} else if d.custom == nil && p.Button == tea.MouseLeft {
				i := p.Y - f.dialogY
				if i >= 0 && i < len(f.dialogChoices) {
					target := f.dialogChoices[i]
					if target >= 0 {
						d.selected = target
						return m.acceptChoice(d)
					}
					if d.workTrust != nil {
						switch target {
						case workspaceTrustGrantTarget:
							return m.workspaceTrustKey(d, "t")
						case workspaceTrustDismissTarget:
							return m.workspaceTrustKey(d, "n")
						}
					}
					if target == outputSkillsTarget && d.loadedContext != nil {
						left := f.dialogX
						if f.dialogFramed {
							left += 2
						}
						if p.X >= left && p.X < left+stringWidth(m.contextSkillsHint()) {
							return m.openContextSkills(d)
						}
					}
					if target == outputCopyTarget && d.output != nil {
						left := f.dialogX
						if f.dialogFramed {
							left += 2
						}
						if p.X >= left && p.X < left+stringWidth(m.outputCopyHint()) {
							return m.copyToolOutput(d)
						}
					}
				}
			}
		case tea.MouseWheelMsg:
			if inside && d.output != nil {
				if p.Button == tea.MouseWheelUp {
					m.outputMove(d, -3, 0)
				} else if p.Button == tea.MouseWheelDown {
					m.outputMove(d, 3, 0)
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
			if f.dialogFramed {
				x, y = x+2, y+1
			}
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
				if i, known := m.host.byID[h.action.ID]; known && p.Y >= f.slashStart && p.Y < f.slashEnd && m.host.actions[i].action.Slash != nil {
					return m.runSlash(m.host.actions[i])
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
