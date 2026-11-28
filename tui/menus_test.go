package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestMenusGoldens(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true, Services: MenuServices{
		Models: func(context.Context) ([]x.CatalogModel, error) {
			return []x.CatalogModel{{Model: sdk.Model{Provider: "fake", ModelID: "reasoner"}, Label: "Reasoning", ThinkingLevels: []string{"off", "low", "high"}}}, nil
		},
		Rename:  func(context.Context, x.SetTitle) error { return nil },
		Archive: func(context.Context, x.SetArchived) error { return nil },
		Modes: func(context.Context, x.ListModes) ([]x.Choice, error) {
			return []x.Choice{{ID: "review", Label: "Review"}, {ID: "ask", Label: "Ask"}}, nil
		},
		SetMode: func(context.Context, x.SetMode) error { return nil },
	}})
	m.backend = menuBackend{}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 22})
	m.editor.set("/fo")
	golden(t, "menu-slash", m)
	if !strings.Contains(ansi.Strip(m.View().Content), "/fork") || m.editor.value() != "/fo" {
		t.Fatal("ghost completion edited the prompt")
	}
	press(m, tea.KeyTab, 0)
	if m.editor.value() != "/fork " {
		t.Fatalf("slash completion: %q", m.editor.value())
	}
	m.editor.set("")

	act := func(id x.ActionID) tea.Cmd {
		t.Helper()
		at, ok := m.host.byID[id]
		if !ok {
			t.Fatalf("missing action %s", id)
		}
		return m.runAction(at, "")
	}
	_, readTurn := m.Update(act(switcherID)())
	threadTurnStep(t, m, readTurn)
	golden(t, "menu-switch", m)
	press(m, tea.KeyEscape, 0)
	m.finishWork(act(forkPickerID)().(workMsg))
	golden(t, "menu-fork", m)
	press(m, tea.KeyEscape, 0)
	m.finishWork(act(menusID + ".model")().(workMsg))
	golden(t, "menu-model", m)
	press(m, tea.KeyEnter, 0)
	golden(t, "menu-thinking", m)
	press(m, tea.KeyEscape, 0)
	m.finishWork(act(menusID + ".mode")().(workMsg))
	golden(t, "menu-mode", m)
	press(m, tea.KeyEscape, 0)
	for _, op := range []string{"archive", "reset", "compact"} {
		act(x.ActionID(menusID + "." + op))
		golden(t, "menu-"+op, m)
		press(m, tea.KeyEscape, 0)
	}
	act(paletteID)
	m.focusedDialog().query = "rename"
	m.focusedDialog().filter()
	m.acceptChoice(m.focusedDialog())
	if len(m.overlays) != 2 {
		t.Fatal("rename did not stack over the palette")
	}
	golden(t, "menu-nested-rename", m)
	press(m, tea.KeyEscape, 0)
	if len(m.overlays) != 1 || !m.overlays[0].palette {
		t.Fatal("Escape closed more than rename")
	}
	press(m, tea.KeyEscape, 0)
	m.options.Services = MenuServices{}
	if got := m.contextFor(s.info.ID).Features; got.Has(x.Files) || got.Has(x.Archive) || got.Has(x.Titles) || got.Has(x.Mode) {
		t.Fatal("unavailable services advertised")
	}
}

type modelMenuFixture struct{ menuBackend }

func (modelMenuFixture) Hello(context.Context) (sdk.Hello, error) {
	return sdk.Hello{Capabilities: map[string]uint64{"modelCatalog": sdk.ModelCatalogVersion}}, nil
}

func (modelMenuFixture) ModelCatalog(context.Context, sdk.ID) (sdk.ModelCatalog, error) {
	return fullModelMenuFixture(), nil
}

func (modelMenuFixture) ConfigureCatalogThread(context.Context, sdk.ID, *sdk.Model, string) error {
	return fmt.Errorf("unexpected configuration")
}

func fullModelMenuFixture() sdk.ModelCatalog {
	current := sdk.Model{Provider: "fixture", ModelID: "removed"}
	catalog := sdk.ModelCatalog{Current: &current, Default: sdk.Model{Provider: "fixture", ModelID: "model-01"}}
	catalog.Models = append(catalog.Models, sdk.ModelCatalogRow{Model: current, Label: "Removed model", Reason: ptr("missing_model")})
	for i := 1; i < sdk.ModelCatalogLimit; i++ {
		id := fmt.Sprintf("model-%02d", i)
		catalog.Models = append(catalog.Models, sdk.ModelCatalogRow{Model: sdk.Model{Provider: "fixture", ModelID: id},
			Label: "Fixture " + id, Available: true, ThinkingLevels: []string{"off", "low"}})
	}
	return catalog
}

