package tui

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type fileBrowseBackend interface {
	WorkspaceFileScope(context.Context, sdk.WorkspaceFileTarget) (sdk.WorkspaceFileScope, error)
	ListWorkspaceFiles(context.Context, sdk.WorkspaceFileTarget, string, string, int, string) (sdk.WorkspaceFilePage, error)
}

type fileSearchBackend interface {
	SearchWorkspaceFiles(context.Context, sdk.WorkspaceFileTarget, string, int) (sdk.WorkspaceFileSearchResult, error)
}

type fileScopeWatchBackend interface {
	WatchWorkspaceFileScope(context.Context, sdk.WorkspaceFileTarget) <-chan sdk.WorkspaceFileScopeUpdate
}

// The picker detects these at runtime, so a signature drift would silently
// disable it. Keep the SDK client's methods checked at compile time.
var _ interface {
	fileBrowseBackend
	fileSearchBackend
	fileScopeWatchBackend
} = (*sdk.Client)(nil)

const fileSearchDelay = 150 * time.Millisecond

type fileScopeLease struct {
	owner                  sdk.ID
	target                 sdk.WorkspaceFileTarget
	navigation, generation uint64
	scope                  sdk.WorkspaceFileScope
	pushed                 bool
	revision               string
	ctx                    context.Context
	cancel                 context.CancelFunc
}

type fileScopeMsg struct {
	generation uint64
	scope      sdk.WorkspaceFileScope
}
type fileScopeWatchMsg struct {
	generation uint64
	source     <-chan sdk.WorkspaceFileScopeUpdate
	update     sdk.WorkspaceFileScopeUpdate
	ok         bool
}
type renewFileScope uint64

func (m *Model) stopFileScope() {
	if m.fileScope.cancel != nil {
		m.fileScope.cancel()
	}
	m.fileScope = fileScopeLease{}
	m.stopFileRequest()
}

// fileTarget selects what authorizes the picker: the active thread or, before
// the first message, a prospective thread in the local launch directory, as
// the intro preview does. Typing never creates a thread to browse files.
func (m *Model) fileTarget() (sdk.WorkspaceFileTarget, bool) {
	if m.active != "" {
		return sdk.WorkspaceFileTarget{ThreadID: m.active}, m.active.Valid() && m.connection == x.Connected
	}
	// workspaceFiles:2 (negotiated with search) accepts a prospective target.
	// With no thread there's no feed, so each scope reply proves reachability.
	cwd := m.options.NewThread.CWD
	return sdk.WorkspaceFileTarget{CWD: cwd}, m.nativeFileSearch && m.options.WorkspaceLocal && filepath.IsAbs(cwd)
}

func (m *Model) syncFileScope() tea.Cmd {
	target, ok := m.fileTarget()
	if !m.nativeFiles || !ok || m.options.Services.Files != nil {
		if m.fileScope.generation != 0 {
			m.stopFileScope()
		}
		return nil
	}
	watcher, hasWatch := m.backend.(fileScopeWatchBackend)
	pushed := m.fileScopePush && hasWatch
	if s := m.fileScope; s.owner == m.active && s.navigation == m.navigation && s.target == target && s.pushed == pushed {
		return nil
	}
	m.stopFileScope()
	ctx, cancel := context.WithCancel(m.ctx)
	s := fileScopeLease{owner: m.active, target: target, navigation: m.navigation, generation: m.token(), pushed: pushed, ctx: ctx, cancel: cancel}
	m.fileScope = s
	if !pushed {
		return m.fileScopeCommand()
	}
	return func() tea.Msg {
		if ctx.Err() != nil {
			return nil
		}
		return awaitFileScopeWatch(s, watcher.WatchWorkspaceFileScope(ctx, target))()
	}
}

func awaitFileScopeWatch(s fileScopeLease, source <-chan sdk.WorkspaceFileScopeUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-s.ctx.Done():
			return nil
		case v, ok := <-source:
			return fileScopeWatchMsg{s.generation, source, v, ok}
		}
	}
}

