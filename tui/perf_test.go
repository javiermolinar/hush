package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"runtime"
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

// The performance contract: every keystroke (drafts up to 16 KB) and every
// streaming patch (answers up to 64 KB, with links and 16 KB tool outputs)
// costs at most 1 ms p99 in Update + View, inside the 2.08 ms frame budget.
const frameBudget, contractBudget = 2080 * time.Microsecond, time.Millisecond

// frameTargets overrides the frame budget. A case that misses its target
// today names the plan step that fixes it, and reports until that step lands.
var frameTargets = map[string]struct {
	limit     time.Duration
	knownMiss string
}{
	"typing-1k":         {contractBudget, "prompt-editor"},
	"typing-4k":         {contractBudget, "prompt-editor"},
	"typing-16k":        {contractBudget, "prompt-editor"},
	"replica-streaming": {contractBudget, ""},
	"image-typing":      {contractBudget, ""},
}

var typingDrafts = map[string]int{"typing-1k": 1 << 10, "typing-4k": 4 << 10, "typing-16k": 16 << 10}

// multilineDraft is prose with wrapped paragraphs, list lines and blank lines.
func multilineDraft(size int) string {
	const unit = "Check the cached value before the request, then update the handler so the retry keeps its " +
		"original deadline and reports the failure. Keep the existing tests.\n- list the changed files\n- rerun the suite\n\n"
	return strings.Repeat(unit, size/len(unit)+1)[:size]
}

// replicaWindow is a subscribe result for thread 1 after entries 1 and 2: its
// window cycles through a user prompt (200 B), an assistant answer (800 B)
// with a tool call and a tool result of toolBytes, while a run streams.
func replicaWindow(entries, toolBytes int) json.RawMessage {
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	prompt := strings.Repeat("Please review the cached handler. ", 7)[:200]
	answer := strings.Repeat("The handler reads the cached value before the request. ", 15)[:800]
	result := strings.Repeat("ok  github.com/javiermolinar/hush/sdk  0.7s\n", toolBytes/44+1)[:toolBytes]
	window := make([]any, entries)
	for i := range window {
		id := i + 3
		var message map[string]any
		switch i % 3 {
		case 0:
			message = map[string]any{"role": "user", "timestampMs": 1795435568245, "blocks": []any{text(prompt)}}
		case 1:
			message = map[string]any{"role": "assistant", "timestampMs": 1795435568245, "stopReason": "toolUse", "blocks": []any{text(answer),
				map[string]any{"type": "toolCall", "id": fmt.Sprint("call-", id), "name": "bash", "arguments": map[string]any{"command": "go test ./..."}}}}
		default:
			message = map[string]any{"role": "toolResult", "timestampMs": 1795435568245, "callId": fmt.Sprint("call-", id-1), "name": "bash",
				"isError": false, "blocks": []any{text(result)}}
		}
		window[i] = map[string]any{"id": fmt.Sprint(id), "threadId": "1", "kind": "model", "messages": []any{message}}
	}
	state := map[string]any{
		"thread": map[string]any{"id": "1"}, "agent": map[string]any{},
		"usage":  map[string]any{"inputTokens": 0, "outputTokens": 0, "cacheReadTokens": 0, "cacheWriteTokens": 0, "totalTokens": 0, "costUsd": 0},
		"window": map[string]any{"beforeEntryId": "2", "lastEntryId": fmt.Sprint(entries + 2), "entries": window},
		"tools":  []any{}, "queue": []any{}, "compactions": []any{},
		"run":       map[string]any{"taskId": "900", "submissionIds": []any{"901"}},
		"streaming": map[string]any{"attempt": 1, "blocks": []any{text("")}},
	}
	raw, err := json.Marshal(map[string]any{"subscriptionId": "bench-sub", "epoch": "bench-epoch", "rev": 0, "state": state})
	if err != nil {
		panic(err)
	}
	return raw
}

