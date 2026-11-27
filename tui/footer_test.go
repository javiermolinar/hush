package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestFooterGoldens(t *testing.T) {
	m, state := demo(t, Options{Fills: FillOff, ReducedMotion: true, SegmentOrder: map[string]int{"fixture/footer:extra": 4}, Extensions: []x.Registration{{
		ID: "fixture/footer", APIVersion: x.APIVersion, Segments: []x.Segment{{Key: "extra", Priority: 1, Slot: x.StatusLeft, Order: 80, Render: func(x.RenderContext) x.Line {
			return x.Line{{Text: "inbox 2", Tone: x.Accent}}
		}}},
	}}})
	state.info.Status, state.info.Title, state.info.Branch = x.Running, "Working thread", "agent/1"
	state.info.Thinking = "high"
	state.info.Model = sdk.Model{Provider: "test", ModelID: "model-v1"}
	state.info.ContextUse = x.ContextUse{Known: true, Tokens: 6400, Window: 10000}
	state.info.ContextThreshold = 8000
	state.info.Usage = sdk.Usage{TotalTokens: 45000, CostUSD: 1.23}
	for _, id := range []sdk.ID{"2", "3", "4", "5"} {
		m.ensureThread(id)
		m.openedTab(id)
	}
	m.threads["2"].info.Status = x.Parked
	m.threads["2"].info.Forked = true
	m.threads["3"].info.Status = x.Done
	m.threads["4"].info.Status = x.Stopped
	m.threads["5"].info.Status = x.Scheduled
	m.Update(tea.WindowSizeMsg{Width: 108, Height: 18})
	m.editor.set("Keep this draft")
	golden(t, "footer-wide", m)
	v := m.View()
	if v.ProgressBar == nil || !strings.HasPrefix(v.WindowTitle, "(1)") || !strings.Contains(v.Content, "inbox 2") || !strings.Contains(v.Content, "80%") {
		t.Fatalf("missing live or extension chrome: %q", v.Content)
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 16})
	m.editor.set("first prompt line\nsecond prompt line\nthird prompt line")
	golden(t, "footer-narrow", m)
	rows := strings.Split(ansi.Strip(m.View().Content), "\n")
	if !strings.Contains(rows[len(rows)-1], "Working thread") || !strings.Contains(rows[len(rows)-1], "+") ||
		!strings.Contains(rows[m.frame.ruleY], "esc stop") || strings.Contains(rows[m.frame.ruleY], "alt+enter") ||
		strings.Contains(strings.Join(rows[m.frame.ruleY+1:len(rows)-2], "\n"), "esc stop") {
		t.Fatal("tabs/overflow or rule hints regression")
	}
	m.editor.set("")
	m.Update(tea.WindowSizeMsg{Width: 88, Height: 15})
	press(m, 'x', tea.ModCtrl)
	golden(t, "footer-leader", m)
	m.leader = ""
	m.connection = x.Connecting
	state.observed = true
	golden(t, "footer-connection", m)
}

func TestFooterUsageDetails(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 21})
	s.info.Workspace, s.info.Branch, s.info.Model.ModelID = "/work/hush", "main", "test-model"
	s.info.ContextUse = x.ContextUse{Known: true, Tokens: 26_000, Window: 100_000}
	s.info.Usage = sdk.Usage{TotalTokens: 16_000_000, CostUSD: 5.48}
	m.editor.set("keep the draft")
	assertFrame(t, m)
	y := m.height - 2
	line := ansi.Strip(m.frame.rows[y])
	if tab := ansi.Strip(m.frame.rows[y+1]); !strings.Contains(tab, "⎇ main") {
		t.Fatalf("missing branch on thread tab: %q", tab)
	}
	if rule := ansi.Strip(m.frame.rows[m.frame.ruleY]); !strings.Contains(rule, "test-model") {
		t.Fatalf("missing model on the rule: %q", rule)
	}
	for _, want := range []string{"Repo hush", "ctx ", "26%", "$5.48"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in status %q", want, line)
		}
	}
	if strings.Contains(line, "tok") || strings.Contains(line, "16.0M") {
		t.Fatalf("cumulative tokens should be in details, not the status: %q", line)
	}
	col := stringWidth(line[:strings.Index(line, "$5.48")])
	m.Update(tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: col, Y: y}))
	frame := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"Usage details", "Total tokens: 16000000", "Current context: 26000 of 100000 tokens", "keep the draft"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	if got := m.frame.dialogY + m.frame.dialogHeight; got != m.frame.ruleY {
		t.Fatalf("usage panel bottom = %d, want input rule %d", got, m.frame.ruleY)
	}
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != nil {
		t.Fatal("usage dialog did not close")
	}
}