func (m *Model) receiveFileScopeWatch(msg fileScopeWatchMsg) tea.Cmd {
	s := m.fileScope
	if !s.pushed || msg.generation != s.generation || s.owner != m.active || s.navigation != m.navigation {
		return nil
	}
	if sdk.IsCode(msg.update.Err, "unsupported_capability") {
		m.fileScopePush = false
		return m.syncFileScope()
	}
	if !msg.ok {
		return nil
	}
	return tea.Batch(m.applyFileScope(msg.update.Scope, msg.update.Revision), awaitFileScopeWatch(s, msg.source))
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
		scope, err := backend.WorkspaceFileScope(ctx, s.target)
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
	if s.pushed || v.generation != s.generation || s.owner != m.active || s.navigation != m.navigation {
		return nil
	}
	return tea.Batch(m.applyFileScope(v.scope, ""), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return renewFileScope(v.generation) }))
}

func (m *Model) applyFileScope(scope sdk.WorkspaceFileScope, revision string) tea.Cmd {
	s := &m.fileScope
	if s.scope != scope || s.revision != revision {
		m.stopFileRequest()
		m.files = filePicker{}
	}
	s.scope, s.revision = scope, revision
	return m.refreshFilePicker()
}

func (m *Model) currentFileScope() sdk.WorkspaceFileScope {
	target, ok := m.fileTarget()
	if ok && m.fileScope.owner == m.active && m.fileScope.navigation == m.navigation && m.fileScope.target == target {
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

type nativeFileSearchMsg struct {
	generation uint64
	result     sdk.WorkspaceFileSearchResult
	err        error
}

func (m *Model) stopFileRequest() {
	if cancel := m.files.cancel; cancel != nil {
		cancel()
		m.files.cancel, m.files.request = nil, nil
		m.files.waiting = false
		m.files.generation = 0
	}
}

func browseFiles(ctx context.Context, backend fileBrowseBackend, target sdk.WorkspaceFileTarget, prefix, cursor string, limit int) x.ClientResult {
	ctx, cancel := context.WithTimeout(ctx, menuTimeout+time.Second)
	defer cancel()
	root, directory := "workspace", strings.TrimSuffix(prefix, "/")
	if strings.HasPrefix(prefix, "~/") {
		root, directory = "home", strings.TrimSuffix(strings.TrimPrefix(prefix, "~/"), "/")
	}
	page, err := backend.ListWorkspaceFiles(ctx, target, root, directory, limit, cursor)
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
	backend, ok := m.backend.(fileBrowseBackend)
	if !ok {
		m.stopFileRequest()
		return nil
	}
	p := m.files
	return func() tea.Msg {
		result := browseFiles(p.request, backend, p.target, p.prefix, cursor, 100)
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

func (m *Model) searchNativeFiles() tea.Cmd {
	backend := m.backend.(fileSearchBackend)
	p := m.files
	return func() tea.Msg {
		timer := time.NewTimer(fileSearchDelay)
		defer timer.Stop()
		select {
		case <-p.request.Done():
			return nil
		case <-timer.C:
		}
		if p.request.Err() != nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(p.request, menuTimeout+time.Second)
		defer cancel()
		result, err := backend.SearchWorkspaceFiles(ctx, p.target, p.query, sdk.WorkspaceFileSearchLimit)
		if p.request.Err() != nil {
			return nil
		}
		return nativeFileSearchMsg{p.generation, result, err}
	}
}

func (m *Model) receiveNativeFileSearch(v nativeFileSearchMsg) {
	p := &m.files
	if p.cancel == nil || v.generation != p.generation || p.owner != m.active || p.navigation != m.navigation ||
		p.query == "" || !m.currentFileScope().Workspace {
		return
	}
	p.cancel()
	p.cancel, p.request = nil, nil
	p.waiting, p.failed = false, v.err != nil
	if p.failed {
		return
	}
	p.partial = v.result.Partial
	for _, hit := range v.result.Matches {
		if validFileRef(hit.Path) && hit.Path != "~" && !strings.HasPrefix(hit.Path, "~/") {
			p.records = append(p.records, x.File{Path: hit.Path, Directory: hit.Directory})
			if len(p.records) >= sdk.WorkspaceFileSearchLimit {
				break
			}
		}
	}
}