type modelSearchFixture struct{ modelMenuFixture }

func (modelSearchFixture) Hello(context.Context) (sdk.Hello, error) {
	return sdk.Hello{Capabilities: map[string]uint64{"modelCatalog": sdk.ModelCatalogSearchVersion}}, nil
}

func (modelSearchFixture) SearchModelCatalog(ctx context.Context, thread sdk.ID, query string) (sdk.ModelCatalog, error) {
	if ctx.Err() != nil || thread != "1" {
		return sdk.ModelCatalog{}, sdk.ErrProtocol
	}
	catalog := fullModelMenuFixture()
	if query != "" {
		catalog.Models = nil
		if strings.HasPrefix("openrouter", strings.ToLower(query)) {
			catalog.Models = []sdk.ModelCatalogRow{{Model: sdk.Model{Provider: "openrouter", ModelID: "fixture/late"},
				Label: "Late model", Available: true, ThinkingLevels: []string{"off", "high"}}}
		}
	}
	return catalog, nil
}

func TestNativeModelSearchLatestQuery(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	m.backend = modelSearchFixture{}
	m.Update(m.menuProbe()())
	m.Update(tea.WindowSizeMsg{Width: 46, Height: 20})
	open := m.runAction(m.host.byID[modelPickerID], "")
	if m.sheet == nil || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Searching models") {
		t.Fatal("picker must be interactive before the server replies")
	}
	m.Update(open())
	if len(m.sheet.choices) != sdk.ModelCatalogLimit {
		t.Fatal("missing default rows")
	}
	_, old := m.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	oldToken := m.sheet.token
	_, latest := m.Update(tea.PasteMsg{Content: "penrouter"})
	if old() != nil {
		t.Fatal("superseded search still ran")
	}
	m.Update(modelSearchResult{token: oldToken, catalog: fullModelMenuFixture()})
	assertFrame(t, m)
	press(m, tea.KeyEnter, 0)
	if m.sheet == nil || len(m.sheet.choices) != 0 || m.sheet.query != "openrouter" {
		t.Fatal("stale rows were selectable while the latest search was pending")
	}
	m.Update(latest())
	if m.sheet.query != "openrouter" || len(m.sheet.choices) != 1 || m.sheet.choices[0].Label != "openrouter/fixture/late" {
		t.Fatal("search didn't replace the old page or preserve the query")
	}
	assertFrame(t, m)
	press(m, tea.KeyEnter, 0)
	if m.sheet == nil || len(m.sheet.choices) != 2 || m.sheet.choices[1].ID != "high" {
		t.Fatal("search result did not open the verified thinking tiers")
	}
	press(m, tea.KeyEscape, 0)
	late := m.runAction(m.host.byID[modelPickerID], "openrouter")
	if m.sheet.query != "openrouter" {
		t.Fatal("slash argument did not seed the query")
	}
	reply := late() // simulate a result received after Escape
	press(m, tea.KeyEscape, 0)
	m.Update(reply)
	if m.sheet != nil || len(m.pending) != 0 {
		t.Fatal("late result reopened the picker")
	}
}

func TestNativeModelPickerNarrowKeyboardAndBareHost(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	m.backend = menuBackend{}
	thinking := m.host.byID[x.ActionID(menusID+".thinking")]
	m.runAction(thinking, "")
	if m.notice.text != "Thinking levels unavailable on this host" || len(m.pending) != 0 || m.sheet != nil ||
		m.runAction(m.host.byID[modelPickerID], "") != nil {
		t.Fatal("bare host attempted configuration or model listing")
	}
	m.backend = modelMenuFixture{}
	m.Update(m.menuProbe()())
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 16})
	m.finishWork(m.runAction(m.host.byID[modelPickerID], "")().(workMsg))
	if m.sheet == nil || len(m.sheet.choices) != min(sdk.ModelCatalogLimit, maxChoices) || !strings.HasPrefix(m.sheet.choices[0].Label, "Current (unavailable)") {
		t.Fatal("native catalog did not retain its missing current row")
	}
	assertFrame(t, m)
	press(m, tea.KeyDown, 0)
	press(m, tea.KeyEnter, 0)
	if m.sheet == nil || len(m.sheet.choices) != 2 || m.sheet.choices[1].ID != "low" {
		t.Fatal("native picker did not open the model's level choices")
	}
	assertFrame(t, m)
	press(m, tea.KeyEscape, 0)
	m.finishWork(m.runAction(thinking, "")().(workMsg))
	if m.sheet != nil || !strings.Contains(m.notice.text, "Restore it") || len(m.pending) != 0 {
		t.Fatal("native thinking action did not handle the missing model")
	}
}

