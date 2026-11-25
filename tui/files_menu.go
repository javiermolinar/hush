package tui

import (
	"context"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const maxFileSuggestions = 5
const browsePathsID x.ActionID = menusID + ".browse-paths"

var browsePathsAction = registeredAction{action: x.Action{
	ID: browsePathsID, Title: "Browse paths…", Slash: &x.Slash{},
}}

type fileTokenKind uint8

const (
	fileMention fileTokenKind = iota
	fileHome
	fileAbsolute
	fileRoots
)

const (
	workspaceRootChoice = "root:workspace"
	homeRootChoice      = "root:home"
)

type filePicker struct {
	owner                   sdk.ID
	target                  sdk.WorkspaceFileTarget // the scope lease's, for native requests
	navigation              uint64
	prefix, query, base     string
	kind                    fileTokenKind
	start                   int
	request                 context.Context
	cancel                  context.CancelFunc
	sourceText              string
	records                 []x.File
	waiting, failed, hidden bool
	partial                 bool
	selected                int
	generation              uint64
}

type fileToken struct {
	start, end       int
	dir, query, base string // dir is the listing prefix; base restores an absolute workspace path
	kind             fileTokenKind
}

func fileTokenAt(text string, at int) (fileToken, bool) {
	if !byteBoundary(text, at) {
		return fileToken{}, false
	}
	before := text[:at]
	line := strings.LastIndexByte(before, '\n') + 1
	start, end, tail := 0, len(text), ""
	quoted := false
	if i := strings.LastIndexByte(before[line:], '"'); i >= 0 {
		quote := line + i
		start = quote
		if quote > line && before[quote-1] == '@' {
			start--
		}
		prev, _ := utf8.DecodeLastRuneInString(before[:start])
		tail = before[quote+1:]
		quoted = (start == 0 || unicode.IsSpace(prev)) &&
			(start < quote || strings.HasPrefix(tail, "/") || strings.HasPrefix(tail, "~"))
	}
	if quoted {
		suffix := text[at:]
		if i := strings.IndexAny(suffix, "\"\n"); i >= 0 {
			end = at + i
			if suffix[i] == '"' {
				end++
			}
		}
	} else {
		start = strings.LastIndexFunc(before, unicode.IsSpace) + 1
		// The index is the delimiter's first byte.
		for start < at && !byteBoundary(text, start) {
			start++
		}
		if start >= at {
			return fileToken{}, false
		}
		tail = before[start:]
	}
	kind := fileMention
	switch {
	case text[start] == '@':
		if !quoted {
			tail = tail[1:]
		}
		if strings.HasPrefix(tail, "/") {
			return fileToken{}, false
		}
	case tail == "~" || strings.HasPrefix(tail, "~/"):
		kind = fileHome
		if tail == "~" {
			tail = "~/"
		}
	case strings.HasPrefix(tail, "/"):
		kind = fileAbsolute
	default:
		return fileToken{}, false
	}
	if strings.ContainsAny(tail, "\\\"@:") || hasControl(tail) {
		return fileToken{}, false
	}
	if !quoted {
		if i := strings.IndexFunc(text[at:], unicode.IsSpace); i >= 0 {
			end = at + i
		}
	}
	dir, query := "", tail
	if i := strings.LastIndexByte(tail, '/'); i >= 0 {
		dir, query = tail[:i+1], tail[i+1:]
	}
	// Absolute tokens are checked after removing the known workspace prefix.
	if kind != fileAbsolute && dir != "" && !validFileRef(strings.TrimSuffix(dir, "/")) {
		return fileToken{}, false
	}
	return fileToken{start: start, end: end, dir: dir, query: query, kind: kind}, true
}

func validFileRef(p string) bool {
	return p != "" && p != "." && p != ".." && !strings.HasPrefix(p, "../") && !strings.HasPrefix(p, "/") && p == path.Clean(p) &&
		!hasControl(p) && !strings.ContainsAny(p, "\\\"@:") && len(p) <= 2048
}

func (m *Model) pathRootsAvailable() bool {
	// Extension file providers do not advertise separate root grants.
	scope := m.currentFileScope()
	return m.options.Services.Files == nil && (scope.Workspace || scope.Home)
}

func (m *Model) workspaceBrowsePath() string {
	if !m.currentFileScope().Workspace {
		return ""
	}
	cwd := m.fileScope.target.CWD
	if m.active != "" {
		cwd = m.threads[m.active].info.Workspace
	}
	if cwd == "/" || !validWorkspaceDirectory(cwd) || path.Clean(cwd) != cwd || strings.ContainsAny(cwd, "\\\"@:") || hasControl(cwd) {
		return ""
	}
	return cwd + "/"
}

func (m *Model) currentFileToken() (fileToken, bool) {
	text := m.editor.value()
	v, ok := fileTokenAt(text, m.editor.cursorByte())
	if !ok || m.editor.overlapsPaste(v.start, v.end) || v.kind != fileMention && !m.pathRootsAvailable() {
		return fileToken{}, false
	}
	if v.kind == fileAbsolute {
		if v.dir == "/" && v.query == "" {
			// A leading unquoted slash is for commands unless the user chose
			// the Browse paths row. Never interpret it as a filesystem root.
			p := &m.files
			if v.start == 0 && text[0] == '/' &&
				(p.kind != fileRoots || p.sourceText != text || p.owner != m.active || p.navigation != m.navigation) {
				return fileToken{}, false
			}
			v.kind, v.dir = fileRoots, ""
			return v, true
		}
		base := m.workspaceBrowsePath()
		dir, inside := strings.CutPrefix(v.dir, base)
		// Only the relative directory is sent to the backend. A top-level
		// tilde is reserved for the separately granted home listing.
		if base == "" || !inside || strings.HasPrefix(dir, "~/") || dir != "" && !validFileRef(strings.TrimSuffix(dir, "/")) {
			return fileToken{}, false
		}
		v.base, v.dir = base, dir
	}
	return v, m.fileAvailable(v.dir)
}

func (m *Model) openPathRoots() {
	if m.focusedDialog() != nil || m.slashHidden || m.editor.value() != "/" || !m.editor.atEnd() || !m.pathRootsAvailable() {
		return
	}
	m.stopFileRequest()
	m.slashHidden = true
	m.files = filePicker{kind: fileRoots, sourceText: "/", owner: m.active, navigation: m.navigation}
	m.refreshFilePicker()
}

func (m *Model) fileSearchQuery(v fileToken) string {
	if v.kind != fileMention || v.dir != "" || !m.nativeFileSearch || m.options.Services.Files != nil || !m.currentFileScope().Workspace {
		return ""
	}
	q := strings.TrimSpace(v.query)
	if q == "~" || len(q) > sdk.WorkspaceFileSearchQueryBytes {
		return ""
	}
	return q
}

func (m *Model) fileSuggestions() (fileToken, []x.File, bool) {
	if m.files.generation == 0 || m.files.hidden || m.files.owner != m.active || m.files.navigation != m.navigation {
		return fileToken{}, nil, false
	}
	v, ok := m.currentFileToken()
	if !ok || !m.files.matches(v, m.fileSearchQuery(v)) {
		return fileToken{}, nil, false
	}
	if v.kind == fileRoots {
		return v, m.files.records, true
	}
	if m.files.query != "" {
		return v, m.files.records, true // the backend matched and ranked these paths
	}
	var choices []x.File
	for _, item := range m.files.records {
		if !strings.HasPrefix(item.Path, v.dir) {
			continue
		}
		if !fuzzyMatch(strings.TrimPrefix(item.Path, v.dir), v.query) {
			continue
		}
		choices = append(choices, item)
		if len(choices) >= maxFileSuggestions {
			break
		}
	}
	return v, choices, true
}

func (p *filePicker) matches(v fileToken, query string) bool {
	return p.start == v.start && p.prefix == v.dir && p.query == query && p.kind == v.kind && p.base == v.base
}

func (m *Model) refreshFilePicker() tea.Cmd {
	text := m.editor.value()
	var v fileToken
	var ok bool
	// Before the first message only a prospective native scope can authorize
	// the picker (see fileTarget). Typing never creates a thread.
	if m.focusedDialog() == nil && (m.active != "" || m.options.Services.Files == nil) && m.fileAvailable("") && strings.ContainsAny(text, "@~/") {
		v, ok = m.currentFileToken()
	}
	p := &m.files
	if !ok {
		m.stopFileRequest()
		if p.kind != fileMention && p.sourceText != text {
			*p = filePicker{}
		}
		return nil
	}
	if p.sourceText != text {
		p.hidden = false
		p.sourceText = text
		p.selected = 0
	}
	if p.hidden {
		return nil
	}
	query := m.fileSearchQuery(v)
	if p.generation != 0 && p.owner == m.active && p.navigation == m.navigation && p.matches(v, query) {
		return nil
	}
	m.stopFileRequest()
	generation := m.token()
	*p = filePicker{owner: m.active, target: m.fileScope.target, navigation: m.navigation, start: v.start, prefix: v.dir, query: query,
		kind: v.kind, base: v.base, sourceText: text, generation: generation}
	if v.kind == fileRoots {
		scope := m.currentFileScope()
		if scope.Workspace {
			p.records = append(p.records, x.File{Path: workspaceRootChoice, Directory: true})
		}
		if scope.Home {
			p.records = append(p.records, x.File{Path: homeRootChoice, Directory: true})
		}
		return nil
	}
	p.waiting = true
	if m.options.Services.Files == nil {
		if query != "" {
			p.request, p.cancel = context.WithCancel(m.fileScope.ctx)
			return m.searchNativeFiles()
		}
		scope := m.currentFileScope()
		if v.kind == fileMention && v.dir == "" && scope.Home {
			p.records = []x.File{{Path: "~", Directory: true}}
			if !scope.Workspace {
				p.waiting = false
				return nil
			}
		}
		p.request, p.cancel = context.WithCancel(m.fileScope.ctx) // fileAvailable implies the lease
		return m.nativeFilePage("", 4)
	}
	r := m.route(m.active, menusID)
	b := maxEffects
	return m.admit(r, m.filePage(r, v.dir, generation, "", 4), &b)
}

func (m *Model) filePage(r ownerRoute, dir string, generation uint64, cursor string, pages int) x.Outcome {
	return x.Outcome{x.Client{Op: x.ListFiles{Query: dir, Cursor: cursor, Limit: 100}, Done: func(_ x.Context, result x.ClientResult) x.Outcome {
		if !m.foreground(r) || m.files.generation != generation {
			return nil
		}
		if m.appendFilePage(dir, result, cursor, pages) {
			return m.filePage(r, dir, generation, result.Cursor, pages-1)
		}
		return nil
	}}}
}

func (m *Model) appendFilePage(dir string, result x.ClientResult, cursor string, pages int) bool {
	p := &m.files
	p.waiting, p.failed = false, result.Err != nil
	if p.failed {
		return false
	}
	seen := make(map[string]bool, len(p.records))
	for _, known := range p.records {
		seen[known.Path] = true
	}
	for _, item := range result.Files {
		item.Path = strings.TrimSuffix(item.Path, "/")
		if validFileRef(item.Path) && !seen[item.Path] && strings.HasPrefix(item.Path, dir) && !strings.Contains(strings.TrimPrefix(item.Path, dir), "/") {
			p.records = append(p.records, item)
			seen[item.Path] = true
		}
	}
	// Append later pages in service order so the highlighted choice stays put.
	if cursor == "" && m.options.Services.Files != nil {
		slices.SortStableFunc(p.records, func(a, b x.File) int {
			if a.Directory != b.Directory {
				if a.Directory {
					return -1
				}
				return 1
			}
			return strings.Compare(a.Path, b.Path)
		})
	}
	p.waiting = pages > 1 && result.Cursor != "" && result.Cursor != cursor
	return p.waiting
}

func (m *Model) replaceFile(target string) tea.Cmd {
	v, choices, ok := m.fileSuggestions()
	if !ok {
		return nil
	}
	selected := m.files.selected
	if target != "" {
		selected = slices.IndexFunc(choices, func(f x.File) bool { return f.Path == target })
	}
	if selected < 0 || selected >= len(choices) {
		return nil
	}
	f := choices[selected]
	value := v.base + f.Path
	if f.Directory {
		value += "/"
	}
	if v.kind == fileRoots {
		value = m.rootChoicePath(f.Path)
	}
	quoted := strings.IndexFunc(value, unicode.IsSpace) >= 0
	if quoted {
		value = `"` + value + `"`
	}
	if v.kind == fileMention {
		value = "@" + value
	}
	cursor := len(value)
	if f.Directory && quoted {
		cursor-- // remain inside the existing quotes without eating a suffix
	} else if !f.Directory && v.end == len(m.editor.value()) {
		value += " "
		cursor++
	}
	m.editor.replaceAt(v.start, v.end, value, cursor)
	m.editor.resetHistory()
	if !f.Directory && v.kind != fileMention {
		// The caret can still be next to a path when there is text after it.
		// Don't consume the next Enter by suggesting the same file again.
		m.files.hidden, m.files.sourceText = true, m.editor.value()
		m.stopFileRequest()
		return nil
	}
	return m.refreshFilePicker()
}

func (m *Model) rootChoicePath(choice string) string {
	if choice == homeRootChoice {
		return "~/"
	}
	if base := m.workspaceBrowsePath(); base != "" {
		return base
	}
	return "@" // a host need not supply an absolute workspace path
}

func (m *Model) fileRows(w, budget int) ([]displayRow, int) {
	_, choices, ok := m.fileSuggestions()
	if !ok || budget <= 0 {
		return nil, 0
	}
	heading := "Workspace files"
	switch {
	case m.files.kind == fileRoots:
		heading = "Granted locations"
	case strings.HasPrefix(m.files.prefix, "~/"):
		heading = "Backend home files"
	}
	switch {
	case m.files.waiting && m.files.query != "":
		heading += " · searching"
	case m.files.waiting:
		heading += " · loading"
	case m.files.failed:
		heading += " · unavailable"
	case m.files.partial:
		heading += " · partial results"
	}
	var out []displayRow
	if budget > 1 {
		out = append(out, displayRow{text: m.paint(x.Muted, heading)})
		budget--
	}
	headerRows := len(out)
	limit := min(maxFileSuggestions, budget)
	first := max(0, m.files.selected-limit+1)
	for i := first; i < min(first+limit, len(choices)); i++ {
		item := choices[i]
		label := item.Path
		if item.Directory {
			label += "/"
		}
		hint := "complete " + m.files.base + label
		switch m.files.kind {
		case fileMention:
			hint = "complete @" + label
		case fileHome, fileAbsolute:
			label = strings.TrimPrefix(label, m.files.prefix)
		case fileRoots:
			name := "Workspace"
			if item.Path == homeRootChoice {
				name = "Backend home"
			}
			label, hint = name+" · "+m.rootChoicePath(item.Path), "browse "+name
		}
		pref, tone := "  ", x.Plain
		if i == m.files.selected {
			pref, tone = "› ", x.Accent
		}
		out = append(out, displayRow{text: m.paint(tone, pref+label), hits: []spanHit{{0, min(w, 2+stringWidth(label)), x.ActionRef{ID: filePickID, Target: item.Path}, hint}}})
	}
	if len(out) == headerRows {
		label := "No matches"
		switch {
		case m.files.waiting && m.files.query != "":
			label = "Searching workspace…"
		case m.files.waiting:
			label = "Fetching directory…"
		case m.files.partial:
			label = "No matches in scanned paths"
		}
		out = append(out, displayRow{text: m.paint(x.Muted, "  "+label)})
	}
	return out, len(choices)
}
