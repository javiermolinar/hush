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
	group       paletteGroup
	alias       string
	description string
	keys        []string // shell bindings are dispatched through promptKey, not the registry
}

func paletteGroupFor(a registeredAction) paletteGroup {
	switch a.action.ID {
	case coreID + ".edit", coreID + ".queue", filePickID:
		return palettePrompt
	case coreID + ".details", coreID + ".copy", coreID + ".code", expandRowID:
		return paletteView
	case coreID + ".quit", themesID:
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
		item := paletteItem{Choice: x.Choice{ID: string(a.action.ID), Label: a.action.Title, Description: m.actionBinding(a)}, group: paletteGroupFor(a), description: a.action.Description}
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
	if (threadBusy(t) || canStopPlan(t)) && !t.sending && !t.aborting {
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

func (m *Model) filterInput(query string, w int, placeholder string) (text string, cursor int) {
	s := oneLine(query)
	room := max(1, w-3) // leave a cell for the caret
	if n := stringWidth(s); n > room {
		s = ansi.TruncateLeft(s, n-room+1, "…")
	}
	label := m.paint(x.Accent, "› ")
	if s == "" {
		return label + m.control(ansi.Truncate(placeholder, max(0, w-2), "…")), 2
	}
	return label + s, 2 + stringWidth(s)
}

func (m *Model) paletteRows(d *dialog, w, h int) menuFrame {
	count := len(d.filtered)
	c := menuChrome{title: d.title, count: menuCount(count, "command", "commands"), minBody: 2}
	hints := []string{"↑↓ move", "type to filter"}
	if choice, ok := d.choice(); ok {
		c.consequence = chromeLine(m.menuAction("Run", oneLine(choice.Label)))
		hints = append(hints, "↵ run")
	}
	c.keys = chromeLine(m.menuKeys(w, hints...))
	return m.menuShell(w, h, c, func(budget int) menuFrame {
		var out menuFrame
		if budget >= 2 {
			filter, x := m.filterInput(d.query, w, "Type to filter")
			out.cursor = tea.NewCursor(x, 0)
			out.add(filter, -1)
			budget--
		}
		if count == 0 {
			out.add(m.quiet("No commands match · edit the filter"), -1)
			return out
		}
		item := d.paletteItems[d.filtered[d.selected]]
		var detail []string
		if item.description != "" {
			detail = append(detail, item.description)
		}
		if item.Description != "" {
			detail = append(detail, item.Description)
		}
		if item.alias != "" {
			detail = append(detail, item.alias)
		}
		columns := m.menuColumns(w, budget, func(lw, lh int) []string { return m.menuDetail(item.Label, strings.Join(detail, "\n"), lw, lh) }, func(lw, lh int) menuFrame {
			var list menuFrame
			grouped := strings.TrimSpace(d.query) == "" && lh > 1
			start, _ := menuWindow(d.selected, count, lh)
			if grouped {
				start = d.selected
				used, previous := 0, paletteGroup(255)
				for i := d.selected; i >= 0; i-- {
					group := d.paletteItems[d.filtered[i]].group
					cost := 1
					if group != previous {
						cost++
					}
					if used+cost > lh {
						break
					}
					start, used, previous = i, used+cost, group
				}
			}
			previous := paletteGroup(255)
			for i := start; i < count && len(list.rows) < lh; i++ {
				row := d.paletteItems[d.filtered[i]]
				if grouped && row.group != previous {
					if len(list.rows)+1 >= lh {
						break // no orphaned group heading
					}
					list.add(m.quiet(row.group.String()), -1)
					previous = row.group
				}
				list.add(m.menuRow(lw, 0, menuItem{title: row.Label, focus: i == d.selected}), i)
			}
			return list
		})
		out.appendBody(columns)
		return out
	})
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
