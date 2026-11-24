# Loaded context (`loadedContext:1`)

A persistent standalone Hush host can negotiate this independent, read-only
capability. `ext.loadedContext.get` takes exactly `{threadId}` for an existing,
unarchived native thread. Its result is `{sources:[{kind,path}]}` in next-prompt
order, or `{sources:[]}` when none are eligible. `kind` is `personal` for
`APPEND_SYSTEM.md`, then `workspace` for each applicable `AGENTS.md` from the
repository boundary to the authorized thread checkout. Outside a repository,
only the checkout's file applies. A workspace source requires current Hush work
trust. Paths are absolute backend paths; do not probe them on the client.

This is a fresh resolution of instruction sources, not a saved snapshot of an
old prompt, a rendered prompt, a list of every resource on disk, or a guarantee
that an external file will not change before submission. Files that the prompt
resolver would skip, such as symlinks, invalid files, or empty content, are not
listed. The request does not admit work, create a thread, or reveal file contents.
Older peers get unchanged native v1 frames and need not offer this capability.
