package tui

import (
	"path"
	"slices"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const maxFileSuggestions = 5

type filePicker struct {
	owner                   sdk.ID
	navigation              uint64
	prefix                  string
	sourceText              string
	records                 []x.File
	waiting, failed, hidden bool
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
	start := strings.LastIndexFunc(text[:at], unicode.IsSpace) + 1
	// LastIndexFunc returns a byte offset at the delimiter's first byte.
	for start < at && !byteBoundary(text, start) {
		start++
	}
	if start >= at || text[start] != '@' {
		return fileToken{}, false
	}
	tail := text[start+1 : at]
	if strings.ContainsAny(tail, "\\@:") || strings.HasPrefix(tail, "/") {
		return fileToken{}, false
	}
	dir, query := "", tail
	if i := strings.LastIndexByte(tail, '/'); i >= 0 {
		dir, query = tail[:i+1], tail[i+1:]
	}
	if dir != "" && !validFileRef(strings.TrimSuffix(dir, "/")) {
		return fileToken{}, false
	}
	end := at
	for i, c := range text[at:] {
		if unicode.IsSpace(c) {
			end = at + i
			break
		}
		end = at + i + len(string(c))
	}
	return fileToken{start, end, dir, query}, true
}

func validFileRef(p string) bool {
	return p != "" && p != "." && p != ".." && !strings.HasPrefix(p, "../") && !strings.HasPrefix(p, "/") && p == path.Clean(p) &&
		!hasControl(p) && !strings.ContainsAny(p, "\\@:") && strings.IndexFunc(p, unicode.IsSpace) < 0 && len(p) <= 2048
}

func (m *Model) fileSuggestions() (fileToken, []x.File, bool) {
	if !m.menuFeatures().Has(x.Files) || m.active == "" || m.files.hidden || m.files.owner != m.active || m.files.navigation != m.navigation {
		return fileToken{}, nil, false
	}
	v, ok := fileTokenAt(m.editor.value(), m.editor.cursorByte())
	if !ok || v.dir != m.files.prefix {
		return fileToken{}, nil, false
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
	if m.focusedDialog() != nil || !m.menuFeatures().Has(x.Files) || m.active == "" {
		return nil
	}
	if !strings.ContainsRune(m.editor.value(), '@') {
		return nil
	}
	v, ok := fileTokenAt(m.editor.value(), m.editor.cursorByte())
	if !ok {
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
	if p.owner == m.active && p.navigation == m.navigation && p.prefix == v.dir {
		return nil
	}
	generation := m.token()
	*p = filePicker{owner: m.active, navigation: m.navigation, prefix: v.dir, sourceText: text, waiting: true, generation: generation}
	r := m.route(m.active, menusID)
	b := maxEffects
	return m.admit(r, m.filePage(r, v.dir, generation, "", 4), &b)
}

func (m *Model) filePage(r ownerRoute, dir string, generation uint64, cursor string, pages int) x.Outcome {
	return x.Outcome{x.Client{Op: x.ListFiles{Query: dir, Cursor: cursor, Limit: 100}, Done: func(_ x.Context, result x.ClientResult) x.Outcome {
		if !m.foreground(r) || m.files.generation != generation {
			return nil
		}
		m.files.waiting = false
		if result.Err != nil {
			m.files.failed = true
			return nil
		}
		seen := make(map[string]bool, len(m.files.records))
		for _, known := range m.files.records {
			seen[known.Path] = true
		}
		for _, item := range result.Files {
			item.Path = strings.TrimSuffix(item.Path, "/")
			if validFileRef(item.Path) && !seen[item.Path] && strings.HasPrefix(item.Path, dir) && !strings.Contains(strings.TrimPrefix(item.Path, dir), "/") {
				m.files.records = append(m.files.records, item)
				seen[item.Path] = true
			}
		}
		// Append later pages in service order so the highlighted choice stays put.
		if cursor == "" {
			slices.SortStableFunc(m.files.records, func(a, b x.File) int {
				if a.Directory != b.Directory {
					if a.Directory {
						return -1
					}
					return 1
				}
				return strings.Compare(a.Path, b.Path)
			})
		}
		if pages > 1 && result.Cursor != "" && result.Cursor != cursor {
			m.files.waiting = true
			return m.filePage(r, dir, generation, result.Cursor, pages-1)
		}
		return nil
	}}}
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
	end := v.end
	suffix := " "
	if f.Directory {
		suffix = "/"
	} else if end < len(m.editor.value()) {
		suffix = ""
	}
	m.editor.replace(v.start, end, "@"+f.Path+suffix)
	m.editor.resetHistory()
	return m.refreshFilePicker()
}

func (m *Model) fileRows(w int) []displayRow {
	_, choices, ok := m.fileSuggestions()
	if !ok {
		return nil
	}
	heading := "Workspace files"
	if m.files.waiting {
		heading += " · loading"
	}
	if m.files.failed {
		heading += " · unavailable"
	}
	out := []displayRow{{text: paint(x.Muted, heading)}}
	for i, item := range choices {
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
		if m.files.waiting {
			label = "Fetching directory…"
		}
		out = append(out, displayRow{text: paint(x.Muted, "  "+label)})
	}
	return out
}
