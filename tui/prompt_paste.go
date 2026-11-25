package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/tui/extension"
)

const pasteDoesNotFit = "Paste does not fit in the prompt (32 KiB limit)"

// A draft stores the compact editor text and the payloads at their actual byte
// ranges. Marker-looking text typed or pasted by the user has no special meaning.
// The paste slice is immutable so drafts can also be saved in tabs and history.
type promptDraft struct {
	text      string
	pastes    []foldedPaste
	lastPaste int
}

type foldedPaste struct {
	start, end int
	body       string
}

func pasteDraft(text string, id int) promptDraft {
	return importedDraft(text, id, false)
}

func importedDraft(text string, id int, keepTabs bool) promptDraft {
	var label string
	if lines := strings.Count(text, "\n") + 1; lines > 10 {
		label = fmt.Sprintf("[paste #%d +%d lines]", id, lines)
	} else if chars := utf8.RuneCountInString(text); chars > 1000 || (keepTabs && strings.ContainsRune(text, '\t')) {
		label = fmt.Sprintf("[paste #%d %d chars]", id, chars)
	} else {
		return promptDraft{text: strings.ReplaceAll(text, "\t", " ")}
	}
	return promptDraft{text: label, pastes: []foldedPaste{{0, len(label), text}}, lastPaste: id}
}

func (d promptDraft) inputBytes(start, end int) int {
	n := end - start
	for _, p := range d.pastes {
		if p.start >= end {
			break
		}
		if p.start >= start && p.end <= end {
			n += len(p.body) - (p.end - p.start)
		}
	}
	return n
}

func (d promptDraft) expandedBytes() int { return d.inputBytes(0, len(d.text)) }

// start and end are on paste boundaries.
func (d promptDraft) expandedRange(start, end int) string {
	if !d.overlapsPaste(start, end) {
		return d.text[start:end]
	}
	var out strings.Builder
	out.Grow(d.inputBytes(start, end))
	for _, p := range d.pastes {
		if p.start >= end {
			break
		}
		if p.start >= start {
			out.WriteString(d.text[start:p.start])
			out.WriteString(p.body)
			start = p.end
		}
	}
	out.WriteString(d.text[start:end])
	return out.String()
}

func (d promptDraft) expanded() string { return d.expandedRange(0, len(d.text)) }

func (d promptDraft) overlapsPaste(start, end int) bool {
	for _, p := range d.pastes {
		if p.start >= end {
			break
		}
		if p.end > start {
			return true
		}
	}
	return false
}

// Widen an edit or selection that starts or ends inside a marker.
func (d promptDraft) coverPastes(start, end int) (int, int) {
	for _, p := range d.pastes {
		if p.start >= end {
			break
		}
		if p.end > start {
			start, end = min(start, p.start), max(end, p.end)
		}
	}
	return start, end
}

// splice expects edits on paste boundaries, never through a marker.
func (d promptDraft) splice(start, end int, insert promptDraft) promptDraft {
	out := promptDraft{text: d.text[:start] + insert.text + d.text[end:], lastPaste: max(d.lastPaste, insert.lastPaste)}
	if out.text == "" {
		return promptDraft{}
	}
	if len(insert.pastes) == 0 && (len(d.pastes) == 0 || start >= d.pastes[len(d.pastes)-1].end) {
		out.pastes = d.pastes
		return out
	}
	out.pastes = make([]foldedPaste, 0, len(d.pastes)+len(insert.pastes))
	i := 0
	for ; i < len(d.pastes) && d.pastes[i].end <= start; i++ {
		out.pastes = append(out.pastes, d.pastes[i])
	}
	for _, p := range insert.pastes {
		out.pastes = append(out.pastes, foldedPaste{start + p.start, start + p.end, p.body})
	}
	delta := len(insert.text) - (end - start)
	for _, p := range d.pastes[i:] {
		if p.start >= end {
			out.pastes = append(out.pastes, foldedPaste{p.start + delta, p.end + delta, p.body})
		}
	}
	return out
}

func (p *promptEditor) selectionBytes() (start, end int, ok bool) {
	a, b, ok := p.area.Selection()
	if !ok {
		at := p.cursorByte()
		return at, at, false
	}
	return p.byteAt(a.Row, a.Col), p.byteAt(b.Row, b.Col), true
}

func (p *promptEditor) fits(start, end int, insert promptDraft) bool {
	return len(p.text)-(end-start)+len(insert.text) <= maxPromptBytes &&
		p.expandedBytes()-p.inputBytes(start, end)+insert.expandedBytes() <= maxPromptBytes
}

func (p *promptEditor) insertDraft(insert promptDraft) bool {
	start, end, _ := p.selectionBytes()
	a, b := p.coverPastes(start, end)
	if !p.fits(a, b, insert) {
		return false
	}
	next := p.splice(a, b, insert)
	if a != start || b != end {
		return p.setDraftAt(next, a+len(insert.text))
	}
	at := p.cursorByte()
	p.restoreViewport()
	p.area, _ = p.area.Update(tea.PasteMsg{Content: insert.text})
	if p.area.Value() != next.text {
		// The textarea also has a logical line limit. Do not keep a partial paste.
		p.setDraftAt(p.promptDraft, at)
		return false
	}
	p.memo.rows = nil
	p.adoptDraft(next)
	return true
}