func BenchmarkNativeModelPickerFrame(b *testing.B) {
	m, err := New(b.Context(), modelMenuFixture{}, Options{ThreadID: "1", ReducedMotion: true})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	m.Update(m.menuProbe()())
	m.Update(tea.WindowSizeMsg{Width: 46, Height: 20})
	m.finishWork(m.runAction(m.host.byID[modelPickerID], "")().(workMsg))
	if m.sheet == nil {
		b.Fatal("missing full native model sheet")
	}
	b.ReportAllocs()
	for b.Loop() {
		m.Update(tea.KeyPressMsg{Code: '6', Text: "6"})
		_ = m.View()
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		_ = m.View()
	}
}

type directoryMenuBackend struct{ menuBackend }

func (directoryMenuBackend) ThreadDirectory(_ context.Context, limit int, cursor string) (sdk.ThreadDirectoryPage, error) {
	if limit != 50 {
		return sdk.ThreadDirectoryPage{}, sdk.ErrProtocol
	}
	stamp := uint64(time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC).UnixMilli())
	if cursor == "" {
		return sdk.ThreadDirectoryPage{Threads: []sdk.ThreadDirectoryRow{
			{ID: "17", Title: "Investigate a latency regression", LastMessageMS: stamp},
			{ID: "1", Title: "Fix the date picker", LastMessageMS: stamp - 86400000},
		}, NextCursor: ptr("fixture-cursor")}, nil
	}
	if cursor == "fixture-cursor" {
		return sdk.ThreadDirectoryPage{Threads: []sdk.ThreadDirectoryRow{{ID: "4", ParentID: "1", Title: "Earlier debugging session"}}}, nil
	}
	return sdk.ThreadDirectoryPage{}, sdk.ErrProtocol
}

func TestDirectorySheetShowsNamesAndDatesAndOpensOlderPage(t *testing.T) {
	m, state := demo(t, Options{ReducedMotion: true})
	state.lastAnswer = previewMarkdown
	m.backend = directoryMenuBackend{}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 25})
	open := m.runAction(m.host.byID[switcherID], "")
	threadPickerStep(t, m, open)
	frame := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "Investigate a latency regression") || !strings.Contains(frame, time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC).Local().Format("2006-01-02")+" · #17") ||
		!strings.Contains(frame, "Workspace unknown") || !strings.Contains(frame, "Fix the date picker") || m.sheet.choices[0].ID != "17" {
		t.Fatalf("wrong thread directory:\n%s", frame)
	}
	threadTurnStep(t, m, press(m, tea.KeyDown, 0))
	if detail := ansi.Strip(assertFrame(t, m)); !strings.Contains(detail, "Prompt: Open the fixture") || !strings.Contains(detail, "Reply:  Done") {
		t.Fatalf("missing the focused thread's last turn:\n%s", detail)
	}
	// Hosts without search still filter against the cached local answer.
	m.changeDialogQuery(m.sheet, "the docs")
	if item, ok := m.sheet.choice(); !ok || item.ID != "1" {
		t.Fatalf("local message filtering lost the cached answer: %+v", item)
	}
	m.changeDialogQuery(m.sheet, "")
	press(m, tea.KeyEnd, 0)
	next := m.acceptChoice(m.sheet)
	threadPickerStep(t, m, next)
	frame = ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "Earlier debugging session") || !strings.Contains(frame, "fork of #1") || strings.Contains(frame, "Older threads") {
		t.Fatalf("wrong older page:\n%s", frame)
	}
}

type menuBackend struct{ inertBackend }

