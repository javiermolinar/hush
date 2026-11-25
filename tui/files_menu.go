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

type filePicker struct {
	owner                   sdk.ID
	target                  sdk.WorkspaceFileTarget // the scope lease's, for native requests
	navigation              uint64
	prefix, query           string
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
	start, end int
	dir, query string
}

func fileTokenAt(text string, at int) (fileToken, bool) {
	if !byteBoundary(text, at) {
		return fileToken{}, false
	}
	before := text[:at]
	line := strings.LastIndexByte(before, '\n') + 1
	quote := strings.LastIndex(before[line:], `@"`)
	start, end, tail := 0, at, ""
	quoted := false
	if quote >= 0 {
		start = line + quote
		prev, _ := utf8.DecodeLastRuneInString(before[:start])
		quoted = (start == 0 || unicode.IsSpace(prev)) && !strings.ContainsRune(before[start+2:], '"')
	}
	if quoted {
		tail = before[start+2:]
		suffix := text[at:]
		i := strings.IndexAny(suffix, "\"\n")
		switch {
		case i < 0:
			end = len(text)
		case suffix[i] == '"':
			end = at + i + 1
		default:
			end = at + i
		}
	} else {
		start = strings.LastIndexFunc(before, unicode.IsSpace) + 1
		// The index is the delimiter's first byte.
		for start < at && !byteBoundary(text, start) {
			start++
		}
		if start >= at || text[start] != '@' {
			return fileToken{}, false
		}
		tail = before[start+1:]
		if i := strings.IndexFunc(text[at:], unicode.IsSpace); i >= 0 {
			end = at + i
		} else {
			end = len(text)
		}
	}
	if strings.ContainsAny(tail, "\\\"@:") || strings.HasPrefix(tail, "/") || hasControl(tail) {
		return fileToken{}, false
	}
	dir, query := "", tail
	if i := strings.LastIndexByte(tail, '/'); i >= 0 {
		dir, query = tail[:i+1], tail[i+1:]
	}
	if dir != "" && !validFileRef(strings.TrimSuffix(dir, "/")) {
		return fileToken{}, false
	}
	return fileToken{start, end, dir, query}, true
}

func validFileRef(p string) bool {
	return p != "" && p != "." && p != ".." && !strings.HasPrefix(p, "../") && !strings.HasPrefix(p, "/") && p == path.Clean(p) &&
		!hasControl(p) && !strings.ContainsAny(p, "\\\"@:") && len(p) <= 2048
}

func (m *Model) fileSearchQuery(v fileToken) string {
	if v.dir != "" || !m.nativeFileSearch || m.options.Services.Files != nil || !m.currentFileScope().Workspace {
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
	v, ok := fileTokenAt(m.editor.value(), m.editor.cursorByte())
	if !ok || v.start != m.files.start || v.dir != m.files.prefix || !m.fileAvailable(v.dir) || m.fileSearchQuery(v) != m.files.query {
		return fileToken{}, nil, false
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

func (m *Model) refreshFilePicker() tea.Cmd {
	if m.focusedDialog() != nil || !strings.ContainsRune(m.editor.value(), '@') {
		m.stopFileRequest()
		return nil
	}
	v, ok := fileTokenAt(m.editor.value(), m.editor.cursorByte())
	// Before the first message only a prospective native scope can authorize
	// the picker (see fileTarget). Typing never creates a thread.
	if !ok || m.active == "" && m.options.Services.Files != nil || !m.fileAvailable(v.dir) {
		m.stopFileRequest()
		return nil
	}
	p := &m.files
	text := m.editor.value()
	if p.sourceText != text {
		p.hidden = false
		p.sourceText = text
		p.selected = 0
	}
	if p.hidden {
		return nil
	}
	query := m.fileSearchQuery(v)
	if p.generation != 0 && p.owner == m.active && p.navigation == m.navigation && p.start == v.start && p.prefix == v.dir && p.query == query {
		return nil
	}
	m.stopFileRequest()
	generation := m.token()
	*p = filePicker{owner: m.active, target: m.fileScope.target, navigation: m.navigation, start: v.start, prefix: v.dir, query: query,
		sourceText: text, waiting: true, generation: generation}
	if m.options.Services.Files == nil {
		if query != "" {
			p.request, p.cancel = context.WithCancel(m.fileScope.ctx)
			return m.searchNativeFiles()
		}
		scope := m.currentFileScope()
		if v.dir == "" && scope.Home {
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
	value := f.Path
	if f.Directory {
		value += "/"
	}
	quoted := strings.IndexFunc(value, unicode.IsSpace) >= 0
	if quoted {
		value = `"` + value + `"`
	}
	value = "@" + value
	cursor := len(value)
	if f.Directory && quoted {
		cursor-- // remain inside the existing quotes without eating a suffix
	} else if !f.Directory && v.end == len(m.editor.value()) {
		value += " "
		cursor++
	}
	m.editor.replaceAt(v.start, v.end, value, cursor)
	m.editor.resetHistory()
	return m.refreshFilePicker()
}

func (m *Model) fileRows(w int) []displayRow {
	_, choices, ok := m.fileSuggestions()
	if !ok {
		return nil
	}
	heading := "Workspace files"
	if strings.HasPrefix(m.files.prefix, "~/") {
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
	out := []displayRow{{text: paint(x.Muted, heading)}}
	first := max(0, m.files.selected-maxFileSuggestions+1)
	for i := first; i < min(first+maxFileSuggestions, len(choices)); i++ {
		item := choices[i]
		label := item.Path
		if item.Directory {
			label += "/"
		}
		pref, tone := "  ", x.Plain
		if i == m.files.selected {
			pref, tone = "› ", x.Accent
		}
		out = append(out, displayRow{text: paint(tone, pref+label), hits: []spanHit{{0, min(w, 2+stringWidth(label)), x.ActionRef{ID: filePickID, Target: item.Path}, "complete @" + label}}})
	}
	if len(out) == 1 {
		label := "No matches"
		switch {
		case m.files.waiting && m.files.query != "":
			label = "Searching workspace…"
		case m.files.waiting:
			label = "Fetching directory…"
		case m.files.partial:
			label = "No matches in scanned paths"
		}
		out = append(out, displayRow{text: paint(x.Muted, "  "+label)})
	}
	return out
}
