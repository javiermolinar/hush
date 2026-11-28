package tui

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

func threadTurnStep(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("picker did not schedule a preview read")
	}
	v, ok := cmd().(threadTurnResult)
	if !ok || v.err != nil {
		t.Fatalf("preview read failed: %+v", v)
	}
	m.Update(v)
}

func lastTurnBackend(list func(sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error)) threadPickerBackend {
	return threadPickerBackend{list: list, history: func(_ context.Context, id sdk.ID, _ sdk.HistoryRange, _ int) (sdk.HistoryPage, error) {
		page := sdk.HistoryPage{ThreadID: id}
		switch id {
		case "21":
			page.Entries = []sdk.Entry{entry(4, "user", "Check the cached logs"), entry(5, "assistant", "The cache is healthy.")}
		case "22":
			page.Entries = []sdk.Entry{entry(6, "user", "Trace the slow requests")}
		}
		return page, nil
	}}
}

func TestThreadPickerShowsLastTurnWithoutSwitching(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, all := range []bool{false, true} {
		name, action := "threads", switcherID
		if all {
			name, action = "threads-all", switchAllID
		}
		t.Run(name, func(t *testing.T) {
			m, _ := demo(t, Options{ReducedMotion: true, Fills: FillOn})
			m.options.NewThread.CWD, m.options.WorkspaceLocal = "/launch/hush", true
			setTypographyPalette(m, typographyPalettes[1])
			m.editor.set("Keep this draft")
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			m.backend = lastTurnBackend(func(opts sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error) {
				if opts.Workspace != "/launch/hush" || opts.BrowseAll != all {
					t.Fatalf("wrong list scope: %+v", opts)
				}
				ws := &sdk.ThreadWorkspace{ID: "hush-id", Name: "hush"}
				return sdk.ThreadDirectoryPage{Threads: []sdk.ThreadDirectoryRow{
					{ID: "21", Title: "Logs", Status: "done"},
					{ID: "22", Title: "Tracing", Status: "running"},
				}, Workspaces: []*sdk.ThreadWorkspace{ws, ws}, WorkspaceMatches: []string{"inside", "inside"}}, nil
			})
			_, read := m.Update(m.runAction(m.host.byID[action], "")())
			if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Loading last turn…") {
				t.Fatalf("missing loading state:\n%s", frame)
			}
			threadTurnStep(t, m, read)
			assertFrame(t, m)
			var right []string
			for y, row := range m.frame.rows {
				if _, detail, ok := strings.Cut(ansi.Strip(row), "│"); ok {
					right = append(right, detail)
					if strings.Contains(detail, "Prompt:") {
						m.mouse(tea.MouseClickMsg(tea.Mouse{X: m.frame.dialogX + m.frame.dialogListWidth + 4, Y: y, Button: tea.MouseLeft}))
					}
				}
			}
			if details := strings.Join(right, "\n"); !strings.Contains(details, "Prompt: Check the cached logs") || !strings.Contains(details, "Reply:  The cache is healthy.") {
				t.Fatalf("last turn was not in the right pane:\n%s", details)
			}
			if m.active != "1" || m.sheet == nil || m.threads["21"] != nil || m.editor.value() != "Keep this draft" {
				t.Fatal("previewing changed the active session, draft or tabs")
			}
			want := ansi.Strip(m.View().Content)
			for _, mode := range []surfaceMode{surfaceTransparent, surfaceTinted, surfaceSolid} {
				m.applyTheme(namedTheme{name: string(mode), surface: mode, base: baseDark})
				body := assertFrame(t, m)
				if f := m.frame; ansi.Strip(body) != want || f.dialogY+f.dialogHeight != f.ruleY ||
					strings.HasPrefix(f.rows[f.dialogY], "\x1b[48;2;") != (mode != surfaceTransparent) {
					t.Fatalf("%s changed the preview or its dock", mode)
				}
			}
			threadTurnStep(t, m, press(m, tea.KeyDown, 0))
			m.Update(tea.WindowSizeMsg{Width: 58, Height: 24})
			if frame := ansi.Strip(assertFrame(t, m)); m.frame.dialogListWidth != 0 || !strings.Contains(frame, "Prompt: Trace the slow requests") || strings.Contains(frame, "The cache is healthy.") {
				t.Fatalf("narrow preview did not follow the highlighted thread:\n%s", frame)
			}
		})
	}
}

