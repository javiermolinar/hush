package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	"github.com/javiermolinar/hush/tui/extension"
)

const maxPromptBytes = 32 << 10
const maxPromptHistory = 100

var shellKeys = struct {
	send, followUp, newline, older, newer, up, down, pageUp, pageDown, top, follow, leader, escape, clear, quit, complete key.Binding
}{
	send: binding("enter", "send"), followUp: binding("alt+enter", "follow-up"),
	newline: key.NewBinding(key.WithKeys("shift+enter", "ctrl+j"), key.WithHelp("shift+enter", "newline")),
	older:   binding("up", "history"), newer: binding("down", "history"),
	up: binding("shift+up", "scroll up"), down: binding("shift+down", "scroll down"),
	pageUp: binding("pgup", "page up"), pageDown: binding("pgdown", "page down"),
	top: binding("ctrl+home", "top"), follow: binding("ctrl+end", "to the end"),
	leader: binding("ctrl+x", "leader"), escape: binding("esc", "stop"), clear: binding("ctrl+c", "clear"),
	quit: binding("ctrl+d", "quit"), complete: binding("tab", "complete"),
}

func binding(chord, label string) key.Binding {
	return key.NewBinding(key.WithKeys(chord), key.WithHelp(chord, label))
}
func keyLabel(k key.Binding) string { h := k.Help(); return h.Key + " " + h.Desc }

type promptEditor struct {
	area         textarea.Model
	text         string
	revision     uint64
	historyIndex int
	scratch      string
	memo         *promptLayout
}

type promptLayoutKey struct {
	width, height, limit int
	focused              bool
}

type promptLayout struct {
	key    promptLayoutKey
	rows   []string
	cursor *tea.Cursor
}

func newPrompt() promptEditor {
	t := textarea.New()
	t.Prompt = ""
	t.ShowLineNumbers = false
	t.CharLimit = maxPromptBytes
	t.MaxHeight = 0 // cap content by bytes, not the editor's viewport height
	t.SetStyles(textarea.Styles{})
	t.SetWidth(68)
	t.SetHeight(6)
	return promptEditor{area: t, historyIndex: -1, memo: &promptLayout{}}
}

func cleanPrompt(value string) string {
	value = limitText(value, maxPromptBytes)
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, value)
}

func (p *promptEditor) value() string { return p.text }
func (p *promptEditor) set(text string) {
	p.text = cleanPrompt(text)
	p.area.SetValue(p.text)
	p.syncViewport()
	p.revision++
}
func (p *promptEditor) cursorByte() int {
	row, col := p.area.Line(), p.area.LineInfo()
	base := 0
	for line := range strings.SplitSeq(p.text, "\n") {
		if row == 0 {
			// In the pinned Bubbles version, ColumnOffset and StartColumn
			// index runes; CharOffset is the terminal cell width.
			n := col.StartColumn + col.ColumnOffset
			i := 0
			for i < len(line) && n > 0 {
				_, size := utf8.DecodeRuneInString(line[i:])
				i += size
				n--
			}
			return base + i
		}
		base += len(line) + 1
		row--
	}
	return len(p.text)
}

func (p *promptEditor) replace(start, end int, value string) {
	if !byteBoundary(p.text, start) || !byteBoundary(p.text, end) || start > end || len(p.text)-end+start+len(value) > maxPromptBytes {
		return
	}
	prefix := p.text[:start] + value
	p.set(prefix + p.text[end:])
	row := strings.Count(prefix, "\n")
	for n := 0; p.area.Line() > row && n < maxPromptBytes; n++ {
		p.area.CursorUp()
	}
	column := prefix[strings.LastIndexByte(prefix, '\n')+1:]
	p.area.SetCursorColumn(utf8.RuneCountInString(column))
	p.syncViewport()
}

