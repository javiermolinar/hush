package tui

import (
	"context"
	"flag"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

// Only the unit tests use a backend stub. TestBackendE2E uses the real SDK and
// the reference backend's existing faux provider and tool implementation.
type inertBackend struct{ Backend }

func modelFor(t testing.TB, options Options) *Model {
	t.Helper()
	if options.NewThread.CWD == "" {
		options.NewThread.CWD = "/workspace"
	}
	m, err := New(t.Context(), inertBackend{}, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	m.editor.area.Focus()
	return m
}

func entry(id int, role, body string) sdk.Entry {
	return sdk.Entry{ID: sdk.ID(fmt.Sprint(id)), ThreadID: "1", Kind: "model", Messages: []sdk.Message{{Role: role, TimestampMS: 1795435568245, Blocks: []sdk.Block{{Type: "text", Text: body}}}}}
}

func demo(t testing.TB, opts Options) (*Model, *threadState) {
	opts.ThreadID = "1"
	m := modelFor(t, opts)
	s := m.threads[m.active]
	s.info.Workspace = "/workspace"
	m.connection = x.Connected
	s.info.Status = x.Idle
	s.merge([]sdk.Entry{entry(1, "user", "What changed in @app/main.go:12?"), entry(2, "assistant", "# Answer\n\nSee [the docs](https://example.org/docs) and `app/main.go:12`.\n\n```go\nfunc main() { println(42) }\n```")})
	return m, s
}

func press(m *Model, code rune, mod tea.KeyMod) tea.Cmd {
	_, out := m.Update(tea.KeyPressMsg{Code: code, Mod: mod})
	return out
}

func assertFrame(t testing.TB, m *Model) string {
	t.Helper()
	s := m.View().Content
	rows := strings.Split(s, "\n")
	if len(rows) != m.height {
		t.Fatalf("got %d rows; want %d\n%s", len(rows), m.height, s)
	}
	for i, row := range rows {
		if w := ansi.StringWidth(row); w > m.width {
			t.Fatalf("row %d spans %d > %d cells: %q", i, w, m.width, row)
		}
		if strings.ContainsAny(ansi.Strip(row), "\x00\x07\x1b\r\t") {
			t.Fatalf("control in row %d: %q", i, row)
		}
	}
	return s
}

func TestFrameGeometry(t *testing.T) {
	t.Run("workspace-trust", testWorkspaceTrustFrameGeometry)
	t.Run("file-picker", testFilePickerFrameGeometry)
	t.Run("thread-picker", testThreadPickerFrameGeometry)
	t.Run("loaded-context", testLoadedContextFrameGeometry)
	t.Run("configuration", testConfigFrameGeometry)
	for _, size := range [][2]int{{39, 11}, {40, 12}, {64, 16}, {80, 24}, {120, 30}, {200, 60}} {
		m, s := demo(t, Options{Fills: FillOff, ProviderLogin: loginFixture()})
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if size[0] < 40 {
			if v := assertFrame(t, m); !strings.Contains(v, "terminal too small") {
				t.Fatal(v)
			}
			continue
		}
		v := assertFrame(t, m)
		if assertFrame(t, m) != v {
			t.Fatal("repeat view is not deterministic")
		}
		m.notify(m.active, strings.Repeat("notice ", 200), x.Warning)
		m.hover = strings.Repeat("https://example.org/", 20)
		m.Update(tea.PasteMsg{Content: "first line\nsecond line\nfinal typed line"})
		if !strings.Contains(ansi.Strip(assertFrame(t, m)), "final typed line") {
			t.Fatal("notice or paste clipped the end of the prompt")
		}
		m.editor.set(strings.Repeat("utterlylongtoken ", 55) + "prompt tail")
		long := ansi.Strip(assertFrame(t, m))
		if !strings.Contains(long, "tail") || !strings.HasSuffix(m.editor.value(), "prompt tail") {
			t.Fatalf("long prompt tail not visible at %v:\n%s", size, long)
		}
		m.editor.set("")
		s.info.Status = x.Running
		s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{{Type: "text", Text: "Live\n\n```sh\necho 'hello'"}}})
		s.streamRevision++
		s.queue = []sdk.QueueItem{{Mode: "steer", Text: strings.Repeat("wide 界界\n\x1b[2J", 40)}, {Mode: "followUp", Text: "then test"}}
		assertFrame(t, m)
		m.runAction(m.host.byID[usageID], "")
		assertFrame(t, m)
		press(m, tea.KeyEscape, 0)
		m.backend = modelSearchFixture{}
		m.Update(m.menuProbe()())
		search := m.runAction(m.host.byID[modelPickerID], "openrouter")
		assertFrame(t, m) // searching
		m.Update(search())
		assertFrame(t, m) // result list
		m.runAction(m.host.byID[menusID+".login"], "")
		assertFrame(t, m)
		b := maxEffects
		m.openDialog(m.route(m.active, coreID), x.SelectDialog{Title: "Select", Placement: x.Sheet, Choices: []x.Choice{{ID: "1", Label: "one"}}}, &b)
		assertFrame(t, m)
		m.openPalette(m.route(m.active, coreID), &b)
		assertFrame(t, m)
		m.openDialog(m.route(m.active, coreID), x.InputDialog{Title: "Edit", Initial: "value"}, &b)
		assertFrame(t, m)
	}
}

