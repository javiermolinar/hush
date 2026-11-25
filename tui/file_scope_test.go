package tui

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type fileScopeFixture struct {
	inertBackend
	capable, projectPaths bool
	scope                 sdk.WorkspaceFileScope
	list                  func(context.Context, sdk.WorkspaceFileTarget, string, string, int, string) (sdk.WorkspaceFilePage, error)
	search                func(context.Context, sdk.WorkspaceFileTarget, string, int) (sdk.WorkspaceFileSearchResult, error)
}

func (f fileScopeFixture) Hello(context.Context) (sdk.Hello, error) {
	caps := map[string]uint64{}
	if f.capable {
		caps["workspaceFiles"] = sdk.WorkspaceFilesVersion
		if f.search != nil {
			caps["workspaceFiles"] = sdk.WorkspaceFileSearchVersion
		}
		if f.projectPaths {
			caps["workspaceFiles"] = sdk.WorkspaceFileProjectPathsVersion
		}
	}
	return sdk.Hello{Capabilities: caps}, nil
}

// Thread 1 and the fresh session's launch directory share the fixture scope.
func (f fileScopeFixture) WorkspaceFileScope(_ context.Context, target sdk.WorkspaceFileTarget) (sdk.WorkspaceFileScope, error) {
	if target == (sdk.WorkspaceFileTarget{ThreadID: "1"}) || target == (sdk.WorkspaceFileTarget{CWD: "/workspace"}) {
		return f.scope, nil
	}
	return sdk.WorkspaceFileScope{}, nil
}
func (f fileScopeFixture) ListWorkspaceFiles(ctx context.Context, target sdk.WorkspaceFileTarget, root, dir string, limit int, cursor string) (sdk.WorkspaceFilePage, error) {
	return f.list(ctx, target, root, dir, limit, cursor)
}

func (f fileScopeFixture) SearchWorkspaceFiles(ctx context.Context, target sdk.WorkspaceFileTarget, query string, limit int) (sdk.WorkspaceFileSearchResult, error) {
	return f.search(ctx, target, query, limit)
}

func openBrowseScope(t testing.TB, m *Model, fixture fileScopeFixture) {
	t.Helper()
	m.backend = fixture
	probe := m.menuProbe()().(menuCapabilities)
	_, cmd := m.Update(probe)
	if fixture.capable {
		if cmd == nil {
			t.Fatal("no workspace scope request")
		}
		m.Update(cmd().(fileScopeMsg))
	}
}

func TestGoldenWorkspaceFilesHint(t *testing.T) {
	for _, scenario := range []struct {
		name                           string
		capable, allowed, narrow, hint bool
	}{
		{"available", true, true, false, true}, {"older-host", false, false, false, false},
		{"unauthorized", true, false, false, false}, {"narrow", true, true, true, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
			width, height := 82, 20
			if scenario.narrow {
				width, height, s.following = 40, 12, false
			}
			m.Update(tea.WindowSizeMsg{Width: width, Height: height})
			openBrowseScope(t, m, fileScopeFixture{capable: scenario.capable, scope: sdk.WorkspaceFileScope{Workspace: scenario.allowed}})
			frame := ansi.Strip(assertFrame(t, m))
			if strings.Contains(frame, "@ files") != scenario.hint || !scenario.hint && strings.Contains(frame, "@ file") {
				// The demo's @app path is in the transcript; only a full hint is allowed in the rule.
				t.Fatal(frame)
			}
			golden(t, "files-hint-"+scenario.name, m)
		})
	}
}

func TestQuotedWorkspaceFileLinks(t *testing.T) {
	m, _ := demo(t, Options{})
	text := m.linkify(`Open @"sub folder/my file.md", not @"~/private file.md".`, "/workspace")
	if !strings.Contains(text, "file:///workspace/sub%20folder/my%20file.md") || strings.Contains(text, "file:///workspace/~/") {
		t.Fatal(text)
	}
}

