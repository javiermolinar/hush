package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

// A panel owns full content-width rows ending at the live rule. Placement
// (Sheet or Overlay) still controls dialog lifetime, not frame geometry.
type menuFrame struct {
	rows                  []string
	targets               []int // -1 for chrome, otherwise a row or action in the owner's input handler
	cursor                *tea.Cursor
	listWidth, bodyHeight int
}

func (m *Model) menuFrame(d *dialog, available int) menuFrame {
	if available <= 0 {
		return menuFrame{}
	}
	w, n := m.pane().width, min(available, max(1, m.height*7/10))
	if d.full {
		n = available
	}
	var frame menuFrame
	switch {
	case d.planWorkspace != nil:
		frame = m.planWorkspaceRows(d, w, n)
	case d.planHistory != nil:
		frame = m.planHistoryRows(d, w, n)
	case d.planReview != nil:
		frame = m.planReviewRows(d, w, n)
	case d.plan != nil:
		frame = m.planRows(d, w, n)
	case d.git != nil:
		frame = m.gitReviewRows(d, w, n)
	case d.workTrust != nil:
		frame.rows, frame.targets = m.workspaceTrustRows(d.workTrust, w, n)
	case d.config != nil:
		frame = m.configDockRows(d, w, n)
	case d.palette:
		frame.rows, frame.targets = m.paletteRows(d, w, n)
		frame.cursor = m.paletteCursor(d, w, len(frame.rows))
	case d.themePicker != nil:
		frame.rows, frame.targets = m.themeRows(d, w, n)
	case d.output != nil:
		frame.rows, frame.targets = m.outputRows(d, w, n)
	default:
		frame = m.dialogRows(d, w, n)
	}
	return frame
}

// Menu chrome is shared by list panels, form panels and document viewers.
// Targets on the chrome are optional; they are not list indices.
type menuLine struct {
	text   string
	target int
}

func chromeLine(text string) menuLine { return menuLine{text, -1} }

type menuChrome struct {
	title, scope, count string
	headerTarget        int // zero means no action; chrome actions are negative
	consequence, keys   menuLine
	minBody             int
}

func (f *menuFrame) add(text string, target int) {
	f.rows = append(f.rows, text)
	f.targets = append(f.targets, target)
}

func (f *menuFrame) appendBody(body menuFrame) {
	f.rows = append(f.rows, body.rows...)
	f.targets = append(f.targets, body.targets...)
	f.listWidth = body.listWidth
}

func (m *Model) menuShell(w, n int, c menuChrome, body func(int) menuFrame) menuFrame {
	if n <= 0 {
		return menuFrame{}
	}
	floor := max(1, c.minBody)
	h := n
	header := c.title != "" && (h > floor+1 || h > floor && c.consequence.text == "")
	if header {
		h--
	}
	action := c.consequence.text != "" && h > floor
	if action {
		h--
	}
	keys := c.keys.text != "" && h > floor
	if keys {
		h--
	}
	gaps := keys && m.height >= 24 && h >= floor+4
	if gaps {
		h -= 2
	}
	content := body(h)
	var out menuFrame
	out.listWidth = content.listWidth
	if header {
		out.add(m.menuHeader(w, c.title, c.scope, c.count), min(-1, c.headerTarget))
	}
	if content.cursor != nil {
		out.cursor = content.cursor
		out.cursor.Position.Y += len(out.rows)
	}
	out.rows = append(out.rows, content.rows...)
	out.targets = append(out.targets, content.targets...)
	out.bodyHeight = len(content.rows)
	if gaps {
		out.add("", -1)
	}
	if action {
		out.add(c.consequence.text, c.consequence.target)
	}
	if keys {
		out.add(c.keys.text, c.keys.target)
	}
	if gaps {
		out.add("", -1)
	}
	for i, row := range out.rows {
		out.rows[i] = ansi.Truncate(row, w, "…")
	}
	return out
}

func (m *Model) menuHeader(w int, title, scope, count string) string {
	count = ansi.Truncate(oneLine(count), max(0, w/2), "…")
	title = oneLine(title)
	if scope != "" {
		title += " · " + oneLine(scope)
	}
	reserve := 0
	if count != "" {
		reserve = stringWidth(count) + 1
	}
	title = ansi.Truncate(title, max(1, w-reserve-2), "…")
	strokes := max(0, w-stringWidth(title)-reserve-1)
	line := strongText(title) + m.quiet(" "+strings.Repeat("─", strokes))
	if count != "" {
		line += " " + m.quiet(count)
	}
	return line
}

