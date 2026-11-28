# Structured plans (`plan:2`, experimental)

Persistent Hush hosts store plans that any thread can save from chat and the
TUI browses in `/plan`. A plan is a list of steps; running selected steps
submits one model turn in a thread. Invocation-private and legacy format-1
stores do not offer `plan:2`. The native core frames are unchanged. `plan:2`
replaces `plan:1`, `planWorkspace:1`, `planChat:1`, `planChatActivity:1`,
`planCatalog:1`, `planWorktree:1` and `planDraft:1`; a host offers only
`plan:2`.

## Steps and statuses

```
{ id, title, prompt, requires?, delegate?, status, reason? }
```

- `prompt` is the step's self-contained outcome and how to check it.
- `requires` lists earlier steps it builds on. It is context for the run, not
  a gate: a run can select a step whose prerequisites are pending.
- `delegate` is a subagent specification, `{ read_only?, worktree?, pwd?,
  thinking? }`. A review step is `{ read_only: true }`. The run's model passes
  only the step ID to `subagent`; the backend fills the rest from the run's
  revision.
- `status` is `pending`, `done` or `blocked`; `blocked` carries a `reason`.
  `done` is final; to redo work, add a step with a new ID. `blocked` can be
  selected again. Statuses are keyed by step ID and carry across revisions; a
  removed step drops its status.
- A plan is complete when every step is done.

## Save from chat

Ask for a plan in any words. Every ordinary turn with a trusted workspace
offers three model tools alongside its normal tools:

- `plan_save({ planId?, baseRevision, title, steps })` saves a new plan
  (`baseRevision` 0, no `planId`) in the thread's workspace, or a revision of
  one. A revision must name the current revision; a stale one fails without
  changing the plan. A closed plan cannot be revised.
- `plan_read({ planId? })` reads a plan with its statuses, or without
  `planId` lists the workspace's plans.
- `plan_report({ steps })` records `done` or `blocked` for steps selected in
  the current run. It fails outside a run, for unselected steps, and for
  `blocked` on a done step.

Saving a plan or opening `/plan` does not authorize implementation, and the
model is told not to start the work unless asked. There are no drafts,
previews or restricted planning turns. Earlier preview entries stay readable
as text.

## Runs

A run selects steps of one revision and submits one turn for them. The turn's
input is a `hush.plan.chat` entry whose display text is
`Plan run · rN · Title · K steps`; the model sees the whole plan with statuses
and the selected IDs. The model works inline steps itself, calls `subagent`
with `{ step }` for delegated ones, and reports with `plan_report`. A step the
turn does not report stays `pending`. A delegate's result is input to the
report, not the status itself.

- **Run here** (`run`) submits the turn in a thread of the plan's workspace. It
  queues behind a busy turn and continues that thread's conversation.
- **Run in a new worktree** (`runInWorktree`) creates a checkout from committed
  `HEAD`, a new thread in it and the run, in one call. The checkout keeps the
  source's subdirectory and inherits its workspace grant. The model names the
  branch from the plan and step titles; on failure Hush uses the plan title.
  Uncommitted files and the conversation are not copied, and Hush does not
  merge the changes back.

A step is in at most one active run; a selection that includes a step being
worked on fails with `busy`. A done step cannot be selected. A run delegates
at most five steps, the per-turn subagent limit, which inline work and retries
share. A plan with an active run cannot close.

When the turn settles, the run's record ends `finished`, or `failed` when the
input was withdrawn or the turn did not finish, and the thread gets a
`hush.plan.result` entry summarizing the outcomes. A Continue does not replay
a run input after its run ended; select the steps again from `/plan`.

Every run is idempotent by `requestId`: a retry with the same selection
returns the same run, and the same key with a different selection fails.
`runInWorktree` keys on the plan and request; a retry after a lost reply
returns the same thread and run, and a retry that arrives while the first call
is still creating the checkout waits for that call's result. If a checkout was reserved but not finished,
the error names the thread to inspect in `/threads`; Hush does not reuse or
delete it.

## Discovery

Plans are global so they stay discoverable: the list covers every workspace,
the caller's workspace first, open plans before closed ones. `list`, `get`,
`history` and `close` name a directory (`cwd`) that must be trusted; it
authorizes the read, not a run. `activity` is keyed by thread instead, like
core `history`, and returns only that thread's own run. Running needs a thread
in the plan's workspace or `runInWorktree`.

