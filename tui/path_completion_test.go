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

func pathCompletionPage(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("no listing command")
	}
	msg := cmd()
	if _, ok := msg.(nativeFilePage); !ok {
		t.Fatalf("expected a directory reply, got %T", msg)
	}
	m.Update(msg)
}

func checkPathChoices(t *testing.T, m *Model, want ...string) {
	t.Helper()
	_, choices, open := m.fileSuggestions()
	var got []string
	for _, c := range choices {
		got = append(got, c.Path)
	}
	if !open || !slices.Equal(got, want) {
		t.Fatalf("picker open=%t, choices=%q, want %q", open, got, want)
	}
}

func TestPathCompletionHome(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	var listed []string
	openBrowseScope(t, m, fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Home: true},
		list: func(_ context.Context, target sdk.WorkspaceFileTarget, root, dir string, _ int, _ string) (sdk.WorkspaceFilePage, error) {
			if target.ThreadID != "1" || root != "home" {
				t.Fatalf("unexpected listing target %+v, root %q", target, root)
			}
			listed = append(listed, dir)
			if dir == "" {
				return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "source files", Directory: true}}}, nil
			}
			return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "notes.md"}}}, nil
		}})
	m.editor.set("Read  then summarize")
	m.editor.area.SetCursorColumn(len("Read "))
	_, cmd := m.Update(tea.KeyPressMsg{Code: '~', Text: "~"})
	pathCompletionPage(t, m, cmd)
	checkPathChoices(t, m, "~/source files")
	cmd = press(m, tea.KeyTab, 0)
	if got := m.editor.value(); got != `Read "~/source files/" then summarize` || m.editor.cursorByte() != len(`Read "~/source files/`) {
		t.Fatalf("folder completion or cursor: %q (%d)", got, m.editor.cursorByte())
	}
	pathCompletionPage(t, m, cmd)
	checkPathChoices(t, m, "~/source files/notes.md")
	press(m, tea.KeyEnter, 0)
	if got := m.editor.value(); got != `Read "~/source files/notes.md" then summarize` || s.sending {
		t.Fatalf("file completion changed the suffix or submitted: %q", got)
	}
	if !slices.Equal(listed, []string{"", "source files"}) {
		t.Fatalf("listed %q", listed)
	}
}

func TestPathCompletionWorkspace(t *testing.T) {
	for _, draft := range []string{"Read /", "/workspace/src/"} {
		t.Run(draft, func(t *testing.T) {
			m, s := demo(t, Options{ReducedMotion: true})
			var dirs []string
			openBrowseScope(t, m, fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true, Home: true},
				list: func(_ context.Context, target sdk.WorkspaceFileTarget, root, dir string, _ int, _ string) (sdk.WorkspaceFilePage, error) {
					if target.ThreadID != "1" || root != "workspace" {
						t.Fatalf("unexpected listing target %+v, root %q", target, root)
					}
					dirs = append(dirs, dir)
					if dir == "" {
						return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "src", Directory: true}}}, nil
					}
					return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "main.go"}}}, nil
				}})
			m.editor.set(draft)
			cmd := m.refreshFilePicker()
			prefix := ""
			wantDirs := []string{"src"}
			if draft == "Read /" {
				if cmd != nil {
					t.Fatal("root choices must not list the filesystem")
				}
				checkPathChoices(t, m, workspaceRootChoice, homeRootChoice)
				pathCompletionPage(t, m, press(m, tea.KeyTab, 0))
				checkPathChoices(t, m, "src")
				cmd = press(m, tea.KeyEnter, 0)
				prefix = "Read "
				wantDirs = []string{"", "src"}
			}
			if got := m.editor.value(); got != prefix+"/workspace/src/" || s.sending {
				t.Fatalf("folder completion: %q", got)
			}
			pathCompletionPage(t, m, cmd)
			checkPathChoices(t, m, "src/main.go")
			// A preserved suffix must not keep selecting an already completed file.
			m.editor.set(prefix + "/workspace/src/ma then summarize")
			m.editor.area.SetCursorColumn(len(prefix + "/workspace/src/ma"))
			if cmd := m.refreshFilePicker(); cmd != nil {
				t.Fatal("filtering an open directory should not make a new request")
			}
			press(m, tea.KeyEnter, 0)
			if got := m.editor.value(); got != prefix+"/workspace/src/main.go then summarize" || s.sending {
				t.Fatalf("file completion changed the suffix or submitted: %q", got)
			}
			if !slices.Equal(dirs, wantDirs) {
				t.Fatalf("listed %q; want %q", dirs, wantDirs)
			}
			press(m, tea.KeyEnter, 0)
			if !s.sending {
				t.Fatal("Enter after completion did not send the draft")
			}
		})
	}
}

