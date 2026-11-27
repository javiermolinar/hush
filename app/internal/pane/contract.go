// Package pane defines the client-side contract for local multiplexer control.
package pane

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/javiermolinar/hush/sdk"
)

type Placement string

const (
	Right Placement = "right"
	Below Placement = "below"
	Left  Placement = "left"
	Above Placement = "above"
	Tab   Placement = "tab"
)

// Capabilities describes an available adapter. Right and below are required.
// The zero value offers only those placements and no output capture.
type Capabilities struct {
	Left  bool
	Above bool
	Tabs  bool
	Read  bool
}

func (c Capabilities) Placements() []Placement {
	placements := []Placement{Right, Below}
	for _, p := range []Placement{Left, Above, Tab} {
		if c.Supports(p) {
			placements = append(placements, p)
		}
	}
	return placements
}

func (c Capabilities) Supports(p Placement) bool {
	switch p {
	case Right, Below:
		return true
	case Left:
		return c.Left
	case Above:
		return c.Above
	case Tab:
		return c.Tabs
	default:
		return false
	}
}

// Scope is a client-local identity for a verified binding to a multiplexer
// instance. Keep it for the life of that binding; use a new one after rebinding.
// The zero value owns no references.
type Scope struct{ key *byte }

func NewScope() Scope { return Scope{key: new(byte)} }

// Ref is an opaque, comparable client-side reference, not a wire or model ID.
// A zero Ref is invalid. A reference does not attest that a pane is still live.
type Ref struct {
	scope Scope
	id    string
}

// Ref binds an exact adapter-local pane ID to this scope. Adapters must mint
// references only for verified IDs, not for transient indexes or focus aliases.
func (s Scope) Ref(id string) Ref {
	if s.key == nil || id == "" {
		return Ref{}
	}
	return Ref{scope: s, id: id}
}

// Resolve recovers the ID only in the issuing scope. The adapter must still
// verify it identifies the same live pane before using it.
func (s Scope) Resolve(ref Ref) (string, bool) {
	if s.key == nil || ref.scope != s || ref.id == "" {
		return "", false
	}
	return ref.id, true
}

type LaunchMode string

const (
	LaunchArgv  LaunchMode = "argv"
	LaunchShell LaunchMode = "shell"
)

// Launch is a tagged request: Program and Args are for argv; Text is for
// explicit shell execution. Neither mode is implemented by typing into a pane.
type Launch struct {
	Mode    LaunchMode
	Program string
	Args    []string
	Text    string
}

type OpenRequest struct {
	Placement Placement
	Dir       string                // client-local working directory, not a model argument
	Directory *sdk.PaneFileIdentity // required when Dir comes from the backend
	Launch    Launch
}

// Validate checks request shape only. It does not authorize the command or
// verify the working directory, origin, or adapter availability.
func (r OpenRequest) Validate(c Capabilities) error {
	if !c.Supports(r.Placement) {
		return errors.New("unsupported pane placement")
	}
	if !filepath.IsAbs(r.Dir) || hasNUL(r.Dir) {
		return errors.New("expected an absolute client working directory")
	}
	l := r.Launch
	switch l.Mode {
	case LaunchArgv:
		if l.Program == "" || hasNUL(l.Program) || l.Text != "" {
			return errors.New("argv launch requires a program and no shell text")
		}
		for _, arg := range l.Args {
			if hasNUL(arg) {
				return errors.New("argv argument contains NUL")
			}
		}
	case LaunchShell:
		if l.Text == "" || hasNUL(l.Text) || l.Program != "" || l.Args != nil {
			return errors.New("shell launch requires text and no argv")
		}
	default:
		return errors.New("unknown pane launch mode")
	}
	return nil
}

func hasNUL(s string) bool { return strings.IndexByte(s, 0) >= 0 }

type ListRequest struct{}

type FocusRequest struct{ Pane Ref }

const (
	MaxReadLines = 200
	MaxReadBytes = 16 * 1024
)

type ReadRequest struct {
	Pane  Ref
	Lines int
}

// Validate checks capability and request shape. It does not authorize the
// read or verify the pane's scope or liveness.
func (r ReadRequest) Validate(c Capabilities) error {
	if !c.Read {
		return errors.New("pane output capture is not supported")
	}
	if r.Pane == (Ref{}) {
		return errors.New("expected a pane reference")
	}
	if r.Lines < 1 || r.Lines > MaxReadLines {
		return errors.New("pane read line count is out of range")
	}
	return nil
}

// ReadSnapshot is a plain UTF-8 tail of the terminal's available text, at most
// the requested number of lines and MaxReadBytes, without terminal controls.
// Truncated reports text omitted to meet those limits, not history the
// multiplexer discarded before the read.
type ReadSnapshot struct {
	Text      string
	Truncated bool
}

type Outcome string

const (
	Confirmed Outcome = "confirmed"
	Rejected  Outcome = "rejected" // known not to have been dispatched
	Unknown   Outcome = "unknown"  // lacks confirmation; a mutation may have happened
)

// Result carries a value only for a confirmed observation or effect. A missing
// result (the zero value) is unknown, never a rejection or success.
type Result[T any] struct {
	outcome Outcome
	value   T
	err     error
}

func Confirm[T any](value T) Result[T]  { return Result[T]{outcome: Confirmed, value: value} }
func Reject[T any](err error) Result[T] { return Result[T]{outcome: Rejected, err: err} }
func Uncertain[T any](err error) Result[T] {
	return Result[T]{outcome: Unknown, err: err}
}

func (r Result[T]) Outcome() Outcome {
	if r.outcome == "" {
		return Unknown
	}
	return r.outcome
}

func (r Result[T]) Value() (T, bool) { return r.value, r.outcome == Confirmed }

// Err is an optional local diagnostic. It is not evidence of dispatch or a
// model-facing error message.
func (r Result[T]) Err() error { return r.err }

// Adapter is bound to the live Hush pane, never to whatever is currently
// focused. Operations may block; callers must run them off the UI loop.
// Revalidate the binding and the exact target before dispatch. Do not retry
// an open or focus whose native outcome is unknown.
type Adapter interface {
	Capabilities() Capabilities
	// Verify checks that the pinned local instance and calling terminal still
	// match. It must not change focus, create a pane, or rebind the adapter.
	Verify(context.Context) error
	// Open confirms only creation of a pane whose exact ID the native reply
	// supplied and the adapter verified, not command readiness, exit status,
	// or focus. Validate the request before dispatch.
	Open(context.Context, OpenRequest) Result[Ref]
	// List returns a read-only snapshot from the bound scope, with no
	// guarantee that those panes will still exist on a later call.
	List(context.Context, ListRequest) Result[[]Ref]
	// Focus confirms focus of the exact requested pane at the time of the
	// operation. Reject foreign references before dispatch.
	Focus(context.Context, FocusRequest) Result[Ref]
	// Read captures the exact terminal without changing focus. The caller must
	// supply a reference from a confirmed command open on its live route.
	// Listing a pane does not grant read access.
	Read(context.Context, ReadRequest) Result[ReadSnapshot]
}
