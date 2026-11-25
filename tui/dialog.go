package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const maxDialogs = 8
const maxChoices = 512
const selectQueryMaxBytes = 128
const minOverlayFrameRows = 5
const minOverlayBodyRows = 3

type dialog struct {
	token                       uint64
	activityTarget              string
	promptRevision              uint64
	route                       ownerRoute
	budget                      *int
	placement                   x.Placement
	anchorBottom                bool
	promptDock                  bool // the existing input rule is the panel’s bottom edge
	title, message, query       string
	choices                     []x.Choice
	filtered                    []int
	actions                     []x.ActionID
	selected                    int
	multiline, confirm, palette bool
	paletteItems                []paletteItem
	input                       *promptEditor
	custom                      tea.Model
	output                      *outputViewer
	review                      bool
	modelSearch                 *modelSearchState
	threadPicker                *threadPicker
	config                      *configPanel
	themePicker                 *themePicker
	git                         *gitReview
	workTrust                   *workspaceTrustDialog
	loadedContext               *contextDialog
	plan                        *planPicker
	planWorkspace               *planWorkspacePicker
	planHistory                 *planHistoryPicker
	planReview                  *planReviewPicker
	result                      func(tea.Model) (any, bool)
	done                        func(x.Context, any, bool) x.Outcome
}

type dialogMsg struct {
	token uint64
	msg   tea.Msg
}

func (m *Model) focusedDialog() *dialog {
	if n := len(m.overlays); n > 0 {
		return m.overlays[n-1]
	}
	return m.sheet
}

func (m *Model) openDialog(r ownerRoute, e x.Effect, budget *int) tea.Cmd {
	if !m.foreground(r) {
		return nil
	}
	d := &dialog{route: r, token: m.token(), budget: budget}
	var cmd tea.Cmd
	switch v := e.(type) {
	case x.SelectDialog:
		d.placement, d.title, d.actions = v.Placement, v.Title, append([]x.ActionID(nil), v.Actions[:min(24, len(v.Actions))]...)
		seen := make(map[string]bool)
		for _, c := range v.Choices[:min(maxChoices, len(v.Choices))] {
			if c.ID == "" || len(c.ID) > 1024 || seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			c.Label, c.Description = limitText(c.Label, 512), limitText(c.Description, 1024)
			d.choices = append(d.choices, c)
		}
		d.filter()
		if v.Done != nil {
			d.done = func(c x.Context, val any, ok bool) x.Outcome {
				s, _ := val.(string)
				return v.Done(c, x.Reply[string]{Value: s, Accepted: ok})
			}
		}
	case x.ConfirmDialog:
		d.placement, d.title, d.message, d.confirm = v.Placement, v.Title, limitText(v.Message, 2048), true
		d.choices = []x.Choice{{ID: "no", Label: "No"}, {ID: "yes", Label: "Yes"}}
		d.filter()
		if v.Done != nil {
			d.done = func(c x.Context, val any, ok bool) x.Outcome {
				return v.Done(c, x.Reply[bool]{Value: val == "yes", Accepted: ok})
			}
		}
	case x.ReviewDialog:
		d.title = oneLine(v.Title)
		m.setReviewText(d, v.Text)
		if v.Done != nil {
			d.done = func(c x.Context, _ any, ok bool) x.Outcome {
				return v.Done(c, x.Reply[bool]{Value: ok, Accepted: ok})
			}
		}
	case x.InputDialog:
		d.placement, d.title, d.multiline = v.Placement, v.Title, v.Multiline
		p := newPrompt()
		initial := v.Initial
		if !v.Multiline {
			initial = strings.ReplaceAll(initial, "\n", " ")
		}
		p.area.Placeholder = oneLine(limitText(v.Placeholder, 256))
		p.set(initial)
		d.input = &p
		m.resizeDialogEditor(d)
		cmd = p.area.Focus()
		p.syncViewport()
		if v.Done != nil {
			d.done = func(c x.Context, val any, ok bool) x.Outcome {
				s, _ := val.(string)
				return v.Done(c, x.Reply[string]{Value: s, Accepted: ok})
			}
		}
	case x.CustomDialog:
		if !m.options.CustomModels || v.New == nil || v.Result == nil || len(m.overlays) >= maxDialogs {
			return m.notifyRoute(r, "Custom dialog unavailable", x.Warning)
		}
		d.title, d.result = v.Title, v.Result
		ctx := m.renderContext(m.threads[r.owner], m.dialogWidth(d)-4, max(1, m.height/2-3), false)
		d.custom = v.New(ctx)
		if d.custom == nil {
			return m.notifyRoute(r, "Custom dialog returned no model", x.Warning)
		}
		if v.Done != nil {
			d.done = func(c x.Context, val any, ok bool) x.Outcome {
				return v.Done(c, x.Reply[any]{Value: val, Accepted: ok})
			}
		}
		cmd = scopeDialog(d.token, d.custom.Init())
	default:
		return nil
	}
	d.title = limitText(d.title, 256)
	out := m.pushDialog(d)
	if d.custom != nil {
		if value, ok := d.result(d.custom); ok {
			return tea.Batch(out, m.closeDialog(d, value, true))
		}
	}
	return tea.Batch(out, cmd)
}

