package tui

import (
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type promptLine struct {
	start int // byte offset in the draft
	width int
	count int // row budget used by rows(), zero if not measured
	rows  []string
	depth int // maximum rows requested when wrapping this line
	x     int // caret cell at the end of the line
	ascii promptASCIIWrap
}

func (l *promptLine) atWidth(width int) *promptLine {
	if l.width != width {
		*l = promptLine{start: l.start, width: width}
	}
	return l
}

// promptASCIIWrap tracks committed row breaks separately from the unfinished
// word and the trailing caret cell. Appending a key only scans the new bytes;
// neither the earlier words nor the earlier logical lines are rewrapped.
type promptASCIIWrap struct {
	starts        []int
	scanned, word int
	unicode       bool
}

func (a *promptASCIIWrap) advance(text string, width int) bool {
	if a.unicode {
		return false
	}
	if a.starts == nil {
		a.starts = append(a.starts, 0)
	}
	for i := a.scanned; i < len(text); i++ {
		c := text[i]
		if c < ' ' || c > '~' {
			a.unicode = true
			return false
		}
		start := a.starts[len(a.starts)-1]
		if c == ' ' {
			if i+1-start > width {
				a.starts = append(a.starts, a.word)
			}
			a.word = i + 1
		} else if i+1-a.word == width {
			// A full-width word (or a piece of a longer word) stands
			// alone. Subsequent bytes start a fresh pending word.
			if a.word > start {
				a.starts = append(a.starts, a.word)
			}
			a.word = i + 1
		}
	}
	a.scanned = len(text)
	return true
}

func (a *promptASCIIWrap) tail(text string, width, depth int, rows []string) ([]string, int) {
	end := len(text)
	last := a.starts[len(a.starts)-1]
	if end-last >= width {
		// The unfinished word can move to a new row as it grows. At
		// exactly the width, the caret occupies a row of its own.
		cut := end
		if end-last > width {
			cut = a.word
		}
		rows = append(rows, text[cut:end])
		end = cut
	}
	x := len(text) - last
	if len(rows) != 0 {
		x = len(rows[0])
	}
	for i := len(a.starts) - 1; i >= 0 && len(rows) < depth; i-- {
		start := a.starts[i]
		rows = append(rows, text[start:end])
		end = start
	}
	return rows, x
}

type promptLayoutKey struct {
	width, height, limit int
	line, column         int
	focused, selected    bool
}

type promptLayout struct {
	key    promptLayoutKey
	rows   []string
	cursor *tea.Cursor
	wrap   *textarea.Model // Bubbles-compatible layout for one Unicode line
}

func (p *promptEditor) layout(limit int) ([]string, *tea.Cursor) {
	key := promptLayoutKey{
		width: p.area.Width(), height: p.area.Height(), limit: limit,
		line: p.area.Line(), column: p.area.Column(), focused: p.area.Focused(), selected: p.area.HasSelection(),
	}
	memo := p.memo
	if memo.rows == nil || memo.key != key {
		memo.key = key
		if p.tailDirty && p.atEnd() && !key.selected && !p.area.VirtualCursor() && p.area.Placeholder == "" {
			memo.rows, memo.cursor = p.layoutTail(key)
		} else {
			memo.rows, memo.cursor = p.rows(key.width, limit), p.area.Cursor()
		}
	}
	return memo.rows, memo.cursor
}

func (p *promptEditor) wrapLine(row, width, depth int) *promptLine {
	line := p.lines[row].atWidth(width)
	if line.depth >= depth {
		return line
	}
	text := p.lineText(row)
	if line.ascii.advance(text, width) {
		line.rows, line.x = line.ascii.tail(text, width, depth, line.rows[:0])
	} else {
		// Delegate grapheme boundaries and widths to the same textarea
		// used for cursor movement, but only for this visible logical line.
		if p.memo.wrap == nil {
			wrap := newPromptArea()
			wrap.SetVirtualCursor(false)
			wrap.Focus()
			p.memo.wrap = &wrap
		}
		wrap := p.memo.wrap
		if wrap.Width() != width {
			wrap.SetWidth(width)
		}
		if wrap.Height() != depth {
			wrap.SetHeight(depth)
		}
		wrap.SetValue(text)
		info := wrap.LineInfo()
		*wrap, _ = wrap.Update(nil)
		rows := strings.SplitN(wrap.View(), "\n", min(depth, info.Height)+1)
		line.rows = line.rows[:0]
		for i := min(depth, info.Height) - 1; i >= 0; i-- {
			line.rows = append(line.rows, rows[i])
		}
		line.x = info.CharOffset
	}
	line.depth = depth
	return line
}

func (p *promptEditor) layoutTail(key promptLayoutKey) ([]string, *tea.Cursor) {
	width, height := max(1, key.width), max(1, key.height)
	// At EOF the viewport ends on the caret row. Walk backwards only
	// until that viewport is filled and only pad the rows we will show.
	rows := make([]string, height)
	used, x := 0, 0
	for i := len(p.lines) - 1; i >= 0 && used < height; i-- {
		left := height - used
		line := p.wrapLine(i, width, left)
		if i == len(p.lines)-1 {
			x = line.x
		}
		for _, text := range line.rows[:min(left, len(line.rows))] {
			used++
			rows[height-used] = fit(text, width)
		}
	}
	copy(rows, rows[height-used:])
	want := p.rowBudget(width, min(key.limit, height))
	for i := used; i < want; i++ {
		rows[i] = strings.Repeat(" ", width)
	}
	var caret *tea.Cursor
	if key.focused {
		caret = tea.NewCursor(x, used-1)
		style := p.area.Styles().Cursor
		caret.Color, caret.Shape, caret.Blink = style.Color, style.Shape, style.Blink
	}
	return rows[:want], caret
}

func (p *promptEditor) rowBudget(width, limit int) int {
	// Match the existing visual budget, which excludes a trailing caret-only
	// row when the text fills the width exactly. Usually only the first few
	// logical lines are needed, and they don't change during tail typing.
	width, limit = max(1, width), max(1, limit)
	n := 0
	for i := range p.lines {
		line := p.lines[i].atWidth(width)
		if line.count == 0 {
			text := p.lineText(i)
			if !line.ascii.unicode && line.ascii.scanned == len(text) {
				// For ASCII, later bytes cannot undo an x/ansi wrap
				// already emitted in a prefix. Stop as soon as we can
				// prove the entire viewport is occupied.
				if end := width * (limit - n + 1); end < len(text) &&
					strings.Count(ansi.Wrap(text[:end], width, ""), "\n") >= limit-n-1 {
					return limit
				}
			}
			line.count = 1 + strings.Count(ansi.Wrap(text, width, ""), "\n")
		}
		n += line.count
		if n >= limit {
			return limit
		}
	}
	return max(1, n)
}

func (p *promptEditor) rows(width, maxRows int) []string {
	width = max(1, width)
	n := 0
	for s := range strings.SplitSeq(p.text, "\n") {
		n += 1 + strings.Count(ansi.Wrap(s, width, ""), "\n")
		if n >= maxRows {
			break
		}
	}
	n = max(1, min(maxRows, n))
	rows := strings.SplitN(p.area.View(), "\n", n+1)
	return rows[:min(n, len(rows))]
}
