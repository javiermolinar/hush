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

`title` is the explicit Hush name when set, then the opt-in generated title, then an automatic title based on the first exchange.
The reference backend saves these in its presentation document, using the
invocation name for an expanded skill. Empty threads have no derived title.
Use [intro v2](intro.md) to rename a thread.

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
  An opted-in worktree requires Git, a trusted source, and a committed HEAD.
  The reference host checks the source directory's workspace grant before
  checkout. The connection cannot grant or change trust.
  It creates a private checkout and unique branch from committed HEAD, not from
  working files, then creates a conversation with its `cwd` and allocation metadata
  in one Durable commit. The new checkout does not inherit a Hush workspace grant.
  Creation is not keyed; do not retry an uncertain reply.
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

## Workspace names and search (`threadWorkspace:2`)

Offer `threadWorkspace: [2, 1]` with a v2 directory. Version 2 preserves
workspace scoping and returns `workspaces` on every list, scoped or unscoped.
It is aligned one-to-one with `threads`. Each value is either `null` for an
unknown workspace or `{id, name}`. `id` identifies the canonical workspace
for grouping; `name` is a display label, not a unique key or a path. Linked
worktrees share their repository’s name and ID. `workspaceMatches` is still
present only when the request supplies `workspace`.

An optional `query` (1–128 characters) finds a case-insensitive literal phrase
in the workspace name, thread title, or committed user and assistant message
text. Spaces can match underscores and hyphens in workspace names. Message
search starts at three characters; it excludes reasoning, tool results and
arguments, and expanded skill instructions. Forks can match inherited messages
through their fork point. Matches are filtered before recency ordering and
pagination, including under workspace and archive filters. Search cursors are
also bound to the normalized query. While the index builds, partial results
have `indexing: true` and no cursor; refresh the same query to see more matches.
