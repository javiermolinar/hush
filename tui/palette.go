package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

const paletteMaxHeight = 20 // includes the border

type paletteGroup uint8

const (
	paletteThread paletteGroup = iota
	palettePrompt
	paletteView
	paletteApp
	paletteExtension
)

func (g paletteGroup) String() string {
	return [...]string{"Thread", "Prompt", "View", "App", "Extensions"}[g]
}

type paletteItem struct {
	x.Choice
	group paletteGroup
	alias string
	keys  []string // shell bindings are dispatched through promptKey, not the registry
}

func paletteGroupFor(a registeredAction) paletteGroup {
	switch a.action.ID {
	case coreID + ".edit", coreID + ".queue", filePickID:
		return palettePrompt
	case coreID + ".details", coreID + ".copy", coreID + ".code", expandRowID:
		return paletteView
	case coreID + ".quit":
		return paletteApp
	case coreID + ".close":
		return paletteThread
	}
	if a.owner == menusID || a.owner == footerID {
		return paletteThread
	}
	return paletteExtension
}

func (m *Model) openPalette(r ownerRoute, budget *int) tea.Cmd {
	d := &dialog{token: m.token(), route: r, budget: budget, title: "Commands", palette: true, promptRevision: m.editor.revision}
	if m.currentFrame() {
		d.activityTarget = m.frame.openActivity
	}
	var items []paletteItem
	for _, a := range m.host.actions {
		// The palette is already open; selecting it again would only stack a duplicate.
		if a.action.ID == paletteID || !m.eligible(a, r.owner) {
			continue
		}
		item := paletteItem{Choice: x.Choice{ID: string(a.action.ID), Label: a.action.Title, Description: m.actionBinding(a)}, group: paletteGroupFor(a)}
		if a.action.Slash != nil {
			item.alias = "/" + a.action.Slash.Name
		}
		items = append(items, item)
	}
	items = append(items, m.shellPaletteItems(m.threads[r.owner])...)
	slices.SortStableFunc(items, func(a, b paletteItem) int {
		if byGroup := cmp.Compare(a.group, b.group); byGroup != 0 {
			return byGroup
		}
		return cmp.Compare(palettePriority(a.ID), palettePriority(b.ID))
	})
	d.paletteItems = items
	d.choices = make([]x.Choice, 0, len(items))
	for _, item := range items {
		d.choices = append(d.choices, item.Choice)
	}
	d.filter()
	return m.pushDialog(d)
}

func (m *Model) shellPaletteItems(t *threadState) []paletteItem {
	var items []paletteItem
	add := func(group paletteGroup, id, label string, keys ...string) {
		items = append(items, paletteItem{
			Choice: x.Choice{ID: "shell:" + id, Label: label, Description: strings.Join(keys, " / ")},
			group:  group, keys: keys,
		})
	}
	hasScrollback := t.intro != nil || len(t.ids) > 0 || t.stream.Present
	atFrame := m.currentFrame()
	if hasScrollback {
		if !t.following && t.anchor != (rowAnchor{id: t.idAt(t.firstBlock())}) || !atFrame || m.frame.hasAbove {
			add(paletteView, "top", "Jump to start of scrollback", shellKeys.top.Help().Key)
		}
		if !atFrame || m.frame.hasAbove {
			add(paletteView, "up", "Scroll up", shellKeys.up.Help().Key)
			add(paletteView, "page-up", "Page up", shellKeys.pageUp.Help().Key)
		}
		if !t.following {
			add(paletteView, "follow", "Resume following output", shellKeys.follow.Help().Key)
			if !atFrame || m.frame.hasBelow {
				add(paletteView, "down", "Scroll down", shellKeys.down.Help().Key)
				add(paletteView, "page-down", "Page down", shellKeys.pageDown.Help().Key)
			}
		}
	}
	add(palettePrompt, "newline", "Insert a newline", shellKeys.newline.Keys()...)
	draft := m.editor.value()
	if draft != "" {
		add(palettePrompt, "clear", "Clear draft", shellKeys.clear.Help().Key)
		switch {
		case len(m.slashMatches()) > 0:
			add(palettePrompt, "run-command", "Run selected slash command", shellKeys.send.Help().Key)
		case threadBusy(t) && !t.sending && !t.aborting:
			add(palettePrompt, "steer", "Send steering message", shellKeys.send.Help().Key)
			add(palettePrompt, "follow-up", "Queue a follow-up", shellKeys.followUp.Help().Key)
		case !t.sending && !t.aborting:
			add(palettePrompt, "send", "Send draft or run slash command", shellKeys.send.Help().Key)
		}
	} else if atFrame && m.frame.openActivity != "" {
		add(paletteView, "open-activity", "Open visible activity details", shellKeys.send.Help().Key)
	}
	if len(t.history) != 0 && (draft == "" || m.editor.historyIndex >= 0) {
		add(palettePrompt, "older", "Recall older prompt", shellKeys.older.Help().Key)
	}
	if m.editor.historyIndex >= 0 {
		add(palettePrompt, "newer", "Recall newer prompt", shellKeys.newer.Help().Key)
	}
	if _, matches, open := m.fileSuggestions(); open && len(matches) > 0 ||
		len(m.slashMatches()) > 0 || m.slashCompleter() != nil {
		add(palettePrompt, "complete", "Complete current command or file", shellKeys.complete.Help().Key)
	}
	if threadBusy(t) && !t.sending && !t.aborting {
		add(paletteThread, "stop", "Stop run (confirm with Esc)", shellKeys.escape.Help().Key, shellKeys.escape.Help().Key)
	}
	return items
}

