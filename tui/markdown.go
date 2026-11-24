package tui

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
	"github.com/yuin/goldmark"
	ast "github.com/yuin/goldmark/ast"
	gfm "github.com/yuin/goldmark/extension"
	table "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var markdown = goldmark.New(goldmark.WithExtensions(gfm.Table, gfm.Strikethrough, gfm.TaskList))

// The immutable-entry cache owns layouts. The live cache additionally keeps a
// stable prefix and completed code lines, so each new token only lays out the tail.
type markdownStream struct {
	width           int
	theme           uint64
	workspace       string
	text            string
	stable, scanned int
	blank, unsafe   bool
	fence           string
	fenceAt         int
	completed       []string
	codeCount       int
	open            *openFence
}

type openFence struct {
	at, next, widest, width, index int
	lang                           string
	state                          hlState
	before, source, rows           []string
}

type document struct {
	m         *Model
	src       []byte
	workspace string
	key       renderKey
	codes     int
}

func (m *Model) renderDocument(key renderKey, format x.Format, source, lang string, width int, workspace string, live bool) []string {
	if width <= 0 {
		return nil
	}
	switch format {
	case x.UserText:
		return m.renderText(source, width, workspace)
	case x.Markdown:
		if live && key.owner == m.active {
			return m.streamMarkdown(key, source, width, workspace)
		}
		d := document{m: m, workspace: workspace, key: key}
		return d.render(source, width)
	case x.Code:
		d := document{m: m, workspace: workspace, key: key}
		return d.code(strings.Split(strings.TrimSuffix(safeText(source), "\n"), "\n"), lang, width, false)
	case x.Diff:
		var out []string
		for s := range strings.SplitSeq(safeText(source), "\n") {
			out = append(out, m.diffRows(s, width)...)
		}
		return out
	default:
		return m.renderText(source, width, workspace)
	}
}

func (d *document) render(source string, w int) []string {
	d.src = []byte(safeText(source))
	return d.children(markdown.Parser().Parse(text.NewReader(d.src)), w)
}

func (d *document) children(parent ast.Node, w int) []string {
	var out []string
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		if rows := d.block(n, w); len(rows) > 0 {
			if len(out) > 0 {
				out = append(out, "")
			}
			out = append(out, rows...)
		}
	}
	return out
}

func (d *document) block(n ast.Node, w int) []string {
	w = max(1, w)
	switch v := n.(type) {
	case *ast.Heading:
		return wrapped("\x1b[1m"+d.inline(n, true)+"\x1b[22m", w)
	case *ast.ThematicBreak:
		return []string{paint(x.Muted, "· · ·")}
	case *ast.Paragraph, *ast.TextBlock:
		return wrapped(d.inline(n, true), w)
	case *ast.FencedCodeBlock:
		return d.code(d.codeSource(n), strings.ToLower(string(v.Language(d.src))), w, true)
	case *ast.CodeBlock:
		return d.code(d.codeSource(n), "", w, true)
	case *ast.Blockquote:
		if w < 4 {
			return d.children(n, w)
		}
		rows := d.children(n, w-2)
		for i, s := range rows {
			rows[i] = paint(x.Muted, "│ ") + "\x1b[3m" + s + "\x1b[23m"
		}
		return rows
	case *ast.List:
		var out []string
		index := v.Start
		for item := v.FirstChild(); item != nil; item = item.NextSibling() {
			bullet := "• "
			if v.IsOrdered() {
				bullet = strconv.Itoa(index) + ". "
				index++
			}
			if p := item.FirstChild(); p != nil {
				if task, ok := p.FirstChild().(*table.TaskCheckBox); ok {
					if task.IsChecked {
						bullet = "✓ "
					} else {
						bullet = "☐ "
					}
				}
			}
			indent := stringWidth(bullet)
			if w-indent < 2 {
				out = append(out, d.children(item, w)...)
				continue
			}
			rows := d.children(item, w-indent)
			for i, s := range rows {
				if i == 0 {
					out = append(out, paint(x.Muted, bullet)+s)
				} else {
					out = append(out, strings.Repeat(" ", indent)+s)
				}
			}
		}
		return out
	case *table.Table:
		return d.layoutTable(v, w)
	case *ast.HTMLBlock:
		var b strings.Builder
		for i := 0; i < v.Lines().Len(); i++ {
			line := v.Lines().At(i)
			b.Write(line.Value(d.src))
		}
		s := strings.TrimSpace(b.String())
		if strings.HasPrefix(s, "<!--") {
			return nil
		}
		return wrapped(paint(x.Muted, s), w)
	default:
		return wrapped(d.inline(n, true), w)
	}
}

