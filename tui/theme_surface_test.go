package tui

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestMenuSurfacesPreserveGeometry(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, base := range []themeBase{baseDark, baseLight} {
		t.Run(string(base), func(t *testing.T) {
			m, s := demo(t, Options{ReducedMotion: true, Fills: FillOn})
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			bg, fg := color.Color(color.NRGBA{R: 30, G: 30, B: 46, A: 255}), color.White
			if base == baseLight {
				bg, fg = color.White, color.Black
			}
			m.Update(tea.BackgroundColorMsg{Color: bg})
			m.Update(tea.ForegroundColorMsg{Color: fg})
			m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
			m.editor.set("Keep this draft")
			planPickerFixture(m, s)
			var want string
			for _, mode := range []surfaceMode{surfaceTransparent, surfaceTinted, surfaceSolid} {
				theme, err := decodeTheme("fixture", []byte(fmt.Sprintf(`{"version":2,"base":%q,"surface":%q,"colors":{"accent":"#7676ff","warn":"#aa8800"}}`, base, mode)))
				if err != nil {
					t.Fatal(err)
				}
				m.applyTheme(theme)
				body := assertFrame(t, m)
				plain := ansi.Strip(body)
				if want == "" {
					want = plain
				} else if plain != want {
					t.Fatalf("%s changed the panel geometry", mode)
				}
				panel := m.frame.rows[m.frame.dialogY]
				if (mode != surfaceTransparent) != strings.HasPrefix(panel, "\x1b[48;2;") ||
					m.frame.dialogY+m.frame.dialogHeight != m.frame.ruleY {
					t.Fatalf("%s didn't use its surface at the dock: %q", mode, panel)
				}
				if mode == surfaceTransparent && strings.Contains(body, "\x1b[48;") {
					t.Fatal("transparent surface painted a background")
				}
			}
		})
	}
}

func TestThemeAdjustsLowContrastAndReportsOnce(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	m, _ := demo(t, Options{Fills: FillOff})
	theme, err := decodeTheme("fixture", []byte(`{"version":2,"base":"auto","surface":"transparent","colors":{"accent":"#121212","ok":"#121212","warn":"#121212","err":"#121212"}}`))
	if err != nil {
		t.Fatal(err)
	}
	m.applyTheme(theme)
	m.started = true
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m.Update(tea.BackgroundColorMsg{Color: color.Black})
	for _, tone := range []x.Tone{x.Accent, x.Good, x.Warning, x.Danger} {
		if c := m.palette.swatch(roleForTone(tone)); c == nil || contrastRatio(c, color.Black) < 4.5 {
			t.Fatalf("%s is below 4.5:1", tone)
		}
	}
	if !strings.Contains(m.notice.text, "Raised interface contrast") {
		t.Fatal("adjustment was not reported")
	}
	m.notice = noticeState{}
	m.Update(tea.BackgroundColorMsg{Color: color.Black})
	if m.notice.text != "" {
		t.Fatal("contrast adjustment was reported again")
	}
}
