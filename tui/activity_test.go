package tui

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func toolCallEntry(id int, call, name, arguments string, at time.Time) sdk.Entry {
	row := entry(id, "assistant", "")
	row.Kind = "pi.assistant"
	row.Messages[0].TimestampMS = uint64(at.UnixMilli())
	row.Messages[0].Blocks = []sdk.Block{{Type: "omitted", Kind: "redactedThinking"}, {Type: "toolCall", ID: call, Name: name, Arguments: []byte(arguments)}}
	return row
}

func toolResultEntry(id int, call, name, output string, failed bool, at time.Time) sdk.Entry {
	row := entry(id, "toolResult", output)
	row.Kind = "pi.tool-result"
	msg := &row.Messages[0]
	msg.TimestampMS, msg.CallID, msg.Name, msg.IsError = uint64(at.UnixMilli()), call, name, ptr(failed)
	return row
}

func activityFixture(t testing.TB, options Options) (*Model, *threadState) {
	t.Helper()
	options.ThreadID = "1"
	m := modelFor(t, options)
	m.connection = x.Connected
	s := m.threads[m.active]
	s.info.Workspace = "/work/hush"
	s.info.Status = x.Running
	s.info.Model = sdk.Model{Provider: "local", ModelID: "fixture-model"}
	s.info.Thinking = "max"
	s.info.Usage = sdk.Usage{TotalTokens: 336700, CostUSD: 0.13}
	start := time.Date(2026, 11, 23, 13, 24, 0, 0, time.UTC)
	m.now = start.Add(80 * time.Second)
	s.activity = activityState{task: "500", phase: liveWaiting, since: m.now.Add(-8 * time.Second)}
	hi := entry(2, "assistant", "Hi. What can I help with?")
	hi.Messages[0].Blocks = append([]sdk.Block{{Type: "omitted", Kind: "redactedThinking"}}, hi.Messages[0].Blocks...)
	rows := []sdk.Entry{entry(1, "user", "hi"), hi, entry(3, "user", "Where do the 13.8k tokens come from?")}
	for i := range 14 {
		cmd := []string{"rg -n tokens docs/", "wc -c README.md", "git ls-files | wc -l"}[i%3]
		begin := start.Add(time.Duration(i*5) * time.Second)
		end := begin.Add(200 * time.Millisecond)
		output := "fixture output"
		if i == 0 {
			output = "fixture failed\n\nCommand exited with code 2"
		}
		if i == 13 {
			end = start.Add(72 * time.Second)
		}
		rows = append(rows, toolCallEntry(4+i*2, "call", "bash", fmt.Sprintf(`{"command":%q}`, cmd), begin),
			toolResultEntry(5+i*2, "call", "bash", output, i == 0, end))
	}
	s.merge(rows)
	return m, s
}