func TestThreadPickerRefreshesAfterActiveReplyCommits(t *testing.T) {
	m, state := demo(t, Options{ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	rows := []sdk.Entry{entry(3, "user", "Is the cache healthy?")}
	m.backend = threadPickerBackend{
		list: func(sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error) {
			return sdk.ThreadDirectoryPage{Threads: []sdk.ThreadDirectoryRow{{ID: "1", Title: "Cache"}}}, nil
		},
		history: func(_ context.Context, id sdk.ID, _ sdk.HistoryRange, _ int) (sdk.HistoryPage, error) {
			return sdk.HistoryPage{ThreadID: id, Entries: rows}, nil
		},
	}
	_, load := m.Update(m.runAction(m.host.byID[switcherID], "")())
	threadTurnStep(t, m, load)
	rows = append(rows, entry(4, "assistant", "The cache is healthy."))
	state.merge(rows)
	_, refresh := m.Update(effectTick{})
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Prompt: Is the cache healthy?") || strings.Contains(frame, "Loading last turn…") {
		t.Fatalf("refresh dropped the displayed prompt:\n%s", frame)
	}
	threadTurnStep(t, m, refresh)
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Reply:  The cache is healthy.") {
		t.Fatalf("the committed reply did not reach the picker:\n%s", frame)
	}
}

func TestThreadPickerDiscardsSupersededLastTurns(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.backend = lastTurnBackend(func(opts sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error) {
		page := sdk.ThreadDirectoryPage{Workspaces: []*sdk.ThreadWorkspace{}}
		if opts.Query == "" {
			page.Threads = []sdk.ThreadDirectoryRow{{ID: "21"}, {ID: "22"}}
			page.Workspaces = []*sdk.ThreadWorkspace{nil, nil}
			page.NextCursor = ptr("fixture-next")
		}
		return page, nil
	})
	_, first := m.Update(m.runAction(m.host.byID[switcherID], "")())
	old := first() // returned by the backend, but not yet delivered to the UI
	pending := press(m, tea.KeyDown, 0)
	latest := press(m, tea.KeyUp, 0)
	if pending() != nil {
		t.Fatal("moving away did not cancel a pending preview")
	}
	m.Update(old)
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Loading last turn…") || strings.Contains(frame, "The cache is healthy.") {
		t.Fatalf("accepted stale result after moving away and back:\n%s", frame)
	}
	threadTurnStep(t, m, latest)

	f := m.frame
	_, pending = m.Update(tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelDown, X: f.dialogX + 3, Y: f.dialogY + 2}))
	if pending == nil || m.sheet.threadPicker.turn.thread != "22" {
		t.Fatal("mouse navigation did not follow the focused row")
	}
	_, search := m.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	if pending() != nil {
		t.Fatal("search did not cancel the old preview")
	}
	m.Update(search())
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "No matching threads") || strings.Contains(frame, "Prompt:") {
		t.Fatalf("search retained a stale preview:\n%s", frame)
	}
	_, fresh := m.Update(press(m, tea.KeyBackspace, 0)())
	press(m, tea.KeyEscape, 0)
	if m.sheet != nil || fresh() != nil {
		t.Fatal("closing the picker did not cancel its pending preview")
	}
}

func TestReadThreadTurnFindsLatestExchangeAcrossPages(t *testing.T) {
	latest := entry(6, "assistant", "The **latest** [reply](https://example.org/private) is ready. &#27;")
	latest.Messages[0].Blocks = append([]sdk.Block{{Type: "thinking", Text: "private reasoning"}}, latest.Messages[0].Blocks...)
	read := 0
	backend := threadPickerBackend{history: func(_ context.Context, id sdk.ID, query sdk.HistoryRange, limit int) (sdk.HistoryPage, error) {
		read++
		if id != "41" || limit != threadTurnPageSize {
			t.Fatalf("wrong history request: %s/%d", id, limit)
		}
		page := sdk.HistoryPage{ThreadID: id}
		switch {
		case read == 1 && query == sdk.Before(nil):
			compaction := entry(7, "user", "compaction summary")
			compaction.Kind = "pi.compaction"
			notice := entry(9, "assistant", "worker notice")
			notice.Kind = workerNoticeEntry
			page.Entries = []sdk.Entry{entry(5, "assistant", "Interim commentary"), latest, compaction, entry(8, "toolResult", "tool output"), notice}
			page.NextCursor = ptr("more")
		case read == 2 && query == sdk.PageCursor("more"):
			// The fork inherits this prompt. It belongs to the viewed history
			// even though the entry itself has a different originating ID.
			page.Entries = []sdk.Entry{entry(1, "user", "Old question"), entry(2, "assistant", "Old reply"), entry(3, "user", "Inspect the new issue")}
		default:
			t.Fatalf("unexpected history read %d: %+v", read, query)
		}
		return page, nil
	}}
	turn, err := readThreadTurn(t.Context(), backend, "41")
	if want := (threadTurn{prompt: "Inspect the new issue", reply: "The latest reply is ready. ␛"}); err != nil || turn != want {
		t.Fatalf("last turn: %+v, %v; want %+v", turn, err, want)
	}
}

