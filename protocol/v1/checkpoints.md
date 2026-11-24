# Native checkpoints and rewind, version 1

Offer `"checkpoints":[1]` in hello. A persistent standalone Hush host selects it.
Bare, legacy Pi and invocation-private hosts do not. All calls have a source
`threadId`. A thread and transcript entry are identified by Durable decimal IDs;
Hush checkpoints and rewind operations have lowercase UUID v4 IDs.

| Request | Additional fields | Result |
| --- | --- | --- |
| `ext.checkpoints.list` | none | `{items: checkpoint[]}` |
| `ext.checkpoints.record` | `kind` (`bookmark` or `files`), optional `label`, optional `atEntryId` | checkpoint |
| `ext.checkpoints.effect` | `text` | `{entryId}` |
| `ext.checkpoints.preview` | `mode`, exactly one of `checkpointId` or `atEntryId` | preview |
| `ext.checkpoints.confirm` | preview `id` and exact preview `mode` | rewind receipt |
| `ext.checkpoints.status` | operation `id` | rewind receipt |

A checkpoint has `atEntryId`, `label`, `kind`, and an `id` for native records.
An imported `pi.label` projects as `imported_label` without an ID and never
provides file evidence. Native file records pin only committed Git HEAD, require
trust for the thread's workspace, and use the latest visible committed entry of
an idle thread.
They never stage or include local edits, untracked files or submodule contents.
Bookmark records can refer to any visible entry and don't use Git. Text fields
are single-line, at most 192 UTF-8 bytes and contain no control/format characters.
The host bounds native checkpoint and effect records at 128 per thread and lists
up to 128 entries, including labels in the most recent history page. Display
markers are not model input.

A preview returns `id`, `threadId`, `atEntryId`, `mode` (`files` or `transcript`),
`effects` (an array of recorded `{entryId,text}` after the anchor), and either
`files` (exact `repository`, `commit`, `ref`, `cwd`, `branch`) or a
`reason` when a requested file restore is unavailable. Effects are explicit
records of known actions outside Git. They aren't undone and the list isn't
an audit of all external actions. Preview creates no Git or fork state. It
expires on host restart, eviction, or after 10 minutes. Making a different
choice means requesting a new preview. To accept a transcript-only alternative,
pass `transcript` explicitly. A changed recorded effect invalidates confirmation.

The receipt returns `id`, `threadId`, `atEntryId`, `mode`, and `state`:
`staging`, `ready` with `forkThreadId`, or `failed` with `reason`.
A confirmed decision is durable, including if the response is lost. Get status
or repeat the exact confirmation; don't create a new operation to recover a
lost reply. Transcript-only is a fork with files left alone. File restore uses
the fixed private path and unique branch, rechecks current Hush trust, and
publishes the fork and its checkout metadata together only after verification.
The fork's new checkout does not inherit a workspace grant.
After restart the same task verifies and reuses a fully staged clean checkout,
rather than creating a second one. Unsafe or changed leftovers fail closed and
are retained for host-operator inspection. No original/shared checkout is reset,
deleted, or switched. The parent remains reachable.
