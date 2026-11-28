package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

const (
	threadTurnDelay    = 120 * time.Millisecond
	threadTurnPageSize = 16
	threadTurnMaxPages = 4
)

type threadTurn struct{ prompt, reply string }

type threadTurnPreview struct {
	thread, answer sdk.ID
	token          uint64
	cancel         context.CancelFunc
	failed         bool
	threadTurn
}

func (p *threadTurnPreview) reset() {
	if p.cancel != nil {
		p.cancel()
	}
	*p = threadTurnPreview{}
}

type threadTurnResult struct {
	token uint64
	turn  threadTurn
	err   error
}

func (m *Model) syncThreadTurn() tea.Cmd {
	d := m.sheet
	if d == nil || d.threadPicker == nil || !m.foreground(d.route) {
		return nil
	}
	p := &d.threadPicker.turn
	choice, ok := d.choice()
	id := sdk.ID(choice.ID)
	if !ok || !id.Valid() {
		id = "" // navigation and creation rows have no transcript
	}
	var answer sdk.ID
	if id == m.active && id != "" {
		// Refresh after another reply commits on the existing feed. Other
		// rows need no watch or polling; read them when they gain focus.
		answer = m.threads[id].lastAnswerID
	}
	if p.thread == id && p.answer == answer {
		return nil
	}
	var old threadTurn
	if p.thread == id {
		old = p.threadTurn // keep the old preview visible during a refresh
	}
	p.reset()
	if id == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	backend, token := m.backend, m.token()
	*p = threadTurnPreview{thread: id, answer: answer, token: token, cancel: cancel, threadTurn: old}
	return func() tea.Msg {
		// A key repeat or fast scroll should not read each briefly focused row.
		timer := time.NewTimer(threadTurnDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		call, done := context.WithTimeout(ctx, menuTimeout)
		defer done()
		turn, err := readThreadTurn(call, backend, id)
		if ctx.Err() != nil {
			return nil
		}
		return threadTurnResult{token, turn, err}
	}
}

func (m *Model) receiveThreadTurn(v threadTurnResult) {
	if d := m.sheet; d != nil && d.threadPicker != nil && m.foreground(d.route) {
		p := &d.threadPicker.turn
		if p.token == v.token && p.cancel != nil {
			p.cancel()
			p.cancel, p.failed = nil, v.err != nil
			if !p.failed {
				p.threadTurn = v.turn
			}
		}
	}
}

// History includes tool activity and inherited entries. Work backwards to the
// latest user message, keeping only the most recent visible assistant reply in
// that turn. The page limit prevents huge tool bursts from monopolizing reads.
func readThreadTurn(ctx context.Context, backend Backend, id sdk.ID) (threadTurn, error) {
	var turn threadTurn
	query := sdk.Before(nil)
	for range threadTurnMaxPages {
		if err := ctx.Err(); err != nil {
			return threadTurn{}, err
		}
		page, err := backend.History(ctx, id, query, threadTurnPageSize)
		if err != nil {
			return threadTurn{}, err
		}
		if page.ThreadID != id || len(page.Entries) > threadTurnPageSize ||
			page.NextCursor != nil && (len(page.Entries) == 0 || *page.NextCursor == "") {
			return threadTurn{}, sdk.ErrProtocol
		}
		for ei := len(page.Entries) - 1; ei >= 0; ei-- {
			entry := page.Entries[ei]
			planInput := entry.Kind == "hush.plan.chat" || entry.Kind == "hush.plan.task"
			switch {
			case entry.Kind == "pi.reset":
				if turn.reply != "" {
					return turn, nil // do not pair a reply with a prompt before a reset
				}
				continue
			case entry.Kind == "pi.compaction", strings.HasPrefix(entry.Kind, "hush.") && !planInput:
				continue // summaries and notices can also look like chat messages
			}
			for mi := len(entry.Messages) - 1; mi >= 0; mi-- {
				msg := entry.Messages[mi]
				if planInput && msg.Role == "assistant" {
					msg.Role = "user" // history exposes a display label for the plan input
				}
				switch msg.Role {
				case "user":
					turn.prompt = threadTurnText(msg)
					if turn.prompt == "" {
						turn.prompt = "[No text]"
					}
					return turn, nil
				case "assistant":
					if turn.reply == "" {
						turn.reply = threadTurnText(msg)
					}
				}
			}
			if planInput {
				return turn, nil
			}
		}
		if page.NextCursor == nil {
			break
		}
		query = sdk.PageCursor(*page.NextCursor)
	}
	return turn, nil
}

func threadTurnText(msg sdk.Message) string {
	const skillPrefix = `<skill name="`
	var b strings.Builder
	truncated := false
	for _, block := range msg.Blocks {
		s := block.Text
		switch {
		case msg.Role == "user" && block.Type == "omitted" && block.Kind == "image":
			s = "[Image attached]"
		case block.Type != "text" || s == "":
			continue
		case msg.Role == "user" && strings.HasPrefix(s, skillPrefix):
			// Keep the invocation and arguments, not the expanded instructions.
			name, _, ok := strings.Cut(limitText(s[len(skillPrefix):], 128), `" location=`)
			invocation := "Skill invocation"
			if ok && name != "" {
				invocation = "/skill:" + name
				const end = "\n</skill>"
				if i := strings.LastIndex(s, end); i >= 0 {
					if args, ok := strings.CutPrefix(s[i+len(end):], "\n\n"); ok {
						invocation += " " + limitText(args, menuMarkdownBytes)
					}
				}
			}
			s = invocation
		}
		if b.Len() != 0 {
			b.WriteByte('\n')
		}
		room := menuMarkdownBytes - b.Len()
		b.WriteString(limitText(s, room))
		if len(s) > room {
			truncated = true
			break
		}
	}
	s := b.String()
	if msg.Role == "assistant" {
		s = menuMarkdownText(s)
	} else {
		s = safeText(s)
	}
	s = strings.TrimSpace(s)
	if s != "" && (truncated || len(s) > menuMarkdownBytes) {
		s = strings.TrimSpace(limitText(s, menuMarkdownBytes)) + "…"
	}
	return s
}

func (m *Model) threadPickerDetail(d *dialog, w, h int) []string {
	p := d.threadPicker
	choice := d.choices[d.filtered[d.selected]]
	title, description := p.selectedRow(d).displayTitle(choice), p.detail(d)
	turn := p.turn
	if h < 2 || turn.thread == "" || string(turn.thread) != choice.ID {
		return m.menuDetail(title, description, w, h)
	}
	reserve := 1
	if turn.prompt != "" && turn.reply != "" {
		if h == 2 {
			return m.threadTurnRows(turn.threadTurn, w, h) // the title is already in the list
		}
		reserve = 2
	}
	showHeading := h >= 7
	if showHeading {
		reserve += 2 // gap and section label
	}
	// Keep the title and metadata, but leave room for both sides of the turn.
	rows := m.menuDetail(title, description, w, max(1, min(max(4, h/3), h-reserve)))
	if showHeading {
		rows = append(rows, "", m.quiet("Last turn"))
	}
	if turn.prompt != "" || turn.reply != "" {
		return append(rows, m.threadTurnRows(turn.threadTurn, w, h-len(rows))...)
	}
	switch {
	case turn.cancel != nil:
		return append(rows, m.quiet("Loading last turn…"))
	case turn.failed:
		return append(rows, m.quiet("Could not load last turn"))
	default:
		return append(rows, m.quiet("No recent messages"))
	}
}

func (m *Model) threadTurnRows(turn threadTurn, w, h int) []string {
	wrap := func(label, text string) []string {
		if text == "" {
			return nil
		}
		if w <= len(label) {
			return wrapped(text, w)
		}
		rows := wrapped(text, w-len(label))
		indent := strings.Repeat(" ", len(label))
		for i, row := range rows {
			if i == 0 {
				rows[i] = m.quiet(label) + row
			} else {
				rows[i] = indent + row
			}
		}
		return rows
	}
	prompt := wrap("Prompt: ", turn.prompt)
	reply := wrap("Reply:  ", turn.reply)
	p := min(len(prompt), max(1, h/2))
	r := min(len(reply), h-p)
	p = min(len(prompt), h-r)
	clip := func(rows []string, n int) []string {
		if n > 0 && n < len(rows) {
			rows[n-1] = ansi.Truncate(strings.TrimRight(rows[n-1], " ")+"…", w, "…")
		}
		return rows[:n]
	}
	return append(clip(prompt, p), clip(reply, r)...)
}
