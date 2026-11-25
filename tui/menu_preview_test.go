package tui

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

const previewMarkdown = "# Status\n\n[the docs](https://example.com/secret) and `go test`\nwere **updated**.\x1b"
const previewPlain = "Status the docs and go test were updated.␛"

func TestMenuPreview(t *testing.T) {
	for _, tc := range []struct {
		source, want string
		markdown     bool
	}{
		{previewMarkdown, previewPlain, true},
		{"```go\nprintln(42)\n```\n\n<!-- hidden -->\n\nMore", "println(42) More", true},
		{"*See* [the manual][docs] and ![a chart](chart.png).\n\n[docs]: https://example.com/", "See the manual and a chart.", true},
		{`<div><b>Vis</b>ible <a href="https://example.org/private" title="a > b">status &amp; details</a><br>ready <!-- hidden --></div><p>Next</p>`, "Visible status & details ready Next", true},
		{"<pre><code>if (a &lt; b) {\n  check(&quot;ok&quot;);\n}</code></pre>", `if (a < b) { check("ok"); }`, true},
		{"<!-- hidden\n-->After the comment", "After the comment", true},
		{"<script>check(1 < 2)</script>\n\n<div><style>.private { color: red }</style><template>hidden</template>OK &#27;</div>", "OK ␛", true},
		{"Two\r\nrows, &#27; and &amp; 中文.", "Two␍ rows, ␛ and & 中文.", true},
		{"echo **literal**\nnext", "echo **literal** next", false},
	} {
		if got := menuPreview(tc.source, tc.markdown); got != tc.want {
			t.Errorf("preview of %q = %q, want %q", tc.source, got, tc.want)
		}
	}
	for _, got := range []string{
		menuPreview("# "+strings.Repeat("界", 5000), true),
		menuPreview("\xff"+strings.Repeat("界", 5000), false),
	} {
		if len(got) > menuPreviewBytes || len(got) < menuPreviewBytes-2 || !utf8.ValidString(got) || strings.ContainsAny(got, "\x1b\n") {
			t.Errorf("preview not safely bounded: %q", got)
		}
	}
}

type forkPreviewBackend struct{ menuBackend }

func (forkPreviewBackend) History(_ context.Context, id sdk.ID, _ sdk.HistoryRange, _ int) (sdk.HistoryPage, error) {
	return sdk.HistoryPage{ThreadID: id, Entries: []sdk.Entry{
		entry(1, "user", "Keep # literal"),
		entry(2, "assistant", previewMarkdown),
	}}, nil
}

func TestForkPickerShowsPlainPreview(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.backend = forkPreviewBackend{}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 25})
	m.finishWork(m.runAction(m.host.byID[forkPickerID], "")().(workMsg))
	body := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, previewPlain) || strings.Contains(body, "example.com/secret") || !strings.Contains(body, "Keep # literal") {
		t.Fatalf("unexpected fork previews:\n%s", body)
	}
}