func testFilePickerFrameGeometry(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true, Services: MenuServices{
		Files: func(context.Context, x.ListFiles) ([]x.File, string, error) { return nil, "", nil },
	}})
	m.editor.set(`look @"folder space/file`)
	m.files = filePicker{owner: m.active, generation: m.token(), start: 5, prefix: "folder space/",
		records: []x.File{{Path: "folder space/file with wide characters 界界.go"}}}
	for _, w := range []int{40, 64, 100} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 12})
		assertFrame(t, m)
	}

	m, s := demo(t, Options{ReducedMotion: true})
	s.info.Workspace = "/work/" + strings.Repeat("wide 界", 12)
	openBrowseScope(t, m, fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true, Home: true}})
	m.editor.set("/")
	m.openPathRoots()
	for _, w := range []int{40, 64, 100} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 12})
		assertFrame(t, m)
	}
}

func TestNativeFileSearchDebouncesCancelsAndCompletes(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 75, Height: 20})
	started := make(chan struct{})
	const target = "tempo.fork/modules/source folder/generator.go"
	fixture := fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true},
		search: func(ctx context.Context, scope sdk.WorkspaceFileTarget, query string, limit int) (sdk.WorkspaceFileSearchResult, error) {
			if scope != (sdk.WorkspaceFileTarget{ThreadID: "1"}) || limit != sdk.WorkspaceFileSearchLimit {
				t.Errorf("unexpected target/limit %+v %d", scope, limit)
			}
			if query == "gen" {
				close(started)
				<-ctx.Done()
				return sdk.WorkspaceFileSearchResult{}, ctx.Err()
			}
			if query != "generator.go" {
				t.Errorf("unexpected query %q", query)
			}
			var matches []sdk.WorkspaceFileMatch
			for i := range 5 {
				matches = append(matches, sdk.WorkspaceFileMatch{Path: fmt.Sprintf("repo%d/generator.go", i)})
			}
			matches = append(matches, sdk.WorkspaceFileMatch{Path: target})
			return sdk.WorkspaceFileSearchResult{Matches: matches, Partial: true}, nil
		}}
	openBrowseScope(t, m, fixture)
	m.editor.set("look @g")
	first := m.refreshFilePicker()
	m.editor.set("look @gen")
	second := m.refreshFilePicker()
	if first == nil || second == nil || first() != nil {
		t.Fatal("superseded debounced command wasn't cancelled")
	}
	settled := make(chan tea.Msg, 1)
	go func() { settled <- second() }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("search never reached the backend")
	}
	old := m.files.generation
	m.editor.set("look @generator.go then keep this")
	end := len("look @generator.go")
	m.editor.replaceAt(5, end, "@generator.go", end-5)
	cmd := m.refreshFilePicker()
	select {
	case reply := <-settled:
		if reply != nil {
			t.Fatal("cancelled search returned data")
		}
	case <-time.After(time.Second):
		t.Fatal("stale search didn't cancel")
	}
	m.receiveNativeFileSearch(nativeFileSearchMsg{generation: old, result: sdk.WorkspaceFileSearchResult{
		Matches: []sdk.WorkspaceFileMatch{{Path: "stale/generator.go"}},
	}})
	if _, choices, ok := m.fileSuggestions(); !ok || len(choices) != 0 || !m.files.waiting {
		t.Fatalf("stale matches were selectable: %v, %t", choices, ok)
	}
	m.Update(cmd().(nativeFileSearchMsg))
	for range 5 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	frame := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "partial results") || !strings.Contains(frame, "› "+target) {
		t.Fatal(frame)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if got := m.editor.value(); got != `look @"`+target+`" then keep this` || threadBusy(s) {
		t.Fatalf("file completion changed the suffix or submitted work: %q", got)
	}
}

