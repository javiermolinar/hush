# `piEvents:1` — ordered Pi 1.6 headless extension

Status: **additive wire v1**. This uses the [native request, error and hello envelopes](../), never the state feed or history cursor. It is optional: the server selects `piEvents:1` only when an adapter is installed and the client offers `"piEvents":[1]`. A bare Harness and the default backend without `--pi-events` select nothing. Other versions or absent adapters fail with `unsupported_capability` before work. The [extension schema](pi-events.schema.json) defines the wire records; the [pinned Pi matrix](surface.mts) defines the nested commands, responses and events and distinguishes supported, conditional and excluded rows.

## Methods and ordering

| Method | Parameters | Success result |
|---|---|---|
| `ext.piEvents.catalog` | `{}` | `{models:[...]}`, a public physical model catalog; does not create a thread or attach a stream. |
| `ext.piEvents.open` | `{cwd, threadId?, session?, continue?, fork?, provider?, model?, thinking?, models?, name?, systemPrompt?, appendSystemPrompt?}` | One ready fence `{threadId, epoch, seq:0, header, services}`. Creates a profile session unless explicitly selecting one. `threadId`, `session` (a hush session ID or unique prefix), and `continue:true` are mutually exclusive. `fork:true` requires a source. |
| `ext.piEvents.command` | `{epoch, command}` with the current ready epoch and a Pi command object | `{response}` or `{response, ready}` on a successful `new_session`, `fork` or `clone`. `response` is the single Pi command result, with the exact `command` and optional string `id` (including empty strings). |

`open` is single-use per connection. It is not a probe to repeat on loss: an opened thread or metadata update may already be durable. `header` is a Pi version-3 header with a stable session UUID, cwd, creation timestamp and optional parent. It remains independent of the Durable thread ID, native boot epoch, stream epoch and entry IDs. `services` describes the named services of that binding. The reference host advertises `models`, `titles` and `resources`. It does not advertise `piSessions` or `images`; import, complete Pi entry/tree projection and image inputs fail before admission. Active messages, compaction summary metadata, fork points, parent IDs and titles for hush-created profile sessions are backed by committed data. These sessions have no Pi `sessionFile` to report.

The opening reply precedes live records, and no input is admitted before that fence. The client uses a fresh native request ID for every call; an application-level Pi ID is returned but is not the native correlation or a deduplication key. Commands enter in connection order. A long preflight, compaction or abort can reply later, without holding up other admitted commands. Explicit rebind drains old stream records before the reply, then starts the new stream after that reply. No late event from the old stream is valid after the new fence. A session switch cannot overtake an outstanding deferred command reply.

The live frame is:

```json
{"type":"pi_event","threadId":"42","epoch":"6b663e90-d644-49ac-bb75-b6f6da462166","seq":1,"event":{"type":"agent_start"}}
```

Sequence starts at 1 per fence and increments by exactly one, up to a safe integer. No event attaches to a submission key or command ID. `agent_end` bounds an attempt; `agent_settled` alone bounds the profile queue/retry/compaction drain. Provider boundary hooks, immutable commit records, and pre-fold tool output determine the stream; native patches or ordinary idle cannot repair one. The adapter requires an idle profile-managed thread at attachment and exclusive ownership through drain. A second attachment is `busy`, and ordinary native mutations are refused on these threads. Reads remain available over core v1. Restart may recover native work, but a live consumer cannot join partway through it.

## Terminal loss

One Unix record is still limited to 8 MiB. The server bounds queued events (256 records or 8 MiB) and uses the native socket write bound. Slow readers never block a commit or another peer. Overflow discards unsent events and sends `pi_end` with `{threadId, epoch, reason:"overflow"}` if writable. Failed source evidence similarly sends `source_failed`; an unwritable connection just closes. These records are not Pi events and never imply cancellation or settlement. SDK consumers also bound their unsent prefix. They stop on a gap, duplicate, unexpected identity, malformed record, overflow, or transport loss. There is no resume cursor, automatic reconnect, resubscribe, replay, or invented reply. Inspect durable state independently after indeterminate admission. Deliberately opening a new stream after the previous drain creates a fresh epoch and starts at sequence zero without history.

## CLI projection and bounds

Go JSON output unwraps the header and verified events; RPC unwraps only events and responses, not the ready fence or `pi_end`. Its stdin is strict UTF-8 LF JSONL with optional CRLF; U+2028/U+2029 inside strings remain data. EOF gives outstanding replies a bounded flush and disposes the invocation; it is not a wait for settlement. A failed provider turn is an event, while transport loss is a diagnostic and exit 1. The [mode table](README.md#modes-flags-and-io) specifies text/JSON/RPC outcomes. Profile input and staged queues have explicit limits (2 MiB text; up to 64 staged inputs or 2 MiB in aggregate). No limit silently truncates content. `--no-session` uses a tool-free MemoryStorage host tied to the CLI lifetime; it refuses durable continuation and has no payload logs, media cache, backups or detached work.

The contract is not full Pi CLI parity. Opaque provider diagnostics are redacted at the host boundary; stop reasons and retry decisions remain authoritative. Virtual/deferred providers or task boundaries without matched evidence are not projected. Host-owned threshold and overflow compaction reuse native accepted tasks and verified summary placement; they do not claim identical cut text to Pi. If a restart leaves a native summary without verified Pi estimate metadata, `get_messages` rejects that projection rather than fabricating it. Direct shell and HTML commands, Pi extensions/UI, images, Pi session import/tree projection and approvals stay outside this capability.
