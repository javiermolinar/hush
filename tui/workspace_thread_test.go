package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

// Both checkouts belong to the same workspace, even when just one matches the
// thread search. Tests can also use these separately on successive pages.
func deploymentPage(cwds ...string) sdk.ThreadDirectoryPage {
	page := groupedThreadPage()
	out := sdk.ThreadDirectoryPage{}
	for i, row := range page.Threads {
		if slices.Contains(cwds, row.Checkout.CWD) {
			out.Threads = append(out.Threads, row)
			out.Workspaces = append(out.Workspaces, page.Workspaces[i])
			out.WorkspaceMatches = append(out.WorkspaceMatches, "inside")
		}
	}
	return out
}

type workspaceCreationBackend struct {
	threadPickerBackend
	created []sdk.NewThread
}

func (b *workspaceCreationBackend) NewThread(_ context.Context, opts sdk.NewThread) (sdk.Thread, error) {
	b.created = append(b.created, opts)
	return sdk.Thread{ID: "7"}, nil
}

func workspaceCreationModel(t *testing.T, list func(sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error)) (*Model, *workspaceCreationBackend) {
	t.Helper()
	m, _ := demo(t, Options{NewThread: sdk.NewThread{CWD: "/launch/hush"}, WorkspaceLocal: true, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 88, Height: 23})
	b := &workspaceCreationBackend{threadPickerBackend: threadPickerBackend{list: list}}
	m.backend = b
	return m, b
}

func TestWorkspaceNewThreadHereKeyboard(t *testing.T) {
	m, b := workspaceCreationModel(t, func(opts sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error) {
		switch opts {
		case sdk.ThreadDirectoryOptions{Workspace: "/launch/hush", Limit: 50, BrowseAll: true}:
			return groupedThreadPage(), nil
		case sdk.ThreadDirectoryOptions{Workspace: "/launch/hush", Limit: 50, BrowseAll: true, Query: "deploymen"}:
			page := deploymentPage(deployCWD)
			page.WorkspaceMatches[0] = "outside"
			return page, nil
		case sdk.ThreadDirectoryOptions{Workspace: deployCWD, Limit: 50}:
			return deploymentPage(deployCWD), nil
		default:
			t.Fatalf("unexpected directory request: %+v", opts)
			return sdk.ThreadDirectoryPage{}, sdk.ErrProtocol
		}
	})
	m.ensureThread("4").info.Workspace = "/not-the-recorded-directory"
	m.editor.set("unsent draft")
	threadPickerStep(t, m, m.runAction(m.host.byID[switchAllID], ""))
	d := m.sheet
	stale := m.changeDialogQuery(d, "deployme")
	_, search := m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if d.query != "deploymen" || stale() != nil || len(b.created) != 0 {
		t.Fatal("plain n should only edit the filter and replace the previous search")
	}
	threadPickerStep(t, m, search)
	if c, _ := d.choice(); c.ID != "4" {
		t.Fatalf("search default should open an existing thread: %+v", c)
	}
	press(m, tea.KeyUp, 0)
	if c, _ := d.choice(); c.ID != newThreadHereChoice || c.Description != deployCWD {
		t.Fatalf("Up did not select a new thread in the recorded directory: %+v", c)
	}
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "› + New thread here  "+deployCWD) || !strings.Contains(frame, "ctrl+x n new here") {
		t.Fatalf("missing creation row or keyboard hint:\n%s", frame)
	}
	lookup := press(m, tea.KeyEnter, 0)
	if m.focusedDialog() == d || len(b.created) != 0 {
		t.Fatal("Enter should check the destination off the UI loop before creating")
	}
	workflowWork(t, m, threadPickerStep(t, m, lookup))
	if len(b.created) != 1 || b.created[0].CWD != deployCWD || m.active != "7" || m.sheet != nil ||
		m.focusedDialog() != nil || m.threads["1"].draft != "unsent draft" {
		t.Fatalf("create did not use the selected workspace or preserve the old draft: active=%s created=%+v", m.active, b.created)
	}
}