func (menuBackend) ListThreads(context.Context, int, string) (sdk.ThreadPage, error) {
	return sdk.ThreadPage{Threads: []sdk.Thread{{ID: "1"}, {ID: "14", Parent: &sdk.ForkPoint{ThreadID: "1", AtEntryID: "2"}}}}, nil
}
func (menuBackend) History(_ context.Context, id sdk.ID, _ sdk.HistoryRange, _ int) (sdk.HistoryPage, error) {
	return sdk.HistoryPage{ThreadID: id, Entries: []sdk.Entry{entry(1, "user", "Open the fixture"), entry(2, "assistant", "Done")}}, nil
}

type newThreadMenuBackend struct {
	inertBackend
	cwd string
}

func (b *newThreadMenuBackend) NewThread(_ context.Context, options sdk.NewThread) (sdk.Thread, error) {
	b.cwd = options.CWD
	return sdk.Thread{ID: "2"}, nil
}

func TestNewThreadUsesLaunchDirectory(t *testing.T) {
	const launch = "/launch/project"
	m := modelFor(t, Options{ThreadID: "1", NewThread: sdk.NewThread{CWD: launch}, WorkspaceLocal: true, ReducedMotion: true})
	m.threads[m.active].info.Workspace = "/other/project"
	backend := &newThreadMenuBackend{}
	m.backend = backend

	m.editor.set("/new")
	workflowWork(t, m, press(m, tea.KeyEnter, 0))
	if backend.cwd != launch {
		t.Fatalf("/new used cwd %q, want launch directory %q", backend.cwd, launch)
	}
	if m.active != "2" {
		t.Fatalf("/new did not switch to the new thread: active %q", m.active)
	}
}

func TestSheetShowsCountAndFooterDoesNotSelect(t *testing.T) {
	m, _ := demo(t, Options{Intro: &IntroOptions{}})
	m.Update(tea.WindowSizeMsg{Width: 58, Height: 29})
	var choices []x.Choice
	for i := range 42 {
		choices = append(choices, x.Choice{ID: fmt.Sprint(i), Label: fmt.Sprintf("item %d", i+1)})
	}
	var picked string
	b := maxEffects
	m.openDialog(m.route(m.active, coreID), x.SelectDialog{Title: "Choices", Placement: x.Sheet,
		Choices: choices, Done: func(_ x.Context, r x.Reply[string]) x.Outcome { picked = r.Value; return nil }}, &b)
	d := m.sheet
	d.selected = 13
	frame := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"Type to filter", "↵ Choose  item 14", "↑↓ move", "esc back", "42 choices"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("sheet missing %q:\n%s", want, frame)
		}
	}
	f := m.frame
	click := func(y int) { m.mouse(tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: f.dialogX + 3, Y: y})) }
	click(f.dialogY + f.dialogHeight - 1)
	if picked != "" || m.sheet != d {
		t.Fatal("clicking the sheet footer selected an item")
	}
	for i, choice := range f.dialogChoices {
		if choice == 13 {
			click(f.dialogY + i)
			break
		}
	}
	if picked != "13" || m.sheet != nil {
		t.Fatalf("visible choice hit resolved to %q", picked)
	}
}

func TestCloseSlashClosesActiveTab(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	m.editor.set("draft in the other tab")
	m.switchThread("2")

	m.editor.set("/clos")
	press(m, tea.KeyTab, 0)
	if got := m.editor.value(); got != "/close " {
		t.Fatalf("close command did not autocomplete: %q", got)
	}
	press(m, tea.KeyEnter, 0)
	if m.quitting || m.active != "1" || !slices.Equal(m.tabs, []sdk.ID{"1"}) || m.threads["2"] != nil {
		t.Fatalf("expected only tab 2 to close; active=%q tabs=%v quitting=%v", m.active, m.tabs, m.quitting)
	}
	if got := m.editor.value(); got != "draft in the other tab" {
		t.Fatalf("switching back did not restore the other tab's draft: %q", got)
	}

	m.editor.set("/close")
	press(m, tea.KeyEnter, 0)
	if m.quitting || m.active != "" || len(m.tabs) != 0 || m.editor.value() != "" {
		t.Fatal("closing the last tab should return to the empty prompt without quitting")
	}
}

