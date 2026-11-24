# Standalone skills (`workspaceSkills:1` and `:2`)

A persistent standalone Hush host on macOS or Linux can negotiate this
optional capability. An empty list means no eligible sources in that scope;
there is no requirement for configured roots. Neither version enables Pi
resource discovery or changes core v1.

- `ext.workspaceSkills.list` takes `{threadId}` for an existing, unarchived
  thread and returns `{skills:[{name,description,source,token}]}` in name order.
  `source` is `personal` or `workspace`. A workspace source also requires the
  current Hush workspace grant. The first valid source in discovery order wins for
  any duplicate name. A reply contains up to 64 records and does not include
  file bodies or paths.
- Version 2 also accepts `{cwd}` (an absolute backend workspace path) alone to
  preview a prospective thread. Preview does not create a thread, submit a run,
  or grant trust. The server still requires an existing Hush workspace grant for
  project skills. Clients should only use draft preview when they know the cwd
  is on the backend host, not from an unrelated client filesystem.
- `ext.workspaceSkills.submit` takes `{input,token,environment?}` for an explicit
  `/skill:NAME` input, optionally followed by arguments. Use the same parameters
  and receipt as core `submit`. Images and environment hints require their
  own capabilities. The opaque token binds a staged selection to the exact
  thread, workspace, source file and bytes, grant/config, and backend boot.
  A draft token instead binds to the backend checkout (canonical for trusted
  workspaces), so it can be used after creating the first thread there. It
  cannot be moved to another checkout. A stale token can't fall back to an
  unpinned submit; a key already admitted still returns its saved receipt.
- A manually typed `/skill:NAME` with no token uses normal submission. The host
  resolves and validates the name in the current scope at admission. Merely
  listing a skill or copying its name into a draft never starts work.

See [standalone skill discovery](../../backend/README.md#standalone-skills).
