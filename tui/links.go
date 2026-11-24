package tui

import (
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const oscEnd = "\x1b\\"
const oscLink = "\x1b]8;"

type linkTarget struct {
	url, path string
	line      int
	code      bool
	codeID    string
}

var references = regexp.MustCompile(`https?://[^\s<>"'\x{2400}-\x{2426}]+|@[A-Za-z0-9_./-]+(?::[0-9]+)?|#[0-9]+`)
var pathLine = regexp.MustCompile(`^(.*?)(?::([1-9][0-9]*)|#L([1-9][0-9]*))$`)

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

func safeURL(raw string) string {
	if raw == "" || len(raw) > 8192 || hasControl(raw) || strings.Contains(raw, "\\") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return ""
	}
	switch u.Scheme {
	case "http", "https":
		if u.Hostname() == "" || u.Opaque != "" {
			return ""
		}
	case "mailto":
		if u.Opaque == "" && u.Path == "" {
			return ""
		}
	default:
		return ""
	}
	if hasControl(u.Path) || hasControl(u.Fragment) || hasControl(u.Opaque) {
		return ""
	}
	return u.String()
}

func workspacePath(workspace, raw string) (string, int) {
	if !filepath.IsAbs(workspace) || raw == "" || hasControl(raw) || len(raw) > 4096 || strings.Contains(raw, "://") {
		return "", 0
	}
	num := 0
	if m := pathLine.FindStringSubmatch(raw); m != nil {
		raw = m[1]
		line := m[2]
		if line == "" {
			line = m[3]
		}
		num, _ = strconv.Atoi(line)
	}
	path := raw
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(workspace, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", 0
	}
	return path, num
}

func markdownLink(workspace, dest string) (string, string) {
	if u := safeURL(dest); u != "" {
		return "", u
	}
	if strings.ContainsAny(dest, "?\x00") {
		return "", ""
	}
	if u, err := url.PathUnescape(dest); err == nil {
		dest = u
	}
	if strings.Contains(dest, ":") && pathLine.FindStringSubmatch(dest) == nil {
		return "", ""
	}
	p, line := workspacePath(workspace, dest)
	return fileTarget(p, line)
}

func fileTarget(path string, line int) (string, string) {
	if path == "" {
		return "", ""
	}
	params := ""
	if line > 0 {
		params = "line=" + strconv.Itoa(line)
	}
	return params, (&url.URL{Scheme: "file", Path: path}).String()
}

func hyperlink(text, params, uri string) string {
	if uri == "" || text == "" {
		return text
	}
	return "\x1b[34m" + oscLink + params + ";" + uri + oscEnd + text + oscLink + ";" + oscEnd + "\x1b[39m"
}

func (m *Model) linkify(text, workspace string) string {
	var b strings.Builder
	last := 0
	for _, loc := range references.FindAllStringIndex(text, -1) {
		i, j := loc[0], loc[1]
		if i > 0 {
			p, _ := utf8.DecodeLastRuneInString(text[:i])
			if unicode.IsLetter(p) || unicode.IsNumber(p) || p == '_' {
				continue
			}
		}
		match := strings.TrimRight(text[i:j], ".,;:!?)]}")
		par, uri := "", ""
		switch match[0] {
		case '@':
			par, uri = fileTarget(workspacePath(workspace, match[1:]))
		case '#':
			if m.options.RepositoryURL != "" {
				uri = safeURL(strings.TrimRight(m.options.RepositoryURL, "/") + "/pull/" + match[1:])
			}
		default:
			uri = safeURL(match)
		}
		if uri == "" {
			continue
		}
		b.WriteString(text[last:i])
		b.WriteString(hyperlink(match, par, uri))
		last = i + len(match)
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func linkAt(line string, x int) string {
	var state byte
	col, link := 0, ""
	for line != "" {
		seq, w, n, next := ansi.DecodeSequence(line, state, nil)
		if n <= 0 {
			break
		}
		if body, ok := strings.CutPrefix(seq, oscLink); ok {
			link = strings.TrimSuffix(strings.TrimSuffix(body, "\x07"), oscEnd)
			if link == ";" {
				link = ""
			}
		} else if w > 0 {
			if x >= col && x < col+w {
				return link
			}
			col += w
		}
		state, line = next, line[n:]
	}
	return ""
}

func decodeLink(link string) linkTarget {
	params, uri, ok := strings.Cut(link, ";")
	if !ok || uri == "" {
		return linkTarget{}
	}
	if id, ok := strings.CutPrefix(uri, "hush-copy:"); ok {
		return linkTarget{code: true, codeID: id}
	}
	if u, err := url.Parse(uri); err == nil && u.Scheme == "file" && u.Host == "" && u.User == nil && filepath.IsAbs(u.Path) && !hasControl(u.Path) {
		line, _ := strconv.Atoi(strings.TrimPrefix(params, "line="))
		return linkTarget{path: u.Path, line: line}
	}
	return linkTarget{url: safeURL(uri)}
}

func linkHint(link string) string {
	u := decodeLink(link)
	if u.code {
		return "copy code · click"
	}
	if u.path != "" {
		if u.line > 0 {
			return u.path + ":" + strconv.Itoa(u.line)
		}
		return u.path
	}
	return u.url
}

func (m *Model) openLink(r ownerRoute, link string, budget *int) tea.Cmd {
	target := decodeLink(link)
	if target.code {
		return m.copyCode(r, target.codeID, budget)
	}
	if target.url == "" && target.path == "" {
		return nil
	}
	return m.openTarget(r, target, budget)
}

func (m *Model) renderText(text string, width int, workspace string) []string {
	if width <= 0 {
		return nil
	}
	lines := wrapped(m.linkify(safeText(text), workspace), width)
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// Wrap preserves OSC sequences, but a link can span several physical rows.
// Close and reopen it at those boundaries so each row can be hit-tested on its own.
func balanceRowLinks(lines []string) {
	open := ""
	for i, line := range lines {
		carried := open
		if start := strings.LastIndex(line, oscLink); start >= 0 {
			if seq, _, ok := strings.Cut(line[start:], oscEnd); ok {
				open = seq + oscEnd
				if open == oscLink+";"+oscEnd {
					open = ""
				}
			}
		}
		if carried != "" {
			lines[i] = "\x1b[34m" + carried + line
		}
		if open != "" {
			lines[i] += oscLink + ";" + oscEnd + "\x1b[39m"
		}
	}
}
