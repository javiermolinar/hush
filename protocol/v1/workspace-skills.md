# Standalone skills (`workspaceSkills:1`)

A persistent Hush host with configured standalone skill roots can negotiate this
optional capability. It doesn't enable Pi resource discovery or change core v1.

- `ext.workspaceSkills.list` takes `{threadId}` for an existing, unarchived
  thread and returns `{skills:[{name,description,source,token}]}` in name order.
  `source` is `personal` or `workspace`. A workspace source also requires the
  current Hush work grant. When two enabled roots define the same name for one
  thread, neither definition is eligible in that scope. A reply contains up to
  64 records and does not include file bodies or paths.
- `ext.workspaceSkills.submit` takes `{input,token,environment?}` for an explicit
  `/skill:NAME` input, optionally followed by arguments. Use the same parameters
  and receipt as core `submit`. Images and environment hints require their
  own capabilities. The opaque token binds a staged selection to the exact
  thread, workspace, source file and bytes, grant/config, and backend boot. A
  stale token can't fall back to an unpinned submit; a key that's already been
  admitted still returns its saved receipt.
- A manually typed `/skill:NAME` with no token uses normal submission. The host
  resolves and validates the name in the current scope at admission. Merely
  listing a skill or copying its name into a draft never starts work.

See [standalone skills setup](../../backend/README.md#standalone-skills).
