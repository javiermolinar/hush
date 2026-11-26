# Structured plans (`plan:1`, experimental)

Persistent Hush hosts can draft plans in chat and browse them in `/plan`.
`planChat:1` runs selected tasks as a normal turn in the current thread.
A host with the matching Hush file helper can also run isolated, scoped edits.
Invocation-private and legacy format-1 stores do not offer `plan:1`.
The native core frames are unchanged.

## Draft in chat

Start a normal input with **Create a plan …**, **Revise the plan …**, or
**Plan:**. For example: “Create a plan to replace `theme=latte` with
`theme=mocha` in `settings.txt`.” A draft turn can read files and the current
plan, and save a structured draft. Hush blocks shell, workspace writes, and
indirect tool execution on that turn without changing the thread's configured
tools. Workspace trust still applies. Neither drafting nor opening `/plan`
authorizes implementation.

`hush_plan_current` returns the visible definition and base revision.
`hush_plan_propose` saves a full draft against that base (zero for a new plan).
A stale proposal fails without changing the head. There is no Markdown export
in this slice.

Start with **Preview a plan …** or use an opening such as **Let's work on
creating a new plan …** to draft without saving. The read-only turn offers
`hush_plan_preview` in place of `hush_plan_propose`. In the TUI, click the
preview title or press `ctrl+x i v` to inspect it. Click **Save to /plan**,
press `ctrl+x i w`, or send **ok save it** to save the exact preview when it is
still current in this thread. A later submitted topic or a reset invalidates
that confirmation. Saving doesn't call the model, run tasks, or write a
Markdown file. Without preview saving, use the explicit create or revise
request above.

A normal chat run needs only a task description and prose checks. A step can
also declare one of two scopes to opt into backend verification:

- `edit: {path, contains}`: one exact replacement with a literal newly present
  afterward. Existing single-file plans still work.
- `scope: {write, read?, acceptance: {label, script}}`: one to four existing
  writable files, with optional read-only inputs (at most eight files total).
  The JavaScript body gets a frozen `files` object keyed by exact declared
  path. It must return boolean `false` on the original texts and `true` on the
  staged texts. Both evaluations use fresh VMs with the same virtual clock
  and random seed. There are no host tools, filesystem access, imports, or
  network. The script is limited to 4 KiB, 1.5 seconds per invocation, and
  16 MiB of VM heap. Scripts that emit output do not pass.

All paths are workdir-relative (up to 160 ASCII characters and 16 components),
and files must be existing, plain UTF-8 files up to 32 KiB. Hidden components,
symlinks, hardlinks, and protected runtime paths are excluded. User attributes
are rejected except macOS system provenance. No file creation, deletion,
shell, or general test-suite execution. Prose `checks` are not executed.

## Browse, close, and reopen

`/plan` lists plans across the connected store's workspaces, including other
worktrees. The current thread's active run is first, even while closing. Among
remaining plans, open plans precede closed plans, and the current workspace
comes first in each group. Plans from another workspace show their source and
open read-only.
Switch to a thread in that workspace to attach, run, review, or change the plan's
status. Without `planCatalog:1`, the picker lists only the current workspace.

With `planChat:1`, `/plan` at the local startup prompt browses from the trusted
launch directory without creating a thread or changing the draft. The list
shows status, numbered revision, and task counts. Enter opens the focused plan
without attaching it or starting work. `a` explicitly makes a same-workspace
plan current for this thread; it isn't needed to Run here. `p` opens this
thread's historical pin when it has one. Neither action authorizes Run.

`c` closes a plan or reopens an incomplete closed plan. Closing blocks new Run
admissions immediately, withdraws a still-queued chat run, and lets a placed
turn settle. It doesn't roll back changes. A scoped run cannot start the
remaining tasks, even if someone reopens before the active task settles.
Reopening retains task results and requires a fresh selection and Run. A plan
is complete for verified-plan lifecycle rules only when **every** task at its
current revision is verified. A completed plan can be closed but not
reopened or edited. Start a new draft in place of a closed or completed plan with
base revision `0`; this does not remove the old plan from the workspace list.

