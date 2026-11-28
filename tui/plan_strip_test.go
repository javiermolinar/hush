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

func planStripText(t testing.TB, m *Model) string {
	t.Helper()
	assertFrame(t, m)
	if m.frame.planStrip == "" || m.frame.ruleY < 1 {
		t.Fatal("no visible plan strip")
	}
	return strings.TrimSpace(ansi.Strip(m.frame.rows[m.frame.ruleY-1]))
}

func testPlanStripFrameGeometry(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	installPlanStripFixture(m, s, 24)
	s.planRun.run.Steps[1].Title = strings.Repeat("Wide 界 ", 15) + "\x1b]52;c;not-clipboard\a"
	for _, size := range [][2]int{{12, 3}, {12, 4}, {20, 11}, {40, 12}, {80, 24}, {116, 24}, {180, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		assertFrame(t, m)
		if m.frame.planStrip == "" && size[1] > 3 {
			t.Fatalf("strip did not fit at %v", size)
		}
		press(m, tea.KeyUp, 0)
		frame := assertFrame(t, m)
		if strings.Contains(frame, "\x1b]52;") || strings.Contains(frame, "\a") {
			t.Fatal("plan text emitted terminal controls")
		}
		press(m, tea.KeyEscape, 0)
	}
	if got := planStripText(t, m); !strings.Contains(got, "1/24") || strings.Contains(got, "●●") {
		t.Fatal("long runs should collapse to a count: " + got)
	}
}

func TestPlanStripThemeFrames(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, theme := range []struct {
		name   string
		bg, fg color.Color
	}{
		{"latte", themeRGB(0xeff1f5), themeRGB(0x4c4f69)},
		{"mocha", themeRGB(0x1e1e2e), themeRGB(0xcdd6f4)},
	} {
		t.Run(theme.name, func(t *testing.T) {
			m, s := demo(t, Options{ReducedMotion: true, Fills: FillOn})
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			m.Update(tea.BackgroundColorMsg{Color: theme.bg})
			m.Update(tea.ForegroundColorMsg{Color: theme.fg})
			m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
			preset, _ := presetTheme("panther")
			m.applyTheme(preset)
			installPlanStripFixture(m, s, 14)
			for _, state := range []string{"running", "peek", "failed"} {
				if state == "peek" {
					press(m, tea.KeyUp, 0)
				} else if state == "failed" {
					press(m, tea.KeyEscape, 0)
					s.planRun.run.State, s.planRun.run.Failure = "failed", "The run turn did not finish."
				}
				frame := assertFrame(t, m)
				row := m.frame.rows[m.frame.ruleY-1]
				marks := []struct {
					tone x.Tone
					mark string
				}{{x.Good, "✓"}, {x.Warning, "⏸"}, {x.Warning, "●"}}
				if state == "failed" {
					marks = []struct {
						tone x.Tone
						mark string
					}{{x.Good, "✓"}, {x.Warning, "⏸"}, {x.Muted, "○"}, {x.Danger, "✗ The run turn did not finish."}}
				}
				for _, c := range marks {
					if !strings.Contains(row, m.paint(c.tone, c.mark)) {
						t.Errorf("%s frame lost the %s tone: %q", state, c.mark, row)
					}
				}
				if dir := os.Getenv("HUSH_RELEASE_FRAMES"); dir != "" {
					path := filepath.Join(dir, theme.name+"-plan-strip-"+state+".ansi")
					if err := os.WriteFile(path, []byte(frame+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			d := planPickerFixture(m, s)
			plan := &d.plan.snapshot.Plan
			plan.Steps[0].Status = "done"
			plan.Runs = []sdk.PlanActiveRun{{ID: "41", ThreadID: s.info.ID, StepIDs: []string{"restore"}}}
			for _, mode := range []surfaceMode{surfaceTransparent, surfaceTinted, surfaceSolid} {
				preset.surface = mode
				m.applyTheme(preset)
				assertFrame(t, m)
				panel := strings.Join(m.frame.rows[m.frame.dialogY:m.frame.ruleY], "\n")
				for _, want := range []string{m.paint(x.Good, "✓"), m.paint(x.Warning, "◐"), m.paint(x.Warning, "⏸"), m.paint(x.Muted, "○")} {
					if !strings.Contains(panel, want) {
						t.Errorf("%s picker is missing %q: %q", mode, want, panel)
					}
				}
			}
		})
	}
}
