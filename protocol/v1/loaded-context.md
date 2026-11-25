# Loaded context (`loadedContext:1` and `:2`)

A persistent standalone Hush host can negotiate this independent, read-only
capability. `ext.loadedContext.get` takes exactly `{threadId}` for an existing,
unarchived native thread. Its result is `{sources:[{kind,path}]}` in next-prompt
order, or `{sources:[]}` when none are eligible. Paths are absolute backend
paths; do not probe them on the client.

- Version 2 reports the full [instruction source list](../../backend/README.md#standalone-system-prompt):
  up to three `personal` files (`APPEND_SYSTEM.md` first if present, then the
  personal `AGENTS.md` files), followed by up to 128 `workspace` files. It lists
  direct and `.agents/AGENTS.md` workspace files from the repository boundary
  down to the thread's working directory. Outside a repository, only that
  directory is checked. A workspace source requires current Hush work trust.
- Version 1 reports only the legacy locations from that same resolution: at
  most one `personal` `APPEND_SYSTEM.md`, then up to 64 direct `workspace`
  `AGENTS.md` files. Version 1 cannot report the additional locations, even when
  they contribute to the prompt.

This is a fresh resolution of instruction sources, not a saved snapshot of an
old prompt, a rendered prompt, a list of every resource on disk, or a guarantee
that an external file will not change before submission. Files that the prompt
resolver would skip, such as symlinks, invalid files, or empty content, are not
listed. The request does not admit work, create a thread, or reveal file contents.
Older peers get unchanged native v1 frames and need not offer this capability.