`h` shows run history: the revision, selected task IDs, initiating thread,
transcript entry IDs for task turns, and outcomes. Older runs show worker thread
IDs instead. Records added by this capability stay on the
plan, across revision, binding, and lifecycle changes. Earlier prototype runs
predating the log are not backfilled. Esc dismisses or goes back without changing
plan status or authorizing work.

## Review a plan independently

Press `v` in `/plan` to inspect the focused revision's review. Enter in the
review panel explicitly starts (or retries) it. This does not bind the plan,
change thread permissions, or start implementation. The review runs in the
background; `x` stops it in the initiating thread's review panel, and `o`
opens the read-only reviewer transcript. Use Left and Right to browse revisions.

A fresh, task-owned conversation sees the saved revision and captured text from
explicit draft and revision requests. It has no files, tools, project
instructions, or prior chat. At most six requests are retained (the first and
five latest), with at most 3,000 UTF-16 code units per request. Attachments and
other context are not sent. Missing or partial captured requirements are
reported as a limitation, and their coverage stays unverified.

A bounded report lists the assessment of requirements, acceptance checks, and
dependencies, along with findings and limitations. A display-only copy appears
in the initiating transcript. Reviews are advisory even if a finding is labeled
blocking. They do not alter the plan, affect verification, or authorize fixes.
Changing the plan makes the report stale, but the old revision remains
inspectable. Stop and restart interrupt active reviews; retry is explicit.

## Run here

Use Space to select tasks and Enter to run them as one turn in the active
thread, even if the plan was drafted elsewhere in the same workspace. This
uses the ordinary tools and workspace trust rules. It doesn't change the
binding, fork the conversation, copy the drafting prompt, or submit the unsent
draft. If the thread is busy, the run is a normal follow-up. If there is no
active thread, Run here creates the first thread in the launch directory and
keeps the draft in the composer.

The model receives the exact revision and selected task IDs, with other tasks,
dependencies, and blockers as context rather than selection gates. It should do
necessary setup, run relevant checks, and continue independent selected work
when another task is blocked. No unselected task is added to the run. Task
outcomes in the transcript and picker are advisory, not backend verification.
Reported tasks can be selected again. Closing the picker or disconnecting
leaves admitted work alone; normal Stop applies to the native input.

## Run in a new worktree

Select tasks with Space, then press **w Run in new worktree** instead of Enter.
The action uses the plan's source checkout. If you're looking at a read-only
plan from another workspace, return to its workspace first. Hush asks the model
for a short name using only the plan and selected task titles. It uses the
active thread's model or the startup model selection, not a different provider
as a fallback. If naming fails or cannot produce a branch name, Hush tries the
plan title. The checkout starts from committed `HEAD`; uncommitted files and the
original conversation history are not copied. A plan scoped to a subdirectory
runs from the corresponding subdirectory in the new checkout.

Review and trust the new checkout before the run starts. In the local TUI,
confirm the exact execution directory with **t**. On a socket attachment,
press **y** to copy the full grant command, run it on the backend host, then
press **w** in the trust instructions to start. If you close the instructions,
**w** reopens them. **x** in the plan picker leaves the prepared checkout and
thread idle without deleting them. After admission, Hush opens the new thread
and records a worktree run on the original plan. Advisory reports are not
verification, and Hush does not merge the changes back.

After an unconfirmed setup, **w** retries the same request rather than creating
another checkout. If setup was interrupted after Hush reserved a thread but
could not finish the checkout, the picker identifies that thread in `/threads`.
Hush does not reuse or delete an incomplete checkout automatically.

## Run scoped edits

Press `s` when the task picker shows **s scoped** to opt into isolated
verification of a valid selection on the bound thread. On peers without
`planChat:1`, Enter retains its original scoped or fixture behavior. A scoped
Run reserves the thread until it settles. New chat, steers, and Continue are
not accepted while it is running.

Each selected task runs as a scoped native turn **in that same conversation**,
in authored order. It can read only declared inputs and stage exactly one text
proposal per declared write. Legacy tasks use a single replacement proposal.
Shell, ordinary file tools, and delegation are blocked. Scope comes from the
committed task and input IDs, not from transcript text. Normal thread tools
are unchanged. The thread's selected model and thinking level are pinned for
this Run; different per-task thinking requests remain informational.

