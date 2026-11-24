package extension

import "github.com/javiermolinar/hush/sdk"

// Entry is a bounded, detached transcript record. Record.ThreadID is the
// origin, not necessarily the thread viewing an inherited entry. CustomKind
// and DataJSON are populated only by a source that exposes custom kinds/data.
type Entry struct {
	Record     sdk.Entry
	CustomKind string
	DataJSON   string
	Truncated  bool
}

// Kind is the renderer lookup key. A custom kind takes precedence, if present.
func (e Entry) Kind() string {
	if e.CustomKind != "" {
		return e.CustomKind
	}
	return e.Record.Kind
}

// Tool is the bounded display state of one call. CallID identifies a call in
// its owning thread; TaskID, arguments and result may not yet be available.
// Output is the currently retained output, not a delta. ResultEntryID points
// to the immutable result if available, and ResultText is its text projection.
type Tool struct {
	CallID        string
	Name          string
	Status        string
	TaskID        sdk.ID
	ResultEntryID sdk.ID
	ArgumentsJSON string
	Output        string
	ResultText    string
	DroppedBytes  uint64
	DroppedLines  uint64
	Failed        bool
	Live          bool
	Truncated     bool
}

// RenderContext describes the rendering budget and presentation state for
// this slot. Width is in terminal cells; MaxLines and MaxBytes cap output.
// Workspace is available through the owning Thread. Stable output must be a
// pure function of this context and the supplied input, not mutable closures.
type RenderContext struct {
	Context
	Width    int
	MaxLines int
	MaxBytes int
	Theme    Theme
	Expanded bool
	Live     bool
}

// Theme describes the current terminal palette and fill policy. Use semantic
// roles rather than hard-coded colors. Revision changes on palette changes.
type Theme struct {
	Revision        uint64
	BackgroundKnown bool
	Light           bool
	Fills           bool
	TrueColor       bool
}

// Tone is a semantic color or emphasis role resolved by the shell's theme.
type Tone string

const (
	// Plain uses the default foreground. The zero tone means Plain too.
	Plain Tone = "plain"
	// Muted is secondary text.
	Muted Tone = "muted"
	// Accent marks interactive or highlighted text.
	Accent Tone = "accent"
	// Good marks success.
	Good Tone = "good"
	// Warning marks a warning or parked state.
	Warning Tone = "warning"
	// Danger marks an error.
	Danger Tone = "danger"
)

// Span is unstyled text plus optional presentation and a hit target. The host
// derives hit geometry from the drawn span after wrapping or truncation.
type Span struct {
	Text   string
	Tone   Tone
	Action ActionRef // zero ID makes the span noninteractive
	Hint   string    // optional hover text, not part of the span width
}

// Line is a logical line that can wrap to multiple terminal rows. Chrome
// segments are the exception: the shell truncates or drops those to fit.
type Line []Span

// Format selects a host-rendered document format. No format accepts terminal
// escapes. Markdown, code and diff use the host's normal renderers.
type Format string

const (
	// Text renders literal text and is the default format.
	Text Format = "text"
	// UserText renders a verbatim user message against the terminal’s fill policy.
	UserText Format = "user-text"
	// Markdown renders a Markdown document with the host's link policy.
	Markdown Format = "markdown"
	// Code renders a code block using the optional language hint.
	Code Format = "code"
	// Diff renders a diff using the host's diff styles.
	Diff Format = "diff"
)

// Block is either explicit Lines or a Text document (when Lines is nil).
// The host owns sanitization, layout, links, and generation of terminal escapes.
type Block struct {
	Lines    []Line
	Text     string
	Format   Format
	Language string
	Tone     Tone
}

// Content is a sequence of display blocks, independent of the terminal renderer.
type Content []Block

// EntryRenderer matches a single entry kind. Returning false uses the default
// renderer. The callback is pure and runs within RenderContext's budget.
type EntryRenderer struct {
	Kind   string
	Render func(RenderContext, Entry) (Content, bool)
}

// ToolRenderer matches an exact tool name. The same pure hook may draw call,
// live output and final result using the Tool snapshot. The live activity
// slot passes Live=true and MaxLines=1. False uses the default.
type ToolRenderer struct {
	Name   string
	Render func(RenderContext, Tool) (Content, bool)
}

// ActivityTool identifies an activity item representing a tool call.
const ActivityTool = "tool"

// ActivityItem is a single item in an existing shell activity group. Its
// source IDs can be empty; Kind and Label describe the projected activity.
type ActivityItem struct {
	Kind    string
	Label   string
	EntryID sdk.ID
	CallID  string
}

// Activity is a bounded sample from one group, not a transcript scan. Omitted
// counts the group's additional items outside the supplied sample.
type Activity struct {
	Items   []ActivityItem
	Omitted int
}

// Summarizer supplies a short note alongside the committed result counts.
// Lower Order runs first, breaking ties by qualified key. The first handler
// returning a nonempty line and true supplies the note.
type Summarizer struct {
	Key       string
	Order     int
	Summarize func(RenderContext, Activity) (Line, bool)
}

// SegmentSlot selects where the shell places a one-line segment.
type SegmentSlot uint8

const (
	// StatusLeft places a segment on the left of the meta row.
	StatusLeft SegmentSlot = iota
	// StatusRight places it on the right of the meta row.
	StatusRight
	// Tab places it in each tab, with that tab's thread as owner.
	Tab
)

// Segment is a keyed, pure contribution to tab or status chrome. Lower Order
// draws first within a slot. Higher Priority survives longer when space is
// scarce. An empty line hides the segment; ties resolve by qualified key.
type Segment struct {
	Key      string
	Slot     SegmentSlot
	Order    int
	Priority int
	Render   func(RenderContext) Line
}

// WidgetPlacement selects one of the bounded editor-adjacent widget slots.
type WidgetPlacement uint8

const (
	// AboveEditor places a widget between the transcript and prompt.
	AboveEditor WidgetPlacement = iota
	// BelowEditor places it below the prompt, outside the editor itself.
	BelowEditor
)

// Widget contributes nonfocused content near the editor, not an editor
// replacement. Lower Order draws first; ties resolve by qualified key.
// The shell can hide widgets to preserve prompt, sheet and footer space.
type Widget struct {
	Key       string
	Placement WidgetPlacement
	Order     int
	Render    func(RenderContext) Content
}
