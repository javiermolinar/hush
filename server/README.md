# Native v1 transport

`createCoreServer(harness, options)` serves the [frozen native v1 contract](../protocol/v1/) on a Unix socket.
Open the Harness in the host, call `await server.listen(path)`, and close the server before closing the Harness.
The host owns the storage, scheduler, socket directory and permissions, and cleanup of any socket left by a dead process.
`onConnections` reports the number of attached clients for the host's lifetime policy. A bare Harness serves no optional
capabilities. An explicit `capabilities.piEvents` adapter can provide the separate [ordered Pi protocol](../protocol/v1/pi-1.6/pi-events.md).
The transport validates its records and manages framing, backpressure, and epochs; the host must provide a verified
pre-fold event source and enforce thread ownership.

Each connection admits requests serially. Passive waits detach from admission and have a connection-scoped cancellation
signal. The state observer only records the latest immutable view on the Harness line; projection, Chord diff, and I/O
happen later. Network backpressure folds pending observations and eventually ends the feed; clients that cannot drain
control/reply frames are disconnected. The server caps peers, correlation IDs (262,144 per connection), pending waits
(64 per connection), and the unsent socket buffer (two maximum records plus control frame space). Reconnect for a fresh
snapshot. Signed cursors are scoped to the boot and range; they do not retain state or turn a revision into an entry ID.

Native frames don't export raw provider or task exceptions. Negotiated Pi events include the profile's assistant message
and diagnostic payloads. Neither path logs payloads. Transcript and tool content is application data; do not pass secrets
as prompts or unguarded arguments to visible tools.

Run `npm run server:check` for live Unix socket and SQLite recovery tests using a bare faux Harness. Run
`npm run server:bench` to measure a separate server process over 140 workload conversations plus the root, with the
same phases and local `seq` workloads as the prototype: 20 small, 100 small in batches of 25, and 20 with five large
turns. It reports sampled physical footprint on macOS and RSS on macOS/Linux, not model inference memory.