func TestPromptLayoutCacheTracksEdits(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	check := func() tea.View {
		t.Helper()
		editor, limit := &m.editor, max(1, m.height/4)
		rows, caret := editor.layout(limit)
		if want := editor.rows(editor.area.Width(), limit); !slices.Equal(rows, want) {
			t.Fatalf("memoized editor rows %q differ from the textarea view %q", rows, want)
		}
		wantCaret := editor.area.Cursor()
		if (caret == nil) != (wantCaret == nil) || caret != nil && caret.Position != wantCaret.Position {
			t.Fatal("memoized caret differs from the textarea cursor")
		}
		got, again := m.View(), m.View()
		if got.Content != again.Content || (got.Cursor == nil) != (again.Cursor == nil) ||
			got.Cursor != nil && got.Cursor.Position != again.Cursor.Position {
			t.Fatal("repeated frames changed the prompt or its cursor position")
		}
		return got
	}
	check()
	m.Update(tea.PasteMsg{Content: strings.Repeat("line 界👩‍💻 é\n", 18) + "tail"})
	before := check()
	press(m, tea.KeyLeft, 0)
	left := check()
	if left.Cursor == nil || before.Cursor == nil || left.Cursor.Position.X != before.Cursor.Position.X-1 {
		t.Fatalf("moving the caret without editing the draft did not move the rendered cursor")
	}
	press(m, tea.KeyLeft, tea.ModShift)
	check()
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 16})
	check()
	m.editor.area.Blur()
	if check().Cursor != nil {
		t.Fatal("blurred editor still shows a memoized cursor")
	}
	m.editor.area.Focus()
	m.editor.set("replacement draft ")
	check()
	press(m, tea.KeyLeft, tea.ModShift)
	check()
	m.editor.set(strings.Repeat("a", m.editor.area.Width()))
	check()
	m.editor.newline()
	check()
}

func TestPlainTypingMatchesTextarea(t *testing.T) {
	fast, reference := newPrompt(), newPrompt()
	for _, p := range []*promptEditor{&fast, &reference} {
		p.area.SetVirtualCursor(false)
		p.area.SetWidth(9)
		p.area.SetHeight(3)
		p.area.Focus()
	}
	check := func(k tea.KeyPressMsg) {
		t.Helper()
		fast.update(k, true)
		reference.area, _ = reference.area.Update(k)
		reference.text = reference.area.Value()
		rows, caret := fast.layout(3)
		want := reference.rows(9, 3)
		wantCaret := reference.area.Cursor()
		if fast.text != reference.text || !slices.Equal(rows, want) || caret == nil || wantCaret == nil || caret.Position != wantCaret.Position {
			t.Fatalf("key %q: text %q vs %q, rows %q vs %q, caret %v vs %v", k.String(), fast.text, reference.text, rows, want, caret, wantCaret)
		}
	}
	// Cross two wrap boundaries and then exceed the viewport; Bubbles must
	// take over before a printable key would move the caret out of view.
	for _, ch := range "ABCDEFGHIJKLMNOPQRSTUVWXYZab" {
		check(tea.KeyPressMsg{Code: ch, Text: string(ch)})
	}
	for _, p := range []*promptEditor{&fast, &reference} {
		p.set("a b")
	}
	for _, k := range []tea.KeyPressMsg{
		{Code: tea.KeyLeft}, {Code: 'c', Text: "C", Mod: tea.ModShift},
		{Code: tea.KeyLeft, Mod: tea.ModShift}, {Code: 'd', Text: "d"},
		{Code: '界', Text: "界"}, {Code: 'x', Text: "x"},
	} {
		check(k)
	}
}

