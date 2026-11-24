package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type fileScopeFixture struct {
	inertBackend
	capable bool
	scope   sdk.WorkspaceFileScope
	list    func(context.Context, sdk.ID, string, string, int, string) (sdk.WorkspaceFilePage, error)
}

func (f fileScopeFixture) Hello(context.Context) (sdk.Hello, error) {
	caps := map[string]uint64{}
	if f.capable {
		caps["workspaceFiles"] = sdk.WorkspaceFilesVersion
	}
	return sdk.Hello{Capabilities: caps}, nil
}
func (f fileScopeFixture) WorkspaceFileScope(_ context.Context, id sdk.ID) (sdk.WorkspaceFileScope, error) {
	if id == "1" {
		return f.scope, nil
	}
	return sdk.WorkspaceFileScope{}, nil
}
func (f fileScopeFixture) ListWorkspaceFiles(ctx context.Context, id sdk.ID, root, dir string, limit int, cursor string) (sdk.WorkspaceFilePage, error) {
	return f.list(ctx, id, root, dir, limit, cursor)
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
}

func TestNativeFilesEditorTokenAndHomeScope(t *testing.T) {
	pending := make(chan struct{})
	m, s := demo(t, Options{ReducedMotion: true})
	fixture := fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true, Home: true},
		list: func(ctx context.Context, owner sdk.ID, root, dir string, limit int, cursor string) (sdk.WorkspaceFilePage, error) {
			if owner != "1" || limit != 100 {
				t.Errorf("unexpected owner/limit: %s %d", owner, limit)
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
