# Thread model

Hush models work as Pi Durable conversations. This document records the target design and the
migration order. It describes a direction, not shipped behavior; the protocol documents under
[`protocol/v1`](../protocol/v1/README.md) remain the contract until each phase lands.

## Concepts

| Concept | What it is |
| --- | --- |
| Thread | An ownerless conversation. Users talk to it and can open, fork and archive it. |
| Turn | One run in a thread. Work happens in turns. |
| Delegate | A conversation owned by a turn. It is hidden from core thread reads, and its result returns to that turn. |
| Review | A read-only delegate. |
| Plan | A global document with numbered revisions. Threads refer to plans; plans never own threads. |
| Plan step | A description of work with a status and an optional delegate specification. |

`thread_start` and `w` (run a plan selection in a new worktree) create new ownerless threads. They
are independent work, not delegates.

Delegates stay hidden by design. Core history exposes reasoning and tool arguments, a delegate's tool
profile is bound to its owner, and accepting input would keep the owning turn busy. To continue from a
delegate's result, start or fork a thread.

## Delegate

```
delegate({ prompt, pwd?, read_only = true, worktree?, thinking?, step? })
```

- The child starts in the parent's authorized workspace, or at the root of an existing trusted
  worktree of the same repository. `pwd` must stay inside that checkout. A delegate never acquires a
  new trust grant.
- `read_only` workers get `read` and, when their grant covers the repository root, a read-only Git
  tool (`diff`, `show`, `log`, `status`) bound to the checkout. Git history covers the whole
  repository, so a subdirectory grant gets no Git. Writable workers get the normal coding tools.
- `thinking` overrides the parent's thinking level, validated against the parent's model.
- `step` names a step of the plan selected for the current run. The backend fills the prompt,
  `read_only`, `worktree`, `pwd` and `thinking` from the pinned revision, so the model cannot change a
  step's specification. `step` cannot be combined with the other fields.
- Delegates keep the current subagent rules: at most five starts per turn, including retries and plan
  runs; no nested delegation; writers run one at a time; aborting the parent stops the child; and a
  restart does not replay interrupted work. Five results bound what returns to the parent's context.

The tool keeps the name `subagent` and the `subagents:2` capability. Renaming would change stored tool
names and the wire without changing behavior.

## Plans

A step has:

```
{ id, title, prompt, requires?, delegate?, status, reason? }
```

- A step without `delegate` runs inline in the thread's turn. A review step is a step with
  `delegate: { read_only: true }`.
- `status` is `pending`, `done` or `blocked`. `blocked` carries a reason.
  - `done` is final. To redo work, revise the plan with a new step ID. Editing a done step's text does
    not reset it.
  - `blocked` is not final. Selecting the step again retries it.
  - Statuses are keyed by step ID and carry across revisions. A removed step drops its status.
- A plan is complete when every step is done.

Plans are created and revised by an ordinary tool call in any turn. Plan tools take an explicit plan
ID; the model can list the workspace's plans to find one. There are no drafts, previews or
restricted planning turns. Saving a plan does not authorize implementation.

### Runs

- Enter on selected steps submits one model-driven turn in a thread in the plan's workspace, or in a
  new worktree thread with `w`. The model works inline steps itself and calls `delegate({ step })` for
  delegated ones.
- The turn reports `done` or `blocked` only for steps selected in that run. A step without a report
  stays `pending`. A delegate's outcome is input to that report, not the status itself.
- A run can select at most five steps with a delegate specification, matching the per-turn start
  limit. Inline steps' own delegates and retries share that limit; a step that cannot start stays
  `pending`.
- A step can be in at most one active run. A selection that includes a step already being worked on
  is rejected. The UI derives "running" from the active run record; it is not a status.
- Each run records `{ thread, submission, stepIds, outcomes }`. History opens the thread.

### Discovery

Plans stay global so they remain discoverable: `/plan` lists plans across workspaces. Running still
requires a thread in the plan's workspace or a new worktree thread.

## Checkouts

A thread lives in exactly one checkout, and its working directory never changes. Working in another
checkout means working in another thread.

- Git review follows the thread's checkout. Before a thread exists, it follows the launch directory:
  Git calls accept a thread ID or a trusted backend directory, as workspace files already do.
- `thread_start({ prompt, worktree?, history? })` starts the other thread.
  - `worktree: "new"` creates a managed checkout from committed `HEAD`, as `/worktree` does, keeping
    the source's subdirectory. A branch name selects an existing trusted checkout of the same
    repository; `new` is reserved and cannot name one. Omitting it keeps the current checkout.
  - `history: true` forks the current conversation at the entry before the current input, with the new
    checkout as its working directory. Without it the new thread starts empty.
  - `prompt` becomes the new thread's first input.