func TestWorkspaceNewThreadHereChoosesDirectory(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "available"
		if stale {
			name = "moved"
		}
		t.Run(name, func(t *testing.T) {
			m, b := workspaceCreationModel(t, func(opts sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error) {
				switch opts {
				case sdk.ThreadDirectoryOptions{Workspace: "/launch/hush", Limit: 50, BrowseAll: true}:
					return groupedThreadPage(), nil
				case sdk.ThreadDirectoryOptions{Workspace: "/launch/hush", Limit: 50, BrowseAll: true, Query: "Rollout"}:
					page := deploymentPage(deployCWD)
					page.WorkspaceMatches[0] = "outside"
					return page, nil
				case sdk.ThreadDirectoryOptions{Workspace: deployCWD, Limit: 50}:
					page := deploymentPage(deployCWD)
					page.NextCursor = ptr("older")
					return page, nil
				case sdk.ThreadDirectoryOptions{Workspace: deployCWD, Limit: 50, Cursor: "older"}:
					return deploymentPage(deployCheckout), nil
				case sdk.ThreadDirectoryOptions{Workspace: deployCheckout, Limit: 1}:
					page := deploymentPage(deployCheckout)
					if stale {
						page.Workspaces[0] = &sdk.ThreadWorkspace{ID: "another-workspace", Name: "Not deployment"}
					}
					return page, nil
				default:
					t.Fatalf("unexpected directory request: %+v", opts)
					return sdk.ThreadDirectoryPage{}, sdk.ErrProtocol
				}
			})
			threadPickerStep(t, m, m.runAction(m.host.byID[switchAllID], ""))
			parent := m.sheet
			threadPickerStep(t, m, m.changeDialogQuery(parent, "Rollout"))
			open := func() tea.Cmd {
				press(m, 'x', tea.ModCtrl)
				return press(m, 'n', 0)
			}
			lookup := open()
			if m.focusedDialog() == parent || lookup == nil {
				t.Fatal("Ctrl+X N did not open the working-directory picker")
			}
			press(m, tea.KeyEscape, 0)
			if lookup() != nil || m.focusedDialog() != parent || parent.query != "Rollout" {
				t.Fatal("Escape should cancel the lookup and return to the thread filter")
			}
			if cmd := threadPickerStep(t, m, open()); cmd != nil || len(b.created) != 0 || m.focusedDialog() == parent {
				t.Fatal("a single directory on a partial page must not create a thread")
			}
			press(m, tea.KeyDown, 0)
			threadPickerStep(t, m, press(m, tea.KeyEnter, 0))
			if c, _ := m.focusedDialog().choice(); c.Label != deployCheckout {
				t.Fatalf("a directory omitted by search wasn't offered: %+v", c)
			}
			workflowWork(t, m, press(m, tea.KeyEnter, 0))
			if stale {
				if len(b.created) != 0 || m.active != "1" || !strings.Contains(m.notice.text, "no thread created") {
					t.Fatalf("created in a different workspace after the directory moved: %+v", b.created)
				}
			} else if len(b.created) != 1 || b.created[0].CWD != deployCheckout || m.active != "7" {
				t.Fatalf("did not create in the explicitly chosen checkout: %+v", b.created)
			}
		})
	}
}

func TestWorkspaceNewThreadHereNeedsKnownWorkspace(t *testing.T) {
	m, b := workspaceCreationModel(t, func(sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error) {
		page := deploymentPage(deployCWD)
		page.Workspaces[0] = nil
		page.WorkspaceMatches[0] = "unknown"
		return page, nil
	})
	threadPickerStep(t, m, m.runAction(m.host.byID[switchAllID], ""))
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Workspace unknown") || strings.Contains(frame, "New thread") {
		t.Fatalf("unknown workspace should not offer creation:\n%s", frame)
	}
	press(m, 'x', tea.ModCtrl)
	if cmd := press(m, 'n', 0); cmd != nil || m.focusedDialog() != m.sheet || len(b.created) != 0 {
		t.Fatal("new thread binding must not fall back to /new in an unknown workspace")
	}
}
