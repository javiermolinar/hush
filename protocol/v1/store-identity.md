# Native store identity (`storeIdentity:1`)

This optional capability does not change any core v1 frame. A server selects it only when offered and when the host supplies an identity for the storage it actually opened. `hello.result.epoch` still identifies a server boot, not a store. A standalone invocation-private memory host uses a new pair every invocation; it never claims disk continuity.

After a successful `hello` selecting `storeIdentity:1`, send:

```json
{"type":"request","id":"2","method":"ext.storeIdentity.get","params":{}}
```

The read-only reply is one correlated response:

```json
{"type":"response","id":"2","method":"ext.storeIdentity.get","ok":true,"result":{"storeId":"50585983-c167-459d-9c4b-492d3da95284","generation":"50585983-c167-459d-9c4b-492d3da95285"}}
```

`params` is exactly an empty object. The result has exactly two fields, both canonical lowercase UUIDv4 strings. Errors use the core failure envelope. Before hello, `not_negotiated`; without capability negotiation, `unsupported_capability` naming `storeIdentity`; for other operations on the negotiated capability, `unknown_method`. The identity is fixed for the listener's lifetime. No filesystem location or boot epoch is encoded in it. It is a continuity claim by a trusted host, not peer authentication or a backup-integrity proof.

A logical store ID is assigned at creation; its generation changes on each supported restore. Both values must match before reusing any scoped IDs, history, or a submission key across connections. Store identity is independent of state revisions and history cursors. Reconnect state with a new snapshot and fill any history gap separately after verifying continuity. A connection without this capability can only prove continuity to a previously verified legacy connection by presenting the same fresh server epoch, never by matching a socket path or thread ID.

The SDK offers this capability on all connections. Managed standalone Hush requires it. The first verified connection pins the client, including its other sessions. A conflicting proof requires a new client and fresh attach without carrying over old thread state or submission keys. Only keyed `submit` may be automatically retried after uncertain transport, with unchanged payload and after continuity verification; unkeyed operations remain indeterminate.

See [the backend storage contract](../../backend/README.md#standalone-store-compatibility-and-restore) for offline handling and the limits of rollback detection.
