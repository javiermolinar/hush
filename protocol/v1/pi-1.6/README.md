# Pi 1.6 headless compatibility reference profile

Scope is Pi **1.6.0**, not arbitrary Pi CLI or extension compatibility. This is the reference matrix and the source proof for [`piEvents:1`](pi-events.md). The Pi output and ordered wire schemas remain separate from the [frozen native hush contract](../). The bare native baseline does not advertise `piEvents`.

## Command, response and event inventory

[`surface.mts`](surface.mts) is the matrix and schema index. It has one named row per pinned `RpcCommand`, one per `JsonAgentSessionEvent`, and rows for RPC-only extension records. Each command has a typed request and success/failure schema, target scope, candidate source, recovery class and fixture ID. The compiler checks complete coverage against the pinned command/response unions and all nested `message_update` variants. `core` means the target is implementable portably over a Harness; `host` means a named capability is required; `no` means reject the request before side effects. Reference fixtures assert Pi's observed behavior; source probes show where the pinned Durable Harness can provide the corresponding evidence. Neither is a claim that production hush already implements the row.

Two Pi wire details matter:

- `get_last_assistant_text` serializes `{ "data": {} }` when there is no text, not `{ "data": { "text": null } }` as the published type and command page suggest. Pi skips textless non-`stop` messages, but a newer normal textless `stop` replaces the previous reply. Nonempty text is trimmed.
- A `message_update` carries `usage` and a typed block-indexed `assistantMessageEvent`, not the cumulative `message` or `partial` snapshots. `message_end.message` wins over the accumulated deltas. Provider `start`, `done`, and `error` variants exist in the declaration but normal Pi output reports them as message boundaries instead.

`RpcResponse` repeats the optional command `id`, exactly once per command; command completion may be out of order. Events don't share that ID (direct RPC shell output is excluded). An invalid JSON record gets an uncorrelated `response` with `command: "parse"`, `success: false`, and an error. Read strict LF-delimited JSONL, accepting one optional `\r` before LF. U+2028 and U+2029 inside a JSON string are not record boundaries. JSON starts with a version-3 session record; RPC has no header. The header isn't an agent event. The model projection is public model metadata, not credentials or secret request headers.

## Modes, flags and I/O

| Profile input | Rule | Reference fixture |
| --- | --- | --- |
| No explicit mode; either stdin or stdout not a TTY | Print, not an alternate screen or prompt. | `cli:redirected-flags-and-input`; terminal-only behavior from pinned `cli-integration.md`. |
| `-p`, `--print`, `--mode text` | `--print` selects one-shot print; `--mode text` alone with terminal stdin and stdout still selects the TUI. Positional messages are sequential. | `cli:redirected-flags-and-input`, `oneshot:text:*`. |
| `--mode json` | One-shot JSONL; header then events until automatic work settles. No interactive commands after start. | `cli:redirected-flags-and-input`, `oneshot:json:*`, `wire:tools-and-two-turns`. |
| `--mode rpc` | Commands on stdin; responses and live events on stdout; EOF requests orderly disposal. No header or startup `@file` prompt. | `wire:parse-error-and-crlf-do-not-change-request-correlation`, `cli:redirected-flags-and-input`. |
| Text or `@file` args, stdin, `--` | One-shot stdin is prepended to the first prompt; `@file` is relative to cwd; `--` ends flags. File/image inputs follow negotiated services and trust policy. | `cli:redirected-flags-and-input`, `cli:at-file-image-with-an-image-capable-model`. Native image parity stays at its capability milestone. |
| `--model`, `--provider`, `--thinking`, `--models` | Model lookup and eligible thinking levels use Pi's configured catalog. No headless selection prompt. Missing required host capability fails before admission. | `cli:redirected-flags-and-input`, `wire:idle-queue-models-resources`. |
| `--no-session` | Invocation-private memory only, including on failure or kill. No durable thread, payload log, image cache, backup, or detached continuation. Hush rejects durable continuation before admission; Pi `--no-session --continue` silently uses a fresh in-memory session. | `cli:session-flag-continuation-and-ephemeral-difference`, `oneshot:json:*`. Native hush has an independent CLI exit/kill test; Pi modes still need one after wiring. |
| `--session`, `--session-dir`, `--continue`, `-n`/`--name` | Select or import existing sessions using host support. Headless cannot invoke the interactive resume picker. | `cli:session-flag-continuation-and-ephemeral-difference`, `wire:abandoned-branch-and-compacted-context`. |

Hush's durable commands and raw transport mode are explicitly separate from Pi parity. Direct HTML export, direct RPC shell, and Pi TypeScript extension commands/UI are not part of the profile. `entry_appended` is not a transcript feed: the reference can emit it for Pi-internal context edits as well as extension entries. Do not fabricate either from native history. The table does not claim parity for Pi package management, credential commands, or arbitrary CLI flags.

