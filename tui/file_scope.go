package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type fileBrowseBackend interface {
	WorkspaceFileScope(context.Context, sdk.ID) (sdk.WorkspaceFileScope, error)
	ListWorkspaceFiles(context.Context, sdk.ID, string, string, int, string) (sdk.WorkspaceFilePage, error)
}

type fileScopeLease struct {
	owner                  sdk.ID
	navigation, generation uint64
	scope                  sdk.WorkspaceFileScope
	ctx                    context.Context
	cancel                 context.CancelFunc
}

type fileScopeMsg struct {
	generation uint64
	scope      sdk.WorkspaceFileScope
}
type renewFileScope uint64

func (m *Model) stopFileScope() {
	if m.fileScope.cancel != nil {
		m.fileScope.cancel()
	}
	m.fileScope = fileScopeLease{}
	m.stopFileRequest()
}

func (m *Model) syncFileScope() tea.Cmd {
	if !m.nativeFiles || m.connection != x.Connected || !m.active.Valid() || m.options.Services.Files != nil {
		if m.fileScope.generation != 0 {
			m.stopFileScope()
		}
		return nil
	}
	if m.fileScope.owner == m.active && m.fileScope.navigation == m.navigation {
		return nil
	}
	m.stopFileScope()
	ctx, cancel := context.WithCancel(m.ctx)
	m.fileScope = fileScopeLease{owner: m.active, navigation: m.navigation, generation: m.token(), ctx: ctx, cancel: cancel}
	return m.fileScopeCommand()
}

func (m *Model) fileScopeCommand() tea.Cmd {
	backend, ok := m.backend.(fileBrowseBackend)
	if !ok {
		return nil
	}
	s := m.fileScope
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(s.ctx, menuTimeout+time.Second)
		defer cancel()
		scope, err := backend.WorkspaceFileScope(ctx, s.owner)
		if err != nil {
			scope = sdk.WorkspaceFileScope{}
		}
		if s.ctx.Err() != nil {
			return nil
		}
		return fileScopeMsg{s.generation, scope}
	}
}

func (m *Model) receiveFileScope(v fileScopeMsg) tea.Cmd {
	s := &m.fileScope
	if v.generation != s.generation || s.owner != m.active || s.navigation != m.navigation {
		return nil
	}
	if s.scope != v.scope {
		m.stopFileRequest()
		m.files = filePicker{}
	}
	s.scope = v.scope
	return tea.Batch(m.refreshFilePicker(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return renewFileScope(v.generation) }))
}

func (m *Model) currentFileScope() sdk.WorkspaceFileScope {
	if m.connection == x.Connected && m.fileScope.owner == m.active && m.fileScope.navigation == m.navigation {
		return m.fileScope.scope
	}
	return sdk.WorkspaceFileScope{}
}

func (m *Model) fileAvailable(dir string) bool {
	if m.options.Services.Files != nil {
		return true
	}
	s := m.currentFileScope()
	if strings.HasPrefix(dir, "~/") {
		return s.Home
	}
	return s.Workspace || dir == "" && s.Home
}

type nativeFilePage struct {
	generation uint64
	result     x.ClientResult
	cursor     string
	pages      int
}

func (m *Model) stopFileRequest() {
	if cancel := m.files.cancel; cancel != nil {
		cancel()
		m.files.cancel, m.files.request = nil, nil
		m.files.waiting = false
		m.files.generation = 0
	}
}

func browseFiles(ctx context.Context, backend fileBrowseBackend, owner sdk.ID, prefix, cursor string, limit int) x.ClientResult {
	ctx, cancel := context.WithTimeout(ctx, menuTimeout+time.Second)
	defer cancel()
	root, directory := "workspace", strings.TrimSuffix(prefix, "/")
	if strings.HasPrefix(prefix, "~/") {
		root, directory = "home", strings.TrimSuffix(strings.TrimPrefix(prefix, "~/"), "/")
	}
	page, err := backend.ListWorkspaceFiles(ctx, owner, root, directory, limit, cursor)
	result := x.ClientResult{Err: err, Cursor: page.NextCursor}
	for _, f := range page.Entries {
		if prefix == "" && f.Name == "~" {
			continue
		} // reserved home selector
		result.Files = append(result.Files, x.File{Path: prefix + f.Name, Directory: f.Directory})
	}
	return result
}

func (m *Model) nativeFilePage(cursor string, pages int) tea.Cmd {
	backend := m.backend.(fileBrowseBackend)
	p := m.files
	return func() tea.Msg {
		result := browseFiles(p.request, backend, p.owner, p.prefix, cursor, 100)
		if p.request.Err() != nil {
			return nil
		}
		return nativeFilePage{p.generation, result, cursor, pages}
	}
}

func (m *Model) receiveNativeFilePage(v nativeFilePage) tea.Cmd {
	p := &m.files
	// Buffer names, never editor state, and discard replies for abandoned tokens.
	if p.cancel == nil || v.generation != p.generation || p.owner != m.active || p.navigation != m.navigation || !m.fileAvailable(p.prefix) {
		return nil
	}
	if v.result.Err != nil {
		p.records = nil
	}
	if m.appendFilePage(p.prefix, v.result, v.cursor, v.pages) {
		return m.nativeFilePage(v.result.Cursor, v.pages-1)
	}
	p.cancel()
	p.cancel, p.request = nil, nil
	return nil
}
