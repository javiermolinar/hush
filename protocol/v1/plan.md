# Structured plans (`plan:1`, experimental)

Persistent Hush hosts can draft plans in chat and browse them in `/plan`. A host
with the matching Hush file helper can run scoped text edits. Other tasks stay
inspectable as drafts. Invocation-private and legacy format-1 stores do not
offer `plan:1`. The native core frames are unchanged.

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

A runnable step declares one of two scopes:

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

`/plan` lists plans from the active thread's recorded working directory, not
just the plan bound to that thread. At the local startup prompt, `/plan` creates
an empty thread in the launch directory and opens the list without sending a
message. The list shows status, numbered revision, and verified task count. Open
plans precede closed plans. Enter opens the focused plan without attaching it or
starting work. For a plan from another thread, `a` explicitly makes it the current
plan for this thread; `p` in the list opens this thread's historical pin when it
has one. Neither action authorizes Run.

`c` closes a plan or reopens an incomplete closed plan. Closing blocks new Run
admissions immediately and lets the currently submitted task settle. It does
not roll back changes. The remainder of the old selection cannot start, even if
someone reopens before the active task settles. Reopening retains verified tasks
and requires a fresh selection and Run. A plan is complete only when **every**
task at its current revision is verified. A completed plan can be closed but not
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

## Run from the task picker

Select tasks in `/plan` and press Enter once. It admits the exact revision and
selection and returns to the initiating thread. The transcript shows the accepted
selection, each task's tool calls, and the backend's verification or failure.
The run reserves that thread until it settles. New chat, steers, and Continue
are not accepted while it is running.

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
`after` records a separate timing dependency. Both must be satisfied at Run.

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

### Workspace inventory (`planWorkspace:1`)

Negotiate `planWorkspace:1` along with `plan:1`. Every operation checks the
ownerless thread and scopes to its recorded working directory on the backend.

- `ext.planWorkspace.wait {threadId, page, after?}` watches a page of up to 48
  `{id, title, revision, verified, total, status, closed}` rows and returns
  `{revision, plans, more}`. Page numbers start at zero. Status is `pending`,
  `running`, `attention`, or `complete`; `closed` is a separate lifecycle flag.
  Follow page updates using the page's opaque token. New plans or lifecycle
  changes can move rows across pages.
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
