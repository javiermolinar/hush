package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	x "github.com/javiermolinar/hush/tui/extension"
)

const leaderStepID x.ActionID = coreID + ".leader-step"

// advanceLeader is the existing chord dispatcher, shared by key and menu hits.
func (m *Model) advanceLeader(chord string) tea.Cmd {
	seq := strings.TrimSpace(m.leader + " " + chord)
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

func (m *Model) leaderRows(w, limit int) []displayRow {
	prefix := strings.TrimSpace(m.leader)
	type shortcut struct{ key, title string }
	var shortcuts, tabNavigation []shortcut
	seen := make(map[string]bool)
	for _, a := range m.host.actions {
		seq := a.action.Binding.Leader
		if seq == "" || !m.eligible(a, m.active) {
			continue
		}
		if prefix != "" {
			var ok bool
			if seq, ok = strings.CutPrefix(seq, prefix+" "); !ok {
				continue
			}
		}
		key, _, branch := strings.Cut(seq, " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		title := a.action.Title
		if branch {
			title = "More actions"
		}
		entry := shortcut{key, title}
		if a.action.ID == previousOpenTabID || a.action.ID == nextOpenTabID {
			tabNavigation = append(tabNavigation, entry)
		} else {
			shortcuts = append(shortcuts, entry)
		}
	}
	// Keep thread navigation visible when the leader dock cannot fit every action.
	shortcuts = append(tabNavigation, shortcuts...)
	scope := "ctrl+x"
	if prefix != "" {
		scope += " " + prefix
	}
	c := menuChrome{title: "Leader", scope: scope, count: menuCount(len(shortcuts), "chord", "chords"),
		keys: chromeLine(m.menuKeys(w, "type a chord"))}
	frame := m.menuShell(w, limit, c, func(h int) menuFrame {
		var list menuFrame
		for i, v := range shortcuts[:min(h, len(shortcuts))] {
			key := fit(oneLine(v.key), min(8, w/3))
			list.add(m.quiet(key)+"  "+oneLine(v.title), i)
		}
		if len(list.rows) == 0 {
			list.add(m.quiet("No available actions"), -1)
		}
		return list
	})
	rows := make([]displayRow, len(frame.rows))
	for i, line := range frame.rows {
		rows[i].text = line
		if target := frame.targets[i]; target >= 0 {
			v := shortcuts[target]
			rows[i].hits = []spanHit{{0, w, x.ActionRef{ID: leaderStepID, Target: prefix + "\n" + v.key}, v.title}}
		}
	}
	return rows
}

func (m *Model) clickLeader(target string) tea.Cmd {
	prefix, key, ok := strings.Cut(target, "\n")
	if ok && m.leader != "" && prefix == strings.TrimSpace(m.leader) {
		return m.advanceLeader(key)
	}
	return nil
}