func (d *document) inline(n ast.Node, links bool) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		s := ""
		switch v := c.(type) {
		case *ast.Text:
			s = safeText(string(v.Segment.Value(d.src)))
			if links {
				s = d.m.linkify(s, d.workspace)
			}
			if v.HardLineBreak() {
				s += "\n"
			} else if v.SoftLineBreak() {
				s += " "
			}
		case *ast.String:
			s = safeText(string(v.Value))
			if links {
				s = d.m.linkify(s, d.workspace)
			}
		case *ast.CodeSpan:
			s = strings.TrimSpace(strings.ReplaceAll(d.inline(v, false), "\n", " "))
			if links && !strings.ContainsAny(s, " \t") && (strings.ContainsRune(s, '/') || filepath.Ext(s) != "") {
				params, uri := fileTarget(workspacePath(d.workspace, s))
				if uri != "" {
					s = hyperlink(s, params, uri)
					break
				}
			}
			if links && s != "" {
				if ink := d.m.palette.softInk; ink != "" {
					s = ink + s + resetBackground
				} else {
					s = "\x1b[1m" + s + "\x1b[22m"
				}
			}
		case *ast.Link:
			s = d.inline(v, false)
			if links {
				params, uri := markdownLink(d.workspace, string(v.Destination))
				s = hyperlink(s, params, uri)
			}
		case *ast.Image:
			s = "▣ " + d.inline(v, false)
			if links {
				params, uri := markdownLink(d.workspace, string(v.Destination))
				s = hyperlink(s, params, uri)
			}
		case *ast.AutoLink:
			s = safeText(string(v.Label(d.src)))
			if links {
				dest := string(v.URL(d.src))
				if v.AutoLinkType == ast.AutoLinkEmail && !strings.HasPrefix(dest, "mailto:") {
					dest = "mailto:" + dest
				}
				s = hyperlink(s, "", safeURL(dest))
			}
		case *ast.Emphasis:
			if v.Level == 2 {
				s = "\x1b[1m" + d.inline(v, links) + "\x1b[22m"
			} else {
				s = "\x1b[3m" + d.inline(v, links) + "\x1b[23m"
			}
		case *table.Strikethrough:
			s = "\x1b[9m" + d.inline(v, links) + "\x1b[29m"
		case *table.TaskCheckBox:
			continue // the list marker is the checkbox
		case *ast.RawHTML:
			for i := 0; i < v.Segments.Len(); i++ {
				seg := v.Segments.At(i)
				s += safeText(string(seg.Value(d.src)))
			}
			if strings.HasPrefix(s, "<!--") {
				s = ""
			} else {
				s = paint(x.Muted, s)
			}
		default:
			s = d.inline(c, links)
		}
		b.WriteString(s)
	}
	return b.String()
}

func wrapped(s string, w int) []string {
	if s == "" {
		return nil
	}
	w = max(1, w)
	lines := strings.Split(ansi.Wrap(s, w, ""), "\n")
	var out []string
	for i, line := range lines {
		if stringWidth(line) > w {
			// Wrap can leave an overlong physical line at an exact-width word
			// or hyphen breakpoint. Preserve its text instead of clipping it in fit.
			if out == nil {
				out = append(make([]string, 0, len(lines)+1), lines[:i]...)
			}
			out = append(out, strings.Split(ansi.Hardwrap(line, w, true), "\n")...)
		} else if out != nil {
			out = append(out, line)
		}
	}
	if out != nil {
		lines = out
	}
	if len(lines) > 1 && strings.ContainsRune(s, '\x1b') {
		balanceRowMarkup(lines)
	}
	return lines
}