func (m *Model) setReviewText(d *dialog, text string) {
	d.review = true
	d.output = newOutputViewer(limitText(text, 64<<10))
	d.output.reflowReview(m.dialogWidth(d) - 4)
}

func (m *Model) pushDialog(d *dialog) tea.Cmd {
	m.leader = ""
	m.escapeAt, m.stopAt = time.Time{}, time.Time{}
	if d.placement == x.Sheet {
		previous := m.sheet
		m.sheet = d
		if previous != nil && previous != d {
			return m.replyDialog(previous, nil, false, previous.budget)
		}
	} else {
		if len(m.overlays) >= maxDialogs {
			return m.notifyRoute(d.route, "Too many dialogs", x.Warning)
		}
		m.overlays = append(m.overlays, d)
	}
	return nil
}

func (m *Model) resizeDialogEditor(d *dialog) {
	if d.input == nil {
		return
	}
	d.input.area.SetWidth(max(1, m.dialogWidth(d)-6))
	height := 1
	if d.multiline {
		height = max(1, min(4, m.height/4-1))
	}
	d.input.area.SetHeight(height)
}

func (m *Model) resizeCustom() tea.Cmd {
	var cmds []tea.Cmd
	for _, d := range m.overlays {
		if d.custom == nil {
			continue
		}
		// Size is an ordinary message scoped to the dialog; it cannot resize the parent.
		token, w, h := d.token, max(1, m.dialogWidth(d)-4), max(1, m.height/2-3)
		cmds = append(cmds, func() tea.Msg { return dialogMsg{token, tea.WindowSizeMsg{Width: w, Height: h}} })
	}
	return tea.Batch(cmds...)
}

func scopeDialog(id uint64, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg { return dialogMsg{id, cmd()} }
}

func (m *Model) receiveDialog(msg dialogMsg) tea.Cmd {
	if msg.msg == nil {
		return nil
	}
	for _, d := range m.overlays {
		if d.token == msg.token && d.custom != nil {
			return m.customUpdate(d, msg.msg)
		}
	}
	return nil
}

func (m *Model) customUpdate(d *dialog, msg tea.Msg) tea.Cmd {
	next, cmd := d.custom.Update(msg)
	if next == nil {
		return m.closeDialog(d, nil, false)
	}
	d.custom = next
	if value, ok := d.result(next); ok {
		return m.closeDialog(d, value, true)
	}
	return scopeDialog(d.token, cmd)
}