func TestActivityGoldens(t *testing.T) {
	m, s := activityFixture(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	golden(t, "activity-running", m)
	body := ansi.Strip(m.View().Content)
	if strings.Count(body, "▸ ran 14 · 1 failed · 1m 12s") != 1 || strings.Contains(body, "Thought") || !strings.Contains(body, "Waiting for model · 8s") {
		t.Fatalf("unexpected summary or live line:\n%s", body)
	}
	if !strings.Contains(m.View().Content, "\x1b[31m1 failed\x1b[39m") || strings.Contains(m.View().Content, "\x1b[34m▸") {
		t.Fatal("only failures should carry state color in a collapsed burst")
	}
	s.activity.task = ""
	s.info.Status = x.Done
	golden(t, "activity-finished", m)
	s.activity.task = "500"
	s.info.Status = x.Running
	if !clickActivity(m, "5/0") {
		t.Fatal("summary has no mouse target")
	}
	golden(t, "activity-expanded", m)
	if body := ansi.Strip(m.View().Content); !strings.Contains(body, "… 10 more") || !strings.Contains(body, "exit 2") {
		t.Fatal("expected four child rows")
	}
	press(m, tea.KeyEnter, 0)
	if got := s.entries["5"].burstViews[0].page; got != 1 {
		t.Fatalf("Enter didn't open the remaining results: %d", got)
	}
	press(m, 'o', tea.ModCtrl)
	if !s.expanded {
		t.Fatal("ctrl+o did not expand activity")
	}
	assertFrame(t, m)
	if !clickActivity(m, "5/0/0") || !strings.Contains(ansi.Strip(m.View().Content), "fixture failed") {
		t.Fatal("detail did not show the committed result")
	}
	s.activity.thinkAt = m.now.Add(-3 * time.Second)
	s.activity.thinking = true
	golden(t, "activity-thinking", m)
	s.activity.thinking = false
	s.stream = detachStream(&sdk.Streaming{Attempt: 2, Retry: &sdk.Retry{AtMS: uint64(m.now.Add(4 * time.Second).UnixMilli()), Detail: "model waiting"}})
	s.streamRevision++
	golden(t, "activity-retry", m)
	s.stream = x.Stream{}
	s.streamRevision++
	s.activity.task = ""
	s.activity.failure = sdk.Failure{TaskID: "22", Reason: "model_error", Detail: "provider failed"}
	golden(t, "activity-error", m)
	s.activity.failure = sdk.Failure{}
	s.activity.compacting = true
	s.merge([]sdk.Entry{{ID: "40", ThreadID: "1", Kind: "pi.compaction"}})
	golden(t, "activity-compaction", m)
}

func TestThinkingSpinnerCadence(t *testing.T) {
	type frame struct {
		ms   int
		line string
	}
	for _, mode := range []struct {
		name          string
		reducedMotion bool
		frames        []frame
	}{
		{"animated", false, []frame{
			{0, "◌ Thinking"}, {50, "◌ Thinking"}, {150, "◌ Thinking"}, {200, "◔ Thinking"},
			{400, "◑ Thinking"}, {600, "◕ Thinking"}, {800, "◌ Thinking"}, {1000, "◔ Thinking · 1s"},
		}},
		{"reduced-motion", true, []frame{{0, "◌ Thinking"}, {1000, "◌ Thinking · 1s"}}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			m, s := demo(t, Options{ReducedMotion: mode.reducedMotion})
			start := time.Unix(100, 0)
			m.now = start
			s.activity = activityState{thinking: true, thinkAt: start}
			if m.syncClock() == nil {
				t.Fatal("thinking did not start the effects clock")
			}
			for _, f := range mode.frames {
				if f.ms != 0 {
					m.Update(effectTick{token: m.clockPending, at: start.Add(time.Duration(f.ms) * time.Millisecond)})
				}
				m.View()
				label, _, _ := strings.Cut(ansi.Strip(m.frame.rows[m.frame.ruleY]), "─")
				if got := strings.TrimSpace(label); got != f.line {
					t.Fatalf("at %d ms: got %q, want %q", f.ms, got, f.line)
				}
			}
		})
	}
}

func TestLiveModelFailureRecovery(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	s.activity.failure = sdk.Failure{TaskID: "22", Reason: "model_error"}
	assertFrame(t, m)
	line := func() string { return ansi.Strip(m.frame.rows[m.frame.ruleY]) }
	if !strings.Contains(line(), "✗ no answer · model run failed") || strings.Contains(line(), "/model") || len(m.frame.hits[m.frame.ruleY]) != 0 {
		t.Fatalf("failure without provider details or catalog offered an unsupported action: %q", line())
	}
	m.options.Services.Models = func(context.Context) ([]x.CatalogModel, error) {
		return []x.CatalogModel{{Model: sdk.Model{Provider: "fixture", ModelID: "another-model"}}}, nil
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	assertFrame(t, m)
	if !strings.Contains(line(), "model run failed") || strings.Contains(line(), "/model") {
		t.Fatalf("narrow live row did not prioritize the failure: %q", line())
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	assertFrame(t, m)
	col := strings.Index(line(), "/model to switch")
	if col < 0 {
		t.Fatalf("no recovery action with a model catalog: %q", line())
	}
	y, col := m.frame.ruleY, stringWidth(line()[:col])
	click := tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: col, Y: y})
	_, cmd := m.Update(click)
	if cmd == nil || m.hoverAt(col, y) != "choose a model" {
		t.Fatal("model recovery hint has no selectable action")
	}
	m.Update(cmd())
	if m.sheet == nil || s.info.Model != (sdk.Model{}) || s.activity.failure.TaskID != "22" {
		t.Fatal("the model recovery action should only open the picker")
	}
}

