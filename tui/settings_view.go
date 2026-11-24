package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func modelRef(model sdk.Model) string {
	if model.Provider == "" {
		return model.ModelID
	}
	if model.ModelID == "" {
		return model.Provider
	}
	return model.Provider + "/" + model.ModelID
}

func (m *Model) configSelectedValue(value string) string {
	value = "\x1b[1m" + value + "\x1b[22m"
	if m.palette.activeTab != "" {
		return surface(value, m.palette.activeTab)
	}
	return paint(x.Accent, value)
}

func (m *Model) configScopeLabel(label string, active bool) string {
	if active {
		return m.configSelectedValue(label)
	}
	return m.control(label)
}

func (m *Model) configHeader(d *dialog, w int) string {
	p := d.config
	left := paint(x.Muted, "─ ") + m.control("settings") + paint(x.Muted, " · ")
	thread := m.configScopeLabel("this thread", p.scope == configThread)
	defaults := m.configScopeLabel("default", p.scope == configDefault)
	middle := thread + paint(x.Muted, "  ") + defaults
	tail := paint(x.Muted, " · tab switches scope")
	if p.pending {
		tail = paint(x.Muted, " · saving…")
	}
	line := left + middle + tail + " "
	line += paint(x.Muted, strings.Repeat("─", max(0, w-stringWidth(line))))
	return ansiCut(line, w)
}

func (m *Model) configModelHeader(d *dialog, w int) string {
	scope := "this thread"
	if d.config.scope == configDefault {
		scope = "default"
	}
	line := paint(x.Muted, "─ ") + m.control("model") + paint(x.Muted, " · ") + m.configSelectedValue(scope) + " "
	line += paint(x.Muted, strings.Repeat("─", max(0, w-stringWidth(line))))
	return ansiCut(line, w)
}