func (m *Model) replyDialog(d *dialog, value any, accepted bool, budget *int) tea.Cmd {
	if d.planWorkspace != nil && d.planWorkspace.cancel != nil {
		d.planWorkspace.cancel()
	}
	if d.planReview != nil && d.planReview.cancel != nil {
		d.planReview.cancel()
	}
	if d.plan != nil && d.plan.cancel != nil {
		d.plan.cancel()
	}
	if d.modelSearch != nil {
		d.modelSearch.stop()
	}
	if d.threadPicker != nil {
		d.threadPicker.stop()
	}
	if d.git != nil {
		d.git.stop()
	}
	if d.workTrust != nil && d.workTrust.cancel != nil {
		d.workTrust.cancel()
	}
	if d.loadedContext != nil && d.loadedContext.cancel != nil {
		d.loadedContext.cancel()
	}
	if d.themePicker != nil && !accepted && !d.themePicker.saving {
		m.applyTheme(d.themePicker.before)
	}
	if !m.live(d.route) || d.done == nil {
		return nil
	}
	if budget == nil {
		b := maxEffects
		budget = &b
	}
	return m.admit(d.route, d.done(m.contextFor(d.route.owner), value, accepted), budget)
}

func (m *Model) closeDialog(d *dialog, value any, accepted bool) tea.Cmd {
	if d == m.sheet {
		m.sheet = nil
	} else {
		found := false
		for i, top := range m.overlays {
			if top == d {
				copy(m.overlays[i:], m.overlays[i+1:])
				m.overlays[len(m.overlays)-1] = nil
				m.overlays = m.overlays[:len(m.overlays)-1]
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	focus := m.editor.area.Focus
	if top := m.focusedDialog(); top != nil {
		if top.input != nil {
			focus = top.input.area.Focus
		} else {
			focus = nil
		}
	}
	answer := m.replyDialog(d, value, accepted, d.budget)
	if focus != nil {
		return tea.Batch(answer, focus())
	}
	return answer
}

func fuzzyMatch(value, query string) bool {
	if query == "" {
		return true
	}
	value = strings.ToLower(value)
	query = strings.ToLower(strings.TrimSpace(query))
	for _, r := range query {
		i := strings.IndexRune(value, r)
		if i < 0 {
			return false
		}
		value = value[i+len(string(r)):]
	}
	return true
}

func (d *dialog) filter() {
	d.filtered = d.filtered[:0]
	for i, c := range d.choices {
		if d.query == "" || d.modelSearch != nil || d.threadPicker != nil && d.threadPicker.searchable {
			d.filtered = append(d.filtered, i)
			continue
		}
		text := c.Label + " " + c.Description
		if p := d.threadPicker; p != nil {
			switch c.ID {
			case pageChoice, "refresh", "all-workspaces":
				d.filtered = append(d.filtered, i)
				continue
			}
			text = p.rows[i].workspaceName + " " + string(p.rows[i].status) + " " + text
		}
		if d.palette {
			text = d.paletteFilterText(i)
		}
		if fuzzyMatch(text, d.query) {
			d.filtered = append(d.filtered, i)
		}
	}
	d.selected = 0
	for j, i := range d.filtered {
		c := d.choices[i]
		// Workspace creation is selectable, but Enter after searching should
		// still open the first matching conversation.
		if !c.Disabled && (d.threadPicker == nil || c.ID != newThreadHereChoice) {
			d.selected = j
			break
		}
	}
}

func (d *dialog) move(delta int) {
	n := len(d.filtered)
	for i := d.selected + delta; i >= 0 && i < n; i += delta {
		if !d.choices[d.filtered[i]].Disabled {
			d.selected = i
			return
		}
	}
}

func (d *dialog) choice() (x.Choice, bool) {
	if len(d.filtered) == 0 || d.selected >= len(d.filtered) {
		return x.Choice{}, false
	}
	c := d.choices[d.filtered[d.selected]]
	return c, !c.Disabled
}

func (m *Model) confirmVisible(d *dialog) bool {
	if !m.dialogVisible(d) {
		return false
	}
	// A choice target is drawn only when there is room for the question.
	return slices.ContainsFunc(m.frame.dialogChoices, func(target int) bool { return target >= 0 })
}

func (m *Model) dialogVisible(d *dialog) bool {
	return m.currentFrame() && m.frame.dialogToken == d.token && m.frame.dialogHeight > 0
}

func (m *Model) dialogCanInput(d *dialog) bool {
	return m.pane().usable && (!m.currentFrame() || m.frame.dialogToken != d.token || m.frame.dialogHeight > 0)
}

func (m *Model) acceptChoice(d *dialog) tea.Cmd {
	if d.themePicker != nil {
		return m.acceptTheme(d)
	}
	if d.confirm && !m.confirmVisible(d) {
		return nil
	}
	c, ok := d.choice()
	if !ok {
		return nil
	}
	if p := d.threadPicker; p != nil {
		switch c.ID {
		case pageChoice:
			return m.loadThreadPicker(d, p.cursor, 0)
		case "refresh":
			return m.loadThreadPicker(d, "", 0)
		case "all-workspaces":
			p.all = true
			return m.loadThreadPicker(d, "", 0)
		case newThreadHereChoice:
			return m.openWorkspaceDirectories(d)
		case workspaceDirectoryChoice:
			return m.createWorkspaceThread(d, p.selectedRow(d).directory, false)
		}
	}
	if d.palette {
		if item := d.paletteItems[d.filtered[d.selected]]; len(item.keys) != 0 {
			// The palette has focus, so close it before dispatching a shell key.
			// Stop is exceptional: arm the normal two-Esc confirmation, never
			// synthesize both presses behind the dialog.
			cmd := m.closeDialog(d, nil, false)
			if !m.foreground(d.route) || item.group == palettePrompt && d.promptRevision != m.editor.revision {
				return cmd // an asynchronous edit must not change the meaning of the selected key
			}
			if c.ID == "shell:stop" {
				return tea.Batch(cmd, m.stopKey())
			}
			if c.ID == "shell:open-activity" {
				b := maxEffects
				return tea.Batch(cmd, m.invoke(d.route, x.ActionRef{ID: expandRowID, Target: d.activityTarget}, &b))
			}
			return tea.Batch(cmd, m.promptKey(paletteShellKey(item.keys[0])))
		}
		b := maxEffects
		if i, exists := m.host.byID[x.ActionID(c.ID)]; exists && m.eligible(m.host.actions[i], d.route.owner) {
			// Dialogs opened by palette items return to the same palette on Escape.
			// Direct effects close it only if the action didn't open another surface.
			count, sheet := len(m.overlays), m.sheet
			cmd := m.invoke(d.route, x.ActionRef{ID: x.ActionID(c.ID)}, &b)
			if len(m.overlays) == count && m.sheet == sheet {
				return tea.Batch(cmd, m.closeDialog(d, nil, false))
			}
			if m.sheet != sheet && len(m.overlays) == count {
				return tea.Batch(cmd, m.closeDialog(d, nil, false))
			}
			return cmd
		}
		return nil
	}
	return m.closeDialog(d, c.ID, true)
}

func (m *Model) dialogInput(msg tea.Msg) tea.Cmd {
	d := m.focusedDialog()
	if d == nil {
		return nil
	}
	k, key := msg.(tea.KeyPressMsg)
	chord := ""
	if key {
		chord = normalizeChord(k.String())
	}
	if d.themePicker != nil {
		return m.themePickerKey(d, chord)
	}
	if key {
		if d.config != nil {
			return m.configInput(d, msg, chord)
		}
		if chord == "esc" {
			if d.workTrust != nil {
				return m.workspaceTrustKey(d, chord)
			}
			if d.git != nil && d.git.patch {
				return m.gitDiffKey(d, chord)
			}
			return m.closeDialog(d, nil, false)
		}
	} else if d.config != nil {
		return m.configInput(d, msg, chord)
	}
	if !m.dialogCanInput(d) {
		return nil // Escape still dismisses an undrawn dialog
	}
	if d.custom != nil {
		return m.customUpdate(d, msg)
	}
	if d.planWorkspace != nil {
		return m.planWorkspaceKey(d, chord)
	}
	if d.planHistory != nil {
		return m.planHistoryKey(d, chord)
	}
	if d.planReview != nil {
		return m.planReviewKey(d, chord)
	}
	if d.plan != nil {
		return m.planKey(d, chord)
	}
	if d.git != nil {
		return m.gitDiffKey(d, chord)
	}
	if d.workTrust != nil {
		return m.workspaceTrustKey(d, chord)
	}
	if d.output != nil {
		return m.outputKey(d, chord)
	}
	if key && len(d.actions) > 0 {
		seq := ""
		if m.leader != "" {
			seq = strings.TrimSpace(m.leader + " " + chord)
			m.leader = ""
		}
		if chord == "ctrl+x" {
			m.leader = " "
			return nil
		}
		for _, a := range d.actions {
			i, ok := m.host.byID[a]
			if !ok {
				continue
			}
			bind := m.host.actions[i].action.Binding
			if (seq == "" && chord != "" && bind.Direct == chord) || (seq != "" && bind.Leader == seq) {
				c, _ := d.choice()
				if p := d.threadPicker; p != nil && p.all {
					if a == newThreadID {
						return m.openWorkspaceDirectories(d)
					}
					if c.ID == newThreadHereChoice {
						return nil // this row doesn't target an existing thread
					}
				}
				b := maxEffects
				return m.invoke(d.route, x.ActionRef{ID: a, Target: c.ID}, &b)
			}
			if seq != "" && strings.HasPrefix(bind.Leader, seq+" ") {
				m.leader = seq
			}
		}
		if seq != "" {
			return nil
		}
	}
	if d.input != nil {
		if chord == "enter" {
			return m.closeDialog(d, d.input.value(), true)
		}
		if chord == "ctrl+j" || chord == "shift+enter" {
			if d.multiline {
				d.input.newline()
			}
			return nil
		}
		return d.input.update(msg, d.multiline)
	}
	if chord == "enter" || chord == "tab" {
		return m.acceptChoice(d)
	}
	switch chord {
	case "up", "left":
		d.move(-1)
		return nil
	case "down", "right":
		d.move(1)
		return nil
	case "home", "end":
		if chord == "home" {
			for i, j := range d.filtered {
				if !d.choices[j].Disabled {
					d.selected = i
					break
				}
			}
		} else {
			for i := len(d.filtered) - 1; i >= 0; i-- {
				if !d.choices[d.filtered[i]].Disabled {
					d.selected = i
					break
				}
			}
		}
		return nil
	case "pgup", "pgdown":
		delta := 1
		if chord == "pgup" {
			delta = -1
		}
		count := 0
		if m.currentFrame() && m.frame.dialogToken == d.token {
			for _, choice := range m.frame.dialogChoices {
				if choice >= 0 {
					count++
				}
			}
		}
		for range max(1, count) {
			d.move(delta)
		}
		return nil
	case "backspace", "ctrl+h":
		s := []rune(d.query)
		if len(s) > 0 {
			return m.changeDialogQuery(d, string(s[:len(s)-1]))
		}
		return nil
	case "y", "n":
		if d.confirm && m.confirmVisible(d) {
			return m.closeDialog(d, map[bool]string{true: "yes", false: "no"}[chord == "y"], true)
		}
	}
	if d.confirm {
		return nil
	}
	if k.Text != "" {
		return m.changeDialogQuery(d, d.query+oneLine(k.Text))
	}
	if p, ok := msg.(tea.PasteMsg); ok {
		return m.changeDialogQuery(d, d.query+oneLine(p.Content))
	}
	return nil
}

func (m *Model) changeDialogQuery(d *dialog, query string) tea.Cmd {
	query = limitText(query, selectQueryMaxBytes)
	if query == d.query {
		return nil
	}
	d.query = query
	if d.modelSearch != nil {
		return m.searchModels(d, modelSearchDelay)
	}
	if p := d.threadPicker; p != nil {
		if p.searchable {
			return m.loadThreadPicker(d, "", threadSearchDelay)
		}
		if p.cancel == nil {
			d.token = m.token()
		}
	}
	d.filter()
	return nil
}

func (m *Model) dialogWidth(d *dialog) int {
	pane := m.pane()
	if d.placement == x.Sheet || d.promptDock {
		return pane.width
	}
	if d.git != nil {
		if d.git.patch {
			return max(1, m.width)
		}
		return min(92, pane.width)
	}
	if pane.narrow {
		return pane.width
	}
	if d.palette {
		return min(60, max(12, m.width-8))
	}
	if d.output != nil {
		return min(128, max(12, m.width-8))
	}
	return min(72, max(12, m.width-8))
}
func (m *Model) dialogLimit(d *dialog) int {
	if d.threadPicker != nil {
		return max(2, min(12, m.height/2))
	}
	if d.placement == x.Sheet {
		return max(2, min(9, m.height/3))
	}
	return max(3, min(14, m.height/2-2))
}

// targets maps every drawn row to its filtered choice, or -1 for chrome.
func (m *Model) dialogRows(d *dialog, w, n int) ([]string, []int) {
	if n <= 0 {
		return nil, nil
	}
	title := d.title
	if title == "" {
		title = "Select"
	}
	var rows []string
	var targets []int
	add := func(row string, target int) {
		rows, targets = append(rows, row), append(targets, target)
	}
	if d.custom != nil {
		add(m.paint(x.Accent, oneLine(title)), -1)
		body := scopeView(d.custom.View().Content, min(recordBytes, w*n*8))
		for s := range strings.SplitSeq(body, "\n") {
			if len(rows) >= n {
				break
			}
			row := ansiCut(s, w)
			if strings.Contains(row, "\x1b[") {
				row += ansi.ResetStyle
			}
			add(row, -1)
		}
		return rows, targets
	}

	footer := n >= 3 && (d.threadPicker == nil || n >= 5)
	if d.confirm {
		// Preserve the question and choices before optional chrome. A one-row
		// confirmation can show the question, but cannot be accepted.
		if d.message == "" {
			add(m.paint(x.Accent, oneLine(title)), -1)
			footer = n >= 4
		} else {
			if n >= 5 {
				add(m.paint(x.Accent, oneLine(title)), -1)
			}
			add(m.control(ansi.Truncate(oneLine(d.message), w, "…")), -1)
			footer = n >= 4
		}
	} else if n >= 4 || d.input != nil && n >= 3 {
		add(m.paint(x.Accent, oneLine(title)), -1)
	}
	if footer {
		n--
	}
	if d.input != nil {
		for _, text := range d.input.rows(max(1, w-2), n-len(rows)) {
			add(text, -1)
		}
		if footer {
			plain := m.keyHint("enter", "accept") + "  " + m.keyHint("esc", "cancel")
			full := plain
			if d.multiline {
				full += "  " + m.keyHint("shift+enter", "newline")
			}
			add(m.hintRow(w, full, plain, ""), -1)
		}
		return rows, targets
	}
	if !d.confirm && d.message != "" && n-len(rows) >= 3 {
		add(m.control(oneLine(d.message)), -1)
	}
	if !d.confirm && n-len(rows) >= 2 {
		placeholder := "Type to filter"
		if p := d.threadPicker; p != nil {
			placeholder = "Workspace, thread, or message"
			if p.creation != nil {
				placeholder = "Filter working directories"
			}
		}
		filter, _ := m.filterInput(d.query, w, placeholder)
		add(filter, -1)
	}
	count, budget := len(d.filtered), n-len(rows)
	lo, hi := 0, 0
	if p := d.threadPicker; p != nil {
		if status := p.message(d); status != "" && budget > 0 && (count == 0 || budget > 1) {
			add(m.control(ansi.Truncate("  "+status, w, "…")), -1)
			budget--
			if count == 0 {
				budget = 0 // do not also show the generic empty-list row
			}
		}
		lo, hi = m.threadPickerRows(d, w, budget, add)
	} else {
		lo = min(max(0, d.selected-budget+1), max(0, count-budget))
		hi = min(lo+budget, count)
		for i := lo; i < hi; i++ {
			item := d.choices[d.filtered[i]]
			prefix, tone := "  ", x.Plain
			if i == d.selected {
				prefix, tone = "› ", x.Accent
			}
			if item.Disabled {
				tone = x.Muted
			}
			label, detail := oneLine(item.Label), oneLine(item.Description)
			text := m.paint(tone, ansi.Truncate(prefix+label, w, "…"))
			if detail != "" {
				tail := ansiCut(detail, max(0, w/2))
				text = m.paint(tone, ansiCut(prefix+label, max(0, w-stringWidth(tail)-2))) + "  " + m.control(tail)
			}
			add(text, i)
		}
	}
	if count == 0 && budget > 0 {
		status := "No matches"
		if search := d.modelSearch; search != nil {
			if search.cancel != nil {
				status = "Searching models…"
			} else if search.failed {
				status = "Model search failed · edit to retry"
			}
		}
		add(m.control(ansi.Truncate("  "+status, w, "…")), -1)
	}
	if footer {
		if (d.modelSearch != nil || d.threadPicker != nil) && count == 0 {
			hint := m.keyHint("esc", "close")
			add(m.hintRow(w, hint, hint, ""), -1)
		} else if d.threadPicker != nil {
			add(m.threadPickerFooter(d, w, visibleRange(lo, hi, count)), -1)
		} else {
			add(m.choiceFooter(w, "choose", visibleRange(lo, hi, count)), -1)
		}
	}
	return rows, targets
}

func (m *Model) overlayFramed(d *dialog, available int) bool {
	return !d.promptDock && available >= minOverlayFrameRows && m.dialogWidth(d) >= 24
}

func (m *Model) overlayRows(d *dialog, available int) ([]string, []int) {
	if available <= 0 {
		return nil, nil
	}
	w := m.dialogWidth(d)
	if d.planWorkspace != nil {
		return m.planWorkspaceRows(d, w, min(15, available))
	}
	if d.planHistory != nil {
		return m.planHistoryRows(d, w, min(18, available))
	}
	if d.planReview != nil {
		return m.planReviewRows(d, w, min(18, available))
	}
	if d.plan != nil {
		return m.planRows(d, w, min(18, available))
	}
	if !m.overlayFramed(d, available) {
		// Prefer usable body rows to a border in short or narrow panes.
		if d.git != nil {
			return m.gitReviewRows(d, w, available)
		}
		if d.workTrust != nil {
			return m.workspaceTrustRows(d.workTrust, w, available)
		}
		if d.output != nil {
			return m.outputRows(d, w, available)
		}
		if d.palette {
			return m.paletteRows(d, w, available)
		}
		if d.themePicker != nil {
			return m.themeRows(d, w, min(12, available))
		}
		return m.dialogRows(d, w, available)
	}
	var body []string
	var choices []int
	if d.git != nil {
		height := available - 2
		if !d.git.patch {
			height = min(12, height-1) // keep some chat visible behind the popover
		}
		body, choices = m.gitReviewRows(d, w-4, height)
	} else if d.workTrust != nil {
		body, choices = m.workspaceTrustRows(d.workTrust, w-4, min(14, available-2))
	} else if d.palette {
		body, choices = m.paletteRows(d, w-4, min(paletteMaxHeight, available)-2)
	} else if d.themePicker != nil {
		body, choices = m.themeRows(d, w-4, min(12, available-2))
	} else if d.output != nil {
		body, choices = m.outputRows(d, w-4, available-2)
	} else {
		body, choices = m.dialogRows(d, w-4, min(m.dialogLimit(d), available-2))
	}
	rows := make([]string, 0, len(body)+2)
	targets := make([]int, 0, len(body)+2)
	rows = append(rows, m.paint(x.Muted, "╭"+strings.Repeat("─", w-2)+"╮"))
	targets = append(targets, -1)
	edge := m.paint(x.Muted, "│")
	for i, row := range body {
		if d.output == nil { // outputRows already fit their body width
			row = fit(row, w-4)
		}
		rows = append(rows, edge+" "+row+" "+edge)
		targets = append(targets, choices[i])
	}
	rows = append(rows, m.paint(x.Muted, "╰"+strings.Repeat("─", w-2)+"╯"))
	return rows, append(targets, -1)
}

func threadLabel(id sdk.ID) string {
	if id == "" {
		return "new thread"
	}
	return fmt.Sprintf("thread %s", id)
}