func clickActivity(m *Model, target string) bool {
	for y, hits := range m.frame.hits {
		for _, h := range hits {
			if h.action.ID != expandRowID || h.action.Target != target {
				continue
			}
			click := tea.Mouse{Button: tea.MouseLeft, X: h.first, Y: y}
			m.mouse(tea.MouseClickMsg(click))
			m.mouse(tea.MouseReleaseMsg(click))
			return true
		}
	}
	return false
}

func TestBurstBackfillSplitsOnlyAtProse(t *testing.T) {
	_, s := demo(t, Options{})
	ref := time.Unix(100, 0)
	call := func(id int, n string) sdk.Entry {
		return toolCallEntry(id, "repeat", "bash", fmt.Sprintf(`{"command":%q}`, n), ref.Add(time.Duration(id)*time.Second))
	}
	result := func(id int) sdk.Entry {
		return toolResultEntry(id, "repeat", "bash", "ok", false, ref.Add(time.Duration(id)*time.Second))
	}
	s.merge([]sdk.Entry{result(4), call(7, "new"), result(8), call(10, "later"), result(11)})
	if got := len(s.entries["4"].bursts[0].children); got != 3 {
		t.Fatalf("known results=%d", got)
	}
	s.merge([]sdk.Entry{call(3, "older"), entry(5, "assistant", "This is the boundary.")})
	old, recent := s.entries["4"].bursts[0], s.entries["8"].bursts[0]
	if old == nil || recent == nil || len(old.children) != 1 || len(recent.children) != 2 {
		t.Fatal("backfill did not split the burst at prose")
	}
	if !strings.Contains(old.children[0].call.args, "older") || !strings.Contains(recent.children[0].call.args, "new") || !strings.Contains(recent.children[1].call.args, "later") {
		t.Fatal("argument association lost when a call ID was reused")
	}
	if s.entries["11"].visible || len(s.shown) >= len(s.ids) {
		t.Fatal("folded raw entries were not skipped by the display index")
	}
	s.merge([]sdk.Entry{call(12, "pending")})
	if got := len(recent.children); got != 2 {
		t.Fatalf("pending call counted as completed: %d", got)
	}
	s.merge([]sdk.Entry{entry(13, "user", "next turn"), result(14)})
	if len(recent.children) != 2 || s.entries["14"].bursts[0] == nil {
		t.Fatal("user turn did not close its burst")
	}
}

func TestExpandShowsOnlyReadableThoughts(t *testing.T) {
	m, s := demo(t, Options{})
	r := entry(3, "assistant", "")
	r.Messages[0].Blocks = []sdk.Block{{Type: "thinking", Text: "an actual thought"}}
	redacted := entry(4, "assistant", "")
	redacted.Messages[0].Blocks = []sdk.Block{{Type: "omitted", Kind: "redactedThinking"}}
	s.merge([]sdk.Entry{r, redacted})
	if len(s.shown) != 2 || len(s.detailShown) != 3 {
		t.Fatal("default view or details indexed the wrong block")
	}
	if strings.Contains(ansi.Strip(m.View().Content), "Thought") {
		t.Fatal("collapsed thought was displayed")
	}
	press(m, 'o', tea.ModCtrl)
	frame := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "an actual thought") || strings.Count(frame, "Thought") != 1 {
		t.Fatalf("expected only real thought details:\n%s", frame)
	}
}

