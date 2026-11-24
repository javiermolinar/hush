# Native model catalog (`modelCatalog:3`, `modelCatalog:2`, `modelCatalog:1`)

This optional capability is independent of Pi. The standalone Hush host selects
the highest offered version it supports. A bare or older host can omit it and
continues to serve the frozen native core unchanged.

In version 1, `ext.modelCatalog.list` takes exactly `{ "threadId": "7" }`, using the core
Durable ID rules. An absent thread returns `not_found`. This read does not
create or change a thread or resolve request credentials. Before negotiation the
ordinary `not_negotiated`/`unsupported_capability` behavior applies.

Example `result`:

```json
{
  "current": { "provider": "fixture", "modelId": "old" },
  "default": { "provider": "fixture", "modelId": "new" },
  "models": [
    { "provider": "fixture", "modelId": "old", "label": "old", "available": false, "reason": "missing_model", "thinkingLevels": [] },
    { "provider": "fixture", "modelId": "new", "label": "New model", "available": true, "reason": null, "thinkingLevels": ["off"] }
  ]
}
```

`current` is the stored model reference (or `null` if unset); `default` is
what a new standalone thread uses absent an explicit model. References contain
only `provider` and `modelId`. Rows include each distinct current and default
reference even if not installed. The maximum is 64 unique rows; there is no
server pagination. Current and default come first, then usable physical chat
models, then other installed models as space permits. This is a picker, not a
complete model inventory. Public IDs and labels are nonblank and limited to 256
Unicode scalars; control/format characters and line/paragraph separators are not
allowed. Provider endpoints, model configuration, credentials and provider
error text are not part of this projection.

`available` is a fresh host-local credentials check, not a network health or
inference guarantee. `reason` is `null` for usable rows, `missing_model` for a
removed reference, `credentials_required` when credentials are missing, or
`availability_unknown` if the host could not finish checking. Levels are a nonempty, unique subset of `off`, `minimal`, `low`,
`medium`, `high`, `xhigh`, `max` as reported by the model runtime; a missing model
has no tiers. Availability can change between listing, configuration and
submission. Hush rechecks it at configuration and admission, and the provider
runtime checks again before sending a request.

Clients persist choices with normal `thread.configure`. On a connection that
negotiated this capability the standalone host validates an explicit model or
thinking tier against the resulting model before writing. A model switch from
the picker sends the new model and a supported level together. A failed switch
leaves the existing selection intact. A saved unavailable model is not replaced
automatically. A lost configure reply remains indeterminate: inspect current
state rather than replaying the change.

## Version 2: catalog search

The same method also accepts an optional `query` string, up to 128 UTF-8
bytes, without control/format characters or line/paragraph separators. An
omitted or blank query behaves exactly like version 1. A nonblank query matches
a case-insensitive subsequence of `provider/modelId label` (ignoring leading
and trailing spaces). Filtering happens **before** the row limit; refine the
query for more specific results rather than paging. Availability is checked for
matching providers only. `current` and `default` remain unchanged, but their
rows only appear when they match. An empty `models` list is a successful search
with no matches. The selection and thinking rules are unchanged.

## Version 3: model selection before thread creation

`ext.modelCatalog.list` also accepts `{}` or `{ "query": "…" }` with no
`threadId`. It returns the same catalog for a prospective standalone thread,
with `current: null`. It does not open a workspace, create a thread, or submit
input. A present `threadId` still selects an existing thread as in version 2;
`null` is not an ID.

`ext.modelCatalog.new` accepts `{ cwd?, model, thinkingLevel }`. `cwd` follows
`thread.new`; `model` is a catalog reference and `thinkingLevel` is required.
The host checks installation, availability and support for that exact level
before creating a thread with both values. A failed validation creates no
thread. On success the result is `{ id }`, as in `thread.new`; no prompt is
submitted. As with `thread.new`, a lost reply may mean the thread was created;
inspect the thread list rather than replaying the call. Neither the core nor
older catalog versions change.
