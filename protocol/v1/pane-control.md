# Interactive view routing (`paneControl:1` and `paneControl:2`)

This optional capability routes a [pane operation](../../docs/pane-contract.md)
back to the interactive client. Negotiation alone does not authorize a command.
Negotiate it independently of `clientEnvironment`.
The reference client offers it on its command connection only. Version 1
requires a local Rex or cmux origin. Version 2 also supports a tab in the
submitting TUI without a native adapter. Headless, watch, and metadata
connections do not bind routes. No core frame changes for an unnegotiated peer.

## Bind and admit

- `ext.paneControl.bind` takes `{capabilities: {left, above, tabs, read[, views]}}`,
  all booleans. Right and below are implicit. `views: true` enables idle
  terminal and attach-only Hush view launches. It returns `{route, socket}`. The
  route label is opaque and unique to this binding. The `socket`
  identity has `device`, `inode`, and `birth` (birth time in nanoseconds) as
  decimal strings. The client compares it to the socket it dialed: a forwarded
  or unverified connection does not keep a pane route. No multiplexer endpoint
  or native pane ID is sent. To revoke the binding, send
  `ext.paneControl.unbind` with `{}`. Version 1 binds once per connection.
  Version 2 also requires `threadTabs: true`; optional `noPanes: true` denotes a
  tab-only route and requires the native pane flags to be false. After revoking
  a native v2 route, a connection may bind once more with `noPanes: true` and
  fresh socket verification. It receives a new label for new inputs only; old
  routes, inputs, and handles never transfer. A verified local socket is still
  required. Version 1 peers do not see the new fields.
- `ext.paneControl.submit` takes `{input, route[, environment][, token]}` and
  returns the ordinary `{submissionId}` receipt. `input` is an unchanged core
  submit or negotiated image submit. `environment` requires `clientEnvironment`;
  a skill `token` requires `workspaceSkills` and the same validation as its
  submission extension. Plain submit on a bound socket does not acquire a route.
- The first admission owns the route just as it owns the text. The backend stores
  an inert route label, a 30-minute input deadline, and any `/pane` launch intent
  in private admission metadata. Authority is the matching live connection in
  this backend process, not a thread-to-pane association.
  Admission metadata is never projected into the model prompt or public history.
  Reconnecting or restarting does not restore authority for queued or placed inputs.
- Clients revalidate before routed submission. Before the first write, if no
  route is available, use the original submission method for ordinary chat.
  Do not fall back for an explicit `/pane` command. Routed text recognizes
  exactly `/pane` at the start, followed by an ASCII space or tab and a suffix
  containing at least one code point other than Unicode `White_Space` or
  `U+FEFF`. A retry after loss uses the
  same key, method, and payload; a different connection can recover an existing
  receipt but cannot admit a new input with that stale label. A valid issuing
  connection whose route was revoked can still admit ordinary chat. Once a
  replacement label is bound, the former label can recover an existing receipt
  but cannot admit new input.
- `ext.paneControl.workspace` takes `{threadId}` and returns
  `{dir, directory}` for the thread's current workspace. `directory` has the
  same identity fields as `socket`. This is metadata for an attach-only new
  Hush pane, not permission to execute. V1 requires a live route; v2 requires
  a prior bind but does not require that route to stay live. The client still
  rechecks the socket identity. The child must verify the same backend epoch
  before using any thread IDs.

The generation and dispatch checks use the exact placed inputs of the active
run, not the focused thread or queued entries. Every active input must have the
same unexpired, live route. Mixed origins, Continue, and unbound inputs do not
inherit it. Omit pane tools without an eligible route and default tool policy;
otherwise advertise only supported operations, modes, and placements. Plain
language is sufficient to request a pane or thread view. The latest placed input
has a shared budget of one open (`pane_open` or `thread_open`). Consume it before dispatch, even if the result becomes unknown.
Never substitute the backend shell for a pane; `/pane` is an optional pane-only
shortcut that also withholds backend coding tools, including on an inputless
Continue of an interrupted pane-only run. This restriction restores neither a
route nor an open budget. The frontend revalidates the exact native binding and
targets before effects.

## Dispatch and answer

The backend sends one in-flight `pane_call` on the bound command connection:

```json
{"type":"pane_call","id":"…","deadlineMs":1780000000000,"request":{"op":"list"}}
```

`deadlineMs` is an absolute Unix-ms deadline, at most 10 seconds from dispatch
and no later than any active input's lease. The request has one of these shapes:

| `op` | Other fields |
| --- | --- |
| `open` | `placement`, `dir` and `directory` from the authorized host, and `launch` as in the pane contract |
| `list` | None |
| `focus` | `pane` (Hush handle) |
| `read` | `pane` and `lines` (1–200) |
| `thread_open` (v2) | `threadId`, `where` (`tab`, `split` or `focus`), and, for `split` only, `dir` and `directory` from the target thread's verified workspace |

The wire formats for a view launch are `{mode: "terminal"}` and `{mode: "hush"}`.
The client resolves the
executable and backend socket/epoch from the already-verified connection; those
do not come from the tool or `pane_call`. View launches do not grant readback.
A `thread_open` split attaches to the requested existing thread by socket and
backend epoch. Neither that launch nor a tab registers a submission. `focus`
opens a tab like `tab` and switches the TUI to it, keeping the previous thread's
draft. Only a conversation move sends it (`thread_start` with `history: true`);
the model-facing `thread_open` tool never offers it. A route with `threadTabs`
accepts it. A confirmed `thread_open` echoes `threadId` and `where`.

Dispatch occurs off the protocol reader and TUI loop. The client replies via
`ext.paneControl.result` with `{callId, result}` on the **same connection**.
`callId` is the push frame's `id`, distinct from the request's correlation ID.
A confirmed result has the payload defined by the pane contract; nonconfirmed
results carry only `outcome`. Pane handles are `hp_` followed by 26 Base32
characters (`A`–`Z`, `2`–`7`). Handles and readback grants are issued per
connection. Native errors and identities never travel in these replies.
An empty acknowledgement means the result message was handled, not that an
effect was confirmed. A late or foreign result cannot resolve the pending call.

The backend fences dispatch by tool task ID; the client fences delivery by call
ID. Neither evicts seen IDs to replay a mutation. Reject before sending if the
route is stale, busy, or unsupported. After possible dispatch, disconnect,
timeout, or a malformed or lost reply gives `unknown`. Do not replay, guess a
new pane, use the backend shell, or forward to a different client. When version
2 revokes a live route with an in-flight call on abort or timeout, it may send
`{type: "pane_cancel", id}` to cancel local waiting on that route. Neither
cancellation nor disconnect can undo an effect that already occurred.