After the turn settles, the backend checks acceptance against copies, then
commits publication intent before doing any filesystem writes. The helper
stages output in a private `.hush-plan-<id>` directory, rechecks all inputs,
and atomically exchanges each destination with its staged file. It checks the
exact displaced inode and contents as well as the installed file. A conflict
stops publication and preserves displaced files in the recovery directory;
`files.json` maps staging slots to workspace-relative paths. Filesystems without
atomic exchange support fail closed, rather than silently using a plain rename.

The exchange is not a compare-and-swap against file contents. Other programs
can still change or move files, and a conflicting path may show a proposal
until manually reconciled. Multiple exchanges are not atomic. If publication
or cleanup is interrupted, results and staging files can be left behind. A
cancel, lost reply, trust change, or restart after publication intent can leave
an explicit **publication uncertain** warning in the transcript and run history.
The task stays unverified and later tasks do not start. Inspect the workspace
and named recovery directory before retrying; there is no automatic rollback
or replay. Verification describes the versions observed, not future changes. Model prose alone never counts as evidence.

A Run admits at most 48 file inputs. Across selected tasks, shared reads are
allowed but overlapping writes or reads of another selected task's write must
run in separate selections. Only one scoped batch is active per store. The
helper's root lock doesn't stop editors or agents in other threads.

Esc Esc stops foreground execution. Closing the picker or disconnecting does
not stop it. A failed check, revoked grant, changed file, or restart stops
unfinished work without rolling back published edits. Continue and Retry do not
reauthorize a stopped plan task; select it again in `/plan`. The transcript's
verification records what passed at the time, not that the file cannot change
later.

## Durable storage and bindings

Definitions are immutable numbered records in Hush's existing Pi Durable
`history.sqlite`. A session-scoped head is keyed by `planId`. An edit advances
that head and updates attached threads' pins in one commit, including a
non-model `hush.plan.pin` entry on the editing thread. Plans allow up to 128
revisions and 32 live bindings.

A historical fork copies the pin at its anchor, or starts without a plan if
there was none. Its copy is inspectable but not editable or runnable. It
inherits no progress, assignments, or receipts. Explicit attachment follows
the current head without starting work. A thread follows at most one plan;
attachment is limited to the same working directory. Owned worker and reviewer
histories are inspectable by ID, but native mutation and fork requests fail.

## Wire operations

Offer `capabilities: {"plan":[1]}` in `hello`. Unsupported hosts do not show
`/plan`.

- `ext.plan.wait {threadId, after?}` returns `{revision, plan, runAvailability,
  progress, run, closed?, complete?, settling?}`. `revision` is an opaque
  64-character hex watch token, not the numbered plan revision. Omit `after` for a full snapshot or pass the
  last token for the same thread to wait for a change. Updates may coalesce.
- `ext.plan.attach {threadId, planId}` returns `{}` after attaching an
  ownerless thread to the live head. Reattaching is a no-op. A missing plan or
  mismatched directory is `not_found`; a different existing binding is
  `invalid_request`. This is an SDK operation, not a picker action.
- `ext.plan.run {threadId, planId, revision, stepIds, requestId}` returns
  `{runId}` when the coordinator, exact scope, and receipt are committed,
  not when work finishes. Retry the identical request on the initiating
  thread to get the same receipt; reusing its key for a different request
  fails. A stale revision, blocked or already verified step, unsupported
  edit, or incomplete prerequisite rejects the entire selection. No subset
  or extra prerequisite runs implicitly. Execution unavailable on the host
  returns `unsupported_capability`; a pinned revision cannot run.

A definition has `{id, revision, title, steps}`. Steps have `{id, title,
 description, checks, requires?, after?, blocker?, thinking?, edit?, scope?}`. ID
references point to earlier steps. `requires` records hard requirements;
`after` records a separate timing dependency. Both must be satisfied for a
scoped or fixture Run. Run here treats them as model context.

