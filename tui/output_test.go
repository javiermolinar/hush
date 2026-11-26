package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestOutputLineClipsLikeANSIAtCheckpointBoundaries(t *testing.T) {
	for _, line := range []string{"abcdef", "é界📎e\u0301", strings.Repeat("🇪🇸e\u0301界abc", 125)} {
		r := newOutputLine(line)
		for left := 0; left < r.width; left += 3 {
			if got, want := r.window(left, 18), ansi.Cut(line, left, left+18); got != want {
				t.Fatalf("column %d: got %q, want %q", left, got, want)
			}
		}
	}
}

func TestOutputWindowCacheUsesSizeAndCurrentRows(t *testing.T) {
	v := newOutputViewer(strings.Repeat("🇪🇸e\u0301界abc", 125) + "\nold second row")
	want := func(row, left, width int) string {
		return fit(ansi.Cut(v.lines[row].text, left, left+width), width)
	}
	check := func(row, left, width int) string {
		t.Helper()
		got := v.window(row, left, width)
		if expected := want(row, left, width); got != expected || ansi.StringWidth(got) != width {
			t.Fatalf("window (%d, %d, %d): got %q, want %q", row, left, width, got, expected)
		}
		return got
	}
	before := check(1, 0, 18)
	for i := range 2 * maxOutputWindows {
		check(0, i, 18+i%3)
	}
	if len(v.windows) > maxOutputWindows {
		t.Fatal("output cache exceeded its bound")
	}
	check(1, 0, 18)
	v.reflowReview(12)
	if after := check(1, 0, 18); after == before {
		t.Fatal("reflow reused pixels from the old line at the same index")
	}
}

func TestShortOutputViewerClampsAgainstContentWidth(t *testing.T) {
	m, s := demo(t, Options{Intro: &IntroOptions{}, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m.ensureThread("2")
	m.openedTab("2")
	m.editor.set("one\ntwo\nthree")
	b := maxEffects
	r := m.route(m.active, coreID)
	m.openDialog(r, x.SelectDialog{Placement: x.Sheet, Choices: []x.Choice{{ID: "1"}, {ID: "2"}, {ID: "3"}}}, &b)
	m.notify(m.active, "Fixture warning", x.Warning)
	s.merge([]sdk.Entry{toolResultEntry(3, "r", "fixture", strings.Repeat("x", 80)+"last", false, time.Unix(10, 0))})
	m.openToolOutput(r, "3/0")
	d := m.focusedDialog()
	if d == nil || d.output == nil {
		t.Fatal("viewer did not open")
	}
	assertFrame(t, m)
	if m.frame.dialogWidth != m.pane().width {
		t.Fatal("fixture did not use the dock content width")
	}
	m.outputMove(d, 0, d.output.widest)
	want := d.output.widest - m.frame.dialogWidth
	if d.output.left != want || !strings.Contains(ansi.Strip(assertFrame(t, m)), "last") {
		t.Fatalf("after horizontal scroll: got %d, want %d and the final text", d.output.left, want)
	}
	press(m, tea.KeyLeft, 0)
	if d.output.left != want-m.frame.dialogWidth/3 {
		t.Fatal("first scroll left used the wrong width")
	}
}

func TestToolOutputViewerShowsAndCopiesRetainedResult(t *testing.T) {
	var copied string
	m, state := demo(t, Options{ReducedMotion: true, CopyText: func(_ context.Context, text string) error {
		copied = text
		return nil
	}})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 38})
	var lines []string
	for i := range 42 {
		lines = append(lines, fmt.Sprintf("fixture line %02d", i+1))
	}
	lines = append(lines, strings.Repeat("wide field ", 1800)+"last field", "final row \x1b raw")
	raw := strings.Join(lines, "\n") + "\n"
	s := toolResultEntry(3, "r", "fixture", raw, false, time.Unix(10, 0))
	state.merge([]sdk.Entry{s})
	state.toggleBurst("3/0")
	state.toggleBurst("3/0/0")
	body := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, "fixture line 24") || strings.Contains(body, "fixture line 25") ||
		!strings.Contains(body, "20 more lines · view output") || m.frame.openActivity != "output/3/0" {
		t.Fatalf("expected a 24-line preview and a keyboard target:\n%s", body)
	}
	press(m, tea.KeyEnter, 0)
	d := m.focusedDialog()
	if d == nil || d.output == nil || d.output.raw != raw {
		t.Fatal("overflow did not open the complete retained result")
	}
	body = ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, "Tool output · fixture") || !strings.Contains(body, "fixture line 01") ||
		!strings.Contains(body, "c copy all") {
		t.Fatal("viewer or its controls not visible")
	}
	press(m, tea.KeyEnd, 0)
	body = ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, "final row ␛ raw") || !strings.Contains(body, " / 44") {
		t.Fatal("End did not reach the final result row")
	}
	press(m, tea.KeyRight, 0)
	if d.output.left == 0 {
		t.Fatal("wide result cannot be scrolled horizontally")
	}
	m.outputMove(d, 0, d.output.widest)
	if !strings.Contains(ansi.Strip(assertFrame(t, m)), "last field") {
		t.Fatal("viewer did not expose the tail of a wide line")
	}
	// Copy through the rendered control, not by calling the viewer directly.
	f := m.frame
	cmd := m.mouse(tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: f.dialogX + 2, Y: f.dialogY + f.dialogHeight - 1}))
	if cmd == nil {
		t.Fatal("copy footer has no hit target")
	}
	m.finishWork(cmd().(workMsg))
	if copied != raw || m.focusedDialog() != d {
		t.Fatal("copy was truncated, sanitized, or dismissed the viewer")
	}
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != nil {
		t.Fatal("Escape did not close the output viewer")
	}
}