func (p *promptEditor) resetHistory() { p.historyIndex = -1; p.scratch = "" }
func (p *promptEditor) update(msg tea.Msg, multiline bool) tea.Cmd {
	p.memo.rows = nil
	if paste, ok := msg.(tea.PasteMsg); ok {
		text := cleanPrompt(paste.Content)
		if !multiline {
			text = strings.ReplaceAll(text, "\n", " ")
		}
		p.area, _ = p.area.Update(tea.PasteMsg{Content: limitText(text, maxPromptBytes-len(p.text))})
		p.text = p.area.Value()
		p.revision++
		return nil
	}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		if k.String() == "ctrl+v" { // terminal bracketed paste is handled above
			return nil
		}
		if p.area.Focused() && k.Mod&^tea.ModShift == 0 && k.Code >= ' ' && k.Code <= '~' &&
			len(k.Text) == 1 && p.plainWidth(k.Text) > 0 {
			// A plain insertion that still fits needs no viewport scroll.
			// Update would lay out the entire textarea before View runs.
			p.area.InsertString(k.Text)
			p.text = p.area.Value()
			p.revision++
			return nil
		}
	}
	var cmd tea.Cmd
	p.area, cmd = p.area.Update(msg)
	if _, ok := msg.(tea.KeyPressMsg); ok {
		text := p.area.Value()
		clean := cleanPrompt(text)
		if !multiline {
			clean = strings.ReplaceAll(clean, "\n", " ")
		}
		if clean != text {
			p.set(clean)
		} else {
			p.text = text
			p.revision++
		}
	}
	return cmd
}

func (p *promptEditor) newline() {
	if len(p.text) >= maxPromptBytes {
		return
	}
	p.memo.rows = nil
	p.area, _ = p.area.Update(tea.PasteMsg{Content: "\n"})
	p.text = p.area.Value()
	p.revision++
}

// SetValue and SetHeight reposition using Bubble Tea's previous viewport
// content. Refresh it on the loop, after programmatic changes.
func (p *promptEditor) syncViewport() {
	p.memo.rows = nil
	p.area, _ = p.area.Update(nil)
}

// The main prompt uses a real terminal cursor. Unrelated events need not
// ask Bubbles to rewrap and repaint its viewport, including unused rows.
func (p *promptEditor) layout(limit int) ([]string, *tea.Cursor) {
	key := promptLayoutKey{p.area.Width(), p.area.Height(), limit, p.area.Focused()}
	if p.memo.rows == nil || p.memo.key != key {
		var rows []string
		var caret *tea.Cursor
		if width := p.plainWidth(""); width > 0 {
			rows = make([]string, max(1, min(limit, (len(p.text)+width-1)/width)))
			for i := range rows {
				part := p.text[i*width : min(len(p.text), (i+1)*width)]
				rows[i] = part + strings.Repeat(" ", width-len(part))
			}
			if key.focused {
				col := p.area.Column()
				caret = tea.NewCursor(col%width, col/width)
				style := p.area.Styles().Cursor
				caret.Color, caret.Shape, caret.Blink = style.Color, style.Shape, style.Blink
			}
		} else {
			rows, caret = p.rows(key.width, limit), p.area.Cursor()
		}
		*p.memo = promptLayout{key, rows, caret}
	}
	return p.memo.rows, p.memo.cursor
}

// Printable ASCII in an unstyled single logical line needs no word layout if
// it fits on one row or has no break opportunities. Leave room for the caret
// and defer selection, scrolling, word wraps and Unicode to the textarea.
func (p *promptEditor) plainWidth(extra string) int {
	width, size := p.area.Width(), len(p.text)+len(extra)
	if size > maxPromptBytes || width <= 0 || size/width >= p.area.Height() || p.area.ScrollYOffset() != 0 ||
		p.area.VirtualCursor() || p.area.HasSelection() || p.area.Placeholder != "" {
		return 0
	}
	for _, s := range [...]string{p.text, extra} {
		for i := range len(s) {
			c := s[i]
			if c < ' ' || c > '~' || size >= width && (c == ' ' || c == '-') {
				return 0
			}
		}
	}
	return width
}