func TestProseWrapKeepsEveryCellAndLinkAfterResize(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	token := strings.Repeat("x", 76)
	user := " " + token + " last typed word"
	linkLabel := strings.Repeat("界", 80)
	assistant := "See [" + linkLabel + "](https://example.org/ref) then continue."
	s.merge([]sdk.Entry{entry(3, "user", user), entry(4, "assistant", assistant)})
	for _, width := range []int{200, 80, 40, 53, 80} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 34})
		frame := ansi.Strip(assertFrame(t, m))
		var typed, prose strings.Builder
		for _, row := range strings.Split(frame, "\n") {
			if text, ok := strings.CutPrefix(row, "│ "); ok {
				typed.WriteString(strings.ReplaceAll(strings.TrimSuffix(text, "┃"), " ", ""))
			}
		}
		for _, row := range m.blockRows(s, s.shownIndex("4"), width-4) {
			plain := ansi.Strip(row.text)
			prose.WriteString(strings.ReplaceAll(plain, " ", ""))
			if col := strings.Index(plain, "界"); col >= 0 {
				if uri := linkAt(row.text, stringWidth(plain[:col])); uri != ";https://example.org/ref" {
					t.Fatalf("width %d: wrapped link lost its target: %q", width, uri)
				}
			}
			if col := strings.Index(plain, "then"); col >= 0 && linkAt(row.text, stringWidth(plain[:col])) != "" {
				t.Fatalf("width %d: link overflowed into following prose", width)
			}
		}
		if !strings.Contains(typed.String(), token+"lasttypedword") || prose.String() != "See"+linkLabel+"thencontinue." {
			t.Fatalf("width %d: content was clipped:\ntyped %q\nprose %q", width, typed.String(), prose.String())
		}
	}
}

func TestInlineMaterialsKeepTheirOwnCells(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOn})
	m.Update(tea.BackgroundColorMsg{Color: color.White})
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	const text = "Use `hush stop and resume` then app/main.go:12 or [the guide](https://example.org)"
	for _, mode := range []FillMode{FillOn, FillOff} {
		m.options.Fills = mode
		m.refreshTheme()
		rows := m.renderDocument(renderKey{}, x.Markdown, text, "", 16, "/workspace", false)
		var chip, path, web, plain int
		for _, row := range rows {
			for _, line := range uv.NewStyledString(row).Lines(ansi.GraphemeWidth) {
				for _, cell := range line {
					switch {
					case cell.Link.URL == "file:///workspace/app/main.go":
						path++
						if cell.Style.Underline != uv.UnderlineStyleDotted || cell.Link.Params != "line=12" || cell.Style.Bg != nil || cell.Style.Fg != nil {
							t.Fatalf("bad path cell: %+v", cell)
						}
					case cell.Link.URL == "https://example.org":
						web++
						if cell.Style.Underline != uv.UnderlineStyleSingle || cell.Style.Bg != nil || cell.Style.Fg != nil {
							t.Fatalf("bad web link cell: %+v", cell)
						}
					case cell.Style.Bg != nil || cell.Style.Attrs&uv.AttrBold != 0:
						if (mode == FillOn) != (cell.Style.Bg != nil) || cell.Style.Fg != nil || cell.Style.Underline != uv.UnderlineStyleNone {
							t.Fatalf("bad chip cell: %+v", cell)
						}
						if cell.Content != " " {
							chip++
						}
					case cell.Content != " ":
						plain++
					}
				}
			}
		}
		if chip != len("hushstopandresume") || path != len("app/main.go:12") || web != len("the guide") || plain != len("Usethenor") {
			t.Fatalf("styles escaped the intended cells in %s: chip=%d path=%d web=%d plain=%d\n%q", mode, chip, path, web, plain, rows)
		}
	}
}