func TestSlashCompletionUsesUTF8ByteCursorAcrossWrap(t *testing.T) {
	var request x.CompletionRequest
	m, _ := demo(t, Options{Extensions: []x.Registration{{
		ID: "fixture/slash", APIVersion: x.APIVersion, Actions: []x.Action{{
			ID: "fixture/slash.complete", Title: "Complete fixture", Binding: x.Binding{Leader: "7 c"},
			Run: func(x.Context, x.Invocation) x.Outcome { return nil },
			Slash: &x.Slash{Name: "fixture", Complete: func(_ x.Context, r x.CompletionRequest) []x.Completion {
				request = r
				return []x.Completion{{Replace: x.Replacement{Start: r.Cursor - len("界"), End: r.Cursor, Text: "済"}}}
			}},
		}},
	}}})
	m.ensureThread("2")
	m.openedTab("2")
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 18})
	const command = "/fixture  "
	for _, prefix := range []string{"e\u0301界", strings.Repeat("wide 界 ", 6) + "e\u0301界"} {
		args := prefix + "suffix"
		m.editor.set(command + args)
		m.editor.area.SetCursorColumn(utf8.RuneCountInString(command + prefix))
		press(m, tea.KeyTab, 0)
		if request.Args != args || request.Cursor != len(prefix) {
			t.Fatalf("completion got %+v, want byte offset %d in %q", request, len(prefix), args)
		}
		want := command + strings.TrimSuffix(prefix, "界") + "済suffix"
		if m.active != "1" || m.editor.value() != want || m.editor.cursorByte() != len(strings.TrimSuffix(want, "suffix")) {
			t.Fatalf("expected edit at the cursor, got %q in thread %q", m.editor.value(), m.active)
		}
	}
}

func TestFilesFilterWhilePageLoading(t *testing.T) {
	var requests []x.ListFiles
	pending, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	m, _ := demo(t, Options{ReducedMotion: true, Services: MenuServices{Files: func(ctx context.Context, query x.ListFiles) ([]x.File, string, error) {
		requests = append(requests, query)
		switch {
		case query.Query == "src/":
			return []x.File{{Path: "src/app.go"}}, "", nil
		case query.Cursor != "":
			close(pending)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, "", ctx.Err()
			}
			return []x.File{{Path: "../../outside"}, {Path: "report.md"}}, "", nil
		default:
			return []x.File{{Path: "src", Directory: true}, {Path: "sample.txt"}, {Path: "main.go"}}, "page-two", nil
		}
	}}})
	m.editor.set("check @")
	load := m.refreshFilePicker()
	if load == nil || len(requests) != 0 {
		t.Fatal("network was called on update")
	}
	next := m.finishWork(load().(workMsg))
	if next == nil || !m.files.waiting {
		t.Fatal("missing pending page")
	}
	_, _, ok := m.fileSuggestions()
	if !ok {
		t.Fatal("file suggestions not open")
	}
	// Hold page two in the host while typing against first-page results.
	result := make(chan workMsg, 1)
	go func() { result <- next().(workMsg) }()
	select {
	case <-pending:
	case <-time.After(time.Second):
		t.Fatal("second page did not reach the host")
	}
	m.editor.set("check @ma")
	if m.refreshFilePicker() != nil {
		t.Fatal("query should not start another network request")
	}
	_, matched, _ := m.fileSuggestions()
	if len(matched) != 1 || matched[0].Path != "main.go" || len(requests) != 2 {
		t.Fatalf("bad local filter: %v; requests %v", matched, requests)
	}
	golden(t, "menu-files-loading", m)
	m.promptKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	close(release)
	m.finishWork(<-result)
	if _, _, open := m.fileSuggestions(); open {
		t.Fatal("late page reopened suggestions after Escape")
	}
	m.editor.set("check @sr")
	m.refreshFilePicker()
	if _, _, open := m.fileSuggestions(); !open {
		t.Fatal("editing did not reopen suggestions")
	}
	inside := m.replaceFile("src")
	if inside == nil || m.editor.value() != "check @src/" {
		t.Fatalf("directory completion: %q", m.editor.value())
	}
	m.finishWork(inside().(workMsg))
	golden(t, "menu-files-directory", m)
	m.replaceFile("src/app.go")
	if m.editor.value() != "check @src/app.go " {
		t.Fatalf("file completion: %q", m.editor.value())
	}
	m.editor.set("again @src/app")
	m.refreshFilePicker()
	m.promptKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if _, _, ok := m.fileSuggestions(); ok {
		t.Fatal("Escape did not hide the suggestion list")
	}
	if got := len(requests); got != 3 || !slices.Equal([]string{requests[0].Query, requests[1].Cursor, requests[2].Query}, []string{"", "page-two", "src/"}) {
		t.Fatalf("requests %+v", requests)
	}
}
