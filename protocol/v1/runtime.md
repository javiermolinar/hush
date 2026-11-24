# Runtime lifecycle (`runtime:1`)

This optional capability lets a client ask the selected backend process to shut down. It does not change core v1 frames, stop a particular run, or prove settlement of any submission.

After `hello` selects `runtime:1`, send:

```json
{"type":"request","id":"2","method":"ext.runtime.stop","params":{}}
```

`params` is exactly an empty object. The successful reply is also exactly an empty object:

```json
{"type":"response","id":"2","method":"ext.runtime.stop","ok":true,"result":{}}
```

The server writes this reply before starting its orderly close. It then closes its listener and client connections, closes the Harness and host resources, and releases its storage lock. Existing clients are disconnected. This is a process lifecycle operation, not a run abort, an acknowledgement that active work settled, or a request to delete or replace stored history.

Before hello, the request fails with `not_negotiated`. Without capability negotiation it fails with `unsupported_capability` naming `runtime`; another operation on the negotiated capability fails with `unknown_method`. Nonempty stop parameters fail with `invalid_request`. Errors use the core failure envelope.

A lost reply is indeterminate. Do not automatically replay stop against the socket path: a managed client or another invocation may already have started a replacement with a new epoch. Confirm that the original epoch is unavailable or has changed instead. The reference backend does not restart itself. The next command using Hush's managed local configuration can cold-start the currently selected bundle; an explicit `--socket` remains attach-only.

Socket filesystem permissions remain the access boundary. This capability adds no authentication or remote administration protocol.
