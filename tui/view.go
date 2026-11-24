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
	transcriptHeight                            int
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
	continuation := false
	footerRows := 0
	if m.intro != nil {
		footerRows = 1
		if len(m.tabs) >= 2 {
			footerRows++
		}
	}
	listEnd := m.height - footerRows
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
		below = files
	} else if slash = m.slashMatches(); len(slash) > 0 {
		below = m.slashSheet(slash, w)
		f.slashStart, f.slashEnd = listEnd-len(below)+1, listEnd
	} else {
		if t.intro != nil && !t.intro.frozen && len(m.tabs) < 2 {
			below = []displayRow{m.introHints(w, t.intro.mono())}
		}
		continuation = true
	}
	if footerRows != 0 || continuation {
		below = append(below, m.chromeRows(t, w)...)
	}

	editorRows, cursor := m.promptRows(t, w, slash)
	var live []displayRow
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
	if free > overlayRoom {
		ctx := m.renderContext(t, w, 1, true)
		ctx.MaxBytes = min(ctx.MaxBytes, 4<<10)
		ctx.Expanded = t.expanded
		if t.liveOverride {
			ctx.Expanded = t.liveExpanded
		}
		if content := m.liveActivity(ctx, t); len(content) > 0 {
			live = m.layoutContent(ctx, renderKey{}, content)
		}
	}
	room := max(0, free-max(1, overlayRoom)-len(live))
	queued := t.queueRows(w, room)
	room -= len(queued)
	widgetAbove := m.widgetRows(t, w, x.AboveEditor, min(3, room))
	if continuation {
		widgetBelow := m.widgetRows(t, w, x.BelowEditor, min(3, room-len(widgetAbove)))
		below = append(widgetBelow, below...)
	}
	f.transcriptHeight = max(0, m.height-1-len(editorRows)-len(below)-len(widgetAbove)-len(live)-len(queued)-noticeRows-len(attachments))
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
	padding := strings.Repeat(" ", m.width)
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
		text, cells := clipWidth(r.text, w)
		lead, scroll := margin, ""
		pad := max(0, w-cells)
		if rail && y < f.transcriptHeight {
			scroll = m.scrollCell(&f, y)
		}
		if r.user && m.palette.soft != nil {
			f.rows[y] = fill(lead+text+padding[:pad+1], m.palette.soft) + scroll
		} else {
			if scroll != "" {
				pad++
			}
			if r.user {
				lead = paint(x.Muted, "│ ")
			}
			f.rows[y] = lead + text + padding[:pad] + scroll
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
		for y := 0; y < f.transcriptHeight; y++ {
			if f.rows[y] == "" {
				f.rows[y] = padding[:m.width-1] + m.scrollCell(&f, y)
			}
		}
	}
	y := f.transcriptHeight
	for _, row := range live {
		add(y, row)
		y++
	}
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
	rule := ""
	if !t.following && (t.intro == nil || t.intro.frozen || f.hasBelow || t.anchor != (rowAnchor{id: introEntry})) {
		label := "scroll paused"
		if f.hasBelow {
			label = "output below"
		}
		rule = paint(x.Muted, "─ ") + m.control(label+" · "+shellKeys.follow.Help().Key+" resume") + " "
	}
	add(y, displayRow{text: rule + paint(x.Muted, strings.Repeat("─", max(0, w-stringWidth(rule))))})
	y++
	for _, row := range attachments {
		add(y, row)
		y++
	}
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
		for j, row := range rows {
			y := top + j
			body := fit(row, width)
			if f.rows[y] == "" {
				f.rows[y] = padding[:left] + body + padding[left+width:]
				continue
			}
			base := fit(f.rows[y], m.width)
			f.rows[y] = ansi.Cut(base, 0, left) + body + ansi.Cut(base, left+width, m.width)
		}
		if i == len(m.overlays)-1 {
			f.dialogToken, f.dialogX, f.dialogY = d.token, left, top
			f.dialogWidth, f.dialogHeight = width, len(rows)
			f.dialogFramed = f.transcriptHeight >= minOverlayFrameRows
			f.dialogChoices = choices
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
		out = append(out, displayRow{text: line, hits: []spanHit{{0, w, x.ActionRef{ID: a.action.ID}, a.action.Title}}})
	}
	return out
}

func (m *Model) promptRows(t *threadState, w int, slash []registeredAction) ([]displayRow, *tea.Cursor) {
	label, tone := "you › ", x.Accent
	intro := t.intro != nil && !t.intro.frozen
	if intro {
		label = "main › "
		if t.intro.mono() {
			tone = x.Plain
		}
	}
	if threadBusy(t) && m.focusedDialog() == nil && len(slash) == 0 {
		label = "steer › "
	}
	prefix := stringWidth(label)
	maxRows := max(1, m.height/4)
	output, editorCursor := m.editor.layout(maxRows)
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
					text = strings.TrimRight(text, " ") + paint(x.Muted, tail)
				}
			}
		}
		if i == 0 && len(output) == 1 {
			right := m.promptChrome(t, len(slash) > 0)
			left := strings.TrimRight(text, " ")
			// Leave a cell for the cursor and a gap before the hints.
			room := w - prefix - stringWidth(left) - 2
			if room > 0 && len(right) > 0 {
				for len(right) > 0 && stringWidth(strings.Join(right, "  ")) > room {
					right = right[:len(right)-1]
				}
				tail := strings.Join(right, "  ")
				if tail != "" {
					text = fit(left, w-prefix-stringWidth(tail)) + tail
				} else if len(slash) > 0 {
					text = left
				}
			}
		}
		rows = append(rows, displayRow{text: paint(tone, fit(lead, prefix)) + text})
	}
	return rows, cursor
}

func (m *Model) promptChrome(t *threadState, slashes bool) []string {
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
	if m.hover != "" {
		return []string{m.control(oneLine(m.hover))}
	}
	if threadBusy(t) {
		return []string{m.keyHint("enter", "steer"), m.keyHint("esc", "stop"), m.keyHint("alt+enter", "follow-up")}
	}
	if len(t.attachments) != 0 {
		return []string{m.keyHint("enter", "send"), m.keyHint("ctrl+c", "clear")}
	}
	if m.editor.value() != "" {
		return []string{m.keyHint("enter", "send")}
	}
	if t.intro != nil && !t.intro.frozen && len(m.tabs) < 2 {
		return nil // shortcuts are on the line below the startup prompt
	}
	return []string{m.keyHint("/", "commands"), m.keyHint("?", "all keys")}
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
