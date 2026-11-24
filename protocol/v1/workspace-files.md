# Workspace files (`workspaceFiles:1`)

This optional read-only capability leaves all unnegotiated native v1 frames unchanged.
Persistent Hush hosts advertise it only when a local descriptor-relative listing helper
is configured. It does not authorize reads on its own.

- `ext.workspaceFiles.scope` takes `{threadId}` and returns `{workspace,home}` (two
  booleans). The host resolves the existing, unarchived thread's recorded working
  directory. The caller cannot substitute paths or use a prospective thread.
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
expires within 60 seconds. It is scoped to the backend boot, exact thread, root,
current grant, directory, listing and limit. Changing or revoking a grant, replacing
an ancestor with a symlink, or changing a directory invalidates continuation. An
unknown or unauthorized first page returns `not_found`; an invalid continuation
returns `invalid_cursor`.

Use `hush trust grant|revoke --scope browse --cwd PATH` or
`hush trust grant|revoke --scope home` on the backend host. Neither tool trust nor
Git trust is a browse grant. Socket peers cannot create or change these grants.
The Go CLI passes the absolute helper when launching Hush. The backend keeps one
private helper process warm once a granted root is available; every page still
reauthorizes and rescans the exact directory. A custom standalone
launch can set `HUSH_FILE_LIST_HELPER` to the matching installed `hush` executable.
