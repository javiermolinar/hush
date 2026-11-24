# ADR 0001: Workspace, v1 scope and releases

Status: Accepted

The initial freeze is the [native v1 core](../../protocol/v1/). The [Pi 1.6 ordered event capability](../../protocol/v1/pi-1.6/pi-events.md) is additive and opt-in. Keep future profile services unadvertised until their own conformance checks pass.

## Decision

hush is a Go TUI and headless CLI backed by Pi Durable. The default backend uses the Hush-owned profile and does not load Pi settings, credentials, or trust. The opt-in **Pi 1.6 profile** uses Pi's agent directory (`~/.pi/agent` by default) and loads Pi resources under Pi's trust rules. Compatibility is the pinned profile under `protocol/v1/`, not drop-in parity. The prototype in `pi-durable-go-test` stays separate.

| Path | Responsibility |
|---|---|
| `protocol/v1/` | Language-neutral JSONL wire contract, schemas, compatibility profile and fixtures. The reference backend reuses the tested profile adapters; the core transport consumes only the schemas. |
| `server/` (`@hush/server`) | Portable TypeScript transport over the public Durable Harness API. The core takes a Harness. `hello` advertises optional, explicitly injected host services independently. No dependency on the Pi backend or a TUI. |
| `backend/` (`@hush/backend`) | Reference host: assembles the Harness, configuration, credentials, resources, tools and storage; supplies host services and starts the portable transport. |
| `sdk/` (`github.com/javiermolinar/hush/sdk`) | UI-independent Go client: requests, replica subscriptions, history and ordered live events. |
| `tui/` (`github.com/javiermolinar/hush/tui`) | Bubble Tea v2 shell and public Go extension API. Uses the SDK, not the backend internals. |
| `app/` (`github.com/javiermolinar/hush/app`) | Reference binary `hush`: TUI with built-ins and headless print, JSON, and RPC plus durable commands. Both use the same SDK; headless does not start the TUI. |

Pi Durable remains an unmodified Node library. The backend captures provider, tool and scheduler boundaries in that process, where the public callbacks run; a Go client cannot recover every boundary from a folded state watch. The adapter forwards ordered evidence. Go owns the CLI, standard streams, status codes and TUI.

Use one MIT-licensed monorepo targeting macOS and Linux. A `go.work` joins the Go modules for development; each retains its own module path and semver tags (`sdk/vX.Y.Z`, `tui/vX.Y.Z`, `app/vX.Y.Z`). One npm workspace owns the TypeScript packages and the lockfile. Protocol changes and their conformance tests can land with consumers in one commit. The wire major is independent of package versions and of the Pi compatibility profile.

## Distribution and storage

A release is one tested combination: a Go app embedding the reference backend as one bundled JavaScript file, executed on system Node.js **22.19.0 or later**. Direct Pi, Pi Durable, pi-ai, pi-mcp and transport dependencies are exact pins; the committed npm lockfile carries resolved versions and integrity hashes, including transitive packages. Use lockfile installs at build time. The application neither fetches nor updates dependencies at runtime. A release declares both its v1 wire version and the exact Pi 1.6 compatibility profile it passed.

Durable minor releases have changed the API, and databases written by newer versions are not meant to be opened by older versions. Updating Durable or Pi requires a tested new hush release. Do not treat the backend bundle as an updatable runtime plug-in or silently switch databases to an older Durable version.

## Outside v1

- Executing Pi's TypeScript extensions or importing Pi session files through the native core. The opt-in profile loads Pi configuration and resources, not extension code.
- Authentication beyond Unix-socket permissions and SSH socket forwarding.
- A web client, Chord's app kit, Windows, or bundling the Node executable.

The repository stays local until the user chooses to publish it. Module names do not imply that a remote exists.
