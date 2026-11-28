# Read-only Git review (`gitDiff:1`)

Offer `gitDiff: [1]` in `hello`. The persistent standalone Hush backend can
select it. It does not change the native state subscription, headless output,
or other capabilities. A peer without the selected capability cannot call it.

| Request | Fields | Result |
| --- | --- | --- |
| `ext.gitDiff.summary` | `threadId`, `range` (`head` or `checkpoint`) | `{revision, range, commit, checkpoint, branch?, files}` |
| `ext.gitDiff.file` | `threadId`, `revision`, `file` (zero-based index from that summary) | `{revision, file, lines, truncated}` |

The backend resolves the repository from the stored working directory of the
specified ownerless conversation. It compares at that checkout's Git root,
not at the client's path or the repository's primary checkout. Both operations
require the same current workspace grant as agent tools and `@` browsing, bound
to the thread's exact working directory. Trust does not isolate shared checkouts.
A missing grant is reported as `invalid_request` with the fixed message
`Workspace is not trusted by Hush. Run 'hush trust grant --cwd PATH' on the backend host.`
Clients may use that denial to offer local operator guidance; this extension has
no way to grant trust over the wire. Rows describe the worktree, not changes
attributed to the agent or thread.

`head` compares committed HEAD with the working tree. `checkpoint` uses the
latest native file checkpoint on this thread, verifying its exact pinned commit
and repository by the [checkpoint contract](checkpoints.md). It never substitutes
HEAD for missing file evidence. `commit` is the full lowercase Git object ID
(40 or 64 characters). `checkpoint` indicates that a recorded file checkpoint
exists; its evidence may no longer verify. `branch`, when available, is the
checkout's current local branch name (at most 1024 UTF-8 bytes). It is omitted
for detached HEAD. The backend does not accept paths, refs, or an arbitrary
commit ID from the client.

`files` has at most 128 paths, each at most 1024 UTF-8 bytes, sorted by path.
A row has `path` (repository-relative), `status` (`A`, `D`, `M`, `T`, `U`, or `?`
for untracked), `staged` (index differs from HEAD on this path), and `text`.
When line statistics are available, both nonnegative `added` and `removed`
are present. Counts describe the net comparison, not the sum of two diffs.
Renames appear as additions and deletions. Ignored untracked files, nested
submodule changes, and Git metadata are not reviewed. The standalone host
does not read untracked file contents for a summary: these rows have no line
counts and `text:false` (older hosts can supply counts).
Opening an untracked row checks whether it is still untracked and a small,
regular, single-link UTF-8 file. Other files return a no-preview note.

`revision` is a backend-boot, thread-scoped UUID v4 locator for that bounded
summary. It does not freeze live files. File requests read the current patch
against the summary's baseline, and refuse an expired locator or changed
baseline. No cross-thread locator reuse or retries with a different baseline.
Resolve a fresh summary after `invalid_cursor`. A patch has at most 1500
`lines` and is bounded independently of the core wire limit. `truncated` marks
a clipped preview. Each line has `kind` and `text` (not terminal markup).
`context` lines have both positive `oldLine` and `newLine`; `add` has only
`newLine`, `remove` only `oldLine`. `hunk` and `note` have neither. Display
terminal controls as visible text, including in filenames.

Calls are passive reads. No Git index, worktree, branch, checkpoint, or thread
state is written by review. The backend uses bounded Git commands; external
diff drivers and text conversion are disabled, and configured external filters
or conditional Git includes make review unavailable. Workers and cached
summaries are bounded; callers must handle failures and cancel slow reads or
reads for a thread they no longer display. File mutation, snapshot capture,
last-turn attribution, and line comments are not part of version 1.

## Launch directory (`gitDiff:2`)

Offer `gitDiff: [2, 1]` to review a backend directory before any thread exists.
With version 2, `ext.gitDiff.summary`, `ext.gitDiff.file` and
`ext.gitDiffWatch.wait` accept `cwd`, an absolute backend directory, in place of
`threadId`. Send exactly one of the two. The same workspace grant applies to the
exact directory, and no thread is created or recorded. A launch directory has no
file checkpoint: `checkpoint` is `false`, and the `checkpoint` range is unavailable.
A summary `revision` addresses only the target that produced it, so a launch
directory's locator cannot read a thread's files or the reverse. Never send a
client-local path to a host that does not share that filesystem.

## Push updates (`gitDiffWatch:1`)

Offer `gitDiffWatch:[1]` alongside `gitDiff:[1]`. When both are selected,
`ext.gitDiffWatch.wait` accepts `{threadId, after?}`. It returns
`{revision, summary}`, with a HEAD summary in the shape above, or
`{revision, error:{code,message}}` for an observed unavailable state (for
example a missing grant). An error inside the result advances the watch just
like a summary; malformed requests or admission limits can still fail the RPC.

The outer `revision` is an opaque 64-character lowercase hexadecimal token,
scoped to this backend boot and target. It is **not** the file locator in
`summary.revision`. Omit `after` for the first snapshot or after reconnecting.
With the current token the request waits for a different observation; a stale
token gets the current snapshot. There is at most one outstanding wait per
connection. Close the passive connection to cancel it. Clients can use
`ext.gitDiff.summary` when this capability is absent, or to get a fresh file
locator or compare against a checkpoint.

The standalone host shares the expensive comparison across clients of the
same verified worktree. A write-capable tool completion refreshes watched
checkouts when the tool cannot report which checkout it changed. Changes to
HEAD or the index also trigger a refresh. Other outside edits are picked up on
a 20-second host-side fallback. No reply is sent when the observation is
unchanged.
Workspace grants and the thread's checkout are rechecked before each
published snapshot; a cached comparison is not authorization. The
[Go SDK](../../sdk/git_diff.go) exposes `WatchGitDiff`.