func (m *Model) menuAction(label, effect string) string {
	if effect == "" {
		return strongText("↵ " + label)
	}
	return strongText("↵ "+label) + "  " + effect
}

// Keys are in descending importance. Keep Escape visible in compact frames.
func (m *Model) menuKeys(w int, hints ...string) string {
	const back = "esc back"
	if w <= len(back) {
		return m.quiet(ansi.Truncate(back, w, "…"))
	}
	var kept []string
	space := w - len(back)
	for _, hint := range hints {
		if hint == "" {
			continue
		}
		if need := stringWidth(hint) + 2; need <= space {
			kept = append(kept, hint)
			space -= need
		}
	}
	return m.quiet(strings.Join(append(kept, back), "  "))
}

const menuGutter = " │ "

func menuListWidth(w int) int {
	if w >= 70 {
		return w / 2
	}
	return w
}

func menuDetailWidth(w int) int {
	if list := menuListWidth(w); list < w {
		return w - list - stringWidth(menuGutter)
	}
	return w
}

// The list callback receives only enough rows for the visible window. Narrow
// panes stack the detail, reserving at least one row for the active list item.
func (m *Model) menuColumns(w, h int, detail func(int, int) []string, list func(int, int) menuFrame) menuFrame {
	lw := menuListWidth(w)
	if h < 2 || detail == nil {
		return list(w, h)
	}
	if lw == w {
		lines := detail(w, max(1, h/2))
		out := list(w, h-len(lines))
		for _, line := range lines {
			out.add(line, -1)
		}
		return out
	}
	lines := detail(menuDetailWidth(w), h)
	if len(lines) == 0 {
		return list(w, h)
	}
	out := list(lw, h)
	out.listWidth = lw
	for i := range max(len(out.rows), len(lines)) {
		if i >= len(out.rows) {
			out.add("", -1)
		}
		var right string
		if i < len(lines) {
			right = lines[i]
		}
		out.rows[i] = fit(out.rows[i], lw) + m.quiet(menuGutter) + right
	}
	return out
}

func (m *Model) menuDetail(title, description string, w, h int) []string {
	rows := []string{strongText(ansi.Truncate(oneLine(title), w, "…"))}
	for _, line := range wrapped(safeText(description), w) {
		if len(rows) >= h || description == "" {
			break
		}
		rows = append(rows, m.quiet(line))
	}
	return rows
}

type menuItem struct {
	title, selection, icon, state string
	tone                          x.Tone
	focus, disabled               bool
}

func (m *Model) menuRow(w, stateWidth int, item menuItem) string {
	prefix := "  "
	if item.focus {
		prefix = m.paint(x.Accent, "› ")
	}
	if item.selection != "" {
		prefix += item.selection + " "
	}
	if item.icon != "" {
		prefix += m.paint(item.tone, item.icon) + " "
	}
	room := max(0, w-stringWidth(prefix))
	if stateWidth > 0 && stateWidth+6 > room {
		stateWidth = 0
	}
	nameWidth := room - stateWidth
	if stateWidth > 0 {
		nameWidth -= 2
	}
	label := ansi.Truncate(oneLine(item.title), nameWidth, "…")
	if item.focus {
		label = strongText(label)
	} else if item.disabled {
		label = m.quiet(label)
	}
	row := prefix + label
	if stateWidth > 0 {
		row = fit(row, w-stateWidth) + m.paint(item.tone, ansi.Truncate(oneLine(item.state), stateWidth, "…"))
	}
	return m.cursorRow(row, w, item.focus)
}

func menuWindow(selected, count, budget int) (start, end int) {
	start = min(max(0, selected-budget+1), max(0, count-budget))
	return start, min(count, start+budget)
}

func menuCount(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + plural
}

// Do not intercept f in a text field or a type-to-filter list.
func (d *dialog) canZoom() bool {
	return d.plan != nil || d.planWorkspace != nil || d.planHistory != nil || d.planReview != nil ||
		d.git != nil || d.output != nil || d.workTrust != nil || d.themePicker != nil ||
		d.config != nil && !d.config.pickingModel
}

func (d *dialog) zoomKey() string {
	if d.full {
		return "f fit"
	}
	return "f full"
}
