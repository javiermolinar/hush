package tui

import (
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type typographyPalette struct {
	name   string
	bg, fg color.Color
}

var typographyPalettes = [...]typographyPalette{
	{"latte", themeRGB(0xeff1f5), themeRGB(0x4c4f69)},
	{"mocha", themeRGB(0x1e1e2e), themeRGB(0xcdd6f4)},
}

func setTypographyPalette(m *Model, p typographyPalette) {
	m.Update(tea.BackgroundColorMsg{Color: p.bg})
	m.Update(tea.ForegroundColorMsg{Color: p.fg})
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
}

func TestTypographyGolden(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, p := range typographyPalettes {
		t.Run(p.name, func(t *testing.T) {
			m := modelFor(t, Options{ThreadID: "1", Fills: FillOff, ReducedMotion: true})
			m.connection, m.localTime = x.Connected, time.UTC
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			setTypographyPalette(m, p)
			s := m.threads[m.active]
			s.info.Workspace = "/work/hush"
			start := time.Date(2026, 11, 23, 13, 24, 0, 0, time.UTC)
			end := start.Add(7*time.Minute + 54*time.Second)
			thought := entry(2, "assistant", "")
			thought.Messages[0].Blocks = []sdk.Block{{Type: "thinking", Text: "**Refining pending selection state**"}}
			s.merge([]sdk.Entry{
				entry(1, "user", "Check the saved profile."), thought,
				toolResultEntry(3, "edit", "edit", "Done", false, start),
				toolResultEntry(4, "check", "bash", "Failed", true, end),
				entry(5, "assistant", "The profile is **ready** to use."),
			})
			s.entries["2"].thought = 46 * time.Second
			m.now = end.Add(8 * time.Second)
			s.activity = activityState{task: "6", since: end}

			body := assertFrame(t, m)
			// Keep the styling in the snapshot. Escape bytes are visible in diffs.
			var lines []string
			for row := range strings.SplitSeq(body, "\n") {
				lines = append(lines, strings.ReplaceAll(strings.TrimRight(row, " "), "\x1b", "␛"))
			}
			got := strings.Join(lines, "\n") + "\n"
			path := filepath.Join("testdata", "typography-"+p.name+".txt")
			if *updateGoldens {
				if err := os.WriteFile(path, []byte(got), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if want, err := os.ReadFile(path); err != nil || got != string(want) {
				t.Fatalf("golden %s: %v\n--- got ---\n%s--- want ---\n%s", path, err, got, want)
			}
			if dir := os.Getenv("HUSH_RELEASE_FRAMES"); dir != "" {
				if err := os.WriteFile(filepath.Join(dir, "typography-"+p.name+".ansi"), []byte(body+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestExpandedReasoningNeverBold(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	m, s := demo(t, Options{Fills: FillOn, ReducedMotion: true})
	setTypographyPalette(m, typographyPalettes[1])
	m.palette.overrides[roleLink] = themeRGB(0x89b4fa)
	m.refreshTheme()
	const source = `# Refining pending selection state

**Strong words**, *other words* and [a deliberately wrapped link label](https://example.org).` + "\n\n```go\nconst x = 1 // note\n```\n\n| Group | State |\n| --- | --- |\n| One | ready |"
	thought := entry(3, "assistant", "")
	thought.Messages[0].Blocks = []sdk.Block{{Type: "thinking", Text: source}}
	s.merge([]sdk.Entry{thought})
	s.stream = detachStream(&sdk.Streaming{Blocks: thought.Messages[0].Blocks})
	s.streamRevision++
	press(m, 'o', tea.ModCtrl)
	for _, at := range []int{s.shownIndex("3"), len(s.shown)} {
		body := rowsText(m.blockRows(s, at, 28))
		if !strings.Contains(ansi.Strip(body), "Refining pending selection") || !strings.Contains(ansi.Strip(body), "Strong words") {
			t.Fatalf("expected expanded reasoning in block %d:\n%s", at, body)
		}
		want := m.palette.swatch(roleQuiet)
		linked := false
		for _, line := range uv.NewStyledString(body).Lines(ansi.GraphemeWidth) {
			for _, cell := range line {
				if cell.Content == " " || cell.Content == "" {
					continue
				}
				if cell.Style.Attrs != 0 || !sameRGB(cell.Style.Fg, want) {
					t.Fatalf("reasoning must stay Quiet; cell %q in block %d: %+v", cell.Content, at, cell.Style)
				}
				linked = linked || cell.Link.URL == "https://example.org"
			}
		}
		if !linked {
			t.Fatal("quiet reasoning lost link targets")
		}
	}
}

func sameRGB(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	r, g, b1, _ := a.RGBA()
	r2, g2, b2, _ := b.RGBA()
	return r == r2 && g == g2 && b1 == b2
}

func TestQuietContrast(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, p := range []typographyPalette{
		typographyPalettes[0], typographyPalettes[1],
		{"near-threshold", themeRGB(0x767676), color.Black},
		{"no-foreground-report", themeRGB(0x777777), nil},
		{"low-contrast-foreground", themeRGB(0x808080), themeRGB(0x707070)},
	} {
		t.Run(p.name, func(t *testing.T) {
			m, _ := demo(t, Options{Fills: FillOff})
			setTypographyPalette(m, p)
			m.palette.overrides[roleQuiet] = p.bg // deliberately unreadable theme color
			m.refreshTheme()
			cell := uv.NewStyledString(m.quiet("q")).Lines(ansi.GraphemeWidth)[0][0]
			if cell.Style.Attrs != 0 || cell.Style.Fg == nil || contrastRatio(cell.Style.Fg, p.bg) < 4.5 {
				t.Fatalf("Quiet isn't regular ink at 4.5:1 or better: %+v", cell.Style)
			}
			if p.fg != nil && contrastRatio(p.fg, p.bg) > 5 && contrastRatio(cell.Style.Fg, p.bg) >= contrastRatio(p.fg, p.bg) {
				t.Fatal("Quiet didn't dim relative to a legible default foreground")
			}
		})
	}
	m, _ := demo(t, Options{Fills: FillOff})
	if m.quiet("q") != "q" {
		t.Fatal("without a background report, fall back to regular foreground, not faint or guessed dim ink")
	}
	setTypographyPalette(m, typographyPalettes[1])
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.ANSI256})
	m.Update(uv.UnknownOscEvent("\x1b]4;8;rgb:88/88/88\x07"))
	if got := m.quiet("q"); got != "\x1b[90mq\x1b[39m" {
		t.Fatalf("measured ANSI dim slot should be used: got %q", got)
	}
	m.Update(uv.UnknownOscEvent("\x1b]4;8;rgb:3f/3f/3f\x07"))
	if got := m.quiet("q"); got != "q" {
		t.Fatalf("dim slot below 4.5:1 must fall back to regular: got %q", got)
	}
}

func TestFocusedMenuTitleTypography(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, tc := range []struct {
		name, focused, other string
		open                 func(*testing.T, *Model, *threadState)
	}{
		{"plan", "Save a profile", "Restore saved", func(_ *testing.T, m *Model, s *threadState) { planPickerFixture(m, s) }},
		{"threads", "Go TUI", "List display", func(t *testing.T, m *Model, _ *threadState) { openGroupedThreadPicker(t, m) }},
		{"changes", "src/auth/session.ts", "src/auth/lock.ts", func(_ *testing.T, m *Model, s *threadState) {
			enableGitDiffFixture(m, s)
			m.openGitDiff(m.route(m.active, coreID))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			setTypographyPalette(m, typographyPalettes[1])
			tc.open(t, m, s)
			assertFrame(t, m)
			f := m.frame
			for _, title := range []string{tc.focused, tc.other} {
				wantBold := title == tc.focused
				found := false
				for y, target := range f.dialogChoices {
					if target < 0 {
						continue
					}
					row := f.rows[f.dialogY+y]
					plain := ansi.Strip(row)
					i := strings.Index(plain, title)
					if i < 0 {
						continue
					}
					cells := uv.NewStyledString(row).Lines(ansi.GraphemeWidth)[0]
					at := stringWidth(plain[:i])
					for _, cell := range cells[at : at+stringWidth(title)] {
						if (cell.Style.Attrs&uv.AttrBold != 0) != wantBold || cell.Style.Fg != nil {
							t.Fatalf("%q: title must be full brightness and bold only at the cursor: %+v", title, cell.Style)
						}
					}
					if wantBold && !strings.Contains(plain[:i], "› ") {
						t.Fatal("focus relies on weight alone")
					}
					found = true
					break
				}
				if !found {
					t.Fatalf("missing menu row for %q:\n%s", title, ansi.Strip(strings.Join(f.rows, "\n")))
				}
			}
		})
	}
}
