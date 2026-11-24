# Thread directory

Offer `threadDirectory: [2, 1]` in `hello` for the newest version the host supports.
Version 1 enables `ext.threadDirectory.list`; version 2 adds status, checkout metadata,
scoped watches, and management. See [the independent schema](thread-directory.schema.json).
The reference backend offers this on its persistent host. Native `thread.list`,
`thread.new` and `thread.fork` are unchanged.

Pass a `limit` (1–100) and optionally the prior reply's `nextCursor`. The result
lists unarchived ownerless conversations ordered by last committed visible chat message
timestamp, descending, then numeric conversation ID descending. Textless
tool calls, tool results and maintenance records do not count as chat.
A fork can show its inherited chat. A thread without chat sorts after dated
threads and has no `lastMessageMs`.

`title` is the explicit Hush name when set, then the Pi session name, then the
opt-in generated title, then an excerpt of the first committed user message.
The reference backend saves these in its presentation document, using the
invocation name for an expanded skill. Empty threads have no derived title.
Use [intro v2](intro.md) to rename a thread without changing its Pi session data.

The reference backend builds a disposable in-memory index cooperatively and
updates it from coalesced commit observations. Listing reads only the index,
not a page of history for each thread. While building, `indexing: true`
means the first page is partial and has no continuation; refresh from the
first page. The cursor is valid only on the issuing backend boot. It is a
live sort-key fence rather than a snapshot; reopen the picker to see threads
whose recent activity moved them ahead of the fence.

## Version 2

- `ext.threadDirectory.list`: same limit, cursor and ordering. `includeArchived: true`
  includes archived rows. Cursors are bound to both the version and that filter.
- `ext.threadDirectory.get`: pass `threadIds` (1–24 distinct IDs). Returns `{revision,
  threads}`; unknown, task-owned and not-yet-indexed IDs are omitted. Archive is
  visible on explicitly requested rows. Pass the last `revision` as `after` to wait
  for a replacement; an unrelated directory change may wake the wait. Only one
  passive wait per connection. Reconnect without `after`.
- `ext.threadDirectory.new`: optional `cwd`, `title` and `worktree` (defaults to false).
  Paths are on the backend. Omitting `cwd` uses the backend's project directory.
  An opted-in worktree requires Git, host-authorized repository trust and a committed HEAD.
  A standalone Hush host checks its private Hush grants before checkout; the
  legacy Pi host uses Pi trust. The connection cannot grant or change trust.
  It creates a private checkout and unique branch from committed HEAD, not from
  working files, then creates a conversation with its `cwd` and allocation metadata
  in one Durable commit. Creation is not keyed; do not retry an uncertain reply.
- `ext.threadDirectory.archive`: set `{threadId, archived}`; returns `{}`. A live
  native run or compaction rejects archive as `busy`. Archive is presentation, not
  deletion, permission enforcement, or cancellation. History and explicit attachment
  by ID still work. It never modifies or removes a checkout or a branch.

Rows add `status`, `archived`, and `checkout`. Status comes from Durable’s live
activity and committed transcript: foreground run (`running`), waiting for work
or a tool wake (`parked`), future background wake (`scheduled`), retained completed
answer (`done`), or no observed work or terminal answer (`idle`). Undismissed
failures and aborts report `failed` and `stopped`. This is not a receipt or a promise
that the conversation will accept input. These states are recomputed after restart;
this capability does not create scheduled work.

`checkout.kind` is `shared` (optionally `cwd`) or `managed` (`cwd` and recorded
`branch`). `managed` records the allocation that still matches the conversation's
working directory and has no other indexed thread at that path. A conversation
fork inherits the working directory, not its allocation. This metadata does not
attest that the worktree still exists, is clean, or is safe to restore. The service
has no file restore or rewind. It is not a timestamp-to-entry lookup.

## Workspace selection (`threadWorkspace:1`)

Offer `threadWorkspace: [1]` alongside `threadDirectory: [2, 1]`. It is only
selected with a v2 directory that supports workspace queries. On that
connection, `ext.threadDirectory.list` accepts `workspace` (a host-local
launch directory) and, only with it, `browseAll: true`. An unscoped v1 or v2
list has its original request and result shape. Never send a client-local
path when attaching to a host that does not share that filesystem.

The backend canonicalizes launch and indexed working directories the same way.
Git subdirectories and linked worktrees share the real Git common directory;
non-repository folders use exact canonical path scope. A missing or
unresolvable indexed path has unknown scope. Workspace identity is a directory
index key, not an access control or a change to the tool working directory.
Rebuilding the disposable index re-resolves stored paths.

With `workspace` and without `browseAll`, the server filters before recency
ordering and pagination. The cursor is bound to the canonical workspace,
archive filter, scope mode and backend epoch. With `browseAll` the list is not
filtered, but relative workspace membership is still returned. A workspace
reply adds `workspaceMatches`, exactly one `inside`, `outside` or `unknown`
value per thread in the same order. Filtered replies contain only `inside`
rows. A partial index still returns `indexing: true` and no continuation.
