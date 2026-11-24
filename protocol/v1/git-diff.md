# Read-only Git review (`gitDiff:1`)

Offer `gitDiff: [1]` in `hello`. The persistent standalone Hush backend can
select it. It does not change the native state subscription, headless output,
or Pi compatibility. A peer without the selected capability cannot call it.

| Request | Fields | Result |
| --- | --- | --- |
| `ext.gitDiff.summary` | `threadId`, `range` (`head` or `checkpoint`) | `{revision, range, commit, checkpoint, branch?, files}` |
| `ext.gitDiff.file` | `threadId`, `revision`, `file` (zero-based index from that summary) | `{revision, file, lines, truncated}` |

The backend resolves the repository from the stored working directory of the
specified ownerless conversation. It compares at that checkout's Git root,
not at the client's path or the repository's primary checkout. Both operations
require current Hush workspace trust; a grant does not isolate shared checkouts.
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
submodule changes, and Git metadata are not reviewed. Only small, regular,
single-link UTF-8 untracked files have text previews. Binary and special files
can be listed without a preview or line counts.

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