func (d *document) layoutTable(n *table.Table, w int) []string {
	var data [][]string
	for row := n.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			cells = append(cells, d.inline(cell, true))
		}
		data = append(data, cells)
	}
	if len(data) == 0 || len(data[0]) == 0 {
		return nil
	}
	widths := make([]int, len(data[0]))
	for _, r := range data {
		for i, s := range r[:min(len(r), len(widths))] {
			widths[i] = max(widths[i], stringWidth(s))
		}
	}
	need := 2 * (len(widths) - 1)
	for _, col := range widths {
		need += col
	}
	var out []string
	if need > w {
		for _, r := range data[1:] {
			if len(r) == 0 {
				continue
			}
			if len(out) > 0 {
				out = append(out, "")
			}
			out = append(out, wrapped("\x1b[1m"+r[0]+"\x1b[22m", w)...)
			for j := 1; j < min(len(r), len(widths)); j++ {
				out = append(out, wrapped(paint(x.Muted, data[0][j]+": ")+r[j], w)...)
			}
		}
		if len(out) == 0 {
			out = wrapped(strings.Join(data[0], " · "), w)
		}
		return out
	}
	for i, row := range data {
		cells := make([]string, len(widths))
		for j, v := range widths {
			s := ""
			if j < len(row) {
				s = row[j]
			}
			cells[j] = fit(s, v)
		}
		line := strings.TrimRight(strings.Join(cells, "  "), " ")
		if i == 0 {
			out = append(out, "\x1b[1m"+line+"\x1b[22m", paint(x.Muted, strings.Repeat("─", need)))
		} else {
			out = append(out, line)
		}
	}
	return out
}

func (d *document) codeSource(n ast.Node) []string {
	var rows []string
	for i := 0; i < n.Lines().Len(); i++ {
		seg := n.Lines().At(i)
		rows = append(rows, strings.TrimSuffix(string(seg.Value(d.src)), "\n"))
	}
	return rows
}

func frameWidth(w, widest int) int { return min(w, max(40, ((widest+13)/10)*10)) }

func codeID(key renderKey, index int) string {
	return string(key.entry) + "/" + strconv.Itoa(key.block) + "/" + strconv.Itoa(index)
}

func (d *document) frameCode(lang string, width, index int, copyable bool) (string, string) {
	title := "── " + oneLine(lang)
	if lang != "" {
		title += " "
	}
	action := ""
	if copyable && width >= 12 {
		id := codeID(d.key, index)
		label := "copy"
		action = " " + hyperlink(label, "", "hush-copy:"+id) + "     "
	}
	title = ansiCut(title, max(0, width-stringWidth(action)-1))
	top := paint(x.Muted, title) + action + paint(x.Muted, strings.Repeat("─", max(0, width-stringWidth(title)-stringWidth(action))))
	return top, paint(x.Muted, strings.Repeat("─", width))
}

func (d *document) code(source []string, lang string, w int, copyable bool) []string {
	if len(source) == 0 {
		source = []string{""}
	}
	widest := 0
	for _, s := range source {
		widest = max(widest, stringWidth(s))
	}
	width := frameWidth(w, widest)
	index := d.codes
	d.codes++
	top, bottom := d.frameCode(lang, width, index, copyable)
	out := []string{top}
	var state hlState
	for _, s := range source {
		var rs []string
		rs, state = d.codeLine(s, lang, max(1, width-2), state)
		out = append(out, rs...)
	}
	return append(out, bottom)
}

func (d *document) codeLine(s, lang string, w int, state hlState) ([]string, hlState) {
	switch lang {
	case "diff", "patch":
		rs := d.m.diffRows(s, w)
		for i := range rs {
			rs[i] = "  " + rs[i]
		}
		return rs, state
	}
	if sx := syntaxes[lang]; sx != nil {
		var a []codeSpan
		a, state = sx.line1(s, state)
		var styles strings.Builder
		for _, part := range a {
			styles.WriteString(part.ink)
			styles.WriteString(part.s)
			if part.ink != "" {
				styles.WriteString("\x1b[0m")
			}
		}
		s = styles.String()
	}
	var out []string
	for i, r := range strings.Split(ansi.Hardwrap(s, w, true), "\n") {
		if i == 0 {
			out = append(out, "  "+r)
		} else {
			out = append(out, paint(x.Muted, "↪ ")+r)
		}
	}
	return out, state
}

