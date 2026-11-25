# hush protocol v1 — native core

Status: **Frozen native core**

This is the native contract for the Go SDK, TUI, and native headless CLI. [The schema](core.schema.json) defines wire shapes; this document defines ordering, cross-field checks, and recovery that JSON Schema cannot express. [Golden JSONL](fixtures/) and the reference tests are the conformance inputs.

## Headless I/O

Print writes the final answer to stdout; diagnostics go to stderr. A failed invocation exits nonzero.
Use `--no-wait --json` for a native admission receipt and `submission wait` for its exact outcome.
`--mode protocol` relays raw native frames over an existing socket and never starts a backend.
`--no-session` starts a local, tool-free in-memory backend for one invocation.

## Wire and negotiation

- The transport is a Unix stream socket. Write one UTF-8 JSON object followed by LF per frame; allow CRLF on input. Other Unicode line separators are data. No BOM, duplicate object keys, nonfinite numbers, or unpaired surrogates. Exclude the LF (and optional CR) from the 8 MiB limit per record. Do not silently truncate a record or an immutable entry to make it fit. The server can fail a request with `limit` before admission, end a subscription, or close a connection that cannot send a valid frame. Socket permissions and peer access are the host's responsibility; no authentication protocol is implied.
- Clients send `{ "type":"request", "id":"…", "method":"…", "params":{…} }`. IDs are unique for the lifetime of the connection. A valid request gets one correlated `{ "type":"response", "id":"…", "method":"…", "ok":true, "result":{…} }` or `ok:false` with `error`. Later settlement does not send a second response. A lost connection can lose an already-written or still-pending response. For malformed data with no usable envelope or for reuse of a correlation ID, close the connection rather than invent a response. Malformed parameters with a usable, unique envelope return `invalid_request`; an unknown unqualified method returns `unknown_method`.
- First send `hello` with `versions:[1]` (or a list containing 1). Reply `{ "version":1, "epoch":"…", "capabilities":{} }`. The epoch is a fresh token per server start over one Harness, not the storage identity. Until a successful `hello`, other commands return `not_negotiated` with no work; no common major gives `unsupported_version`. A second successful negotiation on the same connection is prohibited (`already_negotiated`).
- The reference backend selects `hushProfile:1` only on the standalone Hush profile, if offered.
  It marks a runtime profile, not a boot or store identity, and has no operations.
