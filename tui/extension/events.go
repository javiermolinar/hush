package extension

import "github.com/javiermolinar/hush/sdk"

// EventKind identifies a shell observation, not an interceptable runtime hook.
type EventKind string

const (
	// ConnectionChanged observes a shell connection transition or initial state.
	ConnectionChanged EventKind = "connection"
	// ThreadSwitched observes a committed change of active thread.
	ThreadSwitched EventKind = "thread.switch"
	// EntryObserved observes a new-to-this-feed committed entry.
	EntryObserved EventKind = "entry"
	// StreamChanged observes a change to the projected assistant stream.
	StreamChanged EventKind = "stream"
	// ToolStarted observes a call entering a running state.
	ToolStarted EventKind = "tool.start"
	// ToolOutput observes a change to retained tool output.
	ToolOutput EventKind = "tool.output"
	// ToolResult observes a known terminal outcome, possibly without result text.
	ToolResult EventKind = "tool.result"
	// StatusChanged observes a change in projected thread status or metadata.
	StatusChanged EventKind = "status"
	// UsageChanged observes change to reported cumulative usage.
	UsageChanged EventKind = "usage"
	// ApprovalChanged observes a capability-gated approval change.
	ApprovalChanged EventKind = "approval"
)

// Stream is a bounded assistant-generation snapshot, not a token delta.
// When Present is false it represents the absence of a live stream.
type Stream struct {
	Present   bool
	Value     sdk.Streaming
	Truncated bool
}

// Approval is a capability-gated display snapshot. ID is an opaque stable
// handle within the owning thread; Pending distinguishes open from closed.
type Approval struct {
	ID      string
	CallID  string
	Summary string
	Pending bool
}

// Event accompanies a post-observation Context. The context supplies the new
// connection, status or usage values. Other payloads are populated only for
// matching kinds. PreviousThreadID is used only for ThreadSwitched.
type Event struct {
	Kind             EventKind
	PreviousThreadID sdk.ID
	Entry            Entry
	Stream           Stream
	Tool             Tool
	Approval         Approval
}

// Subscription receives one kind of event after the host commits the change.
// The callback runs on the loop, must be bounded, does no I/O, and cannot veto
// or emit effects. It may record instance-local observations for later actions.
type Subscription struct {
	Kind    EventKind
	Observe func(Context, Event)
}