func TestFitTitleAtWordBoundaries(t *testing.T) {
	for _, c := range []struct {
		text, want string
		width      int
	}{
		{"tab-click bug fixes", "tab-click bug…", 16},
		{"skills loading", "skills loading", 14},
		{"界界界 tester", "界界界…", 7},
		{"oneword", "…", 5},
	} {
		if got := fitTitle(c.text, c.width); got != c.want || stringWidth(got) > c.width {
			t.Fatalf("fitTitle(%q, %d) = %q, want %q", c.text, c.width, got, c.want)
		}
	}
}

func TestOpenThreadTabNavigationGolden(t *testing.T) {
	m, first := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	first.info.Title = "First: Reviewing a large workspace change"
	thought := entry(3, "assistant", "")
	thought.Messages[0].Blocks = []sdk.Block{{Type: "thinking", Text: "Visible thought"}}
	first.merge([]sdk.Entry{thought})
	second := m.ensureThread("2")
	second.info.Title, second.draft.text = "Second: Tracking a long build configuration change", "second draft"
	for _, id := range []sdk.ID{"2", "3", "4"} {
		m.ensureThread(id)
		m.openedTab(id)
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	assertFrame(t, m)
	row := ansi.Strip(m.frame.rows[m.height-1])
	if !strings.Contains(row, "+") || strings.Contains(row, "Second:") || m.frame.toggleThought == "" {
		t.Fatalf("fixture needs a hidden next tab and a visible thought: %q", row)
	}
	check := func(want sdk.ID) {
		t.Helper()
		if m.active != want {
			t.Fatalf("active thread = %q, want %q", m.active, want)
		}
	}
	press(m, tea.KeyTab, 0)
	check("2")
	if m.editor.value() != "second draft" || first.entries["3"].reasoningDetails(first) {
		t.Fatal("Tab did not restore the hidden tab's draft, or it expanded the old thought")
	}
	m.editor.set("updated second draft")
	for _, want := range []sdk.ID{"3", "4", "1"} {
		press(m, tea.KeyTab, 0)
		check(want)
	}
	press(m, 'x', tea.ModCtrl)
	golden(t, "footer-thread-navigation", m)
	press(m, tea.KeyLeft, 0)
	check("4")
	for _, want := range []sdk.ID{"3", "2"} {
		press(m, 'x', tea.ModCtrl)
		press(m, tea.KeyLeft, 0)
		check(want)
	}
	if m.editor.value() != "updated second draft" {
		t.Fatal("cycling back did not restore the edited draft")
	}
	press(m, 'x', tea.ModCtrl)
	press(m, tea.KeyRight, 0)
	check("3")
	press(m, 'x', tea.ModCtrl)
	press(m, '[', 0)
	check("2")
}

func TestThreadTabKeepsFileCompletionAndShiftTabThinking(t *testing.T) {
	m, _ := demo(t, Options{Services: MenuServices{
		Files: func(context.Context, x.ListFiles) ([]x.File, string, error) {
			return []x.File{{Path: "main.go"}}, "", nil
		},
		Models: func(context.Context) ([]x.CatalogModel, error) { return nil, nil },
	}})
	m.ensureThread("2")
	m.openedTab("2")
	m.editor.set("Read @ma")
	load := m.refreshFilePicker()
	press(m, tea.KeyTab, 0)
	if m.active != "1" || m.editor.value() != "Read @ma" {
		t.Fatal("Tab switched threads while file completion was loading")
	}
	m.finishWork(reply[workMsg](t, load))
	press(m, tea.KeyTab, 0)
	if m.active != "1" || m.editor.value() != "Read @main.go " {
		t.Fatalf("Tab did not complete the file: thread %q, draft %q", m.active, m.editor.value())
	}
	press(m, tea.KeyTab, tea.ModShift)
	if m.active != "1" || m.sheet == nil || m.sheet.title != "Thinking level" {
		t.Fatal("Shift+Tab did not open the thinking picker in the current thread")
	}
}

func TestMouseSwitchesTabs(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff})
	m.ensureThread("2").info.Title = "Second thread"
	m.openedTab("2")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.View()
	for y, hits := range m.frame.hits {
		for _, hit := range hits {
			if hit.action.ID != activateTabID || hit.action.Target != "2" {
				continue
			}
			p := tea.Mouse{X: hit.first, Y: y, Button: tea.MouseLeft}
			m.Update(tea.MouseClickMsg(p))
			if m.active != "2" {
				t.Fatal("click on second tab did not switch threads")
			}
			return
		}
	}
	t.Fatal("no mouse hit on second tab")
}