func TestScrollAnchorSurvivesTailGrowth(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	for i := 3; i <= 80; i++ {
		s.merge([]sdk.Entry{entry(i, "assistant", fmt.Sprintf("immutable row %d", i))})
	}
	assertFrame(t, m)
	m.scroll(-12)
	before := assertFrame(t, m)
	floor := m.frame.transcriptHeight
	content := func(frame string) string {
		lines := strings.Split(frame, "\n")[:floor]
		for i, row := range lines {
			lines[i] = ansi.Cut(row, 0, m.width-2) // the scrollbar may move independently
		}
		return strings.Join(lines, "\n")
	}
	before = content(before)
	at := m.frame.top
	s.merge([]sdk.Entry{entry(81, "assistant", strings.Repeat("tail\n", 60))})
	s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{{Type: "text", Text: "tail is streaming"}}})
	s.activity.responding = true
	s.streamRevision++
	after := assertFrame(t, m)
	if m.frame.top != at || before != content(after) {
		t.Fatal("visible anchor drifted")
	}
	press(m, tea.KeyEnd, tea.ModCtrl)
	if !s.following || !strings.Contains(assertFrame(t, m), "tail is streaming") {
		t.Fatal("ctrl+end failed to follow")
	}
}

func TestScrollPositionAndResumeAffordance(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	for i := 3; i <= 80; i++ {
		s.merge([]sdk.Entry{entry(i, "assistant", fmt.Sprintf("row %d", i))})
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	body := ansi.Strip(assertFrame(t, m))
	bottom := m.frame.thumbStart
	if !m.frame.hasAbove || m.frame.hasBelow || bottom == 0 || !strings.Contains(body, "┃") || strings.Contains(body, "resume") {
		t.Fatalf("wrong scrollbar at the tail:\n%s", body)
	}
	press(m, tea.KeyHome, tea.ModCtrl)
	body = ansi.Strip(assertFrame(t, m))
	if m.frame.hasAbove || !m.frame.hasBelow || m.frame.thumbStart != 0 ||
		!strings.Contains(body, "output below · ctrl+end resume") {
		t.Fatalf("missing paused-following affordance:\n%s", body)
	}
	press(m, tea.KeyEnd, tea.ModCtrl)
	if body := ansi.Strip(assertFrame(t, m)); m.frame.thumbStart != bottom || strings.Contains(body, "resume") {
		t.Fatal("Ctrl+End did not resume following")
	}
}

func TestScrollbarIgnoresToolOnlyLiveBlocks(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{{Type: "toolCall", ID: "call", Name: "fixture"}}})
	s.streamRevision++
	s.activity.responding = true // activity evidence is not a transcript row
	assertFrame(t, m)
	if m.frame.hasAbove || m.frame.hasBelow {
		t.Fatal("tool-only activity was counted as a hidden transcript block")
	}
}

func TestDedicatedNoticePreservesPromptAndHints(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m.notify(m.active, "Backend rejected the request: "+strings.Repeat("detail ", 40), x.Danger)
	m.editor.set("try")
	frame := ansi.Strip(assertFrame(t, m))
	rows := strings.Split(frame, "\n")
	rule := m.frame.ruleY
	if rule < 1 || !strings.Contains(rows[rule-1], "Backend rejected") || !strings.Contains(rows[rule-1], "…") ||
		!strings.Contains(rows[rule], "enter send") || !strings.Contains(rows[rule+1], "you › try") || strings.Contains(rows[rule+1], "enter send") {
		t.Fatalf("notice, rule, or prompt misplaced:\n%s", frame)
	}
	s.info.Status = x.Running
	frame = ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "enter steer") || !strings.Contains(frame, "esc stop") {
		t.Fatalf("notice hid busy controls:\n%s", frame)
	}
}

func TestControlInkChecksContrast(t *testing.T) {
	for _, colors := range []struct{ background, foreground color.Color }{
		{color.White, color.NRGBA{R: 27, G: 28, B: 35, A: 255}},
		{color.NRGBA{R: 30, G: 30, B: 46, A: 255}, color.NRGBA{R: 205, G: 214, B: 244, A: 255}},
	} {
		m, s := demo(t, Options{Fills: FillOn})
		m.Update(tea.BackgroundColorMsg{Color: colors.background})
		m.Update(tea.ForegroundColorMsg{Color: colors.foreground})
		m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
		ink := m.palette.control
		if ink == nil || contrastRatio(ink, colors.background) < 4.5 {
			t.Fatalf("insufficient contrast with %v", colors.background)
		}
		s.info.Status = x.Running
		for _, hint := range m.ruleHints(s, false, false) {
			if strings.Contains(hint, "\x1b[2m") {
				t.Fatalf("actionable key is faint: %q", hint)
			}
		}
		m.options.Fills = FillOff
		m.refreshTheme()
		if m.control("control") != "control" {
			t.Fatal("unknown/translucent background did not use normal foreground")
		}
	}
}