func (m *Model) diffRows(s string, w int) []string {
	tone, idx := x.Muted, 0
	switch {
	case strings.HasPrefix(s, "+") && !strings.HasPrefix(s, "+++"):
		tone, idx = x.Good, 2
	case strings.HasPrefix(s, "-") && !strings.HasPrefix(s, "---"):
		tone, idx = x.Danger, 1
	}
	rows := strings.Split(ansi.Hardwrap(s, max(1, w), true), "\n")
	for i, r := range rows {
		rows[i] = paint(tone, r)
		if idx != 0 && m.theme.Fills {
			rows[i] = fill(fit(rows[i], w), blendPalette(m.palette.background, m.palette.colors[idx], 0.20))
		}
	}
	return rows
}

var refDefinition = regexp.MustCompile(`^ {0,3}\[[^\]]+\]:`)

func fenceAfter(open, line string) string {
	s := strings.TrimLeft(line, " ")
	if len(line)-len(s) > 3 {
		return open
	}
	if open != "" {
		if strings.HasPrefix(s, open) && strings.TrimSpace(strings.TrimLeft(s, open[:1])) == "" {
			return ""
		}
		return open
	}
	if strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~") {
		info := strings.TrimLeft(s, s[:1])
		if s[0] == '`' && strings.ContainsRune(info, '`') {
			return ""
		}
		return s[:len(s)-len(info)]
	}
	return ""
}

func (m *Model) streamMarkdown(k renderKey, s string, w int, workspace string) []string {
	t := m.threads[k.owner]
	if t.liveDocs == nil {
		t.liveDocs = make(map[int]*markdownStream)
	}
	st := t.liveDocs[k.block]
	if st == nil || st.width != w || st.theme != k.theme || st.workspace != workspace || !strings.HasPrefix(s, st.text) {
		st = &markdownStream{width: w, theme: k.theme, workspace: workspace}
		t.liveDocs[k.block] = st
	}
	oldStable := st.stable
	for {
		i := strings.IndexByte(s[st.scanned:], '\n')
		if i < 0 {
			break
		}
		at := st.scanned
		line := s[at : at+i]
		st.scanned += i + 1
		// Cut only at an independently starting paragraph/heading after a
		// blank, outside fences/HTML/ref definitions. Lists/quotes can continue.
		if st.blank && st.fence == "" && !st.unsafe && line != "" && !strings.ContainsRune(" \t-*+<[>=_~", rune(line[0])) && !(line[0] >= '0' && line[0] <= '9') {
			st.stable = at
		}
		if strings.Contains(line, "[") || strings.HasPrefix(strings.TrimSpace(line), "<") || refDefinition.MatchString(line) {
			st.unsafe = true
		}
		next := fenceAfter(st.fence, line)
		if st.fence == "" && next != "" {
			st.fenceAt = at
		}
		st.fence, st.blank = next, strings.TrimSpace(line) == ""
	}
	d := document{m: m, workspace: workspace, key: k, codes: st.codeCount}
	if st.stable > oldStable {
		st.completed = joinRows(st.completed, d.render(s[oldStable:st.stable], w))
		st.codeCount = d.codes
		if len(st.completed) > recordRows {
			st.completed = append([]string(nil), st.completed[len(st.completed)-recordRows:]...)
		}
	}
	st.text = s
	var tail []string
	if st.fence != "" && st.fenceAt >= st.stable && s[st.fenceAt] == st.fence[0] && fenceAfter(st.fence, s[st.scanned:]) != "" {
		tail = d.liveFence(st, s)
	} else {
		st.open = nil
		tail = d.render(s[st.stable:], w)
	}
	return joinRows(append([]string(nil), st.completed...), tail)
}

func joinRows(before, after []string) []string {
	if len(before) > 0 && len(after) > 0 {
		before = append(before, "")
	}
	return append(before, after...)
}