| Outcome | Print stdout / stderr / exit | JSON stdout / stderr / exit | RPC |
| --- | --- | --- | --- |
| Answer | Final text blocks, LF per block / no error / 0 | Session header and events / no error / 0 | Correlated admission response and events through `agent_settled`; stays attached. |
| Assistant error or abort | No final text / final diagnostic / 1 | Authoritative failed/aborted assistant event / no mode error / 0 | Failure or abort after admission is in the events, never a second prompt response. |
| Length limit | Partial text / truncation warning / 1 | Authoritative `length` message / no mode error / 0 | Partial message in events; remains attached. |
| Command rejected before admission | Diagnostic / 1 if the invocation cannot run | Diagnostic / 1 if the invocation cannot run | One `success:false` response; remains usable. |
| Invocation or transport error | No fabricated answer / diagnostic / 1 | No fabricated terminal event / diagnostic / 1 | No synthetic successful response or settlement / diagnostic / 1. |

## Recovery classes

- **Read:** reconnection can query current data, never a historical response. `get_entries.since` is the entry cursor; an unknown ID is an error. A replica revision or event sequence is not an entry cursor.
- **Mutation:** a lost reply is indeterminate. Inspect state or report unknown; do not replay. **Keyed submit:** the hush transport alone may repeat the same submission key to get the same receipt. Never replay Pi RPC commands on reconnect. Admission is not settlement.
- **Live:** subscribe with a ready fence before admission. Keep the per-thread producer order through serialization. On restart, sequence gap or overflow explicitly fail the invocation; there is no event replay or silent resubscribe. `agent_end` closes a low-level run/attempt, while `agent_settled` is the automatic-work drain fence, not the receipt of one input. Do not invent missing events from replica state.
- **Unsupported or absent host service:** fail before execution. `profile:failures` pins the negative replies; `profile:unavailable-never-dispatches` runs the typed pre-dispatch gate and verifies no handler call, provider request, or Harness commit. Production RPC routes must pass the same checks after wiring.

## Reproduce

Run `npm ci --ignore-scripts` and `npm run profile:check` from the root. The TypeScript check covers the pinned wire unions; `profile:documented-inventory` also checks the pinned RPC and JSON documentation. `skipLibCheck` is scoped to the profile because upstream generated model declarations omit NodeNext JSON import attributes. The wire tests run Pi's `runPrintMode`/`runRpcMode` with a scripted provider. The CLI fixtures run the actual pinned Pi executable with a loopback provider. All use temporary profiles and disable extensions; no operator sessions, external provider requests or shell tools are involved. Times and IDs are checked by relation, not by value.

### Ordered source proof

[`ordered-run.mts`](ordered-run.mts) matches a provider tap to the generation invocation's abort signal. It gets immutable inputs/results and task transitions from stored commits, not from folded snapshots. In the fast-tool fixture a deliberately blocked watch misses the tool's running state; the trace still matches Pi's two turns and both output chunks. A Durable generation task can hand off to another **within the same Pi run**. The tests hold `afterRun` open past submission settlement, compare generation retries through recovery and exhaustion, and show that an abort without a committed partial must have a matched terminal provider event or fail closed. Unoffered tools and invalid arguments use the pre-commit rejection hook: `execute()` never runs, and the trace omits execution duration even if Durable recorded phase time. The journal tests readiness, ordering, epoch changes and bounded overflow without replay; it is not a socket transport.

[`ordered-compaction.mts`](ordered-compaction.mts) matches manual compaction retry order and the successful blocking-overflow path. The owner generation's next `beforeRequest` hook verifies the exact committed result before emitting `compaction_end` with `willRetry:true` and re-entering the agent. Pi can produce a separate split-turn summary; the overflow fixture compares order and result semantics, not identical summary text or cuts. [`ordered-metadata.mts`](ordered-metadata.mts) checks committed effective thinking levels and an injected title document that survives SQLite reopen.

### Local queue adapter proof

A bare Durable `submit` starts an idle conversation, or refuses it with `whenIdle: "reject"`; it does not stage a Pi `steer` or `follow_up`. [`local-queue.mts`](local-queue.mts) implements a separate hush-owned document for Pi RPC inputs, without editing `pi.inbox` or `pi.live` directly:

- Enqueue stores input without starting a run. The payload lives in the owning Harness: SQLite for durable conversations or invocation-private `MemoryStorage` for ephemeral ones. Forks start with an empty queue.
- The default Pi policy is one input at a time per queue. The first pending steer joins the next prompt, after the prompt's user message and before the first model request. Prompt admission, that entry and removal from staging share one transaction. Later steers take priority over follow-ups. The adapter transfers one input via public transactional submit after tools or at a successful answer boundary, in the same commit as its removal from staging.
- `clear_queue` removes both staged lists in one transaction and returns only text. If clear commits before handoff, the item does not run. After handoff Durable owns the input. Abort stops the active work but leaves inputs that have not been selected pending.