func (p *promptEditor) rows(width, maxRows int) []string {
	// The actual textarea viewport is never taller than the editor budget.
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

func (m *Model) resizeEditors() tea.Cmd {
	m.editor.area.SetWidth(max(1, m.width-12))
	m.editor.area.SetHeight(max(1, m.height/4))
	m.editor.syncViewport()
	if m.sheet != nil {
		m.resizeDialogEditor(m.sheet)
	}
	for _, d := range m.overlays {
		m.resizeDialogEditor(d)
		if d.review {
			d.output.reflowReview(m.dialogWidth(d) - 4)
		}
	}
	return nil
}

func (m *Model) updatePrompt(msg tea.Msg) tea.Cmd {
	old := m.editor.value()
	cmd := m.editor.update(msg, true)
	if old != m.editor.value() {
		m.slashHidden = false
		m.slashIndex = 0
		m.editor.resetHistory()
	}
	return cmd
}

func (m *Model) recallHistory(older bool) {
	h := m.threads[m.active].history
	p := &m.editor
	if len(h) == 0 {
		return
	}
	if p.historyIndex < 0 {
		if !older {
			return
		}
		p.scratch = p.text
		p.historyIndex = len(h)
	}
	if older {
		p.historyIndex = max(0, p.historyIndex-1)
	} else {
		p.historyIndex++
	}
	if p.historyIndex >= len(h) {
		p.set(p.scratch)
		p.resetHistory()
	} else {
		p.set(h[p.historyIndex])
	}
	m.slashHidden = false
}

func (m *Model) promptKey(k tea.KeyPressMsg) tea.Cmd {
	chord := k.String()
	if !key.Matches(k, shellKeys.escape) {
		m.escapeAt, m.stopAt = time.Time{}, time.Time{}
	}
	if m.leader != "" {
		seq := strings.TrimSpace(m.leader + " " + normalizeChord(chord))
		m.leader = ""
		if i, ok := m.host.leader[seq]; ok {
			return m.runAction(i, "")
		}
		if chord != "esc" && chord != "ctrl+x" {
			for s, i := range m.host.leader {
				if strings.HasPrefix(s, seq+" ") && m.eligible(m.host.actions[i], m.active) {
					m.leader = seq
					break
				}
			}
		}
		return nil
	}
	if key.Matches(k, shellKeys.leader) {
		m.leader = " "
		return nil
	}
	if i, ok := m.host.direct[normalizeChord(chord)]; ok && (chord != "ctrl+[" || m.disambiguated) && (chord != "ctrl+f" || m.editor.value() == "") && m.eligible(m.host.actions[i], m.active) {
		return m.runAction(i, "")
	}
	t := m.threads[m.active]
	if chord == "esc" && !m.stopAt.IsZero() {
		if time.Since(m.stopAt) <= 2*time.Second && threadBusy(t) {
			return m.stopKey() // an advertised confirmation takes priority over suggestions
		}
		m.stopAt = time.Time{}
	}
	if _, files, open := m.fileSuggestions(); open {
		switch chord {
		case "esc":
			m.files.hidden = true
			return nil
		case "up":
			m.files.selected = max(0, m.files.selected-1)
			return nil
		case "down":
			m.files.selected = min(max(0, len(files)-1), m.files.selected+1)
			return nil
		case "tab", "enter":
			if len(files) > 0 {
				return m.replaceFile("")
			}
			if chord == "tab" || m.files.waiting {
				return nil
			}
		}
	}
	if list := m.slashMatches(); len(list) > 0 {
		switch chord {
		case "up":
			m.slashIndex = max(0, m.slashIndex-1)
			return nil
		case "down":
			m.slashIndex = min(len(list)-1, m.slashIndex+1)
			return nil
		}
	}
	switch {
	case key.Matches(k, shellKeys.send):
		text := m.editor.value()
		if strings.HasPrefix(text, "/skill:") && m.menuFeatures().Has(extension.Skills) {
			return m.sendPrompt(sdk.Steer)
		}
		if strings.HasPrefix(text, "/") && !strings.Contains(text, "\n") {
			if list := m.slashMatches(); len(list) > 0 {
				return m.runSlash(list[min(m.slashIndex, len(list)-1)])
			}
			name, args, _ := slashParts(text)
			if i, ok := m.host.slash[name]; ok {
				if !m.eligible(m.host.actions[i], m.active) {
					return m.notify(m.active, "Command unavailable", extension.Warning)
				}
				return m.runSlashWithArgs(m.host.actions[i], args)
			}
			return m.notify(m.active, "Unknown slash command", extension.Warning)
		}
		if m.editor.value() == "" && len(t.attachments) == 0 && !t.attaching && m.currentFrame() && m.frame.openActivity != "" {
			b := maxEffects
			return m.invoke(m.route(m.active, coreID), extension.ActionRef{ID: expandRowID, Target: m.frame.openActivity}, &b)
		}
		return m.sendPrompt(sdk.Steer)
	case key.Matches(k, shellKeys.followUp):
		return m.sendPrompt(sdk.FollowUp)
	case key.Matches(k, shellKeys.newline):
		m.editor.newline()
		m.editor.resetHistory()
		m.slashHidden = false
	case key.Matches(k, shellKeys.older):
		m.recallHistory(true)
	case key.Matches(k, shellKeys.newer):
		m.recallHistory(false)
	case key.Matches(k, shellKeys.up):
		return m.scroll(-1)
	case key.Matches(k, shellKeys.down):
		return m.scroll(1)
	case key.Matches(k, shellKeys.pageUp):
		return m.scroll(-max(1, m.frame.transcriptHeight-1))
	case key.Matches(k, shellKeys.pageDown):
		return m.scroll(max(1, m.frame.transcriptHeight-1))
	case key.Matches(k, shellKeys.top):
		if t.intro != nil || len(t.ids) > 0 || t.stream.Present {
			t.following = false
			anchor := t.idAt(t.firstBlock())
			if anchor == "" && len(t.ids) > 0 {
				anchor = t.ids[0]
			}
			t.anchor = rowAnchor{anchor, 0}
			return m.maybeOlder(true)
		}
	case key.Matches(k, shellKeys.follow):
		t.following = true
	case key.Matches(k, shellKeys.escape):
		if len(m.slashMatches()) > 0 {
			m.slashHidden = true
			return nil
		}
		if !threadBusy(t) && m.canHintFork() && !t.sending && !t.aborting {
			now := time.Now()
			if !m.escapeAt.IsZero() && now.Sub(m.escapeAt) <= 450*time.Millisecond {
				m.escapeAt = time.Time{}
				return m.runAction(m.host.byID[forkPickerID], "")
			}
			m.escapeAt = now
			return nil
		}
		return m.stopKey()
	case key.Matches(k, shellKeys.clear):
		if t.attaching {
			return m.notify(m.active, "Wait for the pending attachment", extension.Warning)
		}
		if m.editor.value() != "" || len(t.attachments) != 0 {
			t.attachments = nil
			m.editor.set("")
			m.editor.resetHistory()
		} else {
			m.quitting = true
			return tea.Quit
		}
	case key.Matches(k, shellKeys.quit) && m.editor.value() == "" && len(t.attachments) == 0 && !t.attaching:
		m.quitting = true
		return tea.Quit
	case key.Matches(k, shellKeys.complete):
		m.completeSlash()
	case chord == "?" && m.editor.value() == "":
		if i, ok := m.host.byID[paletteID]; ok {
			return m.runAction(i, "")
		}
	default:
		return m.updatePrompt(k)
	}
	return nil
}

func (m *Model) runAction(i int, args string) tea.Cmd {
	budget := maxEffects
	return m.invoke(m.route(m.active, ""), extension.ActionRef{ID: m.host.actions[i].action.ID, Args: args}, &budget)
}

func slashParts(text string) (name, args string, start int) {
	at := strings.IndexAny(text, " \t")
	if at < 0 {
		return strings.TrimPrefix(text, "/"), "", len(text)
	}
	start = at
	for start < len(text) && (text[start] == ' ' || text[start] == '\t') {
		start++
	}
	return text[1:at], text[start:], start
}

func (m *Model) slashMatches() []registeredAction {
	text := m.editor.value()
	if m.slashHidden || !strings.HasPrefix(text, "/") || strings.ContainsAny(text, " \t\n") {
		return nil
	}
	var out []registeredAction
	for _, i := range m.host.slashOrder {
		a := m.host.actions[i]
		if strings.HasPrefix(a.action.Slash.Name, text[1:]) && m.eligible(a, m.active) {
			out = append(out, a)
		}
	}
	return out
}

func (m *Model) runSlash(a registeredAction) tea.Cmd {
	return m.runSlashWithArgs(a, "")
}

func (m *Model) runSlashWithArgs(a registeredAction, args string) tea.Cmd {
	m.editor.set("")
	m.slashIndex = 0
	return m.runAction(m.host.byID[a.action.ID], args)
}

func (m *Model) slashCompleter() func(extension.Context, extension.CompletionRequest) []extension.Completion {
	text := m.editor.value()
	if !strings.HasPrefix(text, "/") || strings.Contains(text, "\n") {
		return nil
	}
	name, _, start := slashParts(text)
	if start == len(text) && !strings.HasSuffix(text, " ") || m.editor.cursorByte() < start {
		return nil
	}
	i, ok := m.host.slash[name]
	if !ok || !m.eligible(m.host.actions[i], m.active) {
		return nil
	}
	return m.host.actions[i].action.Slash.Complete
}

// Convert only declared shell bindings into the key events shared with the editor.
func paletteShellKey(chord string) tea.KeyPressMsg {
	var k tea.KeyPressMsg
	for _, mod := range []struct {
		name string
		mask tea.KeyMod
	}{{"ctrl+", tea.ModCtrl}, {"alt+", tea.ModAlt}, {"shift+", tea.ModShift}} {
		if rest, ok := strings.CutPrefix(chord, mod.name); ok {
			chord, k.Mod = rest, k.Mod|mod.mask
		}
	}
	switch chord {
	case "enter":
		k.Code = tea.KeyEnter
	case "tab":
		k.Code = tea.KeyTab
	case "up":
		k.Code = tea.KeyUp
	case "down":
		k.Code = tea.KeyDown
	case "pgup":
		k.Code = tea.KeyPgUp
	case "pgdown":
		k.Code = tea.KeyPgDown
	case "home":
		k.Code = tea.KeyHome
	case "end":
		k.Code = tea.KeyEnd
	default:
		k.Code, _ = utf8.DecodeRuneInString(chord)
	}
	return k
}

func byteBoundary(s string, i int) bool {
	return i >= 0 && i <= len(s) && (i == len(s) || utf8.RuneStart(s[i]))
}

func (m *Model) completeSlash() {
	text := m.editor.value()
	if !strings.HasPrefix(text, "/") || strings.Contains(text, "\n") {
		return
	}
	if list := m.slashMatches(); len(list) > 0 {
		m.editor.set("/" + list[min(m.slashIndex, len(list)-1)].action.Slash.Name + " ")
		m.slashIndex = 0
		return
	}
	_, args, start := slashParts(text)
	complete := m.slashCompleter()
	if complete == nil {
		return
	}
	cursor := m.editor.cursorByte()
	for j, item := range complete(m.contextFor(m.active), extension.CompletionRequest{Args: args, Cursor: cursor - start, Limit: 20}) {
		if j >= 20 {
			break
		}
		e := item.Replace
		if e.Start <= e.End && byteBoundary(args, e.Start) && byteBoundary(args, e.End) && utf8.ValidString(e.Text) && !strings.ContainsAny(e.Text, "\r\n") && len(text)-(e.End-e.Start)+len(e.Text) <= maxPromptBytes {
			m.editor.replace(start+e.Start, start+e.End, e.Text)
			return
		}
	}
}

type sendResult struct {
	id      sdk.ID
	key     string
	receipt sdk.Receipt
}

type localInput struct {
	id     sdk.ID
	text   string
	images []draftImage
}

func (t *threadState) rememberInput(input localInput) {
	if len(t.stopInputs) == sdk.StopCandidateLimit {
		copy(t.stopInputs, t.stopInputs[1:])
		t.stopInputs = t.stopInputs[:len(t.stopInputs)-1]
		t.stopEvicted = true
	}
	t.stopInputs = append(t.stopInputs, input)
}

type admissionExpired struct {
	owner, submission sdk.ID
	life              uint64
}

func (t *threadState) hasSubmission(id sdk.ID) bool {
	if slices.Contains(t.runInputs, id) {
		return true
	}
	return slices.ContainsFunc(t.queue, func(item sdk.QueueItem) bool { return item.SubmissionID == id })
}

func (m *Model) sendPrompt(mode sdk.BusyMode) tea.Cmd {
	text := m.editor.value()
	r := m.route(m.active, coreID)
	t := m.threads[r.owner]
	if t.attaching {
		return m.notifyRoute(r, "Wait for the pending attachment", extension.Warning)
	}
	if strings.TrimSpace(text) == "" && len(t.attachments) == 0 {
		return nil
	}
	if t.sending || t.aborting {
		return m.notifyRoute(r, "Wait for the pending request", extension.Warning)
	}
	if mode == sdk.Steer && !threadBusy(t) {
		mode = sdk.RejectBusy
	}
	backend, options, environment := m.backend, m.options.NewThread, m.options.ClientEnvironment
	model, thinking := t.info.Model, t.info.Thinking
	before := t.info.LastEntryID
	m.freezeIntro(t)
	t.sending = true
	var pendingInput sdk.QueueItem
	if mode == sdk.Steer || mode == sdk.FollowUp {
		pendingInput = sdk.QueueItem{Mode: string(mode), Text: text}
		t.sendingInput = pendingInput
	}
	sent := t.attachments
	t.attachments = nil
	var images []sdk.Image
	for _, item := range sent {
		images = append(images, item.image)
	}
	m.editor.set("")
	m.editor.resetHistory()
	m.slashHidden = false
	budget := maxEffects
	return m.startWork(r, func(ctx context.Context) (any, error) {
		out := sendResult{id: r.owner}
		if out.id == "" {
			var created sdk.Thread
			var err error
			if model != (sdk.Model{}) {
				catalog, ok := backend.(modelCatalogDraftBackend)
				if !ok {
					return out, extension.ErrUnavailable
				}
				created, err = catalog.NewCatalogThread(ctx, options.CWD, model, thinking)
			} else {
				created, err = backend.NewThread(ctx, options)
			}
			if err != nil {
				return out, err
			}
			if !created.ID.Valid() {
				return out, &sdk.TransportError{Err: sdk.ErrProtocol, Indeterminate: true}
			}
			out.id = created.ID
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var err error
		out.key = submissionKey()
		out.receipt, err = backend.Submit(ctx, sdk.Input{
			ThreadID: out.id, RequestID: out.key, Text: text, WhenBusy: mode, Images: images, Environment: environment,
		})
		if err == nil && !out.receipt.SubmissionID.Valid() {
			err = &sdk.TransportError{Err: sdk.ErrProtocol, Indeterminate: true}
		}
		return out, err
	}, func(v any, err error, _ *int) tea.Cmd {
		t.sending = false
		t.sendingInput = sdk.QueueItem{}
		out, _ := v.(sendResult)
		target := r.owner
		var attach tea.Cmd
		show := m.foreground(r)
		if r.owner == "" && out.id.Valid() {
			target = out.id
			dst := m.ensureThread(target)
			if !dst.observed && dst.footer.workspace == "" {
				dst.footer = t.footer // preview until this thread's own report arrives
				if model != (sdk.Model{}) {
					if dst.footer.model != model {
						dst.footer.context = extension.ContextUse{}
					}
					dst.footer.model, dst.footer.thinking = model, thinking
				}
			}
			if model != (sdk.Model{}) {
				t.info.Model, t.info.Thinking = sdk.Model{}, ""
				t.revision++
			}
			if t.intro != nil {
				dst.intro, t.intro = t.intro, nil
				t.following, t.anchor = true, rowAnchor{}
			}
			if show {
				if dst.draft == "" {
					dst.draft = m.editor.value()
					m.editor.set("")
				}
				attach = m.switchThread(target)
			}
		}
		dst := m.threads[target]
		if err == nil && mode != sdk.RejectBusy {
			dst.rememberInput(localInput{out.receipt.SubmissionID, text, sent})
		}
		if text != "" && err == nil {
			if len(dst.history) == maxPromptHistory {
				copy(dst.history, dst.history[1:])
				dst.history = dst.history[:maxPromptHistory-1]
			}
			dst.history = append(dst.history, text)
		}
		if err != nil {
			note := err.Error()
			uncertain := isIndeterminate(err)
			if !uncertain || out.key == "" {
				m.restoreText(target, text)
				dst.addImages(sent)
				if uncertain {
					note = "Thread creation unconfirmed; prompt not sent. Check /threads before retrying."
				}
			} else {
				note = "Input admission unconfirmed; not restored or replayed (request " + out.key + ")"
				dst.status[coreID+":admission"] = extension.SetStatus{Text: note, Tone: extension.Warning}
			}
			if show {
				return tea.Batch(attach, m.notify(target, note, extension.Danger))
			}
		}
		// The reply can beat the watch. Bridge the idle-to-run transition or
		// show that a steer/follow-up was admitted, not necessarily queued.
		if id := out.receipt.SubmissionID; err == nil && id.Valid() && !dst.hasSubmission(id) {
			starting := dst.info.LastEntryID == before && !threadBusy(dst)
			if starting {
				dst.awaiting = id
			}
			var probe tea.Cmd
			if pendingInput.Mode != "" {
				pendingInput.SubmissionID = id
				dst.rememberAdmission(pendingInput)
				probe = m.probeQueuePlacement(target, id, dst.life)
			}
			if starting || pendingInput.Mode != "" {
				msg := admissionExpired{target, id, dst.life}
				return tea.Batch(attach, probe, tea.Tick(5*time.Second, func(time.Time) tea.Msg { return msg }))
			}
		}
		return attach
	}, &budget)
}

func (m *Model) restoreText(id sdk.ID, text string) {
	if text == "" {
		return
	}
	t := m.threads[id]
	current := t.draft
	if id == m.active {
		current = m.editor.value()
	}
	if current != "" {
		text = limitText(text, max(0, maxPromptBytes-len(current)-1))
		if text == "" {
			return
		}
		text += "\n" + current
	}
	if id == m.active {
		m.editor.set(text)
		m.editor.resetHistory()
		m.slashHidden = false
	} else {
		t.draft = cleanPrompt(text)
	}
}

type stopBackend interface {
	Stop(context.Context, sdk.ID, []sdk.ID) (sdk.StopResult, error)
}

type stopResult struct {
	sdk.StopResult
	legacy bool
}

func (m *Model) stopKey() tea.Cmd {
	t := m.threads[m.active]
	if !threadBusy(t) || t.aborting || t.sending {
		m.stopAt = time.Time{}
		return nil
	}
	now := time.Now()
	if m.stopAt.IsZero() || now.Sub(m.stopAt) > 2*time.Second {
		m.stopAt = now
		return m.notify(m.active, "Esc again to stop", extension.Warning)
	}
	m.stopAt = time.Time{}
	if len(m.pending) >= maxPending {
		return m.notify(m.active, "Too many pending requests; stop not sent", extension.Warning)
	}
	r := m.route(m.active, coreID)
	t.aborting = true
	inputs, evicted, backend := t.stopInputs, t.stopEvicted, m.backend
	ids := make([]sdk.ID, 0, len(inputs))
	for _, input := range inputs {
		ids = append(ids, input.id)
	}
	budget := maxEffects
	return m.startWork(r, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if native, ok := backend.(stopBackend); ok {
			result, err := native.Stop(call, r.owner, ids)
			var unsupported *sdk.ServerError
			if !errors.As(err, &unsupported) || unsupported.Code != "unsupported_capability" || unsupported.Capability != "nativeStop" {
				return stopResult{StopResult: result}, err
			}
		}
		err := backend.Abort(call, r.owner)
		return stopResult{StopResult: sdk.StopResult{Stopped: err == nil}, legacy: true}, err
	}, func(v any, err error, _ *int) tea.Cmd {
		t.aborting = false
		t.stopInputs, t.stopEvicted = nil, false
		out, _ := v.(stopResult)
		return m.finishStop(r, inputs, evicted, out, err)
	}, &budget)
}

func (m *Model) finishStop(r ownerRoute, inputs []localInput, evicted bool, out stopResult, err error) tea.Cmd {
	t := m.threads[r.owner]
	current := t.draft
	if r.owner == m.active {
		current = m.editor.value()
	}
	left := maxPromptBytes - len(current)
	var texts, unknown []string
	restored, tooLarge := 0, 0
	for i, input := range inputs {
		if err != nil || i >= len(out.Candidates) || out.Candidates[i].SubmissionID != input.id {
			unknown = append(unknown, "#"+string(input.id))
			continue
		}
		switch out.Candidates[i].Outcome {
		case sdk.NotWithdrawn:
			continue
		case sdk.Withdrawn:
		default:
			unknown = append(unknown, "#"+string(input.id))
			continue
		}
		cost := len(input.text)
		if cost != 0 && (current != "" || len(texts) != 0) {
			cost++
		}
		if cost > left || !t.restoreInputImages(input.images) {
			tooLarge++
			continue
		}
		if input.text != "" {
			texts = append(texts, input.text)
			left -= cost
		}
		restored++
	}
	m.restoreText(r.owner, strings.Join(texts, "\n"))
	message, tone := "Stopped", extension.Good
	if err != nil || !out.Stopped {
		message, tone = "Stop not confirmed", extension.Warning
	}
	if err != nil && !isIndeterminate(err) {
		message += ": " + err.Error()
	}
	if restored != 0 {
		message += fmt.Sprintf("; restored %d withdrawn input(s)", restored)
	}
	if out.legacy {
		message += "; host cannot verify withdrawals, none restored"
		if len(unknown) != 0 {
			message += ": " + strings.Join(unknown, ", ")
		}
		tone = extension.Warning
	} else if len(unknown) != 0 {
		message += "; unverified inputs not restored: " + strings.Join(unknown, ", ")
		tone = extension.Warning
	}
	if evicted {
		message += "; older receipts not checked"
		tone = extension.Warning
	}
	if tooLarge != 0 {
		message += fmt.Sprintf("; %d withdrawn input(s) did not fit in the draft", tooLarge)
		tone = extension.Warning
	}
	delete(t.status, coreID+":stop")
	if tone == extension.Warning {
		t.status[coreID+":stop"] = extension.SetStatus{Text: message, Tone: tone}
	}
	return m.notifyRoute(r, message, tone)
}

func (t *threadState) restoreInputImages(images []draftImage) bool {
	if len(images) == 0 {
		return true
	}
	before := slices.Clone(t.attachments)
	if t.addImages(images) {
		return true
	}
	t.attachments = before
	return false
}