func TestOverlayEscAndScopedChildMessages(t *testing.T) {
	m, s := demo(t, Options{CustomModels: true})
	s.info.Status = x.Running
	var found string
	b := maxEffects
	r := m.route(m.active, coreID)
	m.openDialog(r, x.SelectDialog{Title: "Pick beneath", Placement: x.Sheet, Choices: []x.Choice{{ID: "keep", Label: "Keep"}}, Done: func(_ x.Context, v x.Reply[string]) x.Outcome { found = v.Value; return nil }}, &b)
	sheet := m.sheet
	m.openPalette(r, &b)
	m.openDialog(r, x.CustomDialog{New: func(x.RenderContext) tea.Model {
		return childModel{content: "\x1b[41mchild\x1b]52;c;payload\x07"}
	}, Result: func(tea.Model) (any, bool) { return nil, false }}, &b)
	v := assertFrame(t, m)
	if strings.Contains(v, "\x1b]52;") || strings.Contains(v, "\x1b[41m") {
		t.Fatal("unsafe custom content reached parent frame")
	}
	press(m, tea.KeyEscape, 0)
	if len(m.overlays) != 1 || m.sheet != sheet || !m.escapeAt.IsZero() {
		t.Fatal("Escape did not close only top overlay")
	}
	press(m, tea.KeyEscape, 0)
	if len(m.overlays) != 0 || m.sheet != sheet {
		t.Fatal("did not return to sheet")
	}
	press(m, tea.KeyEnter, 0)
	if m.sheet != nil || found != "keep" {
		t.Fatal("sheet did not retain selection and result routing")
	}
}

type childModel struct{ content string }

func (childModel) Init() tea.Cmd                         { return nil }
func (c childModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return c, nil }
func (c childModel) View() tea.View                      { return tea.NewView(c.content) }

func TestCustomDialogStyleEndsBeforeBorder(t *testing.T) {
	m, _ := demo(t, Options{CustomModels: true})
	b := maxEffects
	m.openDialog(m.route(m.active, coreID), x.CustomDialog{Title: "Style fixture", New: func(x.RenderContext) tea.Model {
		return childModel{content: "\x1b[31mred without reset\n\x1b[1;35mclipped " + strings.Repeat("styled ", 25) +
			"\n\x1b[32mgreen" + ansi.ResetStyle + " plain"}
	}, Result: func(tea.Model) (any, bool) { return nil, false }}, &b)
	frame := assertFrame(t, m)
	for _, prefix := range []string{"\x1b[31mred without reset", "\x1b[1;35mclipped"} {
		_, tail, found := strings.Cut(frame, prefix)
		row, _, _ := strings.Cut(tail, "\n")
		beforeBorder, _, hasBorder := strings.Cut(row, "│")
		if !found || !hasBorder || !strings.Contains(beforeBorder, ansi.ResetStyle) {
			t.Fatalf("child style reached the border after %q: %q", prefix, row)
		}
	}
	if !strings.Contains(frame, "\x1b[32mgreen"+ansi.ResetStyle+" plain") {
		t.Fatal("child's own style reset was stripped")
	}
}

func TestPasteLinkAndSelection(t *testing.T) {
	var path, site, clipboard string
	var line int
	m, _ := demo(t, Options{WorkspaceLocal: true,
		OpenFile: func(_ context.Context, p string, n int) error { path, line = p, n; return nil },
		OpenURL:  func(_ context.Context, s string) error { site = s; return nil },
		CopyText: func(_ context.Context, s string) error { clipboard = s; return nil },
	})
	assertFrame(t, m)
	clicked := map[string]bool{}
	for y, row := range m.frame.rows[:m.frame.transcriptHeight] {
		for col := 0; col < m.width; col++ {
			link := decodeLink(linkAt(row, col))
			if link.code || link.url == "" && link.path == "" {
				continue
			}
			key := link.path + link.url
			if clicked[key] {
				continue
			}
			clicked[key] = true
			p := tea.Mouse{X: col, Y: y, Button: tea.MouseLeft}
			m.mouse(tea.MouseClickMsg(p))
			cmd := m.mouse(tea.MouseReleaseMsg(p))
			if cmd == nil {
				t.Fatal("no open command", link)
			}
			m.finishWork(cmd().(workMsg))
		}
	}
	if path != "/workspace/app/main.go" || line != 12 || site != "https://example.org/docs" {
		t.Fatalf("file=%s:%d URL=%s", path, line, site)
	}
	assertFrame(t, m)
	for y, row := range m.frame.rows[:m.frame.transcriptHeight] {
		if !strings.Contains(row, "What changed") {
			continue
		}
		plain := ansi.Strip(row)
		x := ansi.StringWidth(plain[:strings.Index(plain, "What changed")])
		m.mouse(tea.MouseClickMsg(tea.Mouse{X: x, Y: y, Button: tea.MouseLeft}))
		cmd := m.mouse(tea.MouseReleaseMsg(tea.Mouse{X: x + 3, Y: y, Button: tea.MouseLeft}))
		if cmd == nil {
			t.Fatal("selection did not copy")
		}
		m.finishWork(cmd().(workMsg))
		break
	}
	if clipboard != "What" {
		t.Fatalf("clipboard %q", clipboard)
	}
	m.options.WorkspaceLocal = false
	b := maxEffects
	cmd := m.openLink(m.route(m.active, coreID), "line=12;file:///workspace/app/main.go", &b)
	m.finishWork(cmd().(workMsg))
	if clipboard != "/workspace/app/main.go" {
		t.Fatal("remote file path not copied")
	}
}