func TestBurstUsesResultsAndAllowsExtensionNotes(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Extensions: []x.Registration{{
		ID: "fixture/activity", APIVersion: x.APIVersion, Activity: []x.Summarizer{{Key: "tests", Summarize: func(_ x.RenderContext, a x.Activity) (x.Line, bool) {
			if len(a.Items) == 2 {
				return x.Line{{Text: "fixture checks", Tone: x.Muted}}, true
			}
			return nil, false
		}}},
	}}})
	start := time.Unix(100, 0)
	s.merge([]sdk.Entry{toolResultEntry(3, "a", "read", "ok", false, start), toolResultEntry(4, "b", "bash", "error", true, start.Add(time.Second))})
	body := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, "read 1 · ran 1 · 1 failed · 1s · fixture checks") {
		t.Fatalf("result counts or extension note missing:\n%s", body)
	}
}

func TestBurstPreviewPrioritizesFailuresThenEdits(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	start := time.Unix(100, 0)
	add := func(i int, name, args, output string, failed bool) {
		call, at := fmt.Sprint(i), start.Add(time.Duration(i)*time.Second)
		s.merge([]sdk.Entry{toolCallEntry(3+2*i, call, name, args, at), toolResultEntry(4+2*i, call, name, output, failed, at.Add(time.Second))})
	}
	for i := range 4 {
		add(i, "read", `{"path":"other.go"}`, "read output", false)
	}
	add(4, "edit", `{"path":"a.go"}`, "edit output", false)
	add(5, "write", `{"path":"b.go"}`, "write output", false)
	add(6, "edit", `{"path":"./a.go"}`, "more edits", false)
	add(7, "bash", `{"command":"go test ./..."}`, "Test failed\n\nCommand exited with code 2", true)
	add(8, "edit", `{"path":"untouched.go"}`, "replace failed", true)
	s.merge([]sdk.Entry{entry(21, "assistant", "The tests need another pass.")})
	assertFrame(t, m)
	if !clickActivity(m, "4/0") {
		t.Fatal("could not open the tool preview")
	}
	body := ansi.Strip(assertFrame(t, m))
	want := []string{"read 4 · ran 1 · edited 2 files · 2 failed", "✗ go test", "exit 2", "✗ edit untouched.go", "✎ edit a.go", "✎ write b.go", "… 5 more · enter open", "The tests need another pass."}
	remaining := body
	for _, text := range want {
		_, after, ok := strings.Cut(remaining, text)
		if !ok {
			t.Fatalf("missing ordered %q in:\n%s", text, body)
		}
		remaining = after
	}
	rows := strings.Split(body, "\n")
	for i, row := range rows {
		if strings.Contains(row, "▾ read 4") && (i < 2 || strings.TrimSpace(rows[i-1]) != "" || !strings.Contains(rows[i-2], "──")) {
			t.Fatalf("missing single blank above burst:\n%s", body)
		}
		if strings.Contains(row, "The tests need another pass.") && (i < 2 || strings.TrimSpace(rows[i-1]) != "" || !strings.Contains(rows[i-2], "… 5 more")) {
			t.Fatalf("missing single blank below burst:\n%s", body)
		}
	}
	if strings.Contains(body, "read other.go") {
		t.Fatal("early successful reads displaced important results")
	}
	if !clickActivity(m, "4/0/7") || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Test failed") {
		t.Fatal("sorted result target did not open its own output")
	}
	press(m, tea.KeyEnter, 0)
	if body := ansi.Strip(assertFrame(t, m)); strings.Count(body, "✗ go test") != 1 || !strings.Contains(body, "read other.go") {
		t.Fatalf("full page lost the priority order or excluded ordinary results:\n%s", body)
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 16})
	s.toggleBurst("4/0")
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "edited 2 files · 2 failed") {
		t.Fatalf("narrow summary lost edits or failures:\n%s", body)
	}
}