`runAvailability` is `disabled`, `pinned`, `scoped`, `fixture`, or `unbound`.
Only `scoped` and `fixture` authorize Run requests, and named inspection can
show progress on `unbound` plans. Progress entries have `{stepId, state,
effectiveThinking, workerThreadId, entryId?, evidence?}`. For a main-thread
turn, `entryId` names its typed transcript entry and the legacy `workerThreadId`
field equals the initiating thread. A non-null `run` has `{id,
stepIds, state, ownerThreadId?, failure?, publicationUncertain?}`. `ownerThreadId` is set for scoped
runs. Run states are `running`, `verified`, `failed`, or `closed` (closed before
all selected tasks started). `plan:1`-only peers see that last state as `failed`
with a close diagnostic. A selected step that has not started has no progress
entry. `--faux` uses `!plan-fixture` and scripted checks with no file writes.
It cannot run or verify ordinary drafts.

### Foreground plan input (`planChat:1`)

Offer `planChat:1` along with `plan:1` and `planWorkspace:1`. This extension never
widens the permissions of `ext.plan.run`.

- `ext.planChat.run {threadId, planId, revision, stepIds, requestId}` commits a
  single native follow-up input and its run receipt and returns `{runId}`.
  The thread must be ownerless and in the plan's recorded workspace. The
  revision must be the open live head, and none of the selected IDs may already
  be verified. Identical retries return the same receipt; the key cannot be
  reused for another selection or Run mode. No plan attachment is changed.
- `ext.planChat.workspace {cwd, page, after?}` and
  `ext.planChat.inspect {cwd, planId, after?}` watch a launch directory without
  creating a thread. `cwd` must name an absolute, trusted backend directory;
  without `planCatalog:1` only plans in that workspace are visible. The reply
  shapes are the same as the thread-scoped inventory and inspection operations.
- Plan snapshots for eligible live plans add `chat: true` and `reports` with
  zero or more `{stepId, state, detail}` records. Task report states are `done`,
  `blocked`, and `incomplete`. They do not populate `progress` or count toward
  `verified`. Inventory rows can include `reportedDone` (excluding verified
  tasks). Chat run snapshots add `chat: true` and `submissionId`; chat history
  records add `chat: true`, optionally `entryId` and `reports`, and no worker
  or per-task turn records. A settled chat run can have state `done` without
  any tasks being verified or even reported. Legacy peers don't receive chat
  records in plan snapshots or run history.

One typed `hush.plan.chat` entry represents the selected input in the
initiating transcript. The backend tracks the normal input's settlement and
records `hush.plan.result` there. This does not open a subagent or worker
conversation. An independent review still uses its explicitly separate,
read-only reviewer conversation.

With `planChatActivity:1` negotiated alongside `planChat:1`,
`ext.planChatActivity.wait {threadId, after?}` follows the oldest unsettled
Run here input admitted to that thread, or the last settled one. It returns
an `ext.plan.wait`-shaped snapshot with an independent watch cursor and
`runAvailability: "disabled"`. With no activity, `plan` and `run` are null.
This read-only projection works for unbound and worktree threads without
changing their plan bindings. A fork does not inherit it. Chat steps share
one native submission; reports are still advisory, not verification.

### Worktree launch (`planWorktree:1`)

Offer `planWorktree:1` with `planChat:1`, `planWorkspace:1`, and `plan:1`.

- `ext.planWorktree.prepare {threadId | cwd, planId, revision, stepIds, requestId, model?, thinkingLevel?}`
  prepares an empty ownerless thread and a managed Git checkout. Exactly one
  source locator is required: a thread in the plan's recorded directory, or
  that absolute directory for a threadless launch. Only a threadless launch
  may include `model: {provider, modelId}` and `thinkingLevel` together. It uses
  backend defaults if neither is present. Hush requires a trusted source, a
  valid selection, and a committed Git tree containing the plan directory.
  The reply is `{threadId, title, checkout: {cwd, root, branch}}`. `root` is the
  worktree root; `cwd` is the execution directory and the exact trust scope.
  This is neither a run receipt nor a grant. Hush reserves a discoverable thread
  before Git writes the checkout. An exact retry (same plan, request ID,
  selection and source) returns the same allocation on reconnect or restart.
  While setup is running it may return `busy`; incomplete reservations identify
  the affected thread and do not write Git again.
