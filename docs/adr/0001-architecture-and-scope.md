# ADR 0001: Workspace, v1 scope and releases

Status: Accepted

The [native v1 core](../../protocol/v1/) is the sole backend protocol.

## Decision

hush is a Go TUI and headless CLI backed by Pi Durable. The backend uses the Hush-owned profile and does not load Pi settings, credentials, or trust. The prototype in `pi-durable-go-test` stays separate.

| Path | Responsibility |
|---|---|
| `protocol/v1/` | Language-neutral JSONL wire contract, schemas, and conformance fixtures. |
| `server/` (`@hush/server`) | Portable TypeScript transport over the public Durable Harness API. The core takes a Harness. `hello` advertises optional, explicitly injected host services independently. No dependency on the reference backend or a TUI. |
| `backend/` (`@hush/backend`) | Reference host: assembles the Harness, configuration, credentials, resources, tools and storage; supplies host services and starts the portable transport. |
| `sdk/` (`github.com/javiermolinar/hush/sdk`) | UI-independent Go client: requests, replica subscriptions, history and durable submission receipts. |
| `tui/` (`github.com/javiermolinar/hush/tui`) | Bubble Tea v2 shell and public Go extension API. Uses the SDK, not the backend internals. |
| `app/` (`github.com/javiermolinar/hush/app`) | Reference binary `hush`: TUI with built-ins and headless print, a raw native relay and durable commands. Both use the same SDK; headless does not start the TUI. |

Pi Durable remains an unmodified Node library. Go owns the CLI, standard streams, status codes and TUI.

Use one MIT-licensed monorepo targeting macOS and Linux. A `go.work` joins the Go modules for development; each retains its own module path and semver tags (`sdk/vX.Y.Z`, `tui/vX.Y.Z`, `app/vX.Y.Z`). One npm workspace owns the TypeScript packages and the lockfile. Protocol changes and their conformance tests can land with consumers in one commit. The wire major is independent of package versions.

## Distribution and storage

A release is one tested combination: a Go app embedding the reference backend as bundled JavaScript and a WASM asset pack, executed on system Node.js **22.19.0 or later**. Pi Durable, Pi AI and transport dependencies are exact pins; the committed npm lockfile carries resolved versions and integrity hashes, including transitive packages. Use lockfile installs at build time. The application neither fetches nor updates dependencies at runtime.

Durable minor releases have changed the API, and databases written by newer versions are not meant to be opened by older versions. Updating Durable or Pi AI requires a tested new hush release. Do not treat the backend bundle as an updatable runtime plug-in or silently switch databases to an older Durable version.

## Outside v1

- Pi's CLI modes, TypeScript extensions and session files.
- Authentication beyond Unix-socket permissions and SSH socket forwarding.
- A web client, Chord's app kit, Windows, or bundling the Node executable.

The repository stays local until the user chooses to publish it. Module names do not imply that a remote exists.
