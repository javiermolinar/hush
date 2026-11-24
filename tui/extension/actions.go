package extension

import (
	"context"
	"errors"

	"github.com/javiermolinar/hush/sdk"
)

// Outcome is an ordered list of requests to the shell. Admission happens on
// the loop; async requests may complete out of order. Put dependent work in
// continuations, not adjacent list entries. The empty outcome is a no-op.
type Outcome []Effect

// Effect is a request interpreted by the shell. Only this package defines
// effect types; extensions do not supply arbitrary shell state transitions.
type Effect interface{ extensionEffect() }

// Work asks the host to run extension-local work off the loop. Run uses only
// explicitly captured data and a cancellation context, not loop-owned state.
// Done runs on the loop with a fresh snapshot for the original owning thread.
// Run transfers ownership of the result and must not mutate it after return.
type Work struct {
	Run  func(context.Context) (any, error)
	Done func(Context, any, error) Outcome
}

func (Work) extensionEffect() {}

// Notice displays transient host-managed feedback. Tone is normally Plain,
// Good, Warning or Danger; the host chooses placement and expiration.
type Notice struct {
	Text string
	Tone Tone
}

func (Notice) extensionEffect() {}

// SetStatus sets one thread-scoped status-line contribution, identified by
// extension ID and Key. Empty Text clears it. The host owns its lifetime.
type SetStatus struct {
	Key      string
	Text     string
	Tone     Tone
	Order    int
	Priority int
}

func (SetStatus) extensionEffect() {}

// Dispatch invokes the referenced registered action with the same owner and
// normal availability checks. The host bounds dispatch chains.
type Dispatch struct{ Action ActionRef }

func (Dispatch) extensionEffect() {}

// LocalOp identifies a shell (not backend) action primitive.
type LocalOp string

const (
	// OpenPalette opens the shell's command palette overlay.
	OpenPalette LocalOp = "palette"
	// CloseTab closes the owning tab, not the durable thread.
	CloseTab LocalOp = "tab.close"
	// CompleteFile accepts a path from the currently visible file completion list.
	CompleteFile LocalOp = "prompt.completeFile"
	// ToggleDetails toggles expansion for the owning transcript.
	ToggleDetails LocalOp = "details.toggle"
	// ToggleActivity toggles just the record with the ID in Local.Text (or live).
	ToggleActivity LocalOp = "activity.toggle"
	// ReplacePrompt sets the owning editor's text to Local.Text.
	ReplacePrompt LocalOp = "prompt.replace"
	// RecallQueued returns eligible queued text to the prompt; the host must
	// not claim it withdrew an already admitted input.
	RecallQueued LocalOp = "prompt.recallQueued"
	// EditPrompt opens the owning prompt in the host-configured editor.
	EditPrompt LocalOp = "prompt.edit"
	// CopyText asks the shell to copy Local.Text using its clipboard policy.
	CopyText LocalOp = "copy"
	// CopyLastAnswer asks the shell to copy the owning thread's last answer.
	CopyLastAnswer LocalOp = "copy.answer"
	// CopyLastCode asks the shell to copy the owning thread's last code block.
	CopyLastCode LocalOp = "copy.code"
	// Quit requests ordinary shell shutdown, not backend shutdown.
	Quit LocalOp = "quit"
)

// Local requests a shell primitive for the owning thread. The shell enforces
// focus and availability rules and performs external I/O away from the loop.
type Local struct {
	Op       LocalOp
	Text     string
	ThreadID sdk.ID // optional target of CloseTab
}

func (Local) extensionEffect() {}

// Client requests a typed client operation. The host checks availability and
// runs I/O off the loop. Done receives a result on the loop for the original
// owner even if the operation acts on a different thread.
type Client struct {
	Op   Operation
	Done func(Context, ClientResult) Outcome
}

func (Client) extensionEffect() {}

// Operation is a supported client request. In thread-scoped operations, a
// zero ThreadID means the captured owner, never the active tab at execution.
// Switch is the exception: it requires an explicit destination.
type Operation interface{ clientOperation() }

// ErrUnavailable reports a client operation the host cannot currently offer.
// Hosts may wrap it with a diagnostic; use errors.Is to check it.
var ErrUnavailable = errors.New("extension operation unavailable")

// NativeCatalog enables the Hush catalog with availability and verified tiers.
const NativeCatalog Features = 1 << 9