// replicaToken applies one streamed token, as Durable commits a partial
// answer, and returns the snapshot the SDK's watch would deliver.
func replicaToken(t testing.TB, r *sdk.Replica) sdk.Snapshot {
	t.Helper()
	s, _ := r.Current()
	patch := fmt.Sprintf(`{"type":"patch","epoch":"bench-epoch","subscriptionId":"bench-sub","baseRev":%d,"nextRev":%d,`+
		`"ops":[["a",["streaming","blocks",0,"text"]," token"]]}`, s.Rev, s.Rev+1)
	if changed, err := r.Apply(json.RawMessage(patch)); !changed || err != nil {
		t.Fatalf("patch not applied: %v", err)
	}
	s, _ = r.Current()
	return s
}

func feedUpdate(m *Model, s sdk.Snapshot) feedMsg {
	return feedMsg{token: m.feed.token, owner: m.active, update: sdk.StateUpdate{Snapshot: s}, ok: true}
}

func TestFrameBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("timing is not measured in short/race runs")
	}
	for _, which := range []string{"idle", "streaming", "typing", "motion", "sheet", "thread-picker", "model-catalog", "palette", "long-idle", "long-scroll", "long-resize", "folded-tool-results", "expanded-tool-results", "footer-overflow", "directory-tabs", "live-tool", "full-tool-output", "rewind-preview", "git-diff", "workspace-trust", "file-picker", "file-search", "skill-slash", "skill-sheet", "loaded-context", "image-preview", "queued-inputs", "expanded-reasoning",
		// Contract cases; they run after the others so their garbage can't skew them.
		"replica-streaming", "typing-1k", "typing-4k", "typing-16k", "image-typing"} {
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
			if which == "expanded-reasoning" {
				longTranscript(t, m, s)
				for _, r := range s.entries {
					for i := range r.entry.Record.Messages {
						msg := &r.entry.Record.Messages[i]
						msg.Blocks = append([]sdk.Block{{Type: "thinking", Text: "Check the implementation and preserve unrelated settings."}}, msg.Blocks...)
					}
				}
				m.applySettings(sdk.SettingsSnapshot{Model: configModel().Model, ThinkingLevel: "high", Reasoning: sdk.ReasoningExpanded})
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
			if size, ok := typingDrafts[which]; ok {
				m.Update(tea.PasteMsg{Content: multilineDraft(size)})
				if len(m.editor.value()) != size {
					t.Fatalf("draft has %d bytes, want %d", len(m.editor.value()), size)
				}
			}
			var replica sdk.Replica
			if which == "replica-streaming" {
				if err := replica.Install(replicaWindow(100, 16<<10), "bench-epoch", m.active); err != nil {
					t.Fatal(err)
				}
				snapshot, _ := replica.Current()
				m.Update(feedUpdate(m, snapshot))
			}
			if which == "image-preview" || which == "image-typing" {
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
			if which == "workspace-trust" {
				s.observed, s.info.Workspace = true, "/work/project/src"
				m.options.WorkspaceTrust = workspaceTrustFixture("/work/" + strings.Repeat("wide界", 18) + "/project")
				openWorkspaceTrustFixture(t, m)
			}
			if which == "file-picker" {
				m.options.Services.Files = func(context.Context, x.ListFiles) ([]x.File, string, error) { return nil, "", nil }
				m.editor.set("@zzzz")
				m.files = filePicker{owner: m.active, generation: m.token()}
				for i := range 400 {
					m.files.records = append(m.files.records, x.File{Path: fmt.Sprintf("file %03d.go", i)})
				}
			}
			if which == "file-search" {
				openBrowseScope(t, m, fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true},
					search: func(context.Context, sdk.WorkspaceFileTarget, string, int) (sdk.WorkspaceFileSearchResult, error) {
						return sdk.WorkspaceFileSearchResult{}, nil
					}})
				m.editor.set("@generator.go")
				m.refreshFilePicker()
				result := sdk.WorkspaceFileSearchResult{Partial: true}
				for i := range sdk.WorkspaceFileSearchLimit {
					result.Matches = append(result.Matches, sdk.WorkspaceFileMatch{Path: fmt.Sprintf("repo%d/%sgenerator.go", i, strings.Repeat("nested folder 界/", 12))})
				}
				m.receiveNativeFileSearch(nativeFileSearchMsg{generation: m.files.generation, result: result})
			}
			if which == "skill-slash" || which == "skill-sheet" {
				m.backend = workspaceMetadataFixture{}
				m.nativeSkills = true
				m.skills = skillsLease{owner: m.active, navigation: m.navigation, generation: m.token()}
				var listed []sdk.WorkspaceSkill
				for i := range 64 {
					listed = append(listed, sdk.WorkspaceSkill{Name: fmt.Sprintf("review-%02d", i), Source: "workspace", Description: "Review the current draft"})
				}
				m.receiveSkills(skillsMsg{m.skills.generation, listed})
				if which == "skill-slash" {
					m.editor.set("/skill:")
				} else {
					budget := maxEffects
					m.admit(m.route(m.active, menusID), stageSkillChoice(listed), &budget)
				}
			}
			if which == "loaded-context" {
				var contents strings.Builder
				for i := range 64 {
					fmt.Fprintf(&contents, "Workspace · AGENTS.md\n  /remote/%02d/%s/AGENTS.md\n", i, strings.Repeat("directory/", 15))
				}
				d := &dialog{route: m.route(m.active, menusID), token: m.token(), title: "Loaded context", loadedContext: &contextDialog{ready: true}}
				m.setContextOutput(d, contents.String())
				m.pushDialog(d)
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
			var threadPage x.ClientResult
			if which == "thread-picker" {
				page := groupedThreadPage()
				for i := len(page.Threads); i < 50; i++ {
					page.Threads = append(page.Threads, sdk.ThreadDirectoryRow{ID: sdk.ID(fmt.Sprint(i + 1)), Title: fmt.Sprintf("Thread %d", i+1), Status: "running"})
					page.Workspaces = append(page.Workspaces, &sdk.ThreadWorkspace{ID: fmt.Sprint(i), Name: fmt.Sprintf("Workspace %d", i)})
					page.WorkspaceMatches = append(page.WorkspaceMatches, "outside")
				}
				threadPage = directoryPageResult(page, 50)
				budget := maxEffects
				m.openThreadPicker(m.route(m.active, menusID), true, &budget)
				m.receiveThreadPicker(threadPickerResult{token: m.sheet.token, result: threadPage})
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
			var passes int
			stream.WriteString("## Streaming\n\n```go\n")
			if _, ok := frameTargets[which]; ok {
				runtime.GC() // start contract cases without an earlier case's garbage
			}
			m.View()
			for i := range 220 {
				var msg tea.Msg
				switch which {
				case "replica-streaming":
					msg = feedUpdate(m, replicaToken(t, &replica)) // applied off the clock, like the SDK's reader
				case "streaming":
					stream.WriteString("func next() { println(42) }\n")
					s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{{Type: "text", Text: stream.String()}}})
					s.streamRevision++
				case "expanded-reasoning":
					stream.WriteString("Check the next implementation detail.\n")
					s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{{Type: "thinking", Text: stream.String()}}})
					s.streamRevision++
				case "typing", "typing-1k", "typing-4k", "typing-16k", "image-typing":
					msg = tea.KeyPressMsg{Code: 'a', Text: "a"}
				case "motion":
					msg = tea.MouseMotionMsg(tea.Mouse{X: 2 + i%10, Y: m.frame.transcriptHeight - 4})
				case "sheet", "git-diff":
					msg = tea.KeyPressMsg{Code: tea.KeyDown}
				case "thread-picker":
					if i%25 == 0 {
						// Run the result projection often enough to measure it at p99.
						msg = threadPickerResult{token: m.sheet.token, result: threadPage}
					} else {
						msg = tea.KeyPressMsg{Code: tea.KeyDown}
					}
				case "model-catalog":
					switch i % 4 {
					case 0:
						msg = tea.KeyPressMsg{Code: 'o', Text: "o"}
					case 2:
						msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
					default:
						msg = modelSearchResult{token: m.sheet.token, catalog: modelPage}
					}
				case "file-search":
					code := tea.KeyDown
					if i%sdk.WorkspaceFileSearchLimit < sdk.WorkspaceFileSearchLimit/2 {
						code = tea.KeyUp
					}
					msg = tea.KeyPressMsg{Code: code}
				case "file-picker", "skill-slash", "skill-sheet":
					keys := [...]tea.KeyPressMsg{{Code: '0', Text: "0"}, {Code: tea.KeyBackspace}}
					msg = keys[i%len(keys)]
				case "loaded-context":
					msg = tea.KeyPressMsg{Code: []rune{tea.KeyDown, tea.KeyPgDown, tea.KeyUp}[i%3]}
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
				if which == "image-typing" {
					// Bubble Tea delivers the scan the key scheduled in a second pass.
					for n := 0; m.images.scanning && n < 4; n++ {
						m.Update(imageScan{})
						m.View()
						if i >= 20 {
							passes++
						}
					}
				}
				if i >= 20 {
					ds = append(ds, time.Since(start))
					passes++
				}
			}
			slices.Sort(ds)
			p99 := ds[len(ds)*99/100-1]
			t.Logf("p50 %s, p95 %s, p99 %s", ds[len(ds)/2-1], ds[len(ds)*95/100-1], p99)
			if which == "image-typing" {
				t.Logf("%.2f Update + View passes per key", float64(passes)/float64(len(ds)))
			}
			target := frameTargets[which]
			checkFrameTarget(t, p99, cmp.Or(target.limit, frameBudget), target.knownMiss)
		})
	}
	t.Run("linked-streaming", testLinkedStreamingBudget)
}