func TestNativeFilesEditorTokenAndHomeScope(t *testing.T) {
	pending := make(chan struct{})
	m, s := demo(t, Options{ReducedMotion: true})
	fixture := fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true, Home: true},
		list: func(ctx context.Context, target sdk.WorkspaceFileTarget, root, dir string, limit int, cursor string) (sdk.WorkspaceFilePage, error) {
			if target != (sdk.WorkspaceFileTarget{ThreadID: "1"}) || limit != 100 {
				t.Errorf("unexpected target/limit: %+v %d", target, limit)
			}
			switch {
			case cursor != "":
				close(pending)
				<-ctx.Done()
				return sdk.WorkspaceFilePage{}, ctx.Err()
			case root == "home":
				return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "home file"}}}, nil
			case dir == "source files":
				return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "nested file.go"}}}, nil
			default:
				return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "source files", Directory: true}}, NextCursor: "next"}, nil
			}
		}}
	openBrowseScope(t, m, fixture)
	_, cmd := m.Update(tea.KeyPressMsg{Code: '@', Text: "@"})
	if cmd == nil || m.editor.value() != "@" || !m.files.waiting || threadBusy(s) {
		t.Fatal("@ did not open a passive picker")
	}
	first := cmd().(nativeFilePage)
	second := m.receiveNativeFilePage(first)
	if second == nil {
		t.Fatal("no next page")
	}
	settled := make(chan tea.Msg, 1)
	go func() { settled <- second() }()
	select {
	case <-pending:
	case <-time.After(time.Second):
		t.Fatal("missing page request")
	}
	m.editor.set("look @source then keep this")
	m.editor.replaceAt(len("look "), len("look @source"), "@source", len("@source"))
	// Move to a different token occurrence. The first pending RPC must cancel.
	cmd = m.refreshFilePicker()
	select {
	case reply := <-settled:
		if reply != nil {
			t.Fatal("cancelled page returned data")
		}
	case <-time.After(time.Second):
		t.Fatal("stale listing was not cancelled")
	}
	m.receiveNativeFilePage(cmd().(nativeFilePage))
	cmd = m.replaceFile("source files")
	if got := m.editor.value(); got != `look @"source files/" then keep this` {
		t.Fatal(got)
	}
	m.receiveNativeFilePage(cmd().(nativeFilePage))
	m.replaceFile("source files/nested file.go")
	if got := m.editor.value(); got != `look @"source files/nested file.go" then keep this` {
		t.Fatal(got)
	}
	if threadBusy(s) {
		t.Fatal("completion submitted work")
	}
	m.editor.set("@~/")
	cmd = m.refreshFilePicker()
	m.receiveNativeFilePage(cmd().(nativeFilePage))
	if _, choices, ok := m.fileSuggestions(); !ok || len(choices) != 1 || choices[0].Path != "~/home file" {
		t.Fatalf("home choices: %v, %t", choices, ok)
	}
	_ = m.switchThread("2")
	m.connection = x.Connected
	cmd = m.syncFileScope()
	m.Update(cmd().(fileScopeMsg))
	m.receiveNativeFilePage(first) // stale reply from first thread
	if m.fileAvailable("") || strings.Contains(ansi.Strip(assertFrame(t, m)), "@ files") {
		t.Fatal("file scope leaked across thread switch")
	}
}

// A fresh session's file backend that must never be asked for a thread.
type freshSessionFiles struct {
	fileScopeFixture
	created *atomic.Int32
}

func (f freshSessionFiles) NewThread(context.Context, sdk.NewThread) (sdk.Thread, error) {
	f.created.Add(1)
	return sdk.Thread{}, x.ErrUnavailable
}

// Opens the picker scope the way a fresh session does: the probe arrives with
// no thread, so no watch ever reports Connected.
func openFreshScope(t testing.TB, m *Model, fixture freshSessionFiles) {
	t.Helper()
	m.backend = fixture
	m.Update(m.menuProbe()().(menuCapabilities))
	if m.fileScope.generation != 0 {
		m.Update(m.fileScopeCommand()().(fileScopeMsg))
	}
}

