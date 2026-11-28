package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const switchAllID = x.ActionID(menusID + ".switch-all")
const deployCWD = "/work/deploy"
const deployCheckout = "/worktrees/deploy/release"

type threadPickerBackend struct {
	inertBackend
	list    func(sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error)
	history func(context.Context, sdk.ID, sdk.HistoryRange, int) (sdk.HistoryPage, error)
}

func (b threadPickerBackend) History(ctx context.Context, id sdk.ID, query sdk.HistoryRange, limit int) (sdk.HistoryPage, error) {
	if b.history == nil {
		return sdk.HistoryPage{ThreadID: id}, nil
	}
	return b.history(ctx, id, query, limit)
}

func (b threadPickerBackend) ListThreadDirectory(_ context.Context, options sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error) {
	return b.list(options)
}

func (b threadPickerBackend) ThreadDirectory(ctx context.Context, limit int, cursor string) (sdk.ThreadDirectoryPage, error) {
	return b.ListThreadDirectory(ctx, sdk.ThreadDirectoryOptions{Limit: limit, Cursor: cursor})
}

func threadPickerStep(t *testing.T, m *Model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("picker did not schedule a read")
	}
	v, ok := cmd().(threadPickerResult)
	if !ok || v.result.Err != nil {
		t.Fatalf("picker read failed: %+v", v)
	}
	return m.receiveThreadPicker(v)
}

func groupedThreadPage() sdk.ThreadDirectoryPage {
	hush := &sdk.ThreadWorkspace{ID: "hush-id", Name: "hush"}
	deploy := &sdk.ThreadWorkspace{ID: "deploy-id", Name: "Deployment tools"}
	return sdk.ThreadDirectoryPage{
		Threads: []sdk.ThreadDirectoryRow{
			{ID: "4", Title: "Rollout review", Status: "failed", Checkout: &sdk.ThreadCheckout{Kind: "shared", CWD: deployCWD}},
			{ID: "3", Title: "Go TUI", Status: "running", Checkout: &sdk.ThreadCheckout{Kind: "shared", CWD: "/launch/hush"}},
			{ID: "2", Title: "Trace annotations", Status: "stopped", Checkout: &sdk.ThreadCheckout{Kind: "managed", CWD: deployCheckout, Branch: "release"}},
			{ID: "1", Title: "List display", Status: "parked", Checkout: &sdk.ThreadCheckout{Kind: "shared", CWD: "/launch/hush"}},
		},
		Workspaces:       []*sdk.ThreadWorkspace{deploy, hush, deploy, hush},
		WorkspaceMatches: []string{"outside", "inside", "outside", "inside"},
	}
}

func openGroupedThreadPicker(t *testing.T, m *Model) {
	t.Helper()
	m.options.NewThread.CWD, m.options.WorkspaceLocal = "/launch/hush", true
	m.backend = threadPickerBackend{
		list: func(opts sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error) {
			switch opts {
			case sdk.ThreadDirectoryOptions{Workspace: "/launch/hush", Limit: 50, BrowseAll: true}:
				return groupedThreadPage(), nil
			case sdk.ThreadDirectoryOptions{Workspace: deployCWD, Limit: 50}:
				return deploymentPage(deployCWD, deployCheckout), nil
			default:
				return sdk.ThreadDirectoryPage{}, sdk.ErrProtocol
			}
		},
		history: func(_ context.Context, id sdk.ID, _ sdk.HistoryRange, _ int) (sdk.HistoryPage, error) {
			page := sdk.HistoryPage{ThreadID: id}
			if id == "3" {
				page.Entries = []sdk.Entry{
					entry(1, "user", "Explain how the thread picker fits on narrow terminals."),
					entry(2, "assistant", "The picker keeps the list and details side by side when there is room.\n\nOn narrow terminals, the details stack below the list."),
				}
			}
			return page, nil
		},
	}
	threadPickerStep(t, m, m.runAction(m.host.byID[switchAllID], ""))
	threadTurnStep(t, m, m.syncThreadTurn())
}