// linkedAnswer is prose with inline links, task boxes, bracketed inline code
// and lists, like most real answers.
func linkedAnswer(size int) string {
	var b strings.Builder
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "### Step %d\n\nRead `items[%d]` before the request; [the guide](https://example.org/guide/%d) covers the retry rules.\n\n"+
			"- [ ] Check `cache[key]` after the change\n- [x] Keep [the notes](https://example.org/notes/%d) current\n"+
			"- Run the suite with `go test ./...` and compare the output.\n\n", i, i, i, i)
	}
	return b.String()[:size]
}

// testLinkedStreamingBudget grows one linked answer to 64 KB by about 40
// bytes per patch and times View per patch near 4, 16 and 64 KB. The target
// is stream-markdown's: at most 0.5 ms p99 per patch at every size.
func testLinkedStreamingBudget(t *testing.T) {
	const step, limit = 40, 500 * time.Microsecond
	m, s := demo(t, Options{ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 42})
	answer := linkedAnswer(64 << 10)
	sizes := []int{4 << 10, 16 << 10, 64 << 10}
	samples := make([][]time.Duration, len(sizes))
	runtime.GC()
	m.View()
	for n := step; n <= len(answer); n += step {
		s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{{Type: "text", Text: answer[:n]}}})
		s.streamRevision++
		start := time.Now()
		m.View()
		d := time.Since(start)
		for i, size := range sizes {
			if n > size/2 && n <= size {
				samples[i] = append(samples[i], d)
			}
		}
	}
	for i, ds := range samples {
		slices.Sort(ds)
		p99 := ds[len(ds)*99/100-1]
		t.Logf("View per patch at %d KB (%d patches): p50 %s, p99 %s", sizes[i]>>10, len(ds), ds[len(ds)/2-1], p99)
		checkFrameTarget(t, p99, limit, "stream-markdown")
	}
}

// checkFrameTarget reports a known miss without failing. Other misses fail
// unless HUSH_PERF_MODE=report.
func checkFrameTarget(t *testing.T, p99, limit time.Duration, knownMiss string) {
	t.Helper()
	switch {
	case p99 <= limit && knownMiss != "":
		t.Logf("known miss now meets its target (p99 %s, limit %s); enforce it when %s lands", p99, limit, knownMiss)
	case p99 <= limit:
	case knownMiss != "":
		t.Logf("performance target missed (known, until %s): p99 %s, limit %s", knownMiss, p99, limit)
	default:
		reportFrameOverrun(t, p99, limit)
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
		reportFrameOverrun(t, p99, 2*time.Millisecond)
	}
}

func reportFrameOverrun(t *testing.T, p99, limit time.Duration) {
	t.Helper()
	if os.Getenv("HUSH_PERF_MODE") == "report" {
		t.Logf("performance target missed (report-only): p99 %s, limit %s", p99, limit)
		return
	}
	t.Fatalf("performance target missed: p99 %s, limit %s", p99, limit)
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