func TestMouseOpensTabOverflow(t *testing.T) {
	m, first := demo(t, Options{Fills: FillOff})
	m.backend = menuBackend{}
	first.info.Title = "❯ ./bin/hush stop hush: unsupported_capability (runtime): Runtime shutdown was not negotiated"
	m.ensureThread("2").info.Title = "there is a bug where other threads cannot be clicked with the mouse just using the /threads menu"
	m.ensureThread("3").info.Title = "changes in the terminal/pane"
	m.ensureThread("4").info.Title = "a fourth thread"
	for _, id := range []sdk.ID{"2", "3", "4"} {
		m.openedTab(id)
	}
	m.switchThread("2")
	m.Update(tea.WindowSizeMsg{Width: 79, Height: 43})
	m.View()
	if row := ansi.Strip(m.frame.rows[m.height-1]); !strings.Contains(row, "+2") {
		t.Fatalf("fixture did not overflow its tabs: %q", row)
	}
	for y, hits := range m.frame.hits {
		for _, hit := range hits {
			if hit.action.ID != switcherID {
				continue
			}
			_, cmd := m.Update(tea.MouseClickMsg(tea.Mouse{X: hit.first, Y: y, Button: tea.MouseLeft}))
			if cmd == nil {
				t.Fatal("overflow click produced no switcher command")
			}
			threadPickerStep(t, m, cmd)
			if m.sheet == nil || m.sheet.threadPicker == nil || len(m.sheet.choices) == 0 {
				t.Fatal("overflow click did not open the thread switcher")
			}
			return
		}
	}
	t.Fatal("no mouse hit on tab overflow")
}

func TestMouseSwitchesTabsWithSheet(t *testing.T) {
	m, first := demo(t, Options{Fills: FillOff, Intro: &IntroOptions{}})
	first.info.Title = "最初のスレッド"
	second := m.ensureThread("2")
	second.info.Title, second.draft.text = "Second thread", "second draft"
	m.openedTab("2")
	m.editor.set("first draft")

	var dismissed int
	b := maxEffects
	route := m.route(m.active, coreID)
	m.openDialog(route, x.SelectDialog{Title: "Picker", Placement: x.Sheet,
		Choices: []x.Choice{{ID: "one", Label: "First choice"}}, Done: func(ctx x.Context, reply x.Reply[string]) x.Outcome {
			if reply.Accepted || ctx.Thread.ID != "1" {
				t.Fatalf("tab click did not dismiss the original sheet: %+v for thread %s", reply, ctx.Thread.ID)
			}
			dismissed++
			return nil
		}}, &b)
	sheet := m.sheet
	m.openPalette(route, &b)
	clickTab := func() {
		m.View()
		y := m.height - 1
		before, _, ok := strings.Cut(ansi.Strip(m.frame.rows[y]), second.info.Title)
		if !ok {
			t.Fatalf("second tab is not visible: %q", m.frame.rows[y])
		}
		m.Update(tea.MouseClickMsg(tea.Mouse{X: stringWidth(before), Y: y, Button: tea.MouseLeft}))
	}
	clickTab()
	if m.active != "1" || m.sheet != sheet || len(m.overlays) != 0 || dismissed != 0 {
		t.Fatal("click outside the overlay should dismiss only the overlay")
	}
	clickTab()
	if m.active != "2" || m.sheet != nil || dismissed != 1 {
		t.Fatal("click on a visible tab should switch threads and dismiss the sheet")
	}
	if first.draft.text != "first draft" || m.editor.value() != "second draft" {
		t.Fatal("click did not preserve each thread's draft")
	}
}