- Moving the conversation to a new worktree is `thread_start({ prompt, worktree: "new", history: true })`
  followed by opening the new thread. The original thread keeps its history and records a notice that
  names the new thread.
- Uncommitted changes are not copied. The tool result says so.
- The shell is not a sandbox, so `cd` in a command still reaches other directories. It does not move
  the thread. The model is told to use `thread_start` to work in another checkout.
- A checkout Hush creates from a trusted source inherits the source's grant for the same directory:
  `/worktree`, plan `w` and `thread_start({ worktree: "new" })`. Its files are the source's committed
  `HEAD`, and the agent already had a shell in the source, so it gains no new access. This removes the
  separate trust step from plan `w`. All three keep the source's subdirectory, which must exist in
  committed `HEAD`, so the grant keeps its scope. Each reserves its thread before Git writes, and grants
  only the directory it verified afterwards. Read-only subagents can read managed checkouts even though
  they live in the backend's data directory.
- A move (`history: true`) opens and focuses the new thread through the submitting TUI's pane route.
  `thread_open` stays background-only. Without a route, for example on a headless turn, the receipt
  has no `opened` field and the thread stays unopened. A declined open reports `opened: false`, and an
  open with an unknown outcome reports `opened: "unknown"`.
- A move forks at the last entry before the current input that every core reader can see, the same rule
  as core `thread.fork`. If none turns up within the 2,048 entries core history reads scan, the move
  fails instead of starting without history. If anything is added to the new thread before its first prompt is admitted,
  for example because you opened it and typed, the prompt is not submitted. A `worktree` branch must
  still be checked out when the first prompt is admitted.

## Removed

- Plan review (`planReview:1`).
- Review delegation: `delegation:1`, `reviewEvidence:1`, `workerActivity:1`, the `hush review` CLI,
  the `review` pane mode, and the spawn and join tools. Code review becomes a read-only delegate.
- Scoped runs and `verified`: `s`, the `edit` and `scope` step fields, acceptance scripts and the file
  helper's plan publication path.
- Fixture plan runs (`--faux` `!plan-fixture`).
- Drafts: previews (`planDraft:1`) and the restricted planning turn.
- Plan bindings: attach, `planWorkspace.use`, fork pins and `hush.plan.pin` entries.

## Phases

1. **Remove plan review.** Independent of the other phases. Done.
2. **Delegate.** Make `read_only` default to true, add `pwd` and `thinking`, add the read-only Git tool,
   and accept plan-run inputs in the turn check. The limit stays five starts per turn. Done. `step` moved
   to phase 3, which adds the step `delegate` field it reads.
3. **Plans (`plan:2`).** Step schema, `delegate({ step })`, three statuses, model-driven runs, explicit plan IDs, one active
   run per step, and removal of drafts, the planning turn, scoped runs, the fixture and bindings.
   Stored progress maps `verified` and reported `done` to `done`, `blocked` to `blocked`, and `failed`
   and `incomplete` to `pending`. Stored `edit`, `scope`, `after` and `thinking` fields are read and
   ignored. Depends on phase 2. Done; see [the plan protocol](../protocol/v1/plan.md).
4. **Remove review delegation.** Depends on the read-only Git tool from phase 2. Done. Old worker
   notices stay readable in every client: with no delegation clients left, core history no longer hides
   any entries.
5. **TUI cleanup.** One delegate roster, status glyphs that follow [the UI style](ui-style.md), a
   `UsageChanged` event when only delegate usage changes, and one meaning for extension
   `Thread.Usage`: the thread's own usage, with delegates' usage in `SubagentUsage`. Done.
6. **Checkouts.** Git calls accept a thread or a launch directory, and the TUI shows Git before the
   first thread exists. Checkouts Hush creates inherit the source's grant. `thread_start` gains
   `worktree` and `history`, a move switches to the new thread, and the model instruction replaces
   `cd` with `thread_start`. Independent of phases 2–5; runs before phase 2. Done.

## Compatibility

- Removed task kinds keep abort-only definitions for one release so stored tasks drain after a
  restart instead of reporting a missing definition: `hush.plan-review` v1 (phase 1),
  `hush.review-worker` (phase 4), and `hush.plan-scoped-run` v1–2 and `hush.plan-fixture-run` v1
  (phase 3).
- Old transcript entries (`hush.plan.review`, `hush.worker.notice`) remain readable as display-only
  text.
- The TUI and backend always ship together. Removed capabilities need no fallback for older TUIs;
  after phases 3 and 4, a mismatched TUI loses `/plan` and the review roster rather than receiving
  partial behavior.