`wire:local-queue-order` and `wire:abort-retains-local-inputs` check the reference behavior. The `durable:local-queue-*` fixtures cover a running tool, clear during a request, abort, image payload retention, SQLite reopen and fork isolation. `durable:ordered-queue-matches-pi-delivery` tags selected inputs at handoff and publishes removal just before delivery; it compares duplicate text across queues and a confirmed idle no-op clear. The profile owns the conversation. Its explicit abort preserves unselected inputs and can restore the exact selected handoff if its native receipt was withdrawn before placement. It does not reinsert a placed entry or emit a second enqueue or delivery. A native abort bypassing the owner is tested to fail closed rather than invent delivery or settlement.

### Compaction result adapter proof

[`compaction-result.mts`](compaction-result.mts) uses a stored-commit observer, attached before scheduling, to capture the built-in compactor's committed `tail` and `firstKept` IDs. Those identify its selected context across retries; a fresh read when `beforeCompact` fires can already see newer entries. The observer only records small task markers, not messages or speculative view revisions.

The adapter waits for the task, then for its exact write submission when placement was deferred. Blocking threshold/overflow compaction returns the entry ID directly. It reads context at the selected `tail` for `tokensBefore` and at the placed summary entry for `estimatedTokensAfter`. This excludes later entries from both estimates. It reuses Pi's token helpers, avoids provider usage from before a compaction or context edit, and counts the plain summary, not Durable's wrapper. It returns the summary and first kept ID from the verified placement. The probe uses stringified native entry IDs; imported Pi sessions still need the host's ID mapping. Optional `usage`/`details` are omitted, not inferred from session totals. Durable still chooses the compaction cut and generates the summary; this is not a claim it produces Pi's identical text or cut.

The `durable:compaction-result-*` fixtures cover retry, delayed manual placement, blocking threshold, overflow recovery, edited and already compacted history, stale/superseded writes, no summary, failure, abort, and a missing selection. Only a verified placement yields `applied`; no task receipt is promoted to success on its own. A custom Durable hook that supplies a summary can bypass the built-in selection checkpoint and is outside this proof. A completed task first seen after its selection was lost is explicitly unavailable.

### Joined automatic compaction boundaries

A Durable background compactor can outlive both a submission receipt and ordinary idle; a Generation hook cannot request the built-in compactor through `hookTask()`. Those pinned negatives explain why the fixture owns the trigger rather than relabeling autonomous work.

[`threshold-fence.mts`](threshold-fence.mts) uses one profile policy on an exclusively owned conversation with Durable autonomous compaction disabled. Threshold decisions use committed usage or the anchored context estimate after compaction/edits. It runs public `Conversation.compact()`. Native task and entry reasons remain `manual`; the reservation records whether the profile triggered threshold or overflow. A task that selects no cut starts no Pi compaction event.

| Boundary | Observed trigger and fence | Paired fixture |
| --- | --- | --- |
| Before prompt | Check the existing branch before native admission; verify placement before `agent_start`. | `durable:profile-pre-prompt-threshold-matches-pi` |
| Between turns | A real `afterTools` or queued-answer `afterRun` hook waits for the compactor's task, **not** its possibly queued write. The successor's `beforeRequest` verifies that its committed request cutoff includes that exact placement before `compaction_end` and `turn_start`. Missing placement fails immediately rather than deadlocking the hook. | `durable:profile-between-tool-turns-uses-placed-write-before-next-request`, `durable:profile-queued-answer-turn-compacts-before-delivery` |
| After answer | Hold `agent_settled` through the compactor's verified placement, failure or abort. A plain prompt still rejects while busy; a staged late input can continue after compaction without an intermediate settlement. | `durable:profile-post-answer-threshold-matches-pi`, `durable:profile-threshold-holds-busy-and-drains-late-input` |
| Overflow | Verify placement and an eligible continuation context first. Then start a new native `continue`, not another input; the original native receipt stays unanswered. Exhaustion, summarizer failure or abort does not issue another continuation. | `durable:profile-threshold_then_overflow-uses-one-policy` plus standalone success, failure, repeat-overflow and abort cases. |

Compactor retry order (including threshold retry/failure), queued writes, disabled policy, no cut, missing selection, and observer loss have explicit fixtures. Loss never invents placement, replays a command, or claims native work was aborted. Recovery uses a fresh invocation after loss; the journal is not a replay source.

### Production checks

The source proofs alone do not certify the producer. `npm run backend:check` exercises the real backend's provider,
commit and tool taps, including a blocked watch, exact compaction placement, and reopen with stable IDs.
`npm run server:check` covers the bounded feed and bare-core negotiation. `npm run headless:check` drives JSON/RPC
and native print through the Go binary, uses stock Pi's `RpcClient`, and checks real loss and invocation-private
exit/kill behavior. See [the capability boundary](pi-events.md) for services that this release does not advertise.

Sources: Pi [`cli-integration.md`](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/cli-integration.md), [`cli.md`](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/cli.md), [`json.md`](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/json.md), [`rpc.md`](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc.md) and [`rpc-commands.md`](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc-commands.md); distributions and declarations from this repository's lockfile. Durable API references are in `AGENTS.md`.
