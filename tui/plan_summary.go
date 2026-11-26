package tui

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

// This is a display projection only. Older entries join step titles with
// commas, and titles can contain commas. Never guess a count for those rows.
func planRunTranscript(text string, width int) x.Block {
	heading, titles, legacy := strings.Cut(text, "\nSelected: ")
	heading = oneLine(heading)
	if legacy && strings.TrimSpace(titles) != "" && !strings.ContainsRune(titles, ',') {
		heading += " · 1 step"
	}
	if cut := strings.LastIndex(heading, " · "); cut >= 0 {
		tail := heading[cut:]
		fields := strings.Fields(strings.TrimPrefix(tail, " · "))
		if len(fields) == 2 && (fields[1] == "step" || fields[1] == "steps") {
			if count, err := strconv.Atoi(fields[0]); err == nil && count > 0 && width-stringWidth(tail) >= 9 {
				heading = ansi.Truncate(heading[:cut], width-stringWidth(tail), "…") + tail
			}
		}
	}
	return activityRow(x.Line{{Text: heading, Tone: x.Muted}}, "", width)
}