func optionWindow(values []string, current string, room int) (int, int) {
	if len(values) == 0 || room <= 0 {
		return 0, 0
	}
	at := 0
	for i, value := range values {
		if value == current {
			at = i
			break
		}
	}
	lo, hi := at, at+1
	width := stringWidth(values[at])
	for lo > 0 || hi < len(values) {
		changed := false
		if lo > 0 {
			n := stringWidth(values[lo-1]) + 2
			if width+n <= room {
				lo--
				width += n
				changed = true
			}
		}
		if hi < len(values) {
			n := stringWidth(values[hi]) + 2
			if width+n <= room {
				hi++
				width += n
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return lo, hi
}

func (m *Model) configOptions(values []string, current string, room int) string {
	lo, hi := optionWindow(values, current, room)
	var out []string
	for _, value := range values[lo:hi] {
		if value == current {
			out = append(out, m.configSelectedValue(value))
		} else {
			out = append(out, m.control(value))
		}
	}
	return strings.Join(out, "  ")
}

func (m *Model) configSettingRow(d *dialog, setting configSetting, w int) string {
	model, thinking, reasoning := m.configCurrent(d)
	selected := d.config.selected == setting
	marker := "  "
	if selected {
		marker = paint(x.Accent, "› ")
	}
	labelWidth := 17
	if w < 56 {
		labelWidth = 12
	}
	label := ""
	value := ""
	switch setting {
	case configThinking:
		label = "thinking"
		value = m.configOptions(m.configLevels(d, model, thinking), thinking, max(1, w-labelWidth-2))
	case configModelSetting:
		label = "model"
		value = modelRef(model)
		hint := paint(x.Muted, "  ↵ pick")
		if stringWidth(value)+stringWidth(hint) <= max(1, w-labelWidth) {
			value += hint
		}
	case configReasoning:
		label = "reasoning"
		current := "folded"
		if reasoning {
			current = "expanded"
		}
		value = m.configOptions([]string{"folded", "expanded"}, current, max(1, w-labelWidth-2))
	}
	styledLabel := m.control(label)
	if selected {
		styledLabel = "\x1b[1m" + styledLabel + "\x1b[22m"
	}
	line := marker + fit(styledLabel, labelWidth-2) + ansi.Truncate(value, max(1, w-labelWidth), "…")
	if selected && m.theme.Fills {
		line = fill(fit(line, w), blendPalette(m.palette.background, m.palette.colors[4], .15))
	}
	return line
}

func (m *Model) configDescription(d *dialog) string {
	if d.config.scope == configDefault {
		switch d.config.selected {
		case configThinking:
			return "Thinking effort for newly created threads. Higher levels may cost more."
		case configModelSetting:
			return "Model used for newly created threads."
		case configReasoning:
			return "Show reasoning text by default without expanding tool output."
		}
	}
	switch d.config.selected {
	case configThinking:
		return "How hard the agent thinks in this thread. Higher levels may cost more."
	case configModelSetting:
		return "Model used by this thread."
	case configReasoning:
		return "Show reasoning text in this thread without expanding tool output."
	}
	return ""
}

func (m *Model) configHints(d *dialog, w int) string {
	full := m.keyHint("↑↓", "move") + "  " + m.keyHint("←→", "change, applies now") + "  " + m.keyHint("tab", "scope") +
		"  " + m.keyHint("/", "models") + "  " + m.keyHint("u", "undo") + "  " + m.keyHint("esc", "close")
	compact := m.keyCap("↑↓") + "  " + m.keyCap("←→") + "  " + m.keyCap("tab") + "  " + m.keyCap("/") + "  " + m.keyCap("u") + "  " + m.keyCap("esc")
	return "  " + m.hintRow(max(1, w-2), full, compact, "")
}

func (m *Model) configDockRows(d *dialog, w, n int) ([]string, []int, *tea.Cursor) {
	if d.config.pickingModel {
		return m.configModelRows(d, w, n)
	}
	var rows []string
	var targets []int
	add := func(row string, target int) {
		if len(rows) < n {
			rows, targets = append(rows, row), append(targets, target)
		}
	}
	add(m.configHeader(d, w), -1)
	if n >= 12 {
		add("", -1)
		add(m.configSettingRow(d, configThinking, w), int(configThinking))
		add("", -1)
		add(m.configSettingRow(d, configModelSetting, w), int(configModelSetting))
		add("", -1)
		add(m.configSettingRow(d, configReasoning, w), int(configReasoning))
		add("", -1)
		add("", -1)
		add("  "+m.control(ansi.Truncate(m.configDescription(d), max(1, w-2), "…")), -1)
		add("", -1)
		add(m.configHints(d, w), -1)
		return rows, targets, nil
	}
	add(m.configSettingRow(d, configThinking, w), int(configThinking))
	add(m.configSettingRow(d, configModelSetting, w), int(configModelSetting))
	add(m.configSettingRow(d, configReasoning, w), int(configReasoning))
	if n >= 7 {
		add("", -1)
	}
	if n >= 6 {
		add("  "+m.control(ansi.Truncate(m.configDescription(d), max(1, w-2), "…")), -1)
	}
	if n >= 8 {
		add("", -1)
	}
	if n >= 5 {
		add(m.configHints(d, w), -1)
	}
	return rows, targets, nil
}

func (m *Model) configModelRows(d *dialog, w, n int) ([]string, []int, *tea.Cursor) {
	var rows []string
	var targets []int
	add := func(row string, target int) {
		if len(rows) < n {
			rows, targets = append(rows, row), append(targets, target)
		}
	}
	add(m.configModelHeader(d, w), -1)
	if n >= 7 {
		add("", -1)
	}
	queryRow := len(rows)
	input, cursorX := m.paletteInput(d.query, w)
	add(input, -1)
	footer := n >= 4
	budget := n - len(rows)
	if footer {
		budget--
	}
	spacious := n >= 10
	if spacious {
		budget = max(1, budget/2) // reserve a blank before the footer; choices use alternating rows
	}
	count := len(d.filtered)
	lo := min(max(0, d.selected-budget+1), max(0, count-budget))
	hi := min(lo+budget, count)
	for i := lo; i < hi; i++ {
		choice := d.choices[d.filtered[i]]
		model := d.modelSearch.models[choice.ID]
		prefix := "  "
		if i == d.selected {
			prefix = paint(x.Accent, "› ")
		}
		ref := modelRef(model.Model)
		detail := model.Label
		if model.Current {
			detail = "current · " + detail
		}
		line := prefix + ref
		if detail != "" {
			tail := m.control(ansi.Truncate(detail, max(1, w/3), "…"))
			line = ansi.Truncate(line, max(1, w-stringWidth(tail)-2), "…") + "  " + tail
		}
		if i == d.selected && m.theme.Fills {
			line = fill(fit(line, w), blendPalette(m.palette.background, m.palette.colors[4], .15))
		}
		add(line, i)
		if spacious && i+1 < hi {
			add("", -1)
		}
	}
	if count == 0 && budget > 0 {
		status := "No matches"
		if d.modelSearch.cancel != nil {
			status = "Searching models…"
		} else if d.modelSearch.failed {
			status = "Model search failed · edit to retry"
		}
		add(m.control("  "+status), -1)
	}
	if footer && n-len(rows) >= 2 {
		add("", -1)
	}
	if footer {
		full := m.keyHint("enter", "use") + "  " + m.keyHint("ctrl+d", "make default") + "  " + m.keyHint("esc", "back to settings")
		compact := m.keyCap("enter") + "  " + m.keyCap("ctrl+d") + "  " + m.keyCap("esc")
		add(m.hintRow(w, full, compact, fmt.Sprintf("%d models", count)), -1)
	}
	return rows, targets, tea.NewCursor(cursorX, queryRow)
}