func TestColorAndClock(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff})
	m.Update(tea.BackgroundColorMsg{Color: color.White})
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	if m.theme.Fills || m.palette.soft != nil || strings.Contains(assertFrame(t, m), "\x1b[48;") {
		t.Fatal("fills off generated background cells")
	}
	m.connection = x.Connecting
	if cmd := m.syncClock(); cmd == nil || m.clockPending == 0 {
		t.Fatal("visible effect did not arm the clock")
	}
	tick := m.clockPending
	m.connection = x.Connected
	if cmd := m.syncClock(); cmd != nil || m.clockPending != 0 {
		t.Fatal("idle clock is still armed")
	}
	m.Update(effectTick{token: tick})
	if m.phase != 0 || s.info.Status != x.Idle {
		t.Fatal("stale tick advanced phase")
	}
}

func TestStreamingMatchesFullRender(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOn, ReducedMotion: true})
	m.Update(tea.BackgroundColorMsg{Color: color.White})
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	for _, s := range []string{
		"Use `hush stop` and `app/main.go:12` before checking the next rule.\n\nThen wait.",
		"# Heading\n\nAn ordinary paragraph with exact content.\n\nNext paragraph.\n\nAnd another\n\n```go\npackage main\nfunc demo() {\n\tprintln(42)\n}\n```\n\nAfter the code.",
		"First paragraph.\n\n> Some quoted lines\n> More\n\n- list\n- item\n\n| Header | Here |\n| --- | --- |\n| data | other |\n",
		"First [ref] paragraph.\n\nNext.\n\n[ref]: https://example.org\n",
	} {
		for i := 0; i <= len(s); i++ {
			key := renderKey{owner: m.active}
			live := m.renderDocument(key, x.Markdown, s[:i], "", 56, "/workspace", true)
			whole := m.renderDocument(key, x.Markdown, s[:i], "", 56, "/workspace", false)
			if strings.Join(live, "\n") != strings.Join(whole, "\n") {
				t.Fatalf("at %d %q\nlive %q\nfull %q", i, s[:i], live, whole)
			}
		}
	}
}

var updateGoldens = flag.Bool("update", false, "rewrite golden terminal frames")

func golden(t *testing.T, name string, m *Model) {
	t.Helper()
	body := assertFrame(t, m)
	var rows []string
	for row := range strings.SplitSeq(body, "\n") {
		rows = append(rows, strings.TrimRight(ansi.Strip(row), " "))
	}
	data := strings.Join(rows, "\n") + "\n"
	path := filepath.Join("testdata", name+".txt")
	if *updateGoldens {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil || data != string(want) {
		t.Fatalf("golden %s: %v\n--- got ---\n%s--- want ---\n%s", name, err, data, want)
	}
}

func TestCoreGoldens(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 64, Height: 18})
	s.info.Status = x.Running
	s.streamRevision++
	s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{{Type: "text", Text: "## Streaming\n\n```go\nfmt.Print(\"Hello\")"}}})
	s.tools = []x.Tool{{CallID: "t", Name: "bash", Live: true, ArgumentsJSON: `{"command":"go test ./..."}`, Output: "ok  example/pkg"}}
	golden(t, "core-stream", m)
	b := maxEffects
	m.openDialog(m.route(m.active, coreID), x.ConfirmDialog{Title: "Confirm", Message: "This is a layered overlay."}, &b)
	golden(t, "core-overlay", m)
}
