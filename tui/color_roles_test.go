package tui

import (
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestPaletteRevisionRefreshesVisibleBlocksOnly(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 48, Height: 23})
	m.Update(tea.BackgroundColorMsg{Color: color.Black})
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	w := m.pane().width
	m.blockRows(s, 0, w)
	offscreen := s.entries["1"].cache.key
	var rows []sdk.Entry
	for i := 3; i < 501; i++ {
		rows = append(rows, entry(i, "assistant", "Earlier entry"))
	}
	const linkLabel = "A long wrapped link with a label that's wider than the terminal pane"
	rows = append(rows, entry(501, "assistant", "["+linkLabel+"](https://example.org/)"))
	s.merge(rows)
	s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{
		{Type: "text", Text: "Live response\n\n```go\nconst count = 12\n```"},
	}})
	s.streamRevision++
	before := assertFrame(t, m)
	linkCells := func() [][2]int {
		var cells [][2]int
		for y, row := range m.frame.rows[:m.frame.transcriptHeight] {
			for col := range stringWidth(row) {
				if v := linkAt(row, col); v == ";https://example.org/" {
					cells = append(cells, [2]int{y, col})
				}
			}
		}
		return cells
	}
	first := linkCells()
	if len(first) < 2 || first[0][0] == first[len(first)-1][0] || !strings.Contains(before, "\x1b[35mconst") {
		t.Fatal("fixture is missing the wrapped link or live syntax")
	}
	other, _ := demo(t, Options{Fills: FillOff})
	m.palette.overrides[roleLink] = color.NRGBA{R: 152, G: 177, B: 246, A: 255}
	m.palette.overrides[roleSyntaxKeyword] = color.NRGBA{R: 198, G: 154, B: 247, A: 255}
	m.refreshTheme()
	after := assertFrame(t, m)
	if ansi.Strip(before) != ansi.Strip(after) || !strings.Contains(after, "\x1b[38;2;198;154;247mconst") ||
		!strings.Contains(after, "\x1b[38;2;152;177;246m") {
		t.Fatalf("palette change altered geometry or did not recolor cached content:\n%s", after)
	}
	second := linkCells()
	if len(first) != len(second) {
		t.Fatal("palette change altered linked cells")
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("palette change moved linked cell %v to %v", first[i], second[i])
		}
	}
	if s.entries["1"].cache.key != offscreen || s.entries["3"].cache.rows != nil ||
		s.entries["501"].cache.key.theme != m.theme.Revision || s.liveCache.key.theme != m.theme.Revision {
		t.Fatal("palette refresh did not stay on the visible path")
	}
	if len(s.liveDocs) == 0 {
		t.Fatal("fixture did not exercise the live Markdown cache")
	}
	for _, stream := range s.liveDocs {
		if stream.theme != m.theme.Revision {
			t.Fatal("live Markdown retained an old palette")
		}
	}
	if other.paint(x.Accent, "accent") != "\x1b[34maccent\x1b[39m" || other.palette.roles[roleLink].ink.start != "" {
		t.Fatal("one instance's overrides changed another instance")
	}
}

func TestPaletteRevisionRepaintsIntro(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	m, _ := introFixture(t, Options{Fills: FillOn, ReducedMotion: true})
	applyIntroFixtureTheme(m)
	before := assertFrame(t, m)
	if ink := m.palette.roles[roleAccent].ink.start; !strings.HasPrefix(ink, "\x1b[38;2;") || !strings.Contains(m.frame.rows[0], ink) {
		t.Fatal("fixture did not render the ANSI-derived intro accent")
	}
	m.palette.overrides[roleAccent] = color.NRGBA{R: 72, G: 46, B: 157, A: 255}
	m.refreshTheme()
	m.introOnly = true // the previous frame's chrome is not reusable after a palette change
	after := assertFrame(t, m)
	if ansi.Strip(before) != ansi.Strip(after) || !strings.Contains(m.frame.rows[0], "\x1b[38;2;72;46;157m") ||
		m.frame.theme != m.theme.Revision || m.intro.memo.inkKey.theme != m.theme.Revision {
		t.Fatal("intro kept stale ink or changed geometry")
	}
}

func TestRexPaletteFrame(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "rex")
	m, s := demo(t, Options{Fills: FillAuto, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m.Update(tea.BackgroundColorMsg{Color: color.NRGBA{R: 30, G: 30, B: 46, A: 255}})
	m.Update(tea.ForegroundColorMsg{Color: color.NRGBA{R: 205, G: 214, B: 244, A: 255}})
	s.merge([]sdk.Entry{entry(3, "assistant", "See the [docs](https://example.org) and `app/main.go:12`.\n\n```go\nconst count = 12\n// ready\n```")})
	body := assertFrame(t, m)
	if m.theme.Fills || strings.Contains(body, "\x1b[48;") || !strings.Contains(body, "\x1b[35mconst") {
		t.Fatalf("no-fill fixture lost semantic roles or painted a fill:\n%s", body)
	}
	if dir := os.Getenv("HUSH_RELEASE_FRAMES"); dir != "" {
		path := filepath.Join(dir, "rex-compact-syntax.ansi")
		if err := os.WriteFile(path, []byte(body+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
