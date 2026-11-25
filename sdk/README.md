# Go SDK for hush

This module has no terminal or Pi CLI dependency. `NewClient(socket, ClientOptions{})` connects lazily. All calls take a
context; `Close` cancels the client's connections. The first call negotiates v1. A connection lost between calls is
replaced on the next call, without replaying old requests. A host can supply `ClientOptions.DialUnix` to prepare the same store's connection off its UI loop. This also covers separate watch and wait connections; the SDK still negotiates and owns the connection. `TransportError.Indeterminate` marks a request that may
have reached the server. Do not repeat unkeyed mutations after that error. `Submit` alone can retry once on transport
loss, with the same thread, key and payload; retain the key for reconciliation with `LookupSubmission`.

A receipt is not settlement. Use `WaitSubmission` or one of the submission lookup methods for the fate of that input.
`WaitIdle` is a separate observation about ordinary thread work. Passive waits use independent connections, so
cancelling their contexts releases the server's waiter without withdrawing an input.

`Watch` returns a channel of **latest** read-only snapshots and errors, not a log. It uses an independent connection
per watch, always taking a new snapshot on a matching-feed gap, overflow, or disconnect. It retries with bounded
backoff. Cancel the old watch on a thread switch. Reconnect does not repair history; use one `Transcript` per viewed
thread and data store. `MergeWindow` immediately retains visible entries, `Sync` fills forward through a snapshot's
fixed upper fence, and `Older` pages backward. Only complete forward scans advance the contiguous anchor.

## Optional thread directory

Offer `threadDirectory: [2, 1]` for `ThreadDirectory(ctx, limit, cursor)`: unarchived
ownerless threads in last-message order, with display titles and timestamps when known.
An incomplete first page has `Indexing` set and no continuation; refresh from the first page.
Cursors do not survive a backend restart. A bare core host still supports `ListThreads`
without metadata.

Version 2 includes status and checkout metadata. `ListThreadDirectory` can include archived
threads, and `WatchThreadDirectory` keeps up to 24 explicit IDs current on one connection.
`CreateThread` can opt into a managed Git worktree; `SetThreadArchived` changes visibility
without changing history or files. Offer `threadWorkspace: [2, 1]` for named workspace
metadata and `ThreadDirectoryOptions.Query` for searching workspace names, titles, and
committed chat before paging. See the [wire contract](../protocol/v1/thread-directory.md).

## Optional native images

Offer `images: [sdk.ImagesVersion]` and populate `Input.Images` to submit
encoded attachments. This uses the same key and receipt as text admission.
`GetImage(ctx, ImageRef)` resolves one committed image marker by the viewed
thread, entry and zero-based message/block indices. The watch and history never
include image bytes. See the [wire contract](../protocol/v1/images.md).

## Native model catalog

Offer `settings: [sdk.SettingsVersion]` when the host supports `/config`. `Settings` reads the
persistent snapshot and `ConfigureSettings` applies a partial `SettingsChange`; both return the
committed defaults and reasoning-display mode. Model defaults affect new threads only.

Offer `modelCatalog: [sdk.ModelCatalogDraftVersion, sdk.ModelCatalogSearchVersion, sdk.ModelCatalogVersion]`
in `ClientOptions.Capabilities` to use the [native model picker](../protocol/v1/model-catalog.md).
`ModelCatalog` lists the default rows. `SearchModelCatalog` requires version 2 or
later and filters before the host's row limit. Version 3 accepts an empty thread
ID in searches to list models before a thread exists. `NewCatalogThread` requires
version 3 and creates a thread with a validated model and thinking level.
`ConfigureCatalogThread` changes an existing selection only on a negotiated connection.

## Workspace metadata

Offer `workspaceFiles: [3, 2, 1]` for `WorkspaceFileScope` and `ListWorkspaceFiles`,
and `SearchWorkspaceFiles` on a host that selects version 2 or later. Version 3
includes the workspace's top-level `.github` directory in browsing and search.
The file methods take a `WorkspaceFileTarget`: an existing thread or, on version 2
or later, the absolute `CWD` of a [prospective thread](../protocol/v1/workspace-files.md#prospective-threads).

Offer `workspaceSkills: [2, 1]` for `ListWorkspaceSkills` and, on version 2,
`PreviewWorkspaceSkills` for a prospective checkout without creating a thread.
Offer `loadedContext:1` for `LoadedContext` on an existing thread. Metadata
reads run on cancelable passive connections.

When submitting a selected standalone skill,
set `Input.SkillToken` to its opaque selection token and use its
`/skill:NAME` command text. The SDK will not downgrade a selected skill
to an unpinned submit on an older host.

## Optional runtime shutdown

Offer `runtime: [sdk.RuntimeVersion]` and call `StopRuntime` to request an orderly backend shutdown. A lost reply is
indeterminate; do not replay it against a possible replacement listener. Confirm that the original hello epoch is
gone. The managed CLI's `hush stop` performs that confirmation and never starts a missing backend. See the
[wire contract](../protocol/v1/runtime.md).

## Optional Pi stream

`OpenPi(ctx, PiOptions{CWD: cwd})` opens a dedicated `piEvents:1` connection and returns the ready fence before it
allows `Send`. Use `ParsePiCommand` on a Pi JSON object, `Send` once, and `Next` from one reader goroutine to consume
verified events and correlated replies in socket order. An explicit session switch carries a new ready fence in
its successful reply. `PiCatalog` reads public models without creating a thread. A bare native server fails with
`unsupported_capability` before opening a session.

This is **not** `Watch`: overflow, a sequence gap, a wrong epoch, or socket loss ends the stream. It won't reconnect
or replay commands. A `TransportError` may report indeterminate admission; inspect the durable state separately.
After terminal loss only an explicit new `OpenPi` against an idle thread creates a fresh stream, with no replay.

`go generate ./...` refreshes the embedded native and Pi schemas from `../protocol/v1/`; tests reject drift. Run
`go vet ./... && go test -race ./...` in this module for the native goldens and scripted Unix-socket checks.
