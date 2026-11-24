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
	uv "github.com/charmbracelet/ultraviolet"
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

// Bare paths need a line number to distinguish them from ordinary dotted words.
var references = regexp.MustCompile(`https?://[^\s<>"'\x{2400}-\x{2426}]+|@"[^"\r\n]+"|@[A-Za-z0-9_./-]+(?::[0-9]+)?|#[0-9]+|/?(?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+\.[A-Za-z0-9_-]+:[1-9][0-9]*`)
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
	ink := "\x1b[4m"
	if strings.HasPrefix(uri, "file:") {
		ink = "\x1b[4:4m" // dotted underline distinguishes paths from web links
	}
	return ink + oscLink + params + ";" + uri + oscEnd + text + oscLink + ";" + oscEnd + "\x1b[24m"
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
			raw := match[1:]
			if strings.HasPrefix(raw, `"`) {
				raw = raw[1 : len(raw)-1]
				if !validFileRef(strings.TrimSuffix(raw, "/")) || strings.HasPrefix(raw, "~/") {
					continue // A backend-home reference is not a workspace link.
				}
			}
			par, uri = fileTarget(workspacePath(workspace, raw))
		case '#':
			if m.options.RepositoryURL != "" {
				uri = safeURL(strings.TrimRight(m.options.RepositoryURL, "/") + "/pull/" + match[1:])
			}
		default:
			if strings.HasPrefix(match, "http://") || strings.HasPrefix(match, "https://") {
				uri = safeURL(match)
			} else if i == 0 || !strings.ContainsRune(":/", rune(text[i-1])) {
				par, uri = fileTarget(workspacePath(workspace, match))
			}
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

// Wrapping leaves SGR and OSC 8 in the text stream. Close and reopen at
// physical row boundaries so a chip doesn't tint the margin or padding, and
// a link remains independently hit-testable on a scrolled continuation row.
func balanceRowMarkup(lines []string) {
	var pen uv.Style
	var link string
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	for i, line := range lines {
		prefix := link
		if !pen.IsZero() {
			prefix = pen.String() + prefix
		}
		for rest := line; ; {
			at := strings.IndexByte(rest, '\x1b')
			if at < 0 {
				break
			}
			seq, _, n, _ := ansi.DecodeSequence(rest[at:], 0, p)
			if n == 0 {
				break
			}
			rest = rest[at+n:]
			switch {
			case ansi.HasCsiPrefix(seq) && p.Command() == 'm':
				uv.ReadStyle(p.Params(), &pen)
			case seq == oscLink+";"+oscEnd:
				link = ""
			case strings.HasPrefix(seq, oscLink):
				link = seq
			}
		}
		suffix := ""
		if link != "" {
			suffix = oscLink + ";" + oscEnd
		}
		if !pen.IsZero() {
			suffix += ansi.ResetStyle
		}
		lines[i] = prefix + line + suffix
	}
}
