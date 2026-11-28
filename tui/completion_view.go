package tui

import (
	"strings"

	x "github.com/javiermolinar/hush/tui/extension"
)

// Completions share menu chrome but keep the composer focused. Their targets
// are existing prompt-editor actions, not dialog choices.
type completionItem struct {
	title, detail string
	action        x.ActionRef
}

func (m *Model) completionRows(w, n int, c menuChrome, items []completionItem, selected, limit int, empty string) []displayRow {
	frame := m.menuShell(w, n, c, func(h int) menuFrame {
		if len(items) == 0 {
			return menuFrame{rows: []string{m.quiet(empty)}, targets: []int{-1}}
		}
		focus := items[selected]
		return m.menuColumns(w, h, func(width, height int) []string {
			return m.menuDetail(focus.title, focus.detail, width, height)
		}, func(lw, lh int) menuFrame {
			var list menuFrame
			lo, hi := menuWindow(selected, len(items), min(limit, lh))
			for i := lo; i < hi; i++ {
				list.add(m.menuRow(lw, 0, menuItem{title: items[i].title, focus: i == selected}), i)
			}
			return list
		})
	})
	rows := make([]displayRow, len(frame.rows))
	hitWidth := w
	if frame.listWidth > 0 {
		hitWidth = frame.listWidth
	}
	for i, line := range frame.rows {
		rows[i].text = line
		if target := frame.targets[i]; target >= 0 {
			item := items[target]
			rows[i].hits = []spanHit{{0, hitWidth, item.action, item.title}}
		}
	}
	return rows
}

func (m *Model) slashSheet(matches []registeredAction, w, budget int) []displayRow {
	selected := min(m.slashIndex, len(matches)-1)
	items := make([]completionItem, 0, len(matches))
	for _, a := range matches {
		name, detail := "/"+a.action.Slash.Name, a.action.Description
		if detail == "" {
			detail = a.action.Title
		}
		if key := m.actionBinding(a); key != "" {
			detail += "\n" + key
		}
		ref := x.ActionRef{ID: a.action.ID}
		if ref.ID == stageSkillID {
			ref.Target = a.skill.name + "\n" + a.skill.token
		}
		if ref.ID == browsePathsID {
			name = "Browse paths…"
		}
		items = append(items, completionItem{title: name, detail: detail, action: ref})
	}
	verb, effect, enterHint := "Run", items[selected].title, "↵ run"
	switch items[selected].action.ID {
	case browsePathsID:
		verb, effect = "Browse granted locations", ""
	case stageSkillID:
		verb, enterHint = "Insert", "↵ insert"
	}
	c := menuChrome{title: "Slash commands", count: menuCount(len(items), "command", "commands"),
		consequence: chromeLine(m.menuAction(verb, effect)),
		keys:        chromeLine(m.menuKeys(w, "↑↓ move", enterHint, "tab complete"))}
	return m.completionRows(w, budget, c, items, selected, 5, "No commands match")
}

func (m *Model) fileRows(w, budget int) ([]displayRow, int) {
	_, choices, ok := m.fileSuggestions()
	if !ok || budget <= 0 {
		return nil, 0
	}
	title, verb := "Workspace files", "Complete"
	switch {
	case m.files.kind == fileRoots:
		title, verb = "Granted locations", "Open"
	case strings.HasPrefix(m.files.prefix, "~/"):
		title = "Backend home files"
	}
	status := "No matches · edit the path"
	switch {
	case m.files.waiting && m.files.query != "":
		status = "Searching workspace…"
	case m.files.waiting:
		status = "Fetching directory…"
	case m.files.failed:
		status = "Could not load paths · edit to retry"
	case m.files.partial:
		status = "No matches in scanned paths · narrow the query"
	}
	items := make([]completionItem, 0, len(choices))
	for _, item := range choices {
		label := item.Path
		if item.Directory {
			label += "/"
		}
		detail := m.files.base + label
		switch m.files.kind {
		case fileMention:
			detail = "@" + label
		case fileHome, fileAbsolute:
			label = strings.TrimPrefix(label, m.files.prefix)
		case fileRoots:
			label = "Workspace"
			if item.Path == homeRootChoice {
				label = "Backend home"
			}
			detail = m.rootChoicePath(item.Path)
		}
		items = append(items, completionItem{title: label, detail: detail, action: x.ActionRef{ID: filePickID, Target: item.Path}})
	}
	selected := max(0, min(m.files.selected, len(items)-1))
	c := menuChrome{title: title, count: menuCount(len(items), "match", "matches")}
	if m.files.partial && len(items) > 0 {
		c.scope = "partial results"
	}
	if len(items) > 0 {
		c.consequence = chromeLine(m.menuAction(verb, oneLine(items[selected].title)))
		c.keys = chromeLine(m.menuKeys(w, "↑↓ move", "↵ "+strings.ToLower(verb), "tab complete"))
	} else {
		c.keys = chromeLine(m.menuKeys(w))
	}
	return m.completionRows(w, budget, c, items, selected, maxFileSuggestions, status), len(choices)
}