func TestFreshSessionBrowsesProspectiveWorkspace(t *testing.T) {
	var created atomic.Int32
	const workflow = "delete-old-tempo-releases.yaml"
	const found = ".github/workflows/" + workflow
	prospective := sdk.WorkspaceFileTarget{CWD: "/workspace"}
	fixture := freshSessionFiles{fileScopeFixture{capable: true, projectPaths: true, scope: sdk.WorkspaceFileScope{Workspace: true},
		list: func(_ context.Context, target sdk.WorkspaceFileTarget, root, dir string, _ int, _ string) (sdk.WorkspaceFilePage, error) {
			if target != prospective || root != "workspace" || dir != "" {
				t.Errorf("unexpected listing %+v %q %q", target, root, dir)
			}
			return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "README.md"}}}, nil
		},
		search: func(_ context.Context, target sdk.WorkspaceFileTarget, query string, _ int) (sdk.WorkspaceFileSearchResult, error) {
			if target != prospective || query != workflow {
				t.Errorf("unexpected search %+v %q", target, query)
			}
			return sdk.WorkspaceFileSearchResult{Matches: []sdk.WorkspaceFileMatch{{Path: found}}}, nil
		}}, &created}
	m := modelFor(t, Options{ReducedMotion: true, WorkspaceLocal: true})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	openFreshScope(t, m, fixture)
	if m.fileScope.target != prospective || !m.fileAvailable("") {
		t.Fatalf("no prospective scope: %+v", m.fileScope)
	}
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "@ files") {
		t.Fatal(frame)
	}

	m.editor.set("look @")
	first := m.refreshFilePicker()().(nativeFilePage)
	m.receiveNativeFilePage(first)
	if _, choices, ok := m.fileSuggestions(); !ok || len(choices) != 1 || choices[0].Path != "README.md" {
		t.Fatalf("browse choices: %v, %t", choices, ok)
	}
	m.editor.set("look @" + workflow)
	m.Update(m.refreshFilePicker()().(nativeFileSearchMsg))
	m.replaceFile(found)
	if got := m.editor.value(); got != "look @"+found+" " {
		t.Fatal(got)
	}
	m.editor.set("look /")
	m.refreshFilePicker()
	checkPathChoices(t, m, workspaceRootChoice)
	pathCompletionPage(t, m, press(m, tea.KeyTab, 0))
	checkPathChoices(t, m, "README.md")
	press(m, tea.KeyTab, 0)
	if got := m.editor.value(); got != "look /workspace/README.md " {
		t.Fatal(got)
	}
	if created.Load() != 0 || m.active != "" {
		t.Fatalf("the picker created a thread: calls=%d active=%q", created.Load(), m.active)
	}

	// Once the first message creates the thread, its own scope takes over and
	// replies for the prospective scope are dropped.
	_ = m.switchThread("1")
	m.connection = x.Connected
	m.Update(m.syncFileScope()().(fileScopeMsg))
	if m.fileScope.target != (sdk.WorkspaceFileTarget{ThreadID: "1"}) || !m.fileAvailable("") {
		t.Fatalf("thread scope: %+v", m.fileScope)
	}
	m.receiveNativeFilePage(first)
	if _, choices, _ := m.fileSuggestions(); len(choices) != 0 {
		t.Fatalf("prospective listing leaked into the thread: %v", choices)
	}
}

func TestFreshSessionFilePickerNeedsProspectiveScope(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		local, search, trusted bool
	}{{"older host", true, false, true}, {"remote", false, true, true}, {"untrusted", true, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var created atomic.Int32
			fixture := freshSessionFiles{fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: tc.trusted},
				list: func(context.Context, sdk.WorkspaceFileTarget, string, string, int, string) (sdk.WorkspaceFilePage, error) {
					t.Error("listed files without an authorized prospective scope")
					return sdk.WorkspaceFilePage{}, nil
				}}, &created}
			if tc.search {
				fixture.search = func(context.Context, sdk.WorkspaceFileTarget, string, int) (sdk.WorkspaceFileSearchResult, error) {
					t.Error("searched without an authorized prospective scope")
					return sdk.WorkspaceFileSearchResult{}, nil
				}
			}
			m := modelFor(t, Options{ReducedMotion: true, WorkspaceLocal: tc.local})
			m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
			openFreshScope(t, m, fixture)
			for _, draft := range []string{"look @", "look @read", "look /", "~", "look /workspace/"} {
				m.editor.set(draft)
				if cmd := m.refreshFilePicker(); cmd != nil || m.fileAvailable("") {
					t.Fatalf("%q opened the picker", draft)
				}
			}
			if created.Load() != 0 || m.active != "" {
				t.Fatalf("the picker created a thread: calls=%d active=%q", created.Load(), m.active)
			}
			m.editor.set("")
			if frame := ansi.Strip(assertFrame(t, m)); strings.Contains(frame, "@ files") {
				t.Fatal(frame)
			}
		})
	}
}