func (m *Model) pasteText(raw string) tea.Cmd {
	if !m.editor.area.Focused() {
		return nil
	}
	if len(raw) <= maxPromptBytes {
		text := sanitizePrompt(raw, true)
		if text == "" {
			return nil
		}
		if m.editor.insertDraft(pasteDraft(text, m.editor.lastPaste+1)) {
			m.promptChanged()
			return nil
		}
	}
	return m.notify(m.active, pasteDoesNotFit, extension.Warning)
}

// Route ordinary insertion through the same byte budget as paste. For Bubbles'
// editing commands, use the caret/selection to locate deletions (a text diff
// alone is ambiguous next to identical literal text).
func (p *promptEditor) updateFoldedKey(k tea.KeyPressMsg) tea.Cmd {
	keys := p.area.KeyMap
	switch {
	case key.Matches(k, keys.InsertNewline):
		p.insertDraft(promptDraft{text: "\n"})
		return nil
	case k.Text != "" && k.Mod&^tea.ModShift == 0:
		p.insertDraft(promptDraft{text: cleanPrompt(k.Text)})
		return nil
	}
	at := p.cursorByte()
	start, end, selected := p.selectionBytes()
	p.restoreViewport()
	var cmd tea.Cmd
	p.area, cmd = p.area.Update(k)
	after := p.area.Value()
	if after == p.text {
		p.skipPaste(at)
		p.revision++
		return cmd
	}
	back := key.Matches(k, keys.DeleteBeforeCursor, keys.DeleteCharacterBackward, keys.DeleteWordBackward)
	forward := key.Matches(k, keys.DeleteAfterCursor, keys.DeleteCharacterForward, keys.DeleteWordForward)
	if back || forward {
		if !selected {
			removed := len(p.text) - len(after)
			if back {
				start = at - removed
			} else {
				end = at + removed
			}
		}
		if !byteBoundary(p.text, start) || !byteBoundary(p.text, end) || start > end ||
			!strings.HasPrefix(after, p.text[:start]) || after[start:] != p.text[end:] {
			p.setDraftAt(p.promptDraft, at)
			return cmd
		}
		a, b := p.coverPastes(start, end)
		next := p.splice(a, b, promptDraft{})
		if next.text != after {
			p.setDraftAt(next, a)
		} else {
			p.adoptDraft(next)
		}
	} else {
		// Case changes and transposition may edit nearby ordinary text, but
		// must not change the label or move part of a paste into plain text.
		start, end, insert := changedPromptRange(p.text, after)
		part := promptDraft{text: insert}
		if p.overlapsPaste(start, end) || !p.fits(start, end, part) {
			p.setDraftAt(p.promptDraft, at)
		} else {
			p.adoptDraft(p.splice(start, end, part))
		}
	}
	p.skipPaste(at)
	return cmd
}

func changedPromptRange(before, after string) (start, end int, insert string) {
	for start < min(len(before), len(after)) && before[start] == after[start] {
		start++
	}
	for !byteBoundary(before, start) || !byteBoundary(after, start) {
		start--
	}
	end, tail := len(before), len(after)
	for end > start && tail > start && before[end-1] == after[tail-1] {
		end, tail = end-1, tail-1
	}
	for !byteBoundary(before, end) || !byteBoundary(after, tail) {
		end, tail = end+1, tail+1
	}
	return start, end, after[start:tail]
}

// A caret or keyboard selection steps over a marker as a unit. Bubbles owns
// the selection anchor, so extend it using its ordinary selection commands.
func (p *promptEditor) skipPaste(from int) {
	at := p.cursorByte()
	for _, paste := range p.pastes {
		if at <= paste.start {
			return
		}
		if at >= paste.end {
			continue
		}
		code, target := tea.KeyRight, paste.end
		if at < from {
			code, target = tea.KeyLeft, paste.start
		}
		if !p.area.HasSelection() {
			col := p.area.Column() + target - at // marker labels are ASCII
			p.area.SetCursorColumn(col)
			if p.area.Column() == col || code == tea.KeyLeft {
				p.syncViewport()
				return
			}
			// A combining character following ']' can make the end of the
			// label fall inside a grapheme. Step past that grapheme instead.
		}
		mod := tea.KeyMod(0)
		if p.area.HasSelection() {
			mod = tea.ModShift
		}
		for p.cursorByte() > paste.start && p.cursorByte() < paste.end {
			row, col := p.area.Line(), p.area.Column()
			p.area, _ = p.area.Update(tea.KeyPressMsg{Code: code, Mod: mod})
			if p.area.Line() == row && p.area.Column() == col {
				break
			}
		}
		return
	}
}
