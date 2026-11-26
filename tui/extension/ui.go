package extension

import tea "charm.land/bubbletea/v2"

// Placement controls dialog lifetime; both kinds dock above the input rule.
type Placement uint8

const (
	// Overlay pushes onto the shell's stack (the default).
	Overlay Placement = iota
	// Sheet replaces the current sheet, below any stacked dialogs.
	Sheet
)

// Reply distinguishes dismissal from a valid zero value (such as selecting
// No or accepting empty text). Value is meaningful only when Accepted is true.
type Reply[T any] struct {
	Value    T
	Accepted bool
}

// Choice is a selectable row with a stable, nonempty ID unique in its list.
// The selector returns the ID, not the label or index.
type Choice struct {
	ID          string
	Label       string
	Description string
	Disabled    bool
	Action      string // optional verb for the consequence line; does not change acceptance
}

// SelectDialog requests a filterable list. Actions optionally expose existing
// registered actions while the list is focused. They receive the highlighted
// choice ID as Invocation.Target and do not themselves dismiss the list.
type SelectDialog struct {
	Placement Placement
	Title     string
	Choices   []Choice
	Actions   []ActionID
	Done      func(Context, Reply[string]) Outcome
}

func (SelectDialog) extensionEffect() {}

// ConfirmDialog requests a yes/no decision. Explicit No is accepted with a
// false value; Escape dismisses without accepting.
type ConfirmDialog struct {
	Placement Placement
	Title     string
	Message   string
	Done      func(Context, Reply[bool]) Outcome
}

func (ConfirmDialog) extensionEffect() {}

// ReviewDialog shows bounded plain-text details with scrolling and copying.
// Enter proceeds to the caller's next decision; Escape dismisses.
type ReviewDialog struct {
	Title string
	Text  string
	Done  func(Context, Reply[bool]) Outcome
}

func (ReviewDialog) extensionEffect() {}

// InputDialog requests text. Multiline opts into a multiline editor; Initial
// and Placeholder are ordinary text, not terminal control sequences.
type InputDialog struct {
	Placement   Placement
	Title       string
	Initial     string
	Placeholder string
	Multiline   bool
	Done        func(Context, Reply[string]) Outcome
}

func (InputDialog) extensionEffect() {}

// CustomDialog pushes an in-process Bubble Tea v2 model onto the dialog
// stack, when CustomModels is enabled. New receives the initial presentation
// context and returns a fresh model. The shell drives it as a component,
// never as a nested program. Result is polled with the latest model after
// Init and Update; true closes just that dialog and delivers the value.
// Escape bypasses the model and dismisses it instead. Only safe foreground
// and emphasis styling is accepted, and it is scoped to each body row.
type CustomDialog struct {
	Title  string
	New    func(RenderContext) tea.Model
	Result func(tea.Model) (any, bool)
	Done   func(Context, Reply[any]) Outcome
}

func (CustomDialog) extensionEffect() {}