func TestBurstBoundaryMatchesStreamingAndCommittedProse(t *testing.T) {
	for _, sameRecord := range []bool{false, true} {
		m, s := demo(t, Options{})
		m.width = 64
		const text = "Here is what changed."
		result := toolResultEntry(3, "call", "bash", "ok", false, time.Unix(100, 0))
		answer := entry(4, "assistant", text)
		if sameRecord {
			result.Messages = append(result.Messages, answer.Messages...)
			s.merge([]sdk.Entry{result})
		} else {
			s.merge([]sdk.Entry{result})
			s.stream = detachStream(&sdk.Streaming{Blocks: answer.Messages[0].Blocks})
			s.streamRevision++
			live := m.blockRows(s, len(s.shown), m.width-4)
			if len(live) != 2 || live[0].text != "" || live[1].text != text {
				t.Fatalf("streamed prose not separated from burst: %+v", live)
			}
			s.stream = x.Stream{}
			s.streamRevision++
			s.merge([]sdk.Entry{answer})
		}
		rows := m.blockRows(s, len(s.shown)-1, m.width-4)
		if n := len(rows); n < 2 || rows[n-2].text != "" || rows[n-1].text != text {
			t.Fatalf("committed prose not separated (same record: %t): %+v", sameRecord, rows)
		}
	}
}

func TestActivityOpenHintFitsWholeOrDrops(t *testing.T) {
	const label = "   … 14 more lines · view output"
	width := stringWidth(label)
	for _, w := range []int{width + 12, width + 13, 200} {
		row := activityOpenRow(label, x.Muted, "fixture", "open", w).Lines[0]
		var text strings.Builder
		for _, span := range row {
			text.WriteString(span.Text)
		}
		want := label
		if w >= width+13 {
			want += " · enter open"
		}
		if text.String() != want {
			t.Fatalf("width %d: got %q want %q", w, text.String(), want)
		}
	}
}

func TestLiveStatusIsOnePinnedRow(t *testing.T) {
	m, s := activityFixture(t, Options{Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 49, Height: 13})
	assertFrame(t, m)
	s.following = false
	s.anchor = rowAnchor{id: "1"}
	s.tools = []x.Tool{{CallID: "a", Name: "bash", Live: true}, {CallID: "b", Name: "read", Live: true}}
	s.activity.thinking = true
	frame := ansi.Strip(assertFrame(t, m))
	if strings.Count(frame, "Running shell command · +1") != 1 || strings.Contains(frame, "Thinking") || strings.Contains(frame, "Waiting for model") {
		t.Fatalf("expected one live label at the bottom:\n%s", frame)
	}
	if m.frame.top.id != "1" {
		t.Fatal("changing live status shifted the scroll anchor")
	}
	y := m.frame.ruleY
	if !strings.Contains(m.frame.rows[y], "Running shell command") || !strings.Contains(m.frame.rows[y], "──") ||
		!strings.Contains(m.frame.rows[y+1], "steer ›") || y != m.frame.transcriptHeight {
		t.Fatal("live activity did not share the rule immediately above the prompt")
	}
}

func TestLiveToolRendererUsesOneRowAndCurrentOutput(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Extensions: []x.Registration{{
		ID: "fixture/tool", APIVersion: x.APIVersion, Tools: []x.ToolRenderer{{Name: "fixture", Render: func(ctx x.RenderContext, tool x.Tool) (x.Content, bool) {
			if !ctx.Live || ctx.MaxLines != 1 || ctx.MaxBytes > 4<<10 || ctx.Thread.ID != "1" || !tool.Live || tool.CallID != "call" {
				t.Errorf("wrong live renderer budget or tool: %+v %+v", ctx, tool)
			}
			if tool.Output == "" {
				return nil, false
			}
			return x.Content{{Lines: []x.Line{{{Text: "custom: " + tool.Output}}, {{Text: "second row"}}}}}, true
		}}},
	}}})
	s.info.Status = x.Running
	s.tools = []x.Tool{{Name: "fixture", CallID: "call", Status: "running", Live: true}}
	m.now = time.Unix(100, 0)
	s.activity.tools = map[string]time.Time{"call": m.now.Add(-3 * time.Second)}
	for _, output := range []string{"", "partial 1", "partial 2"} {
		s.tools[0].Output = output
		frame := ansi.Strip(assertFrame(t, m))
		want := "custom: " + output
		if output == "" {
			want = "Running fixture · 3s"
		}
		if row := ansi.Strip(m.frame.rows[m.frame.ruleY]); !strings.Contains(row, want) || strings.Contains(frame, "second row") {
			t.Fatalf("wrong pinned row for %q: %q", output, row)
		}
	}
	s.tools[0].Name, s.tools[0].Output = "bash", "previous output\nlatest output\n"
	frame := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "Running shell command · 3s") || strings.Contains(frame, "previous output") || strings.Contains(frame, "latest output") {
		t.Fatalf("tool output leaked into the rule:\n%s", frame)
	}
}