func TestPathCompletionLeadingSlash(t *testing.T) {
	for _, selectWith := range []string{"tab", "enter", "click"} {
		t.Run(selectWith, func(t *testing.T) {
			m, _ := demo(t, Options{ReducedMotion: true, Fills: FillOff})
			m.Update(tea.WindowSizeMsg{Width: 72, Height: 18})
			m.editor.set("/")
			defaultAction := m.slashMatches()[0].action.ID
			openBrowseScope(t, m, fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Home: true}})
			frame := ansi.Strip(assertFrame(t, m))
			if !strings.Contains(frame, "Browse paths…") || m.slashMatches()[0].action.ID != defaultAction {
				t.Fatalf("missing browse entry or changed default:\n%s", frame)
			}
			if _, _, open := m.fileSuggestions(); open {
				t.Fatal("a leading slash must open commands by default")
			}
			switch selectWith {
			case "click":
				for y, row := range strings.Split(frame, "\n") {
					if strings.Contains(row, "Browse paths…") {
						m.Update(tea.MouseClickMsg(tea.Mouse{X: 2, Y: y, Button: tea.MouseLeft}))
						break
					}
				}
			case "tab", "enter":
				press(m, tea.KeyDown, 0)
				m.Update(paletteShellKey(selectWith))
			}
			checkPathChoices(t, m, homeRootChoice)
			press(m, tea.KeyEscape, 0)
			if _, _, open := m.fileSuggestions(); open || len(m.slashMatches()) != 0 || m.editor.value() != "/" {
				t.Fatal("Escape must close the picker without changing the draft or running a command")
			}
			press(m, tea.KeyBackspace, 0)
			m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
			if _, _, open := m.fileSuggestions(); open || len(m.slashMatches()) == 0 {
				t.Fatal("retyping a leading slash must return to commands")
			}
		})
	}
}

func TestPathCompletionScopesAndCancellation(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	fixture := fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true, Home: true},
		list: func(_ context.Context, _ sdk.WorkspaceFileTarget, root, dir string, _ int, _ string) (sdk.WorkspaceFilePage, error) {
			if root != "home" || dir != "" {
				t.Fatalf("unexpected listing %q %q", root, dir)
			}
			return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "home.md"}}}, nil
		}}
	openBrowseScope(t, m, fixture)
	noPicker := func(draft string) {
		t.Helper()
		m.editor.set(draft)
		cmd := m.refreshFilePicker()
		if _, _, open := m.fileSuggestions(); open || cmd != nil {
			t.Fatalf("%q started a browse outside the allowed paths", draft)
		}
	}
	for _, draft := range []string{"Read /etc/", "Read /workspace-other/", "Read /workspace/../", "Read /workspace/~/",
		"Read ~/../", "~other/", "https://example.org/", "/not-a-command"} {
		noPicker(draft)
	}
	m.editor.set("Read /")
	m.refreshFilePicker()
	press(m, tea.KeyDown, 0)
	cmd := press(m, tea.KeyEnter, 0)
	if m.editor.value() != "Read ~/" || cmd == nil {
		t.Fatal("home location did not start a home listing")
	}
	ctx := m.files.request
	late := cmd()
	m.applyFileScope(sdk.WorkspaceFileScope{Workspace: true}, "revoked-home")
	if ctx.Err() == nil {
		t.Fatal("revoking a grant did not cancel the listing")
	}
	m.Update(late)
	noPicker("Read ~/")
	m.editor.set("Read /")
	m.refreshFilePicker()
	checkPathChoices(t, m, workspaceRootChoice)
	m.applyFileScope(sdk.WorkspaceFileScope{}, "revoked-all")
	noPicker("Read /")
	noPicker("Read /workspace/src/")
	m.editor.set("/")
	for _, a := range m.slashMatches() {
		if a.action.ID == browsePathsID {
			t.Fatal("command menu advertised a path picker without a grant")
		}
	}
}

func TestPathCompletionDoesNotUseCustomFileService(t *testing.T) {
	m, _ := demo(t, Options{Services: MenuServices{Files: func(context.Context, x.ListFiles) ([]x.File, string, error) {
		t.Fatal("custom file service got a native root request")
		return nil, "", nil
	}}})
	for _, draft := range []string{"~", "~/", "Read /", "Read /workspace/"} {
		m.editor.set(draft)
		cmd := m.refreshFilePicker()
		if _, _, open := m.fileSuggestions(); open || cmd != nil {
			t.Fatalf("%q opened a picker without a native scope", draft)
		}
	}
}