func (d *document) liveFence(st *markdownStream, s string) []string {
	f := st.open
	if f == nil || f.at != st.fenceAt {
		nl := strings.IndexByte(s[st.fenceAt:], '\n') + st.fenceAt
		lang := ""
		if fields := strings.Fields(strings.TrimLeft(s[st.fenceAt:nl], st.fence[:1])); len(fields) > 0 {
			lang = strings.ToLower(fields[0])
		}
		before := d.render(s[st.stable:st.fenceAt], st.width)
		f = &openFence{at: st.fenceAt, next: nl + 1, index: d.codes, lang: lang, before: before}
		st.open = f
	}
	for i := strings.IndexByte(s[f.next:], '\n'); i >= 0; i = strings.IndexByte(s[f.next:], '\n') {
		line := safeText(s[f.next : f.next+i])
		f.next += i + 1
		f.source = append(f.source, line)
		f.widest = max(f.widest, stringWidth(line))
		rows, state := d.codeLine(line, f.lang, max(1, f.width-2), f.state)
		f.state = state
		f.rows = append(f.rows, rows...)
	}
	partial := safeText(s[f.next:])
	width := frameWidth(st.width, max(f.widest, stringWidth(partial)))
	if width != f.width {
		f.width, f.state, f.rows = width, hlState{}, f.rows[:0]
		for _, line := range f.source {
			rows, state := d.codeLine(line, f.lang, max(1, width-2), f.state)
			f.state = state
			f.rows = append(f.rows, rows...)
		}
	}
	top, bottom := d.frameCode(f.lang, width, f.index, true)
	rows := append([]string{top}, f.rows[max(0, len(f.rows)-recordRows):]...)
	if partial != "" || len(f.source) == 0 {
		p, _ := d.codeLine(partial, f.lang, max(1, width-2), f.state)
		rows = append(rows, p...)
	}
	return joinRows(append([]string(nil), f.before...), append(rows, bottom))
}

type copyLabel struct {
	owner sdk.ID
	id    string
	token uint64
}
type copyLabelExpired struct{ token uint64 }

func codeBlocks(source string) []string {
	b := []byte(source)
	root := markdown.Parser().Parse(text.NewReader(b))
	var out []string
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if enter && (n.Kind() == ast.KindCodeBlock || n.Kind() == ast.KindFencedCodeBlock) {
			var s bytes.Buffer
			for i := 0; i < n.Lines().Len(); i++ {
				seg := n.Lines().At(i)
				s.Write(seg.Value(b))
			}
			out = append(out, strings.TrimSuffix(s.String(), "\n"))
		}
		return ast.WalkContinue, nil
	})
	return out
}

func (m *Model) copyCode(r ownerRoute, id string, budget *int) tea.Cmd {
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return nil
	}
	block, err := strconv.Atoi(parts[1])
	if err != nil || block < 0 {
		return nil
	}
	index, err := strconv.Atoi(parts[2])
	if err != nil || index < 0 {
		return nil
	}
	t := m.threads[r.owner]
	var content x.Content
	if parts[0] == "" {
		content = m.liveRecordContent(m.renderContext(t, m.width-4, recordRows, true), t)
	} else if rec := t.entries[sdk.ID(parts[0])]; rec != nil {
		ctx := m.renderContext(t, m.width-4, recordRows, false)
		ctx.Expanded = rec.details(t)
		ctx.ReasoningExpanded = rec.reasoningDetails(t)
		content, _ = m.recordContent(ctx, t, rec)
	}
	if block >= len(content) || content[block].Format != x.Markdown {
		return nil
	}
	codes := codeBlocks(content[block].Text)
	if index >= len(codes) {
		return nil
	}
	token := m.token()
	m.copied = copyLabel{r.owner, id, token}
	// Layout cache remains valid; the last frame substitutes the label.
	return tea.Batch(m.copyText(r, codes[index], budget), tea.Tick(1400*time.Millisecond, func(time.Time) tea.Msg { return copyLabelExpired{token} }))
}

func (m *Model) copyLastCode(r ownerRoute, budget *int) tea.Cmd {
	codes := codeBlocks(m.threads[r.owner].lastAnswer)
	if len(codes) == 0 {
		return m.notifyRoute(r, "No code block to copy", x.Warning)
	}
	return m.copyText(r, codes[len(codes)-1], budget)
}