func palettePriority(id string) int {
	switch x.ActionID(id) {
	case switcherID:
		return 0
	case newThreadID:
		return 1
	case activateTabID, coreID + ".close":
		return 3
	default:
		return 2
	}
}

func (d *dialog) paletteFilterText(i int) string {
	item := d.paletteItems[i]
	return item.group.String() + " " + item.alias + " " + item.Label + " " + item.Description
}

func (m *Model) paletteInput(query string, w int) (text string, cursor int) {
	return m.filterInput(query, w, "Type to filter")
}

func (m *Model) filterInput(query string, w int, placeholder string) (text string, cursor int) {
	s := oneLine(query)
	room := max(1, w-3) // leave a cell for the caret
	if n := stringWidth(s); n > room {
		s = ansi.TruncateLeft(s, n-room+1, "…")
	}
	label := paint(x.Accent, "› ")
	if s == "" {
		return label + m.control(ansi.Truncate(placeholder, max(0, w-2), "…")), 2
	}
	return label + s, 2 + stringWidth(s)
}

// paletteRows returns body rows and the index of the choice on each row (-1
// for chrome). Build only the visible window, retaining context for its first
// group when possible. Headers never count as choices.
func (m *Model) paletteRows(d *dialog, w, h int) ([]string, []int) {
	var rows []string
	var targets []int
	add := func(s string, choice int) {
		rows = append(rows, s)
		targets = append(targets, choice)
	}
	if h <= 0 {
		return nil, nil
	}
	if h >= 3 {
		title := paint(x.Accent, d.title)
		add(title+paint(x.Muted, " "+strings.Repeat("─", max(0, w-stringWidth(title)-1))), -1)
	}
	if h >= 2 {
		filter, _ := m.paletteInput(d.query, w)
		add(filter, -1)
	}
	if h >= 7 {
		add("", -1)
	}
	footer, padFooter := h >= 4, h >= 10
	if footer {
		h--
	}
	if padFooter {
		h--
	}
	budget := h - len(rows)
	grouped := strings.TrimSpace(d.query) == "" && budget > 1
	count := len(d.filtered)
	start, end := 0, 0
	if count == 0 {
		add(paint(x.Muted, "No commands match"), -1)
	} else {
		// Find the earliest start that still leaves room for the selected row.
		// Only inspect as many items as we can possibly show.
		selected := min(d.selected, count-1)
		start = selected
		used, previous := 0, paletteGroup(255)
		for i := selected; i >= 0; i-- {
			group := d.paletteItems[d.filtered[i]].group
			cost := 1
			if grouped && group != previous {
				cost++
			}
			if used+cost > budget {
				break
			}
			start, used, previous = i, used+cost, group
		}
		previous = paletteGroup(255)
		for end = start; end < count && len(rows) < h; end++ {
			item := d.paletteItems[d.filtered[end]]
			if grouped && item.group != previous {
				if len(rows)+1 >= h {
					break // no orphaned heading at the bottom
				}
				add(paint(x.Muted, item.group.String()), -1)
				previous = item.group
			}
			line := m.paletteActionRow(item.Choice, end == selected, w)
			if end == selected && m.theme.Fills {
				line = fill(fit(line, w), blendPalette(m.palette.background, m.palette.colors[4], .15))
			}
			add(line, end)
		}
	}
	if footer {
		if padFooter {
			add("", -1)
		}
		add(m.choiceFooter(w, "run", visibleRange(start, end, count)), -1)
	}
	return rows, targets
}

func visibleRange(start, end, count int) string {
	if end > start && (start > 0 || end < count) {
		return fmt.Sprintf("%d–%d / %d", start+1, end, count)
	}
	return ""
}

func (m *Model) hintRow(w int, full, compact, right string) string {
	room := w
	if right != "" && stringWidth(right)+2 < w {
		room -= stringWidth(right) + 2
	} else {
		right = ""
	}
	if stringWidth(full) > room {
		full = compact
	}
	left := ansi.Truncate(full, room, "…")
	if right != "" {
		left = fit(left, w-stringWidth(right)) + m.control(right)
	}
	return left
}

func (m *Model) choiceFooter(w int, action, right string) string {
	accept := m.keyHint("enter", action)
	full := strings.Join([]string{accept, m.keyHint("↑↓", "move"), m.keyHint("esc", "close")}, "  ")
	compact := strings.Join([]string{accept, m.keyCap("↑↓"), m.keyCap("esc")}, "  ")
	return m.hintRow(w, full, compact, right)
}

func (m *Model) paletteActionRow(c x.Choice, selected bool, width int) string {
	prefix, tone := "  ", x.Plain
	if selected {
		prefix, tone = "› ", x.Accent
	}
	key := oneLine(c.Description)
	if stringWidth(key) > width/3+2 {
		key, _, _ = strings.Cut(key, " / ")
	}
	key = ansi.Truncate(key, max(0, width/3+2), "…")
	keyWidth := stringWidth(key)
	gap := 2
	if keyWidth == 0 {
		gap = 0
	}
	name := ansi.Truncate(prefix+oneLine(c.Label), max(1, width-keyWidth-gap), "…")
	if keyWidth == 0 {
		return paint(tone, name)
	}
	return fit(paint(tone, name), width-keyWidth) + m.control(key)
}

func (m *Model) paletteCursor(d *dialog, left, top, width, height int, framed bool) *tea.Cursor {
	if framed {
		left, top, width, height = left+2, top+1, width-4, height-2
	}
	if height < 2 {
		return nil
	}
	_, x := m.paletteInput(d.query, width)
	if height >= 3 { // title present
		top++
	}
	return tea.NewCursor(left+x, top)
}
