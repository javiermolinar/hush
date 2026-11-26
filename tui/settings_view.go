package tui

import (
	"strings"

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

// The theme picker still cycles named fill choices inline.
func (m *Model) configSelectedValue(value string) string { return strongText(value) }

func (m *Model) configDescription(d *dialog) string {
	if d.config.scope == configDefault {
		switch d.config.selected {
		case configThinking:
			return "Thinking effort for new threads · higher levels may cost more"
		case configModelSetting:
			return "Model for new threads"
		case configReasoning:
			return "Show reasoning in new threads without expanding tools"
		}
	}
	switch d.config.selected {
	case configThinking:
		return "Thinking effort for this thread · higher levels may cost more"
	case configModelSetting:
		return "Model for this thread"
	case configReasoning:
		return "Show reasoning in this thread without expanding tools"
	}
	return ""
}

func (m *Model) configScope(d *dialog) string {
	if d.config.scope == configDefault {
		return "Default"
	}
	return "This thread"
}

func (m *Model) configDockRows(d *dialog, w, n int) menuFrame {
	if d.config.pickingModel {
		return m.selectRows(d, w, n)
	}
	p := d.config
	model, thinking, reasoning := m.configCurrent(d)
	visibility := "Folded"
	if reasoning {
		visibility = "Expanded"
	}
	values := []struct{ title, value string }{
		{"Thinking", thinking}, {"Model", modelRef(model)}, {"Reasoning", visibility},
	}
	c := menuChrome{title: "Settings", scope: m.configScope(d), minBody: 3}
	var hints []string
	if p.pending {
		c.consequence = chromeLine(m.quiet("Wait for settings to save"))
	} else {
		hints = []string{"↑↓ move", "←→ change", "tab scope", "/ models", "u undo", d.zoomKey()}
		switch p.selected {
		case configModelSetting:
			c.consequence = chromeLine(m.menuAction("Choose a model", ""))
		case configThinking:
			next := cycleValue(m.configLevels(d, model, thinking), thinking, 1)
			if next == thinking || next == "" {
				c.consequence = chromeLine(m.quiet("No other thinking level for this model"))
			} else {
				c.consequence = chromeLine(m.menuAction("Set thinking", next+" · applies now"))
			}
		case configReasoning:
			label := "Show reasoning"
			if reasoning {
				label = "Fold reasoning"
			}
			c.consequence = chromeLine(m.menuAction(label, "Applies now"))
		}
	}
	c.keys = chromeLine(m.menuKeys(w, hints...))
	return m.menuShell(w, n, c, func(h int) menuFrame {
		var out menuFrame
		labels := min(14, w/2)
		lo, hi := menuWindow(int(p.selected), len(values), h)
		for i := lo; i < hi; i++ {
			value := values[i]
			focus := configSetting(i) == p.selected
			label, cursor := value.title, "  "
			if focus {
				cursor = m.paint(x.Accent, "› ")
				label = strongText(label)
			}
			text := fit(cursor+label, labels) + "  " + ansi.Truncate(oneLine(value.value), max(0, w-labels-2), "…")
			out.add(m.cursorRow(text, w, focus), i)
		}
		if h > len(out.rows) {
			out.add(m.quiet(ansi.Truncate(m.configDescription(d), w, "…")), -1)
		}
		return out
	})
}