func TestReadThreadTurnStopsAtLatestPromptOrReset(t *testing.T) {
	latest := entry(4, "user", "<skill name=\"review\" location=\"/tmp/local\">\nprivate instructions\n</skill>\n\nCheck the error path")
	latest.Messages[0].Blocks = append(latest.Messages[0].Blocks, sdk.Block{Type: "omitted", Kind: "image"})
	reset := sdk.Entry{ID: "3", ThreadID: "1", Kind: "pi.reset"}
	plan := entry(3, "assistant", "Plan run · r2 · Fix the error path · 2 steps")
	plan.Kind = "hush.plan.chat"
	for _, tc := range []struct {
		name string
		tail []sdk.Entry
		want threadTurn
	}{
		{"unanswered", []sdk.Entry{latest}, threadTurn{prompt: "/skill:review Check the error path\n[Image attached]"}},
		{"plain reset", []sdk.Entry{reset}, threadTurn{prompt: "Previous prompt", reply: "Previous reply"}},
		{"reply after reset", []sdk.Entry{reset, entry(4, "assistant", "New context")}, threadTurn{reply: "New context"}},
		{"plan run", []sdk.Entry{plan, entry(4, "assistant", "Both steps verified.")}, threadTurn{prompt: "Plan run · r2 · Fix the error path · 2 steps", reply: "Both steps verified."}},
	} {
		backend := threadPickerBackend{history: func(_ context.Context, id sdk.ID, _ sdk.HistoryRange, _ int) (sdk.HistoryPage, error) {
			entries := append([]sdk.Entry{entry(1, "user", "Previous prompt"), entry(2, "assistant", "Previous reply")}, tc.tail...)
			return sdk.HistoryPage{ThreadID: id, Entries: entries}, nil
		}}
		if turn, err := readThreadTurn(t.Context(), backend, "1"); err != nil || turn != tc.want {
			t.Errorf("last turn after %s: %+v, %v; want %+v", tc.name, turn, err, tc.want)
		}
	}
}

func TestThreadTurnTextAndLayoutBoundLongMessages(t *testing.T) {
	for _, role := range []string{"user", "assistant"} {
		msg := entry(1, role, "Start\x1b\n"+strings.Repeat("界", menuMarkdownBytes)).Messages[0]
		if got := threadTurnText(msg); !utf8.ValidString(got) || len(got) > menuMarkdownBytes+len("…") || !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '\x1b') {
			t.Fatalf("unbounded or unsafe %s preview: %q", role, got)
		}
	}
	m, _ := demo(t, Options{ReducedMotion: true})
	rows := m.threadTurnRows(threadTurn{strings.Repeat("界", 200), strings.Repeat("界", 200)}, 36, 2)
	if len(rows) != 2 || !strings.HasPrefix(ansi.Strip(rows[0]), "Prompt:") || !strings.HasPrefix(ansi.Strip(rows[1]), "Reply:") {
		t.Fatalf("clipping the prompt hid the reply: %v", rows)
	}
	for _, row := range rows {
		if ansi.StringWidth(row) > 36 || !strings.HasSuffix(ansi.Strip(row), "…") {
			t.Fatalf("preview did not fit with an ellipsis: %q", row)
		}
	}
}

func TestReadThreadTurnBoundsPaging(t *testing.T) {
	reads := 0
	backend := threadPickerBackend{history: func(_ context.Context, id sdk.ID, _ sdk.HistoryRange, _ int) (sdk.HistoryPage, error) {
		reads++
		latest := entry(100-reads, "toolResult", "tool output")
		return sdk.HistoryPage{ThreadID: id, Entries: []sdk.Entry{latest}, NextCursor: ptr(string(latest.ID))}, nil
	}}
	if turn, err := readThreadTurn(t.Context(), backend, "1"); err != nil || turn != (threadTurn{}) || reads != threadTurnMaxPages {
		t.Fatalf("read %d pages, last turn %+v (%v)", reads, turn, err)
	}
}
