# Backend subagents (`subagents:1–2`)

On a trusted, writable persistent host, an ordinary parent turn or a plan run can call the native model tool
`subagent({prompt, read_only?, worktree?, pwd?, thinking?})`, or in a plan run `subagent({step})`. This isn't a socket launch method and doesn't depend on a client
negotiating the log capability. The tool starts a fresh, private backend thread with the parent's model selection,
not its transcript. `thinking` replaces the parent's thinking level for this worker; the parent's model must
support it. Independent read-only calls issued in the same model response can overlap. Writable
workers run exclusively among the parent's workers in admission order. The parent resumes only after the
round's results are ready; there is no separate join or parent submission. There are at most five starts
per original user submission or plan run, invalid requests don't consume a start, and workers can't call `subagent`.

Omit `worktree` to use the parent's exact backend working directory, or give the branch of an existing, separately
trusted checkout in the same Git repository. The child starts at that checkout's root and sees its current files,
including uncommitted changes. This doesn't create, reset, or merge a checkout. `pwd` starts the worker in a
directory inside that checkout, relative to it or absolute. It must be a real directory, not a link, and it needs
no grant of its own. `read_only` defaults to `true`, which permits scoped file reads relative to the worker's
directory and, when the grant covers the repository root, the `git` tool. `false` permits the normal built-in
coding tools, including shell. A checkout is not a sandbox. Workers have no Hush-imposed deadline or tool-round
limit. Aborting the parent stops its child. A restart does not replay interrupted worker work; partial edits may
remain. `--no-session` stays tool-free.

`subagent({step})` takes a step selected in the current plan run that has a `delegate` specification. The backend
builds the prompt from the step and fills `read_only`, `worktree`, `pwd` and `thinking` from the run's revision, so
the model cannot change them; `step` cannot be combined with other fields. It counts toward the same five starts.
See [plans](plan.md#runs).

`git({command, args?})` runs `status`, `diff`, `show` or `log` through the same hardened runner as Git review.
Git history and objects cover the whole repository, so a worker whose grant is for a subdirectory doesn't get the
tool. Parents and writable workers don't get it either.
- Only options that select or format output are accepted. Revisions follow `--end-of-options`, and paths after
  `--` are relative to the worker's directory.
- Paths and revisions must be relative and stay inside the checkout, which also keeps `git diff` from switching to
  `--no-index`.
- `status -v`, `pretty.*` aliases and `%G` signature placeholders, with or without a modifier, are not accepted.
  The repository's `format.pretty` is replaced by `medium`.
- Hush disables external diff drivers, textconv, color, signature checks and submodule status, and does not take
  optional locks. It refuses repositories with clean, smudge or process filters or conditional includes, because
  status and worktree diffs run filters.
- Hush rechecks the checkout's grant before returning output. Output is cut at 48 KiB, and `log` shows 100
  commits unless `-n` is given.

## Follow a log

Offer `subagents: [1]` in `hello`, then use `ext.subagents.wait({threadId, submissionId, after?})` for a user
input in that parent. This is a passive wait and doesn't expose the child's native thread ID or allow input to it.
A reply is `{cursor, events, settled}`. `settled` describes the parent submission. The cursor is an opaque,
boot-scoped token; omit it for a baseline. Pass the last cursor to wait for a changed view (or parent settlement).
Each response replaces the previous view, not an append: progress can coalesce, and live text from a retried
provider attempt can disappear. Reconnect with a new baseline. Disconnecting does not stop the worker.

Events are grouped by worker in start order:

- `{type:"started", workerId, cwd, read_only}` identifies an admitted worker and the resolved directory.
- `{type:"text", workerId, text, truncated?}` carries the current public log, if nonempty. It is free text, not a
  model-authored schema. Only assistant text is projected, not raw tool output, call arguments, or private
  reasoning. The retained tail is bounded to 16 KiB and a bounded window of child entries.
- `{type:"finished", workerId, status, detail?}` confirms a terminal worker; `status` is `ok`, `failed`,
  `cancelled`, or `timed_out` for results saved by older builds. Transport EOF without a terminal event isn't
  evidence of completion.

The parent's tool result uses the bounded committed public text, not uncommitted provider partials, and indicates
failure separately. Native print keeps stdout for the final parent answer.

## Thread overview (`subagents:2`)

Version 2 adds `ext.subagents.overview({threadId, after?})` and keeps `wait`. The overview passively returns
`{threadId, cursor, workers, usage}`. It uses the same replacement cursor semantics and keeps following the
parent after a submission settles. `workers` describes the latest turn with subagents. `usage` sums recorded
usage from all the thread's own subagents, including failures and cancellations, not just the latest turn. It
uses the same fields as core `state.usage`, which is still parent-only. Add the two totals for thread spend;
never add the same replacement update again. Neither total measures context window occupancy.

Worker rows are `{id, title, cwd, readOnly, status, usage, text, truncated, detail?}`. `id` identifies the worker
record, not a client-addressable thread. In addition to terminal statuses, active states distinguish starting,
waiting on the model, reading, running other tools, retrying, responding, and stopping. The log retains the
final committed public partial on Stop, but not abandoned retry attempts or private reasoning. The bottom-rule
badge opens this roster; selecting a worker opens its live log and recorded spend in the same dock.
