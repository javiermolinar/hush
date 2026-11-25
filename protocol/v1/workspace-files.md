# Workspace files (`workspaceFiles:1` and `workspaceFiles:2`)

This optional read-only capability leaves all unnegotiated native v1 frames unchanged.
Persistent Hush hosts advertise it only when a local descriptor-relative listing helper
is configured. It does not authorize reads on its own.

Offer `[2,1]` to negotiate recursive workspace search when available and retain
browsing on older hosts. Versions 1 and 2 both support the same scope and list calls:

- `ext.workspaceFiles.scope` takes `{threadId}` and returns `{workspace,home}` (two
  booleans). The host resolves the existing, unarchived thread's recorded working
  directory. With version 1 the caller cannot substitute paths or use a prospective
  thread; version 2 adds [prospective targets](#prospective-threads).
- `ext.workspaceFiles.list` takes `{threadId,root,directory,limit,cursor?}`. `root` is
  `workspace` or `home`; the latter is always the backend profile's home and requires
  a separate grant. `directory` is `""` or a relative slash-delimited path of up to 32
  components and 2048 UTF-8 bytes. Components are at most 255 bytes; no dot-prefixed
  component, quotes, slashes within a component, `@`, `:`, backslashes, or controls.
  `limit` is 1–100. The result is `{entries:[{name,directory}],nextCursor?}`; the names
  describe immediate children only, with directories first. No file contents, root
  path, symlinks, special files, hard-linked regular files, or hidden names are listed.

The host reads at most 4096 directory entries per request (including filtered
names) and rejects larger directories with `limit`. `nextCursor` is opaque and
expires within 60 seconds. It is scoped to the backend boot, exact thread or
prospective directory, root, current grant, directory, listing and limit. Changing or revoking a grant, replacing
an ancestor with a symlink, or changing a directory invalidates continuation. An
unknown or unauthorized first page returns `not_found`; an invalid continuation
returns `invalid_cursor`.

With `workspaceFiles:2`, `ext.workspaceFiles.search` takes `{threadId,query,limit}`.
It searches only the thread's authorized workspace, never the backend home
separately. Queries are nonblank UTF-8, at most 255 bytes, without slashes,
backslashes, quotes, `@`, `:`, or controls. The limit is 1–32. Results are
`{matches:[{path,directory}],partial}` with workspace-relative paths using the
same component and path limits as `list`. Search returns names only; it does
not traverse symlinks or hidden directories, list hard-linked regular files,
or descend into `node_modules`, `vendor`, `dist`, and `target`. These directories
can still be browsed explicitly. Search does not depend on Git or ripgrep.

Matching is case-insensitive. Exact basename matches rank ahead of prefixes,
substrings, and subsequences, with shallower paths first within each rank. For
a query with multiple words, the words before the last can also narrow matches
by parent path. The breadth-first scan is bounded by elapsed time, entries,
directories, and depth. `partial` is true if those limits or unreadable
subdirectories prevented a complete scan. Search is not paginated, and an
untrusted workspace returns `not_found`.

## Prospective threads

With `workspaceFiles:2`, all three calls also accept `{cwd}` instead of `{threadId}`
to address a thread before it exists. Send exactly one of the two. `cwd` is an
absolute path of at most 4096 UTF-8 bytes without controls, naming an existing
directory on the backend host: the working directory a `thread.new` with that `cwd`
would record. The host applies the same grant and listing rules and neither creates
nor records a conversation. A peer could create that thread and browse it, so this
grants nothing new. A missing directory returns `not_found`. A cursor stays with its
target: a listing started for a prospective thread cannot continue under a thread ID,
or the reverse. Send `cwd` only for a workspace the client shares with the backend host.

The workspace root uses the same grant as agent tools and Git operations:
`hush trust grant|revoke --cwd PATH` on the backend host. The home root uses a
separate `hush trust grant|revoke --scope home` grant for browsing only. Socket
peers cannot create or change these grants.
The Go CLI passes the absolute helper when launching Hush. The backend keeps one
private helper process warm once a granted root is available; every page still
reauthorizes and rescans the exact directory. A custom standalone
launch can set `HUSH_FILE_LIST_HELPER` to the matching installed `hush` executable.