## TUI

`/plan` (`ctrl+x i n`) opens the plan list. Enter opens a plan; `h` shows its
run history; `c` closes or reopens a plan of this workspace. In a plan, Space
selects steps (none selected means the focused step), Enter runs them here and
`w` runs them in a new worktree, which switches to the new thread. `tab` shows
details: the prompt, prerequisites, delegate specification, active run and
blocked reason. Enter in history opens the run's thread. A plan from another
workspace opens read-only. Without a thread, Run here creates one in the
launch directory and keeps the composer draft.

While a thread has an active run, a strip above the prompt shows the selected
steps (`✓` done, `⏸` blocked, `●` working), the count done and the turn's
state; `↑` peeks at the steps and Enter opens the plan.

## Wire operations

All methods are `ext.plan.<method>` and need `plan:2`.

| Method | Params | Result |
| --- | --- | --- |
| `list` (watch) | `{ cwd, page, after? }` | `{ revision, plans: PlanSummary[], more }` |
| `get` (watch) | `{ cwd, planId, after? }` | `{ revision, plan: Plan }` |
| `activity` (watch) | `{ threadId, after? }` | `{ revision, run: ActivityRun \| null }` |
| `history` | `{ cwd, planId, before? }` | `{ runs: RunRecord[], next }` |
| `close` | `{ cwd, planId, closed }` | `{}` |
| `run` | `{ threadId, planId, revision, stepIds, requestId }` | `{ runId }` |
| `runInWorktree` | `{ threadId \| cwd, model?, thinkingLevel?, planId, revision, stepIds, requestId }` | `{ runId, threadId, title, checkout: { cwd, root, branch } }` |

A watch returns at once without `after`, or when its projection differs from
`after`. Watches and `runInWorktree` each hold one pending slot per
connection.

- `Plan` is `{ id, revision, title, workspace, steps: Step[], runs:
  ActiveRun[], closed?, complete? }`; `ActiveRun` is `{ id, threadId,
  stepIds }`.
- `PlanSummary` is `{ id, title, revision, workspace, total, done, blocked,
  running, closed }`. A page holds at most 48.
- `RunRecord` is `{ id, revision, threadId, stepIds, state, outcomes,
  submissionId?, entryId?, failure?, worktree? }`, newest first, 12 per page;
  `next` is the last run of the page or `null`. `state` is `running`,
  `finished` or `failed`. `outcomes` are `{ stepId, status, detail }`.
- `ActivityRun` is the thread's oldest active run, or else its last one: a
  `RunRecord` with `planId`, `title` and its selected `steps` (`{ id, title,
  status, reason? }`). A fork starts with none.
- `runInWorktree` from a launch directory may stage a `model` and
  `thinkingLevel`, supplied together; otherwise the thread's or the host's
  defaults apply.

## Storage and upgrade

A plan's head (`hush.plan-head` v2) holds its workspace, revision, statuses,
active runs and closed flag. Each revision (`hush.plan-revision` v2), the run
log (`hush.plan-run-log` v2), the per-workspace list (`hush.plan-workspace` v2)
and each thread's runs (`hush.plan-chat-activity` v2) are separate documents.
Run receipts are per thread.

Stored `plan:1` documents convert on first read:

- `verified` progress and reported `done` become `done`; reported `blocked`
  stays `blocked` with its detail as the reason; `failed`, `incomplete` and
  everything else become `pending`.
- A step's `description`, `checks` and `blocker` become its prompt; `edit`,
  `scope`, `after` and `thinking` are dropped.
- Runs keep their outcomes; a run active at the upgrade ends `failed`. Plan
  bindings, fork pins and draft sources are ignored.

Scoped-run and fixture tasks drain as aborted after a restart. A scoped plan
turn in progress at the upgrade keeps no tools. Old `hush.plan.task`,
`hush.plan.run` and `hush.plan.pin` entries stay readable as text.

## Verification

```sh
npm run build && npm run backend:bundle
go build -o bin/hush ./app/cmd/hush
HUSH_TEST_BACKEND="$PWD/dist/hush-backend.mjs" go test ./tui -run 'Plan'
HUSH_TEST_CLI="$PWD/bin/hush" node --test backend/test/plan.test.mjs backend/test/plan-worktree.test.mjs app/test/plan.test.mjs
```
