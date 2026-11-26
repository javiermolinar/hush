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

type dialog struct {
	token                       uint64
	activityTarget              string
	promptRevision              uint64
	route                       ownerRoute
	budget                      *int
	placement                   x.Placement
	title, message, query       string
	choices                     []x.Choice
	filtered                    []int
	actions                     []x.ActionID
	selected                    int
	multiline, confirm, palette bool
	full                        bool // expands the dock without moving the composer
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
			c.Action = limitText(c.Action, 128)
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
		ctx := m.renderContext(m.threads[r.owner], m.pane().width, max(1, m.height/2-3), false)
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
	d.output.reflowReview(m.pane().width)
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
	d.input.area.SetWidth(max(1, m.pane().width-2))
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
		token, w, h := d.token, max(1, m.pane().width), max(1, m.height/2-3)
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
		m.previewFills(d.themePicker.fillsBefore)
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
		if chord == "f" && m.dialogCanInput(d) && d.canZoom() {
			d.full = !d.full
			return nil
		}
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

func (m *Model) dialogRows(d *dialog, w, n int) menuFrame {
	title := d.title
	if title == "" {
		title = "Select"
	}
	c := menuChrome{title: title}
	if d.custom != nil {
		return m.menuShell(w, n, c, func(h int) menuFrame {
			var out menuFrame
			body := scopeView(d.custom.View().Content, min(recordBytes, w*h*8))
			for s := range strings.SplitSeq(body, "\n") {
				if len(out.rows) >= h {
					break
				}
				row := ansiCut(s, w)
				if strings.Contains(row, "\x1b[") {
					row += ansi.ResetStyle
				}
				out.add(row, -1)
			}
			return out
		})
	}
	if d.input != nil {
		keys := []string{"↵ accept"}
		if d.multiline {
			keys = append(keys, "shift+enter newline")
		}
		c.consequence = chromeLine(m.menuAction("Accept", ""))
		c.keys = chromeLine(m.menuKeys(w, keys...))
		return m.menuShell(w, n, c, func(h int) menuFrame {
			var out menuFrame
			for _, text := range d.input.rows(max(1, w-2), h) {
				out.add(text, -1)
			}
			return out
		})
	}
	if d.confirm {
		question := d.message
		if question == "" {
			question = title
		}
		c.minBody = 2 // never draw an actionable choice without its question
		c.keys = chromeLine(m.menuKeys(w, "↑↓ move", "↵ choose", "y yes", "n no"))
		choice, _ := d.choice()
		if choice.ID == "yes" {
			c.consequence = chromeLine(m.menuAction("Confirm", ""))
		} else {
			c.consequence = chromeLine(m.menuAction("Keep unchanged", ""))
		}
		return m.menuShell(w, n, c, func(h int) menuFrame {
			var out menuFrame
			out.add(m.control(oneLine(question)), -1)
			lo, hi := menuWindow(d.selected, len(d.filtered), h-1)
			for i := lo; i < hi; i++ {
				item := d.choices[d.filtered[i]]
				out.add(m.menuRow(w, 0, menuItem{title: item.Label, focus: i == d.selected}), i)
			}
			return out
		})
	}
	return m.selectRows(d, w, n)
}

func (m *Model) selectRows(d *dialog, w, n int) menuFrame {
	count := len(d.filtered)
	c := menuChrome{title: d.title, count: menuCount(count, "choice", "choices")}
	if c.title == "" {
		c.title = "Select"
	}
	key := "↵ choose"
	if d.modelSearch != nil {
		c.title, c.count = "Select model", menuCount(count, "model", "models")
	}
	if d.config != nil {
		c.scope = m.configScope(d)
	}
	if choice, ok := d.choice(); ok {
		verb, effect := "Choose", oneLine(choice.Label)
		if choice.Action != "" {
			verb = oneLine(choice.Action)
		}
		if d.threadPicker != nil {
			verb, effect = m.threadPickerAction(d, choice)
		}
		if d.modelSearch != nil {
			item, _ := d.modelMenuItem(choice, true)
			effect = oneLine(item.title)
			switch {
			case item.state == "Unavailable":
				verb = "Show why unavailable"
			case d.config != nil:
				verb = "Use model"
			default:
				verb = "Choose thinking for"
			}
		}
		c.consequence = chromeLine(m.menuAction(verb, effect))
	} else {
		c.consequence = chromeLine(m.quiet("No available choice"))
	}
	c.keys = chromeLine(m.menuKeys(w, "↑↓ move", key, "type to filter"))
	if d.threadPicker != nil {
		c.keys = chromeLine(m.threadPickerKeys(d, w))
	} else if d.config != nil {
		c.keys = chromeLine(m.menuKeys(w, "↑↓ move", "↵ use", "ctrl+d make default", "type to filter"))
	}
	return m.menuShell(w, n, c, func(h int) menuFrame {
		var out menuFrame
		if d.message != "" && h >= 3 {
			out.add(m.quiet(oneLine(d.message)), -1)
		}
		if h-len(out.rows) >= 2 {
			placeholder := "Type to filter"
			if p := d.threadPicker; p != nil {
				placeholder = "Workspace, thread, or message"
				if p.creation != nil {
					placeholder = "Filter working directories"
				}
			}
			filter, x := m.filterInput(d.query, w, placeholder)
			if d.config != nil {
				out.cursor = tea.NewCursor(x, len(out.rows))
			}
			out.add(filter, -1)
		}
		budget := h - len(out.rows)
		if p := d.threadPicker; p != nil {
			if status := p.message(d); status != "" && (count == 0 || budget > 1) {
				out.add(m.quiet(status), -1)
				if count == 0 {
					return out
				}
				budget--
			}
			if count > 0 {
				focus, detail := d.choices[d.filtered[d.selected]], p.detail(d)
				list := m.menuColumns(w, budget, func(width, height int) []string {
					return m.menuDetail(p.selectedRow(d).displayTitle(focus), detail, width, height)
				}, func(lw, lh int) menuFrame {
					var list menuFrame
					m.threadPickerRows(d, lw, lh, list.add)
					return list
				})
				out.appendBody(list)
				return out
			}
		}
		if count == 0 {
			status := "No matches"
			if search := d.modelSearch; search != nil {
				if search.cancel != nil {
					status = "Searching models…"
				} else if search.failed {
					status = "Model search failed · edit to retry"
				}
			}
			out.add(m.quiet(status), -1)
			return out
		}
		var detail func(int, int) []string
		if item, description := d.modelMenuItem(d.choices[d.filtered[d.selected]], true); description != "" {
			detail = func(width, height int) []string { return m.menuDetail(item.title, description, width, height) }
		}
		list := m.menuColumns(w, budget, detail, func(lw, lh int) menuFrame {
			var list menuFrame
			lo, hi := menuWindow(d.selected, count, lh)
			for i := lo; i < hi; i++ {
				item, _ := d.modelMenuItem(d.choices[d.filtered[i]], i == d.selected)
				stateWidth := 0
				if d.modelSearch != nil {
					stateWidth = 11
				}
				list.add(m.menuRow(lw, stateWidth, item), i)
			}
			return list
		})
		out.appendBody(list)
		return out
	})
}

func threadLabel(id sdk.ID) string {
	if id == "" {
		return "new thread"
	}
	return fmt.Sprintf("thread %s", id)
}