- `workspaceWorkTrust:1`, if offered, identifies a persistent Hush host that enforces
  [workspace trust for tool execution](../../backend/README.md#workspace-trust). Enforcement
  does not depend on negotiation. The capability has no socket operations.
- [`storeIdentity:1`](store-identity.md) optionally verifies persistent store and restore-generation continuity; it does not change the core hello shape.
- [`runtime:1`](runtime.md) optionally requests an orderly backend shutdown; it is not a run abort or proof of settlement.
- [`settings:1`](settings.md) optionally reads and updates persistent standalone model defaults and reasoning display. It does not reconfigure existing threads.
- `hello.params.capabilities`, when present, maps extension name to offered positive versions. An extension is usable only when the client offered it and the server selected its supported version. The baseline selects **none**, even if the client offers an unknown extension. `ext.<capability>.<operation>` without the capability returns `unsupported_capability` with the capability name and no work. This namespacing is not an extension payload schema. Each extension supplies its own versioned operations and schemas; it cannot alter unnegotiated v1 frames. Incompatible core changes require another wire major.
- Admission is ordered per connection; there is no total order of request arrival across connections. Long passive waits (`thread.wait`, `submission.wait`) are registered in that order but do not block later admissions while waiting. Their replies can arrive out of order. Mutations whose APIs do not separate admission from completion (notably `thread.abort`) can delay admission of subsequent commands on that connection. A lost reply to an unkeyed mutation is **indeterminate**: inspect current state, don't replay it. There is no ordering dependency on a reply to a passive wait.

Durable IDs (`threadId`, entry/task/submission IDs) are canonical positive decimal strings representing safe integers up to `9007199254740991`. ID domains are distinct. Entry IDs increase in append order within visible history, including inherited fork prefixes; numeric gaps mean nothing. Compare numerically for display order, not lexically, and never advance a resume anchor by adding one. Subscription IDs, epochs, opaque cursors and request IDs have unrelated meanings. IDs are scoped to the same data store: don't merge history if changing endpoints or replacing storage. After a detected invalid anchor rebuild history from the current thread.

## State subscription

`subscribe(threadId[, window:{min,max}])` returns one correlated ready snapshot `{subscriptionId, epoch, rev, state}`. Defaults are `min=50,max=100`; both must be in 1–100 and `min <= max`. The snapshot is the atomic initial observation. A connection has one state subscription; a successful subscribe replaces it, even for the same thread. A failed subscribe leaves it alone. Switch threads by subscribing to the new thread, not changing a hidden server-side input target. Every other thread command is explicit about its target. Concurrent state feeds can use separate connections.

The server sends the snapshot before its patches. Revision 0 is the initial observation, then any increasing safe integers. Patch frames contain `subscriptionId`, `epoch`, `baseRev`, `nextRev` and `ops`. Apply atomically only for the current subscription/epoch with `baseRev == rev` and `nextRev > baseRev`. An op batch can cover several folded source changes. If the matching feed supplies a gap, an already-applied batch, a non-increasing revision, another epoch, or invalid ops/state, do not accept the batch: mark the replica out of sync and require a fresh snapshot. Late frames from a different subscription ID are ignored. No state revision is accepted as an entry cursor or as a live event sequence.

Bound state buffers: on overflow discard unsent patches for that subscription, send `state_end` with `reason:"overflow"` if possible, otherwise close the connection. A failed source similarly uses `source_failed` or closes. Old frames can no longer advance that subscription. The client must negotiate again after disconnect, resubscribe and recover history separately. A slow subscriber must never hold up a Harness commit; other subscribers and the durable run continue. `state_end` is not evidence that a request or run settled.

### Chord operations

Use the decoded Chord 1.6 tuples, with paths inline, not Chord's compressed `WireOp` dictionary. Apply in listed order. Each path is 1–32 segments (string keys on objects or nonnegative integer indexes on arrays). Reject `__proto__`, `constructor`, and `prototype` as path keys. Traverse own properties only. The state root is always an object, so only `r` targets it. Intermediate containers must exist and be the right type. Don't publish partially applied ops or an invalid resulting state.

| Tuple | Effect |
|---|---|
| `["r", state]` | Replace the whole state; only as the first op. The same thread still applies. |
| `["s", path, value]` | Set an own property; on arrays replace a slot or append exactly at its end, never create holes. |
| `["d", path]` | Delete an object key (no effect if absent), or remove an existing array slot and shift the suffix. |
| `["a", path, suffix]` | Append `suffix` to the target string. |
| `["t", path, count]` | Drop `count` UTF-16 code units **from the beginning** of the target string, clamping at its length. For `A😀BC`, count 3 leaves `BC`. The resulting wire state must have well-formed Unicode. |
| `["p", path, index, deleteCount, values]` | Splice the target array. Index and delete count are nonnegative; clamp index to array length and deletion to the remaining length. Insert `values` at that index. |
| `["m", path, permutation]` | Reorder the whole target array: `new[i] = old[permutation[i]]`. The permutation must contain every old index exactly once. |

## Immutable history

Each entry has a globally allocated ID and its originating `threadId`. A fork can show another thread's entries up to its inclusive fork point. An entry's payload for this wire version is invariant under replay, compaction, and page/window overlap; compaction or reset adds a new head marker, it does not rewrite or remove past entries. The snapshot/patch embeds a recent `window` in ascending ID order, `beforeEntryId` (the immediate predecessor or `null`), and `lastEntryId` (last entry in that observation or `null` on an empty thread). Eviction from the window is not transcript deletion.

`history(threadId, range, limit)` returns that thread, entries in ascending ID order **in either direction**, and an opaque `nextCursor` or `null`. `limit` is 1–100. `range` is one of:

- `{"kind":"after","afterEntryId":lower,"throughEntryId":upper}`: exclusive lower, inclusive upper. `lower:null` starts at the beginning; `upper:null` only with `lower:null` means the empty snapshot. Equal nonnull bounds return no entries. Reversed bounds or anchors not visible on the target thread return `invalid_cursor`.
- `{"kind":"before","beforeEntryId":upper}`: take the nearest entries strictly before that ID. `null` means the newest page. Further pages move older. Check nonnull anchors are visible on the target thread.
- `{"kind":"cursor","cursor":"…"}`: continue in the original direction and bounds, with no overlap from that cursor. Do not change the thread or use a listing cursor here. Cursors can expire with the epoch; `invalid_cursor` means reissue an explicit range query. A cursor is not an entry ID.

On reconnect, verify store continuity before taking the new state snapshot. Fill the gap from the last **contiguous** committed entry obtained in the old connection through `snapshot.state.window.lastEntryId` (which fixes the upper fence even if patches arrive). Page the whole range, merge with the visible state/history by `(viewed threadId, entry.id)`, and don't advance the contiguous anchor from a newer window until the gap is covered. Entries already seen outside the current window remain in history. Use backward paging from the oldest retained entry to load older scrollback. A stale embedded state is never grounds to drop durable entries. A history reply cannot repair a patch gap, and state convergence does not guarantee that a short-lived tool transition was seen.

## Methods and sources (Durable 1.6)

| Method | Source or server computation |
|---|---|
| `hello` | Wire-major and negotiated capabilities; fresh process epoch. No Harness mutation. |
| `subscribe` | `Harness.observeConversation()` over a bounded `ConversationView`; buffer ordered immutable observations, project, then derive Chord ops over the **projected** state. Don't do socket I/O in `advance`. |
| `history` | `Conversation.entries()` with visible inclusive bounds, excluding the proper endpoint and encoding the returned pagination state. Verify visibility with an exact bounded scan; never use the window as the whole transcript. |
| `thread.list` | Public `Harness.commit(tx => tx.scanConversations(...), ctx)` with no writes. Ascending IDs, opaque cursor (or `null` result). No all-threads live view in core. |
| `thread.new` | `Harness.createConversation` with ownerless ownership and optional stored `cwd`/model. No title or worktree creation in core. |
| `thread.fork` | `Conversation.fork(atEntryId, {ownership:{kind:"ownerless"}}, ctx)`; `atEntryId` is inclusive. No file restore. |
| `thread.configure` | Only stored model ref, thinking level, and compaction enabled override. `null` clears an override, omission changes nothing. Update `AgentDoc`/exported `configure` transactionally and retain other compaction policy fields. This is not a model catalog lookup or a promise the host has the provider. |
| `thread.abort` | `Conversation.abort()` with ordinary ownership semantics, then reply after ordinary idle. It withdraws native queued inputs as specified by Durable. |
| `thread.wait` | `Conversation.waitForIdle()` for ordinary scope, not a particular prompt or the global scheduler. |
| `thread.compact` | `Conversation.compact()`: success contains a task ID for **admission**. Watch native state to learn ongoing activity. |
| `thread.reset` | `Conversation.reset(handoff, ctx)`; acknowledgement means admitted. It may place the reset later. Original history remains. |
| `submit` | `Conversation.submit({type:"input", content:text, requestId, whenBusy}, ctx)`. Requires a nonempty text payload and a submission key. Busy can reject, steer or follow up; when idle a normal input can start work. |
| `continue` | Unkeyed `Conversation.submit({type:"continue"}, ctx)`; busy rejects. Start without a new user entry, possibly settling unanswered if context owes no answer. |
| `submission.get` | Thread-scoped receipt by ID (`Harness.submission().status()`) or by key (`tx.submissionByRequest(threadId, requestId)`). An unknown/foreign ID or key gives `not_found`. |
| `submission.wait` | Validate the thread-scoped ID, then `Submission.wait()` for terminal `done` or `unanswered`. Cancel waiting on disconnect; this does not withdraw the input. |

`submit` and `continue` return only `{submissionId}`. On an uncertain reply, **only** `submit` may be retried automatically, after verifying store continuity and with the same `(threadId, requestId)` and same payload. Durable keeps the first admission; the key does not update or duplicate it. A new wire request may use a new correlation `id`, which has no effect on deduplication. `queued` is not `placed`; `done` references an `answerEntryId`, while `unanswered` has a reason and may have no user entry. A `continue` has no user entry. Views can momentarily advance faster than a receipt query; neither a receipt nor apparent idle substitutes for the other.

### State and history field sources

| Wire | Projection |
|---|---|
| `state.thread` and thread listing | `ConversationRecord.id`, optional `parent` (`conversationId`, inclusive `at`) and `owner` (`conversationId`,`taskId`). |
| `window` and history entries | `ConversationView.entries`/`before` or exact `Conversation.entries` page. `entry.threadId` is the origin, `kind` is the stored entry discriminator, and `headEntryId` is the stored head when present. A record's `messages` projects `EntryRecord.model`, not edited model context; passive entries may have no messages. Raw entry data, context edits, and full provider payloads are not exposed. |
| Message content | Timestamp is epoch milliseconds. Project user/assistant/tool result text, reasoning text when not redacted, call ID/name/arguments, assistant stop reason, and tool result call ID/name/error flag. System entries are visible with empty blocks (system section content is not in core). Image bytes, redacted thinking and other unprojected blocks show `omitted`. Core defines no fetch; the optional [images capability](images.md) resolves a committed image marker using its thread, entry and block position. No auth, provider headers, signatures, or raw model configuration. |
| `agent` | Stored `pi.agent` overrides (`model`, `thinkingLevel`, `cwd`, `compaction.enabled`); absent means no stored override, not a fabricated provider default. |
| `streaming` | `pi.live.generation` attempt, projected assistant blocks and optional retry deadline/error. It is throttled state, not provider deltas. |
| `run`, `failure`, `compactions`, `compactionFailure` | `pi.live` run/task IDs, inputs and failure code; current compaction IDs, reason, blocking/attempt and latest failure. Omitted run is not proof that every ordinary owned task is idle. |
| `tools` | Model-issued tool slots in call order, status and retained output/dropped counts from `pi.live.tools`; results reference a committed entry. Nested/raw diagnostic payloads are not part of the base. |
| `queue` | User inputs in `pi.inbox`, ID order. Text projection (join text blocks with LF); no passive writes or image bytes. |
| `usage` | Totals over `pi.usage.models` and `.tools` buckets: reported input, output, cache read/write, total tokens and `cost.total` in USD. Zero means no reported spend in a category, not model pricing or context-window knowledge. |

Use `busy`/`idle` for rejected Durable admission, `not_found` for absent scoped records, `invalid_cursor` for invalid page inputs after schema validation, `limit` for explicit bounds, and `internal` for unexpected failures. Error text is diagnostic, not a selector; don't expose credentials. Public API requirements are pinned to Durable 1.6.0 and Chord 1.6.0. Transport resource limits and host credential handling are server work; the fixtures model their visible failure behavior.

## Deferred extensions

The optional [native model catalog](model-catalog.md) supplies the standalone model picker without Pi. The optional [settings capability](settings.md) persists those catalog defaults and the reasoning-display preference. The optional [client environment](client-environment.md) binds normalized client hints to one input without extending core `submit`. The optional [workspace file picker](workspace-files.md) lists names within a backend-authorized root. [Standalone skills](workspace-skills.md) scope text skill discovery and staged selection to a thread or prospective backend checkout. The [loaded context inspector](loaded-context.md) lists authorized instruction-source metadata without reading from the client filesystem. The core does not define model catalog, files/resources/trust, images, presentation title/archive, approvals, schedules, file checkpoint/rewind or ephemeral connection selection. Host services can be specified additively. The optional [intro capability](intro.md) adds a read-only startup report; it does not change core state. The optional [thread directory](thread-directory.md) adds recency-ordered presentation, status, archive and worktree management without changing core methods. The optional [images capability](images.md) admits fitted image inputs and fetches committed images without replicating their bytes. The optional [schedules capability](scheduling.md) manages user-scheduled occurrences on a persistent standalone Hush host. The optional [checkpoints capability](checkpoints.md) previews and confirms transcript or isolated file rewinds. The optional [Git diff capability](git-diff.md) reads thread-scoped worktree summaries and patches. The optional [Git watch](git-diff.md#push-updates-gitdiffwatch1) and [file-scope watch](workspace-files.md#push-updates-workspacescopewatch1) report changes without periodic client requests. The experimental [plan capability](plan.md) exposes structured drafts, scoped execution, and independent plan review on the persistent standalone host.

### Native stop with withdrawal evidence (`nativeStop:1`)

On a connection where both peers negotiated `nativeStop:1`, send
`ext.nativeStop.stop` with `{threadId, submissionIds}`. The list is ordered,
contains at most eight distinct durable IDs, and may be empty. The reference
standalone host checks ownerless/native mutation scope. It checks each ID
belongs to that thread and is an ordinary input, then uses public
`Submission.abort()` on queued candidates before one ordinary conversation
abort. This is distinct from core `thread.abort`.

The result is `{stopped, candidates}`. The boolean confirms ordinary abort
completion. `candidates` contains `{submissionId, outcome}` for each requested
ID, in request order. `withdrawn` means the candidate abort succeeded and the
receipt is terminal `unanswered:aborted` **without** a placed entry.
`not_withdrawn` means no withdrawal by this call (for example, another client
already delivered the steer). `unknown` means the host could not establish
withdrawal evidence; foreign/unknown IDs never produce a positive outcome.
The per-ID outcomes remain useful if `stopped` is false. There is no retry
on ambiguous mutation loss. An unnegotiated legacy `thread.abort` provides
no per-input evidence. Neither queue snapshots nor `unanswered:aborted`
with an `entryId` are authority to re-draft an input.

## Verify

From the repository root after `npm ci --ignore-scripts`, run `npm run protocol:check`. It validates the core JSON Schema and golden streams with a fixture-only replayer, plus a thin public-Harness producer. The producer projects native view observations, verifies convergence against a fresh view, pages actual history, and tests same-key admission through a SQLite reopen. It is not a socket server, SDK or release test. The goldens pin wire recovery. A subprocess fixture also kills and reopens an actual local Harness mid-tool to verify persisted native recovery without replaying an unsafe call. Full socket/process lifecycle and real slow readers remain server-milestone checks.
