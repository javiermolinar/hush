package tui

import (
	"fmt"
	"image/color"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func longTranscript(t testing.TB, m *Model, s *threadState) {
	t.Helper()
	rows := make([]sdk.Entry, 0, 2000)
	for i := 3; i < 2003; i++ {
		rows = append(rows, entry(i, "assistant", fmt.Sprintf("## Entry %d\n\nCheck `tui/perf_test.go` and [the docs](https://example.org).\n\n```go\nfunc test() { println(42) }\n```", i)))
	}
	s.merge(rows)
}

func TestFrameBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("timing is not measured in short/race runs")
	}
	for _, which := range []string{"idle", "streaming", "typing", "motion", "sheet", "model-catalog", "palette", "long-idle", "long-scroll", "long-resize", "folded-tool-results", "expanded-tool-results", "footer-overflow", "directory-tabs", "live-tool", "full-tool-output", "rewind-preview", "git-diff", "image-preview", "queued-inputs"} {
		t.Run(which, func(t *testing.T) {
			opts := Options{ReducedMotion: true}
			switch which {
			case "footer-overflow":
				// Maximum permitted registration and contribution count.
				for id := range 64 {
					reg := x.Registration{ID: fmt.Sprintf("fixture/footer-%d", id), APIVersion: x.APIVersion}
					for n := range 256 {
						reg.Segments = append(reg.Segments, x.Segment{Key: fmt.Sprintf("item-%d", n), Slot: x.SegmentSlot(n % 2),
							Priority: n % 8, Render: func(x.RenderContext) x.Line { return x.Line{{Text: "footer value"}} }})
					}
					opts.Extensions = append(opts.Extensions, reg)
				}
			case "live-tool":
				opts.Extensions = []x.Registration{{ID: "fixture/live", APIVersion: x.APIVersion, Tools: []x.ToolRenderer{{
					Name: "fixture", Render: func(_ x.RenderContext, tool x.Tool) (x.Content, bool) {
						return x.Content{{Lines: []x.Line{{{Text: tool.Output}}}}}, true
					},
				}}}}
			}
			m, s := demo(t, opts)
			m.Update(tea.WindowSizeMsg{Width: 140, Height: 42})
			if which == "live-tool" {
				s.info.Status = x.Running
				s.tools = []x.Tool{{CallID: "fixture", Name: "fixture", Status: "running", Live: true}}
			}
			if strings.HasPrefix(which, "long-") {
				longTranscript(t, m, s)
			}

			if which == "folded-tool-results" || which == "expanded-tool-results" {
				base := time.Unix(1000, 0)
				rows := make([]sdk.Entry, 2000)
				for i := range rows {
					rows[i] = toolResultEntry(i+3, "", "bash", "fixture result", i%100 == 0, base.Add(time.Duration(i)*time.Millisecond))
				}
				s.merge(rows)
				if which == "expanded-tool-results" {
					m.Update(tea.BackgroundColorMsg{Color: color.White})
					m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
					s.toggleBurst("3/0")
					s.toggleBurst("3/0/more")
				}
			}
			if which == "queued-inputs" {
				s.queue = make([]sdk.QueueItem, 2000)
				text := strings.Repeat("queued input\n界界 ", 128)
				for i := range s.queue {
					s.queue[i] = sdk.QueueItem{Mode: "steer", Text: text}
				}
			}
			if which == "image-preview" {
				longTranscript(t, m, s)
				m.images.mode, m.images.capable, s.expanded = ImageBlocks, true, true
				s.merge([]sdk.Entry{imageEntry(2003)})
				m.cacheImage(imageFetched{sdk.ImageRef{ThreadID: m.active, EntryID: "2003", BlockIndex: 1}, sampleImage(320, 180)})
			}
			if which == "git-diff" {
				longTranscript(t, m, s)
				enableGitDiffFixture(m, s)
				m.openGitDiff(m.route(m.active, coreID))
				d := m.focusedDialog()
				p := sdk.GitDiffPatch{}
				for i := range 1500 {
					p.Lines = append(p.Lines, sdk.GitDiffLine{Kind: "add", NewLine: i + 1, Text: "  // newly added line of code, with metadata"})
				}
				d.git.patch, d.git.view = true, newGitPatchView(p)
			}
			if which == "rewind-preview" {
				b := maxEffects
				m.openDialog(m.route(m.active, coreID), x.ReviewDialog{Title: "Review rewind",
					Text: strings.Repeat("#123456: Published release outside Git "+strings.Repeat("fixture location ", 8)+"\n", 128)}, &b)
			}
			if which == "full-tool-output" {
				result := strings.Repeat(strings.Repeat("界", 400)+" fixture\n", 100)
				s.merge([]sdk.Entry{toolResultEntry(3, "result", "fixture", result, false, time.Unix(100, 0))})
				m.openToolOutput(m.route(m.active, coreID), "3/0")
			}
			if which == "sheet" {
				b := maxEffects
				m.openDialog(m.route(m.active, coreID), x.SelectDialog{Placement: x.Sheet, Title: "Select", Choices: []x.Choice{{ID: "1", Label: "one"}, {ID: "2", Label: "two"}}}, &b)
			}
			var modelPage sdk.ModelCatalog
			if which == "model-catalog" {
				m.backend = modelSearchFixture{}
				m.Update(m.menuProbe()())
				m.Update(tea.WindowSizeMsg{Width: 46, Height: 20})
				m.Update(m.runAction(m.host.byID[modelPickerID], "")())
				modelPage = fullModelMenuFixture()
				if m.sheet == nil {
					t.Fatal("missing full native model sheet")
				}
			}
			if which == "palette" {
				b := maxEffects
				m.openPalette(m.route(m.active, coreID), &b)
			}
			var directory sdk.ThreadDirectoryUpdate
			if which == "directory-tabs" {
				for i := 1; i <= maxOpenTabs; i++ {
					id := sdk.ID(fmt.Sprint(i))
					m.ensureThread(id)
					m.openedTab(id)
					directory.Report.Threads = append(directory.Report.Threads, sdk.ThreadDirectoryRow{ID: id, Title: fmt.Sprintf("Fixture thread %d", i), Status: "idle"})
				}
				m.directoryIDs = slices.Clone(m.tabs)
				m.directoryFeed = feedLease{token: m.token(), ctx: m.ctx}
			}
			var ds []time.Duration
			var stream strings.Builder
			stream.WriteString("## Streaming\n\n```go\n")
			m.View()
			for i := range 220 {
				var msg tea.Msg
				switch which {
				case "streaming":
					stream.WriteString("func next() { println(42) }\n")
					s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{{Type: "text", Text: stream.String()}}})
					s.streamRevision++
				case "typing":
					msg = tea.KeyPressMsg{Code: 'a', Text: "a"}
				case "motion":
					msg = tea.MouseMotionMsg(tea.Mouse{X: 2 + i%10, Y: m.frame.transcriptHeight - 4})
				case "sheet", "git-diff":
					msg = tea.KeyPressMsg{Code: tea.KeyDown}
				case "model-catalog":
					switch i % 4 {
					case 0:
						msg = tea.KeyPressMsg{Code: 'o', Text: "o"}
					case 2:
						msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
					default:
						msg = modelSearchResult{token: m.sheet.token, catalog: modelPage}
					}
				case "palette":
					keys := [...]tea.KeyPressMsg{{Code: tea.KeyDown}, {Code: 'c', Text: "c"}, {Code: tea.KeyDown}, {Code: tea.KeyBackspace}}
					msg = keys[i%len(keys)]
				case "folded-tool-results", "expanded-tool-results", "footer-overflow":
					msg = tea.WindowSizeMsg{Width: 96 + i%3, Height: 24 + i%2}
				case "directory-tabs":
					directory.Report.Threads[i%maxOpenTabs].Status = []string{"idle", "running", "parked", "done", "scheduled"}[i%5]
					msg = directoryFeedMsg{token: m.directoryFeed.token, update: directory, ok: true}
				case "live-tool":
					s.tools[0].Output = fmt.Sprintf("working on item %d", i)
					msg = effectTick{at: time.Unix(1000, 0).Add(time.Duration(i) * 50 * time.Millisecond)}
				case "full-tool-output":
					msg = tea.KeyPressMsg{Code: []rune{tea.KeyRight, tea.KeyDown, tea.KeyLeft, tea.KeyUp}[i%4]}
				case "rewind-preview":
					if i%4 == 0 {
						msg = tea.WindowSizeMsg{Width: 90 + i%7, Height: 26}
					} else {
						msg = tea.KeyPressMsg{Code: tea.KeyDown}
					}
				case "long-scroll":
					msg = tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelUp})
				case "long-resize", "image-preview", "queued-inputs":
					msg = tea.WindowSizeMsg{Width: 120 + i%3, Height: 40 + i%2}
				}
				start := time.Now()
				if which == "motion" {
					msg = motionFilter(m, msg)
				}
				if msg != nil {
					m.Update(msg)
				}
				m.View()
				if i >= 20 {
					ds = append(ds, time.Since(start))
				}
			}
			slices.Sort(ds)
			p99 := ds[len(ds)*99/100-1]
			t.Logf("p99 %s", p99)
			if p99 > 2080*time.Microsecond {
				t.Fatalf("p99 %s exceeds 2.08 ms", p99)
			}
		})
	}
}

func TestIntroFrameBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("timing is not measured in short/race runs")
	}
	m, clock := introFixture(t, Options{Fills: FillOn})
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 50})
	applyIntroFixtureTheme(m)
	m.Update(introReportMsg{rich: true, update: IntroUpdate{Report: introFixtureReport(*clock)}})
	var samples []time.Duration
	for range 6 {
		a := &m.intro.motion
		a.start(*clock, true, false)
		m.syncIntroClock()
		m.View()
		for n := 0; a.tick != 0; n++ {
			*clock = clock.Add(introPeriod)
			start := time.Now()
			m.Update(introTick{a.tick, *clock})
			m.View()
			if n > 10 {
				samples = append(samples, time.Since(start))
			}
		}
	}
	slices.Sort(samples)
	p99 := samples[len(samples)*99/100-1]
	t.Logf("p99 %s", p99)
	if p99 >= 2*time.Millisecond {
		t.Fatalf("intro p99 %s exceeds 2 ms", p99)
	}
}

func BenchmarkFrame(b *testing.B) {
	b.Run("cold", func(b *testing.B) {
		m, s := demo(b, Options{ReducedMotion: true})
		longTranscript(b, m, s)
		b.ResetTimer()
		for range b.N {
			for _, id := range s.ids[len(s.ids)-64:] {
				s.entries[id].cache = blockCache{}
			}
			m.View()
		}
	})
	b.Run("resize", func(b *testing.B) {
		m, s := demo(b, Options{ReducedMotion: true})
		longTranscript(b, m, s)
		b.ResetTimer()
		for i := range b.N {
			m.Update(tea.WindowSizeMsg{Width: 40 + i%161, Height: 12 + i%49})
			m.View()
		}
	})
}
