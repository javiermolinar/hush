package tui

import (
	"context"
	"path"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const newThreadHereChoice = "new-here"
const workspaceDirectoryChoice = "directory"

type workspaceThreadTarget struct {
	id, name string
	scope    string // a recorded host path used only to look up the workspace
	preview  string // the path shown on the action row, if unambiguous on this page
}

type workspaceThreadCreation struct {
	parent *dialog
	target workspaceThreadTarget
}

func validWorkspaceDirectory(cwd string) bool {
	// These are POSIX paths on the backend host, not client filesystem paths.
	return path.IsAbs(cwd) && utf8.ValidString(cwd) && utf8.RuneCountInString(cwd) <= 4096 && !strings.ContainsRune(cwd, 0)
}

func workspaceThreadDestination(row threadPickerRow, indices []int, r x.ClientResult, directories map[sdk.ID]string) *workspaceThreadTarget {
	var dest *workspaceThreadTarget
	for _, i := range indices {
		// A synthetic group (e.g. "Other workspaces") isn't a destination.
		if i >= len(r.Workspaces) || r.Workspaces[i] == nil || r.Workspaces[i].ID == "" {
			continue
		}
		if cwd := directories[r.Threads[i].ID]; cwd != "" {
			if dest == nil {
				dest = &workspaceThreadTarget{id: row.workspaceID, name: row.workspaceName, scope: cwd, preview: cwd}
			} else if cwd != dest.scope {
				dest.preview = ""
			}
		}
	}
	return dest
}

func workspaceThreadPage(ctx context.Context, source workspaceDirectoryBackend, id, cwd, cursor string, limit int) x.ClientResult {
	if source == nil || !validWorkspaceDirectory(cwd) {
		return x.ClientResult{Err: x.ErrUnavailable}
	}
	// Look up directories independently of the thread search: a different
	// checkout may have no matching threads, or be on an older page.
	page, err := source.ListThreadDirectory(ctx, sdk.ThreadDirectoryOptions{Workspace: cwd, Cursor: cursor, Limit: limit})
	if err != nil {
		return x.ClientResult{Err: err}
	}
	if len(page.Workspaces) != len(page.Threads) {
		return x.ClientResult{Err: sdk.ErrProtocol}
	}
	for _, ws := range page.Workspaces {
		if ws == nil || ws.ID != id {
			return x.ClientResult{Err: x.ErrUnavailable} // the recorded path moved to another workspace
		}
	}
	if len(page.Threads) == 0 && !page.Indexing {
		return x.ClientResult{Err: x.ErrUnavailable}
	}
	return directoryPageResult(page, limit)
}

func (m *Model) openWorkspaceDirectories(parent *dialog) tea.Cmd {
	if !m.foreground(parent.route) || m.focusedDialog() != parent || len(m.overlays) >= maxDialogs {
		return nil
	}
	target := parent.threadPicker.selectedRow(parent).destination
	if target == nil {
		return nil
	}
	d := &dialog{route: parent.route, title: "New thread · " + oneLine(target.name),
		threadPicker: &threadPicker{creation: &workspaceThreadCreation{parent: parent, target: *target}}}
	return tea.Batch(m.pushDialog(d), m.loadThreadPicker(d, "", 0))
}

func (m *Model) receiveWorkspaceDirectories(d *dialog, cursor string, r x.ClientResult) tea.Cmd {
	p := d.threadPicker
	p.cursor = r.Cursor
	if r.Err != nil {
		p.failed = true
		p.addChoice(d, "refresh", "Retry working directories…", "", threadPickerRow{})
		d.filter()
		return nil
	}
	seen := make(map[string]bool, len(r.Threads))
	for _, t := range r.Threads {
		cwd := t.Workspace
		if validWorkspaceDirectory(cwd) && !seen[cwd] {
			seen[cwd] = true
			p.addChoice(d, workspaceDirectoryChoice, workspaceDirectoryLabel(cwd), "", threadPickerRow{directory: cwd})
		}
	}
	// Only skip the chooser when a complete lookup confirms the exact path
	// already shown on the action row. Never pick from a partial page.
	if cursor == "" && r.Cursor == "" && !r.Indexing && d.query == "" && len(seen) == 1 && seen[p.creation.target.preview] && m.focusedDialog() == d {
		return m.createWorkspaceThread(d, p.creation.target.preview, true)
	}
	if r.Indexing {
		p.addChoice(d, "refresh", "Refresh working directories…", "", threadPickerRow{})
	} else if r.Cursor != "" {
		p.addChoice(d, pageChoice, "Older working directories…", "", threadPickerRow{})
	}
	d.filter()
	return nil
}

func (m *Model) createWorkspaceThread(d *dialog, cwd string, verified bool) tea.Cmd {
	creation, r := d.threadPicker.creation, d.route
	if creation == nil || m.sheet != creation.parent || m.focusedDialog() != d || !m.foreground(r) || !validWorkspaceDirectory(cwd) {
		return nil
	}
	cmd := m.closeDialog(d, nil, false)
	closeSheet := m.closeDialog(creation.parent, nil, false)
	b := maxEffects
	source, _ := m.backend.(workspaceDirectoryBackend)
	create := m.clientJob(r, x.NewThread{Workspace: cwd})
	id := creation.target.id
	work := m.startWork(r, func(ctx context.Context) (any, error) {
		// A manually chosen directory can disappear or change workspace
		// while the picker is open. Never fall back to the launch directory.
		if !verified {
			call, cancel := context.WithTimeout(ctx, menuTimeout)
			page := workspaceThreadPage(call, source, id, cwd, "", 1)
			cancel()
			if page.Err != nil || len(page.Threads) == 0 {
				return x.ClientResult{Err: x.ErrUnavailable}, nil
			}
		}
		return create(ctx)
	}, func(v any, err error, budget *int) tea.Cmd {
		if !m.foreground(r) {
			return nil
		}
		result, _ := v.(x.ClientResult)
		if err != nil {
			result.Err = err
		}
		if result.Err == x.ErrUnavailable {
			return m.notifyRoute(r, "Working directory unavailable; no thread created", x.Warning)
		}
		return m.admit(r, switchResult(m.contextFor(r.owner), result), budget)
	}, &b)
	return tea.Batch(cmd, closeSheet, work)
}

func workspaceDirectoryLabel(cwd string) string {
	s := oneLine(cwd)
	// Bound per-frame work, keeping the leaf and its nearest parents visible.
	if len(s) > 512 {
		start := len(s) - 512 + len("…")
		for !utf8.RuneStart(s[start]) {
			start++
		}
		return "…" + s[start:]
	}
	return s
}

func shortenDirectory(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if n := stringWidth(s); n > w {
		return ansi.TruncateLeft(s, n-w+1, "…")
	}
	return s
}

func (m *Model) threadPickerFooter(d *dialog, w int, right string) string {
	i, ok := m.host.byID[newThreadID]
	if !ok || d.threadPicker.selectedRow(d).destination == nil {
		return m.choiceFooter(w, "choose", right)
	}
	action := "open"
	if c, _ := d.choice(); c.ID == newThreadHereChoice {
		action = "new"
	}
	accept := m.keyHint("enter", action)
	create := m.keyHint("ctrl+x "+m.host.actions[i].action.Binding.Leader, "new here")
	full := strings.Join([]string{accept, m.keyHint("↑↓", "move"), create, m.keyHint("esc", "close")}, "  ")
	compact := strings.Join([]string{accept, create, m.keyCap("↑↓"), m.keyCap("esc")}, "  ")
	if stringWidth(compact) > w {
		return m.choiceFooter(w, action, right)
	}
	if stringWidth(compact)+stringWidth(right)+2 > w {
		right = ""
	}
	return m.hintRow(w, full, compact, right)
}