- `ext.planWorktree.start {threadId, checkout, planId, revision, stepIds, requestId}`
  uses the **new** thread ID and exact `checkout.cwd`. It checks the recorded
  worktree, Git metadata, branch and starting commit, and requires separate
  trust for the execution directory, an idle target thread, a valid model,
  and the exact prepared selection on the same open plan revision. Closing and
  reopening the plan invalidates an unstarted selection. The reply is
  `{runId}`. An exact retry for that same checkout and selection returns the
  same receipt, including after restart. A regular `Run here` request cannot
  use the allocation to run a foreign plan. History adds `worktree: true` to
  worktree chat runs only for peers that negotiated `planWorktree:1`.

### Workspace inventory (`planWorkspace:1`)

Negotiate `planWorkspace:1` along with `plan:1`. Every operation checks the
ownerless thread and scopes to its recorded working directory on the backend,
except for the read-only operations negotiated by `planCatalog:1`.

- `ext.planWorkspace.wait {threadId, page, after?}` watches a page of up to 48
  `{id, title, revision, verified, total, status, closed}` rows and returns
  `{revision, plans, more}`. Page numbers start at zero. Status is `pending`,
  `running`, `attention`, or `complete`; `closed` is a separate lifecycle flag.
  The calling thread's oldest active run is first, including queued Run here
  runs when `planChat:1` is negotiated. Remaining plans are ordered newest first,
  with open plans before closed ones. Follow page updates using that thread's
  opaque token. New plans, runs, or lifecycle changes can move rows across pages.
- `ext.planWorkspace.inspect {threadId, planId, after?}` watches the named live
  head. It does not change the active binding.
- `ext.planWorkspace.use {threadId, planId}` explicitly changes the thread's
  binding and records its new pin. It does not run anything. A thread cannot
  switch away from a plan while it owns an active Run.
- `ext.planWorkspace.setClosed {threadId, planId, closed}` records the requested
  lifecycle state, not a client-side toggle. Reopening a completed plan fails.
- `ext.planWorkspace.history {threadId, planId, before?}` returns `{runs, next}`
  with up to 12 records, newest first. Pass `next` as `before` for the next page.
  A record has `{id, revision, threadId, stepIds, state, workers, turns?,
  failure?, publicationUncertain?}`.
  A native run has `turns` items `{stepId, entryId, state}` and empty `workers`.
  Older runs and the scripted fixture have worker items `{stepId, threadId,
  state}`. Only submitted tasks have a turn or worker record.

The backend stores inventory and run history in Durable documents. Typed
`hush.plan.run`, `hush.plan.task`, and `hush.plan.result` entries appear in the
ordinary transcript feed and history. Lifecycle, plan status, and backend
verification records commit with the relevant run progress. The native core
wire schema is unchanged. Scoped execution requires a matching private file
helper; an old helper leaves the host draft-only. Older in-flight execution
records settle as interrupted on upgrade and are not replayed.

### All workspaces (`planCatalog:1`)

Offer `planCatalog:1` with `plan:1` and `planWorkspace:1`. Send
`ext.planCatalog.wait {threadId, page, after?}` for the same bounded page shape
and wait semantics as `ext.planWorkspace.wait`, including precedence for the
calling thread's active run. Among remaining plans, open plans precede closed
plans. Within each group the calling thread's workspace is first, followed by
other workspaces in sorted path order; newest-created plans appear first within
each workspace. Rows from other workspaces add `workspace`, their recorded backend
working directory. The backend mirrors existing committed workspace summaries;
creating or changing a plan can wake the page even in a different workspace.
On reconnect, start without `after`. `ext.planWorkspace.wait` remains scoped.

With `planCatalog:1`, `ext.planWorkspace.inspect` and
`ext.planWorkspace.history` can read a plan from another workspace. If
`planChat:1` is also selected, `ext.planChat.workspace` and `ext.planChat.inspect`
offer that same view from a trusted launch directory without a thread. Inspect
from outside a plan's workspace returns `workspace` and
`runAvailability: "disabled"`, and omits the `chat` Run affordance. It may
include committed progress and, with `planChat:1`, model reports and a chat run
record. This does not change the binding or grant workspace access. Attach,
both Run modes, lifecycle changes, and independent review retain their original
scope and trust checks.