// NativeCatalogSearch enables backend search across the Hush catalog.
const NativeCatalogSearch Features = 1 << 10

// NativeCatalogDraft enables model selection before the owning thread exists.
const NativeCatalogDraft Features = 1 << 11

// Schedules enables managing durable occurrences owned by a thread.
const Schedules Features = 1 << 12

// Checkpoints enables host-owned bookmarks, snapshot previews and rewind.
const Checkpoints Features = 1 << 13

// ClientResult is a detached reply. Err reports failure, including missing
// features. A submit receipt means admission, not settlement. Fields unused
// by the operation are zero. Cursor is opaque and empty on the last list page.
type ClientResult struct {
	Err              error
	ThreadID         sdk.ID
	SubmissionID     sdk.ID
	TaskID           sdk.ID
	Title            string // normalized name returned by SetTitle
	Threads          []Thread
	Entries          []Entry
	Models           []CatalogModel
	NativeModels     bool // catalog rows and configuration use the native capability
	Skills           []string
	MoreSkills       int
	Files            []File
	Approvals        []Approval
	Schedules        []sdk.ScheduleItem
	Schedule         sdk.ScheduleItem
	Checkpoints      []sdk.Checkpoint
	Checkpoint       sdk.Checkpoint
	RewindPreview    sdk.RewindPreview
	Rewind           sdk.RewindResult
	Modes            []Choice
	Cursor           string
	Directory        bool     // a recency-ordered thread directory page
	Indexing         bool     // first page is incomplete; refresh rather than paging it
	WorkspaceMatches []string // aligned with Threads; absent when workspace scope is not known
	ScopeUnavailable bool     // default local scope is not supported; no unscoped rows were substituted
	WorkspaceScoped  bool     // this page was filtered by workspace on the backend
}

// Submit sends text. An empty RequestID asks the host to allocate a submission
// key. WhenBusy defaults to reject; use sdk.FollowUp for queueing.
type Submit struct {
	ThreadID  sdk.ID
	Text      string
	RequestID string
	WhenBusy  sdk.BusyMode
}

func (Submit) clientOperation() {}

// Steer submits text with native steer busy mode (it can start a run when
// idle). Empty RequestID asks the host to allocate a submission key.
type Steer struct {
	ThreadID  sdk.ID
	Text      string
	RequestID string
}

func (Steer) clientOperation() {}

// Abort asks the backend to abort ordinary activity on the target thread.
type Abort struct{ ThreadID sdk.ID }

func (Abort) clientOperation() {}

// NewThread creates a thread without implicitly switching. Empty Workspace
// and a zero Model omit those overrides. The result contains the new ID.
type NewThread struct {
	Workspace string
	Model     sdk.Model
}

func (NewThread) clientOperation() {}

// NewWorktree creates a thread backed by an isolated checkout of committed
// HEAD. It never changes the source workspace. Requires Worktrees.
type NewWorktree struct {
	Workspace string
	Title     string
}

func (NewWorktree) clientOperation() {}

// ManageSchedule lists occurrences, or applies exactly one explicit add/change.
// The request's thread is resolved by the host from ThreadID or the captured owner.
type ManageSchedule struct {
	ThreadID sdk.ID
	Add      *sdk.ScheduleInput
	Change   *sdk.ScheduleChange
}

func (ManageSchedule) clientOperation() {}

// ManageCheckpoint lists checkpoints when all operation fields are empty.
// Otherwise it records a bookmark or known effect, previews or confirms a
// rewind, or retrieves an existing operation by ID. Exactly one field is set.
type ManageCheckpoint struct {
	ThreadID sdk.ID
	Record   *sdk.CheckpointInput
	Effect   string
	Preview  *sdk.RewindInput
	Confirm  *sdk.RewindDecision
	StatusID string
}

func (ManageCheckpoint) clientOperation() {}

// Fork creates a thread at an inclusive visible entry and returns its ID,
// without switching or restoring files.
type Fork struct {
	ThreadID  sdk.ID
	AtEntryID sdk.ID
}

func (Fork) clientOperation() {}

// Switch activates an explicit target thread, attaching if needed. It
// returns the committed active thread ID but does not rebind Done's owner.
type Switch struct{ ThreadID sdk.ID }

func (Switch) clientOperation() {}

