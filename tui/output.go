package tui

import (
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const (
	outputTargetPrefix = "output/"
	outputCopyTarget   = -2
	toolPreviewLines   = 24
	toolPreviewBytes   = 16 << 10
	maxOutputWindows   = 256
	maxOutputWindowB   = 4 << 10
)

type outputViewer struct {
	raw          string
	lines        []outputLine
	top, left    int
	widest       int
	reviewSource []outputLine
	reviewWidth  int
	windows      map[outputWindow]string
}

type outputWindow struct{ row, left, width int }

type outputLine struct {
	text   string
	width  int
	ascii  bool
	points []outputPoint // every ~128 cells, only for long non-ASCII lines
}

type outputPoint struct{ cell, offset int }

func newOutputLine(text string) outputLine {
	row := outputLine{text: text, width: len(text)}
	if strings.IndexFunc(text, func(r rune) bool { return r >= 128 }) == -1 {
		row.ascii = true
		return row
	}
	if len(text) < 256 {
		row.width = stringWidth(text)
		return row
	}
	row.width = 0
	row.points = append(row.points, outputPoint{})
	for at := 0; at < len(text); {
		if row.width-row.points[len(row.points)-1].cell >= 128 {
			row.points = append(row.points, outputPoint{row.width, at})
		}
		g, w := ansi.FirstGraphemeCluster(text[at:], ansi.GraphemeWidth)
		at += len(g)
		row.width += w
	}
	return row
}

func (r outputLine) window(left, width int) string {
	if left >= r.width {
		return ""
	}
	end := min(left+width, r.width)
	if r.ascii {
		return r.text[left:end]
	}
	if len(r.points) <= 1 {
		return ansi.Cut(r.text, left, end)
	}
	lo := sort.Search(len(r.points), func(i int) bool { return r.points[i].cell > left }) - 1
	hi := sort.Search(len(r.points), func(i int) bool { return r.points[i].cell >= end })
	start := r.points[lo]
	last := len(r.text)
	if hi < len(r.points) {
		last = r.points[hi].offset
	}
	return ansi.Cut(r.text[start.offset:last], left-start.cell, end-start.cell)
}

func (m *Model) openToolOutput(r ownerRoute, target string) tea.Cmd {
	if !m.foreground(r) {
		return nil
	}
	id, index, ok := strings.Cut(target, "/")
	i, err := strconv.Atoi(index)
	record := m.threads[r.owner].entries[sdk.ID(id)]
	if !ok || err != nil || record == nil || i < 0 || i >= len(record.entry.Record.Messages) {
		return nil
	}
	msg := record.entry.Record.Messages[i]
	if msg.Role != "toolResult" {
		return nil
	}
	raw := toolResultText(msg)
	if raw == "" {
		return nil
	}
	title := "Tool output · " + oneLine(msg.Name)
	if record.entry.Truncated {
		title += " (retained)"
	}
	return m.pushDialog(&dialog{route: r, token: m.token(), title: title, output: newOutputViewer(raw)})
}

func newOutputViewer(raw string) *outputViewer {
	v := &outputViewer{raw: raw}
	for text := range strings.SplitSeq(strings.TrimSuffix(safeText(raw), "\n"), "\n") {
		line := newOutputLine(text)
		v.lines = append(v.lines, line)
		v.widest = max(v.widest, line.width)
	}
	return v
}

// Cache only visited windows. A vertical scroll usually reuses all but one
// row, and the overlay can reuse these padded cells without measuring them.
func (v *outputViewer) window(row, left, width int) string {
	key := outputWindow{row, left, width}
	if s, ok := v.windows[key]; ok {
		return s
	}
	s := fit(v.lines[row].window(left, width), width)
	if len(s) <= maxOutputWindowB {
		if v.windows == nil {
			v.windows = make(map[outputWindow]string)
		} else if len(v.windows) >= maxOutputWindows {
			clear(v.windows)
		}
		v.windows[key] = s
	}
	return s
}

func (v *outputViewer) reflowReview(width int) {
	width = max(1, width)
	if width == v.reviewWidth {
		return
	}
	if v.reviewSource == nil {
		v.reviewSource = v.lines
	}
	v.reviewWidth, v.top, v.left = width, 0, 0
	clear(v.windows)
	v.widest = width
	v.lines = make([]outputLine, 0, len(v.reviewSource))
	for _, line := range v.reviewSource {
		if line.width <= width {
			v.lines = append(v.lines, line)
			continue
		}
		if line.ascii {
			for start := 0; start < len(line.text); start += width {
				s := line.text[start:min(len(line.text), start+width)]
				v.lines = append(v.lines, outputLine{text: s, width: len(s), ascii: true})
			}
			continue
		}
		for s := range strings.SplitSeq(ansi.Hardwrap(line.text, width, true), "\n") {
			v.lines = append(v.lines, newOutputLine(s))
		}
	}
}

func (m *Model) outputCopyHint() string { return m.paint(x.Accent, "c copy all") }

// Output rows are already padded to exactly w cells.
func (m *Model) outputRows(d *dialog, w, n int) ([]string, []int) {
	v := d.output
	page := min(len(v.lines), outputPage(n))
	top := min(v.top, len(v.lines)-page)
	left := min(v.left, max(0, v.widest-w))
	rows, targets := make([]string, 0, page+2), make([]int, 0, page+2)
	add := func(s string, target int) {
		rows, targets = append(rows, s), append(targets, target)
	}
	if n >= 3 {
		add(fit(m.paint(x.Accent, ansi.Truncate(d.title, w, "…")), w), -1)
	}
	for i := top; i < top+page; i++ {
		add(v.window(i, left, w), -1)
	}
	if n >= 2 {
		copyHint := m.outputCopyHint()
		full := copyHint + "  " + m.keyHint("↑↓ ←→", "scroll") + "  " + m.keyHint("esc", "close")
		compact := copyHint + "  " + m.keyCap("↑↓ ←→") + "  " + m.keyCap("esc")
		target := outputCopyTarget
		if d.loadedContext != nil {
			compact, target = m.keyHint("esc", "close"), -1
			if m.contextCanOpenSkills(d) {
				compact = m.contextSkillsHint() + "  " + compact
				target = outputSkillsTarget
			}
			full = compact
			if page < len(v.lines) {
				full += "  " + m.keyHint("↑↓", "scroll")
			}
		} else if d.review {
			accept, escape := "continue", "cancel"
			if d.done == nil {
				accept, escape = "close", "close"
			}
			if top+page < len(v.lines) {
				accept = "more"
			}
			key := m.keyHint("enter", accept)
			full = key + "  " + m.keyHint("↑↓", "scroll") + "  " + m.keyHint("esc", escape) + "  " + copyHint
			compact = key + "  " + m.keyCap("esc")
			target = -1
		}
		add(fit(m.hintRow(w, full, compact, visibleRange(top, top+page, len(v.lines))), w), target)
	}
	return rows, targets
}

func (m *Model) copyToolOutput(d *dialog) tea.Cmd {
	budget := maxEffects
	return m.copyText(d.route, d.output.raw, &budget)
}

func outputPage(body int) int {
	switch {
	case body >= 3:
		return body - 2 // title and footer
	case body == 2:
		return 1 // footer only
	default:
		return max(0, body)
	}
}

func (m *Model) outputSize(d *dialog) (int, int) {
	w := m.pane().width
	if !m.currentFrame() || m.frame.dialogToken != d.token {
		return w, 1
	}
	return w, outputPage(m.frame.dialogHeight)
}

func (m *Model) outputMove(d *dialog, rows, cols int) {
	w, page := m.outputSize(d)
	v := d.output
	bottom, right := max(0, len(v.lines)-page), max(0, v.widest-w)
	v.top = min(bottom, max(0, min(v.top, bottom)+rows))
	v.left = min(right, max(0, min(v.left, right)+cols))
}

func (m *Model) outputKey(d *dialog, chord string) tea.Cmd {
	w, page := m.outputSize(d)
	switch chord {
	case "s":
		return m.openContextSkills(d)
	case "enter":
		if d.review && m.dialogVisible(d) && page > 0 {
			if d.output.top+page < len(d.output.lines) {
				m.outputMove(d, page, 0)
				return nil
			}
			return m.closeDialog(d, nil, true)
		}
	case "up", "shift+up":
		m.outputMove(d, -1, 0)
	case "down", "shift+down":
		m.outputMove(d, 1, 0)
	case "pgup":
		m.outputMove(d, -page, 0)
	case "pgdown":
		m.outputMove(d, page, 0)
	case "left":
		m.outputMove(d, 0, -max(1, w/3))
	case "right":
		m.outputMove(d, 0, max(1, w/3))
	case "home", "ctrl+home":
		d.output.top, d.output.left = 0, 0
	case "end", "ctrl+end":
		d.output.top = max(0, len(d.output.lines)-page)
	case "c", "ctrl+c":
		if d.loadedContext == nil {
			return m.copyToolOutput(d)
		}
	}
	return nil
}