### Save a preview (`planDraft:1`)

Offer `planDraft:1` along with `plan:1`. The `hush.plan.preview` transcript
entry is an immutable preview, not a saved plan or model input. The owning
thread can prepare one preview per placed input. Historical previews remain
inspectable after saving, a newer input, or a fork.

`ext.planDraft.save {threadId, previewId}` saves the exact preview and returns
`{planId, revision}`. The IDs name an ownerless thread and its preview entry.
Saving requires workspace trust, a settled draft turn, no active or queued
work in that thread, no newer input or reset, and the same workspace and plan
base as when it was previewed. New saves against stale previews are rejected.
An exact retry returns the original receipt, even after reconnect, restart,
or revision changes. The revision and a `hush.plan.preview-saved` transcript
entry commit together. The request cannot start a run. Text confirmation is
handled by the TUI; other native clients can use the same save operation.

### Independent plan review (`planReview:1`)

Offer `planReview:1` along with `plan:1` and `planWorkspace:1`. The optional
capability is available on normal persistent hosts even when the file helper
is unavailable. Fixtures and invocation-private hosts do not advertise it.
Operations require an ownerless requester in the plan's recorded workspace;
starting work also requires workspace trust and an available model. The
review uses the requesting thread's model and thinking level at admission.

- `ext.planReview.wait {threadId, planId, revision, after?}` returns
  `{cursor, title, revision, headRevision, source, canStart, review}`. The
  numbered `revision` selects an immutable target; `cursor` is an opaque
  64-character hex token for watching changes in the plan or review.
  `source` is `captured`, `partial`, or `missing`. `review` is null before
  the first attempt, otherwise the latest attempt for that exact revision.
- `ext.planReview.start {threadId, planId, revision, requestId}` returns
  `{reviewId}` when admitted, not when a report is ready. New requests can
  target only the current head. An exact retry in the same thread, plan and
  revision returns its original receipt, even after a revision or failure.
  One review may be active per revision or initiating thread. A failed attempt
  can be retried with a fresh request ID, up to four attempts per revision.
- `ext.planReview.cancel {threadId, planId, revision, reviewId}` returns `{}`
  and stops the named attempt if still running. Only its initiating thread
  can cancel it. Closing the panel, plan, or connection does not cancel review.

A review record includes `{id, threadId, model, thinking, state,
reviewerThreadId?, report?, failure?}`. `model` has `{provider, modelId}`.
`state` is `running`, `failed`, or `complete`. A complete report has
`{summary, checks, findings, limitations}`. The `requirements`, `acceptance`,
and `dependencies` checks are `addressed`, `concern`, or `unverified`.
Findings have `{severity, area, stepId?, message}` and are advisory regardless
of severity. `hush.plan.review` entries record admission and the outcome in
the initiating thread without creating model input there.

## Verification

```sh
npm run build && npm run backend:bundle
go build -o bin/hush ./app/cmd/hush
HUSH_TEST_BACKEND="$PWD/dist/hush-backend.mjs" go test ./tui -run '^TestPlanPickerE2E$'
HUSH_TEST_CLI="$PWD/bin/hush" node --test backend/test/plan.test.mjs app/test/plan.test.mjs
```

The PTY test exercises the normal provider path with a loopback model: one
Enter applies and verifies two real edits in order and leaves the third file
untouched. File-set tests exercise isolated acceptance, incomplete proposals,
timeouts, memory limits, stale read inputs, and publishing only declared writes.
Publication tests cover a displaced conflicting edit, invalid and lost helper
replies, cancellation, trust revocation, and restart after a write. The fixture
and failure tests cover rejected scope, receipt retries, trust, and stop/restart. Review tests cover isolated inputs, rejected
tools, report validation, stale revisions, cancellation, and durable results.
Live provider validation, general-purpose implementation, code review and
review gates, pause/resume, and Markdown export remain outside this slice.