// Configure changes native stored overrides. sdk.Set and sdk.Clear preserve
// the distinction between replacement, explicit clear and omission.
type Configure struct {
	ThreadID sdk.ID
	Change   sdk.Configure
}

func (Configure) clientOperation() {}

// ConfigureCatalog uses the negotiated native catalog's configuration path.
// A zero Model changes only the thinking level. With NativeCatalogDraft and
// no thread, an explicit model and level are staged for the first prompt.
type ConfigureCatalog struct {
	ThreadID      sdk.ID
	Model         sdk.Model
	ThinkingLevel string
}

func (ConfigureCatalog) clientOperation() {}

// Compact admits a compaction task, returning its ID, not its result.
type Compact struct {
	ThreadID     sdk.ID
	Instructions string
}

func (Compact) clientOperation() {}

// Reset asks the backend to reset the thread's context. A nonempty Handoff
// is passed to the backend as the reset handoff text.
type Reset struct {
	ThreadID sdk.ID
	Handoff  string
}

func (Reset) clientOperation() {}

// ListThreads reads a bounded page for selectors, not an all-threads live
// feed. Limit is a positive upper bound; Cursor resumes a page.
type ListThreads struct {
	Cursor      string
	Limit       int
	LaunchScope bool // use the shell's launch workspace when local, not the active thread's cwd
	BrowseAll   bool // with LaunchScope, opt out of filtering; classify rows when possible
}

func (ListThreads) clientOperation() {}

// History reads a bounded ascending page strictly before BeforeEntryID, or
// the newest page when it is empty. Continue with the oldest returned ID.
type History struct {
	ThreadID      sdk.ID
	BeforeEntryID sdk.ID
	Limit         int
}

func (History) clientOperation() {}

// Approve allows once or denies a pending approval. Requires Approvals; the
// host validates the approval ID belongs to the target thread.
type Approve struct {
	ThreadID   sdk.ID
	ApprovalID string
	Allow      bool
}

func (Approve) clientOperation() {}

// ListApprovals requests a bounded page of pending approvals. Requires Approvals.
type ListApprovals struct {
	ThreadID sdk.ID
	Cursor   string
	Limit    int
}

func (ListApprovals) clientOperation() {}

// ListSkills reads the host's loaded skill names. It cannot invoke a skill.
type ListSkills struct{ ThreadID sdk.ID }

func (ListSkills) clientOperation() {}

// ListModels reads a bounded catalog page. Requires Catalog. A catalog entry
// does not prove that the durable backend can run the model.
type ListModels struct {
	ThreadID sdk.ID
	Query    string
	Cursor   string
	Limit    int
}

func (ListModels) clientOperation() {}

// CatalogModel is display metadata for a model picker. Absent levels and a
// zero context window are unknown, not proof of support or lack of it.
type CatalogModel struct {
	Model          sdk.Model
	Label          string
	ThinkingLevels []string
	ContextWindow  uint64
	Current        bool   // marks the native thread's saved model
	Unavailable    string // safe recovery hint; empty for usable or legacy rows
}

// ListFiles reads a bounded workspace file page. Requires Files and is
// limited by host workspace policy, not by a base protocol promise.
type ListFiles struct {
	ThreadID sdk.ID
	Query    string
	Cursor   string
	Limit    int
}

func (ListFiles) clientOperation() {}

// File is a workspace-relative listing item, with no file contents.
type File struct {
	Path      string
	Directory bool
}

// SetTitle changes the thread's presentation name. Requires Titles.
type SetTitle struct {
	ThreadID sdk.ID
	Title    string
}

func (SetTitle) clientOperation() {}

// SetArchived changes archive visibility. Requires Archive; it never deletes
// the durable thread or its history.
type SetArchived struct {
	ThreadID sdk.ID
	Archived bool
}

func (SetArchived) clientOperation() {}

// ListModes requests the bounded set of permission mode choices for the
// target thread. Requires Mode. Choice IDs are the values SetMode accepts.
type ListModes struct{ ThreadID sdk.ID }

func (ListModes) clientOperation() {}

// SetMode requests a permission mode recognized by the host. Requires Mode;
// selecting a mode does not decide any pending approval.
type SetMode struct {
	ThreadID sdk.ID
	Mode     string
}

func (SetMode) clientOperation() {}