func TestFooterSelectsPriorityPrefixAndRetainsHits(t *testing.T) {
	hiddenCalls := 0
	item := func(key, title string, slot x.SegmentSlot, order, priority int) x.Segment {
		return x.Segment{Key: key, Slot: slot, Order: order, Priority: priority, Render: func(x.RenderContext) x.Line {
			if key == "hidden" {
				hiddenCalls++
			}
			return x.Line{{Text: title, Action: x.ActionRef{ID: paletteID}}}
		}}
	}
	m, state := demo(t, Options{Extensions: []x.Registration{{ID: "fixture/footer", APIVersion: x.APIVersion, Segments: []x.Segment{
		item("right", "right", x.StatusRight, 0, 60),
		item("left", "界A", x.StatusLeft, 80, 60),
		item("overflow", strings.Repeat("x", 20), x.StatusLeft, 100, 59),
		item("hidden", "lo", x.StatusRight, 1, 58),
	}}}})
	state.info.Workspace = ""
	m.openedTab("2") // no single-tab label to compete with the fixture
	state.status["fixture/status"] = x.SetStatus{Text: "ok", Priority: 62}
	row := m.segmentRow(state, 20, x.StatusLeft)
	if want := "ok · 界A       right"; row.text != want || hiddenCalls != 0 {
		t.Fatalf("wanted %q, got %q; %d evicted renderer calls", want, row.text, hiddenCalls)
	}
	if hits := row.hits; len(hits) != 2 || hits[0].first != 5 || hits[0].last != 8 || hits[1].first != 15 || hits[1].last != 20 {
		t.Fatalf("incorrect hit cells: %+v", hits)
	}
	if row := m.segmentRow(state, 13, x.StatusLeft); row.text != "ok · 界A" {
		t.Fatalf("missing two-cell minimum gap or reversed tie order: %q", row.text)
	}
}

type footerReportBackend struct{ introTestBackend }

func (footerReportBackend) Intro(context.Context, sdk.IntroScope) (sdk.IntroReport, error) {
	return sdk.IntroReport{}, sdk.ErrProtocol
}

func (footerReportBackend) WatchIntro(context.Context, sdk.IntroScope) <-chan sdk.IntroUpdate {
	return nil // tests deliver observations without running the transport
}