func TestLiveRuleKeepsCommittedToolIntentAndArguments(t *testing.T) {
	for _, note := range []string{"", "*Running full tests*"} {
		m, s := demo(t, Options{ReducedMotion: true})
		m.Update(tea.WindowSizeMsg{Width: 104, Height: 24})
		start := time.Unix(100, 0)
		call := toolCallEntry(3, "test", "bash", `{"command":"go test ./..."}`, start)
		call.Messages[0].Blocks = append([]sdk.Block{{Type: "text", Text: note}}, call.Messages[0].Blocks...)
		state := sdk.State{Thread: sdk.Thread{ID: "1"}, Run: &sdk.Run{TaskID: "5"},
			Tools:  []sdk.Tool{{CallID: "test", Name: "bash", Status: "running", Output: ptr("tool out\n...")}},
			Window: sdk.TranscriptWindow{LastEntryID: ptr(sdk.ID("3")), Entries: []sdk.Entry{call}}}
		m.receiveFeed(feedMsg{owner: "1", ok: true, update: sdk.StateUpdate{Snapshot: sdk.Snapshot{State: state}}})
		since := s.activity.tools["test"]
		m.now = since.Add(61 * time.Second)
		m.editor.set("keep this draft")
		assertFrame(t, m)
		want := progressIntent(note)
		if want == "" {
			want = "Running go test ./..."
		}
		rule := m.frame.rows[m.frame.ruleY]
		if !strings.Contains(rule, "\x1b[3m"+want+"\x1b[23m · 1m 01s") || strings.Contains(rule, "tool out") ||
			!strings.Contains(ansi.Strip(rule), "esc stop  enter steer  alt+enter follow-up") {
			t.Fatalf("quiet tool intent not shown with hints: %q", rule)
		}
		m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
		assertFrame(t, m)
		if !strings.Contains(ansi.Strip(m.frame.rows[m.frame.ruleY+1]), "keep this draft") {
			t.Fatal("narrow live rule displaced prompt")
		}
	}
}

func TestTranscriptAndSingleThreadShareLeftEdge(t *testing.T) {
	m, s := activityFixture(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 96, Height: 17})
	body := assertFrame(t, m)
	for _, line := range strings.Split(ansi.Strip(body), "\n") {
		switch {
		case strings.Contains(line, "Where do"):
			if stringWidth(line[:strings.Index(line, "Where do")]) != 2 {
				t.Fatal("user text moved off the content edge")
			}
		case strings.Contains(line, "Hi. What"), strings.Contains(line, "▸ ran 14"), strings.Contains(line, "Waiting for model"), strings.Contains(line, "steer ›"):
			if len(line) < 3 || line[:2] != "  " || line[2] == ' ' {
				t.Fatalf("misaligned %q", line)
			}
		}
	}
	last := ansi.Strip(m.frame.rows[m.height-1])
	if !strings.HasPrefix(last, "  dir hush · fixture-model max") || strings.Count(ansi.Strip(body), "dir hush") != 1 {
		t.Fatalf("single-thread footer:\n%s", body)
	}
	m.options.Fills = FillOn
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m.Update(tea.BackgroundColorMsg{Color: color.White})
	assertFrame(t, m)
	for _, row := range m.frame.rows {
		if line := ansi.Strip(row); strings.Contains(line, "Where do") {
			if !strings.HasPrefix(line, "│ Where do") || !strings.HasPrefix(row, "\x1b[34m│") || strings.Contains(row, "\x1b[48;") {
				t.Fatalf("accent bar should mark user text with no tint or shift: %q", row)
			}
		}
	}
	m.ensureThread("2")
	m.openedTab("2")
	if len(m.chromeRows(s, m.width-4)) != 2 {
		t.Fatal("tab bar should appear when two are open")
	}
}
