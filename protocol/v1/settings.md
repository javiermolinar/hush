# Persistent settings (`settings:1`)

The optional `settings:1` capability exposes the persistent standalone Hush defaults used by `/config`. It is available only on a persistent standalone host without a CLI model override or faux provider. It does not change the core thread configuration methods or Pi's settings API.

Negotiate with `hello.params.capabilities.settings: [1]`. Without negotiation, both methods return `unsupported_capability` with capability `settings`.

## Read

`ext.settings.get` takes `{}` and returns:

```json
{
  "model": { "provider": "openrouter", "modelId": "moonshotai/kimi-k3.1-pro" },
  "thinkingLevel": "high",
  "reasoning": "folded"
}
```

`thinkingLevel` is one of `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`. `reasoning` is `folded` or `expanded` and controls presentation, not provider reasoning effort.

## Change

`ext.settings.set` takes one to three of `model`, `thinkingLevel`, and `reasoning`. Omitted fields retain their stored values. The complete committed snapshot is returned. Empty changes, unknown fields, malformed model references, unsupported levels, and invalid reasoning modes return `invalid_request`.

Model and thinking defaults apply to newly created threads. Existing threads retain their stored selection; use `thread.configure` or `/model` to change them. A new default is reflected in subsequent catalog listings and thread creation without a backend restart. Model references and levels are validated against the host's installed model runtime.

`reasoning: "expanded"` shows supplied live and historical reasoning without expanding tool output. `folded` retains the existing Ctrl+O behavior. Empty or redacted provider reasoning cannot be recovered by this setting.

Updates are serialized and atomically replace the backend host's private `settings.json`; unrelated settings are preserved. A transport failure after `set` is indeterminate: call `get` rather than assuming success or failure. The capability has no update feed, so other already-connected clients are not pushed a new snapshot.
