// Package extension is hush's public contract for trusted, statically linked
// TUI extensions. Both built-ins and external packages supply a Registration.
// Callbacks receive detached snapshots, never the shell model or SDK client.
package extension

import (
	"time"

	"github.com/javiermolinar/hush/sdk"
)

// APIVersion is the SemVer version of this Go source contract, not a wire ABI.
// During v0, extensions target the same minor; patch releases are compatible.
const APIVersion = "0.1.0"

// Registration describes one extension instance for one shell lifetime. A build
// explicitly calls its constructor and passes the registration to the host.
// IDs should be stable and globally unique (for example, "example.org/inbox").
// The host snapshots the registration, validates all contributions before Init,
// and does not support registering new contributions from callbacks.
type Registration struct {
	ID         string
	APIVersion string

	// Init is optional and runs once before dispatch. An error aborts startup.
	Init func(Context) (Outcome, error)
	// Shutdown is optional and runs once after dispatch has stopped and work
	// has been canceled. It is for nonblocking, instance-local cleanup only.
	Shutdown func(Context)

	Actions  []Action
	Events   []Subscription
	Entries  []EntryRenderer
	Tools    []ToolRenderer
	Activity []Summarizer
	Segments []Segment
	Widgets  []Widget
}

// Features is a set of optional operations the host can currently fulfill. It
// combines local host policy and negotiated support; it is not a wire bitset.
type Features uint64

const (
	// Catalog enables the model picker using a native or host-supplied catalog.
	Catalog Features = 1 << iota
	// Files enables workspace file listing for the owning thread.
	Files
	// Archive enables presentation archive changes, not durable deletion.
	Archive
	// Mode enables permission-mode changes.
	Mode
	// Titles enables thread display title changes.
	Titles
	// Approvals enables approval observations and decisions.
	Approvals
	// CustomModels enables local Bubble Tea model dialogs.
	CustomModels
	// Skills enables listing the actual skills the backend loaded.
	Skills
	// Worktrees enables explicit creation of a thread with its own checkout.
	Worktrees
)

// Has reports whether all the requested features are available (including an empty set).
func (f Features) Has(required Features) bool { return f&required == required }

// Connection is the shell's connection state, not a thread's run state.
type Connection string

const (
	// Connecting means the shell is establishing or recovering its connection.
	Connecting Connection = "connecting"
	// Connected means the shell has an established connection.
	Connected Connection = "connected"
	// Disconnected means there is no usable connection.
	Disconnected Connection = "disconnected"
)

// ThreadStatus is a coarse presentation state, not a settlement or an idle
// guarantee. The zero value means the host has no current observation.
type ThreadStatus string

const (
	// Idle means there is no observed activity.
	Idle ThreadStatus = "idle"
	// Running means the host observes live activity.
	Running ThreadStatus = "running"
	// Parked means the thread is waiting for an external condition or input.
	Parked ThreadStatus = "parked"
	// Failed means the host observes a thread failure.
	Failed ThreadStatus = "failed"
	// Stopped means ordinary activity was aborted.
	Stopped ThreadStatus = "stopped"
	// Done is a last-observed transition from work to idle, not a receipt.
	Done ThreadStatus = "done"
	// Scheduled means a host reported future work.
	Scheduled ThreadStatus = "scheduled"
)

// Thread is a value snapshot for chrome and actions, not a transcript or a
// full server state. Empty optional fields indicate unavailable data.
type Thread struct {
	ID               sdk.ID
	LastEntryID      sdk.ID
	Workspace        string
	Branch           string
	Title            string
	Mode             string
	Archived         bool
	Model            sdk.Model
	Thinking         string
	Status           ThreadStatus
	Queued           int
	Usage            sdk.Usage // the thread's own reported usage, as in core state; excludes subagents
	SubagentUsage    sdk.Usage // the thread's subagents' usage, in addition to Usage; zero when not followed
	ContextUse       ContextUse
	ContextThreshold uint64
	Forked           bool
	ParentID         sdk.ID
	Preview          string
	LastMessageAt    time.Time // zero when no committed chat timestamp is known
}

// ContextUse is context occupancy, never cumulative usage. Estimated marks
// a rough size (for example, from rendered prompt bytes rather than usage).
type ContextUse struct {
	Known     bool
	Tokens    uint64
	Window    uint64
	Estimated bool
}

// Context is a detached, read-only snapshot. Thread is the owner of this call
// (zero ID when unbound), which need not be the active tab. Revision changes
// when observations affecting that thread's presentation change; it is local
// to the host instance and is not a wire revision or transcript cursor.
type Context struct {
	Thread           Thread
	ActiveThreadID   sdk.ID
	PreviousThreadID sdk.ID
	Connection       Connection
	Features         Features
	Revision         uint64
}

// ActionID identifies a registered action across surfaces. Use a stable ID
// qualified by the extension ID (for example, "example.org/inbox.review").
type ActionID string

// Action is one invokable command. Palette, leader strip, direct key, slash
// invocation and hit targets all resolve this same action. Requires and the
// optional pure Available predicate apply at display time and dispatch time.
type Action struct {
	ID          ActionID
	Title       string
	Description string
	Binding     Binding
	Slash       *Slash
	Requires    Features
	Available   func(Context) bool
	Run         func(Context, Invocation) Outcome
}

// Binding declares default normalized keys. Direct is an optional chord.
// Leader is a required chord or space-separated sequence after ctrl+x; omit
// the ctrl+x prefix itself. Display the host's effective binding, not this default.
type Binding struct {
	Direct string
	Leader string
}

// ActionRef refers to an action and carries optional arguments and a stable
// target (for example a choice ID). It does not change the owning thread.
type ActionRef struct {
	ID     ActionID
	Args   string
	Target string
}

// Invocation supplies the argument tail for a slash command, or the data in
// an ActionRef or the highlighted choice for a sheet action. Other invocations
// have empty arguments and target.
type Invocation struct {
	Args   string
	Target string
}

// Slash exposes an action as /Name. Complete is optional, bounded and pure;
// it must not fetch data or wait for an async operation.
type Slash struct {
	Name     string // no leading slash
	Complete func(Context, CompletionRequest) []Completion
}

// CompletionRequest gives the exact argument tail, excluding the command
// name and its separating whitespace, its cursor as a UTF-8 byte offset,
// and the maximum number of items to return.
type CompletionRequest struct {
	Args   string
	Cursor int
	Limit  int
}

// Replacement replaces [Start, End) at UTF-8 boundaries in the same argument
// tail. The host applies it only while the editor snapshot still matches.
type Replacement struct {
	Start int
	End   int
	Text  string
}

// Completion is a menu item and its exact editor change; it never submits text.
type Completion struct {
	Label       string
	Description string
	Replace     Replacement
}