func TestPinnedFooterOutlivesIntro(t *testing.T) {
	m, _ := introFixture(t, Options{Fills: FillOff, ReducedMotion: true})
	m.backend = footerReportBackend{}
	m.connection = x.Connected
	m.beginBackendReport()
	lease := m.reportFeed
	report := IntroReport{Workspace: "/work/hush", Model: &sdk.Model{Provider: "fixture", ModelID: "gpt-6.2-sol"},
		Thinking: "max", Context: &IntroContext{Window: 500_000, RenderedBytes: 64_000}}
	publish := func(report IntroReport) {
		t.Helper()
		m.Update(backendReportMsg{token: lease.token, owner: lease.owner, ok: true, update: IntroUpdate{Report: report}})
	}
	publish(report)
	status := func(expect ...string) string {
		t.Helper()
		frame := assertFrame(t, m)
		y := m.height - 1
		if len(m.tabs) > 0 || m.active.Valid() {
			y-- // the active thread has a tab even when it is the only one
		}
		line := ansi.Strip(m.frame.rows[y] + "\n" + m.frame.rows[m.frame.ruleY])
		for _, s := range expect {
			if !strings.Contains(line, s) {
				t.Fatalf("missing %q in bottom bar:\n%s", s, ansi.Strip(frame))
			}
		}
		return line
	}
	status("hush", "gpt-6.2-sol max", "~3%")
	m.editor.set("first prompt")
	cmd := m.sendPrompt(sdk.Steer)
	m.finishWork(cmd().(workMsg))
	tstate := m.threads[m.active]
	if !tstate.intro.frozen || lease.ctx.Err() != nil {
		t.Fatal("freezing the block ended the live footer feed")
	}
	longTranscript(t, m, tstate)
	report.Context = &IntroContext{Window: 500_000, ReportedTokens: ptr(uint64(190_000))}
	publish(report)
	footer := status("hush", "gpt-6.2-sol max", "38%")
	if strings.Contains(footer, "~38%") || m.frame.top.id == introEntry {
		t.Fatal("footer still shows an estimate or the intro did not scroll away")
	}
	press(m, tea.KeyHome, tea.ModCtrl)
	status("38%")
	if m.frame.top.id != introEntry || !strings.Contains(ansi.Strip(m.View().Content), "16k of 500k (estimate)") {
		t.Fatal("live footer modified the frozen intro snapshot")
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	status("hush", "gpt-6.2-sol", "ctx 38%")
	m.editor.set("/")
	status("hush", "gpt-6.2-sol", "ctx 38%")
	if m.frame.slashEnd != m.frame.ruleY {
		t.Fatal("slash rows are not docked above the rule")
	}
	budget := maxEffects
	m.openDialog(m.route(m.active, coreID), x.SelectDialog{Title: "Select", Placement: x.Sheet,
		Choices: []x.Choice{{ID: "one", Label: "First"}}}, &budget)
	status("hush", "gpt-6.2-sol", "ctx 38%")
	if m.frame.dialogY+m.frame.dialogHeight != m.frame.ruleY {
		t.Fatal("sheet is not docked to the input rule")
	}
	m.switchThread("2")
	if lease.ctx.Err() == nil || m.reportFeed.token == lease.token {
		t.Fatal("switch did not replace the report lease")
	}
	publish(report) // delivery from the cancelled lease must not change the new tab
	two := m.threads["2"]
	two.info.Model = sdk.Model{Provider: "fixture", ModelID: "second"}
	two.info.Workspace = "/work/other"
	if line := status("other", "second"); strings.Contains(line, "38%") {
		t.Fatal("context leaked from a different thread")
	}
	lease = m.reportFeed
	report.Workspace = two.info.Workspace
	publish(report)
	if line := status("other", "second"); strings.Contains(line, "38%") {
		t.Fatal("context from the prior model was displayed")
	}
	report.Model = &two.info.Model
	publish(report)
	status("other", "second", "ctx 38%")
}

func TestFeedChromeTransitions(t *testing.T) {
	m, tab := demo(t, Options{ReducedMotion: true, ProjectThread: func(s sdk.Snapshot) ThreadMetadata {
		return ThreadMetadata{ContextUse: x.ContextUse{Known: true, Tokens: s.State.Usage.InputTokens, Window: 1000}}
	}})
	snapshot := sdk.Snapshot{State: sdk.State{Thread: sdk.Thread{ID: "1"}, Run: &sdk.Run{TaskID: "20"}, Usage: sdk.Usage{InputTokens: 900, TotalTokens: 35000}}}
	tab.info = m.threadInfo(tab, snapshot)
	if !strings.Contains(m.View().Content, "90%") || m.View().ProgressBar == nil {
		t.Fatal("live chrome missing")
	}
	snapshot.State.Run = nil
	snapshot.State.Usage.InputTokens = 100
	tab.info = m.threadInfo(tab, snapshot)
	if tab.info.Status != x.Done || !strings.Contains(m.View().Content, "10%") || m.View().ProgressBar != nil {
		t.Fatal("compacted chrome did not update")
	}
	m.focused = false
	if m.desktopStatus("1", x.Running, x.Done) == nil || m.desktopStatus("1", x.Done, x.Done) != nil {
		t.Fatal("incorrect notification transitions")
	}
	m.taken = interceptedKeys{"ctrl+g": true}
	if m.actionBinding(m.host.actions[m.host.byID[coreID+".edit"]]) != "ctrl+x e" {
		t.Fatal("terminal interception not reflected")
	}
}