func TestGoldenThreadPickerGroupsAndStatus(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 26})
	openGroupedThreadPicker(t, m)
	golden(t, "menu-threads-grouped", m)
	d := m.sheet
	f := m.frame
	left := func(row string) string {
		s, _, _ := strings.Cut(ansi.Strip(row), "│")
		return strings.Join(strings.Fields(s), " ")
	}
	var rows []string
	for _, row := range f.rows[f.dialogY : f.dialogY+f.dialogHeight] {
		rows = append(rows, left(row))
	}
	frame, before := strings.Join(rows, "\n"), -1
	for _, expected := range []string{"hush", "◐ Go TUI Running", "⏸ List display Parked", "Deployment tools", "✗ Rollout review Failed", "○ Trace annotations Stopped"} {
		at := strings.Index(frame, expected)
		if at <= before {
			t.Fatalf("missing or misplaced %q:\n%s", expected, frame)
		}
		before = at
	}
	for i, row := range f.rows[f.dialogY : f.dialogY+f.dialogHeight] {
		if left(row) == "Deployment tools" {
			m.mouse(tea.MouseClickMsg(tea.Mouse{X: f.dialogX + 4, Y: f.dialogY + i, Button: tea.MouseLeft}))
			break
		}
	}
	if item, ok := d.choice(); m.sheet != d || !ok || item.ID != "3" || m.active != "1" {
		t.Fatal("workspace headings must not be selectable; default to the first conversation")
	}
	press(m, tea.KeyDown, 0)
	press(m, tea.KeyDown, 0)
	if item, ok := d.choice(); !ok || item.ID != newThreadHereChoice {
		t.Fatalf("missing creation row at the start of the next workspace: %+v", item)
	}
	press(m, tea.KeyDown, 0)
	if item, ok := d.choice(); !ok || item.ID != "4" {
		t.Fatalf("keyboard navigation selected the wrong row across groups: %+v", item)
	}
	press(m, tea.KeyEnter, 0)
	if m.active != "4" || m.sheet != nil {
		t.Fatal("enter did not switch to the highlighted thread")
	}
}

func TestThreadPickerSearchDiscardsStaleResultsAndRetainsQueryOnPagination(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 84, Height: 22})
	var calls []sdk.ThreadDirectoryOptions
	m.backend = threadPickerBackend{list: func(opts sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error) {
		calls = append(calls, opts)
		if opts.Workspace != "" || opts.BrowseAll {
			return sdk.ThreadDirectoryPage{}, sdk.ErrProtocol
		}
		if opts.Query == "" {
			page := groupedThreadPage()
			page.WorkspaceMatches = nil
			return page, nil
		}
		page := sdk.ThreadDirectoryPage{Workspaces: []*sdk.ThreadWorkspace{}}
		if opts.Query == "transcript phrase" {
			id := sdk.ID("93")
			if opts.Cursor == "" {
				page.NextCursor = ptr("fixture-next")
			} else {
				id = "52"
			}
			page.Threads = []sdk.ThreadDirectoryRow{{ID: id, Title: "Incident notes", Status: "done"}}
			page.Workspaces = append(page.Workspaces, &sdk.ThreadWorkspace{ID: "work", Name: "SRE"})
		}
		return page, nil
	}}
	open := m.runAction(m.host.byID[switcherID], "")
	d := m.sheet
	if cmd := m.changeDialogQuery(d, "first draft"); cmd != nil {
		t.Fatal("a query before capability discovery should not replace the initial load")
	}
	stale := threadPickerStep(t, m, open)
	token := d.token
	search := m.changeDialogQuery(d, "transcript phrase")
	if v := stale(); v != nil {
		t.Fatalf("superseded query hit the backend: %+v", v)
	}
	m.receiveThreadPicker(threadPickerResult{token: token, result: directoryPageResult(groupedThreadPage(), 50)})
	if len(d.choices) != 0 || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Searching threads") {
		t.Fatal("stale results were selectable while searching")
	}
	threadPickerStep(t, m, search)
	if got, ok := d.choice(); !ok || got.ID != "93" {
		t.Fatalf("did not show an older thread matched by message content: %+v", got)
	}
	press(m, tea.KeyEnd, 0)
	threadPickerStep(t, m, m.acceptChoice(d))
	if got, ok := d.choice(); !ok || got.ID != "52" || d.query != "transcript phrase" {
		t.Fatalf("older page lost the query: %+v (%q)", got, d.query)
	}
	if want := []sdk.ThreadDirectoryOptions{{Limit: 50}, {Limit: 50, Query: "transcript phrase"}, {Limit: 50, Query: "transcript phrase", Cursor: "fixture-next"}}; !slices.Equal(calls, want) {
		t.Fatalf("picker requests: %+v, want %+v", calls, want)
	}
	threadPickerStep(t, m, m.changeDialogQuery(d, "no match"))
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "No matching threads") || strings.Contains(frame, "SRE") {
		t.Fatalf("empty search kept an obsolete workspace heading:\n%s", frame)
	}
	pending := m.changeDialogQuery(d, "still searching")
	press(m, tea.KeyEscape, 0)
	if m.sheet != nil || pending() != nil {
		t.Fatal("closing the picker did not cancel its pending search")
	}
}

func testThreadPickerFrameGeometry(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	openGroupedThreadPicker(t, m)
	for _, size := range [][2]int{{40, 12}, {53, 15}, {80, 22}, {120, 26}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for i := range m.sheet.filtered {
			m.sheet.selected = i
			frame := ansi.Strip(assertFrame(t, m))
			id := m.sheet.filtered[i]
			row := m.sheet.threadPicker.rows[id]
			_, status, _ := threadPickerStatus(row.status)
			if row.workspaceName != "" && (!strings.Contains(frame, row.workspaceName) || !strings.Contains(frame, status)) {
				t.Fatalf("%dx%d hid the selected row's workspace or status:\n%s", size[0], size[1], frame)
			}
		}
	}
}
