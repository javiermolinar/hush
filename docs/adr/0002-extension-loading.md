# ADR 0002: Link Go extensions at build time

Status: Accepted for v1

Use explicitly registered Go packages compiled with hush. Unlike Pi's runtime TypeScript extension mechanism, adding or upgrading a Go extension in v1 requires a rebuild. A custom build can generate a main with imports and constructor calls; don't require import-time global registration. Built-ins and external packages use the same public API.

## Options

| Loader | Installation and isolation | UI consequences |
| --- | --- | --- |
| Compiled registration, like [xcaddy](https://github.com/caddyserver/xcaddy) | Compile the chosen packages into one binary. This is trusted code in the same process, not a sandbox. | Native Go types and ordinary Bubble Tea models for host-mediated custom overlays. No wire encoding in the renderer. |
| [Yaegi](https://github.com/traefik/yaegi), as used for [Traefik Go plugins](https://plugins.traefik.io/install) | Load source through an embedded interpreter. Compiled host APIs need exported bindings; source/runtime compatibility is a second support surface. In-process, not a boundary for arbitrary code. | Possible in-process callbacks; passing arbitrary native Bubble Tea v2 types through interpreted/compiled boundaries would need validation. Not benchmarked. |
| Child process, JSON-RPC on stdio | Build the extension separately and start it on demand. A child crash need not crash the TUI, but running as the user is not capability isolation. [HashiCorp go-plugin](https://github.com/hashicorp/go-plugin) is a process-lifecycle precedent; it uses net/rpc or gRPC, not our stdio protocol. | Exchange data (lines, rows, widgets, actions), not Go model objects. Need handshake, framing, cancellation and backpressure. Measure synchronous calls here; don't put them on the frame path. |
| [WASM with wazero](https://wazero.io/docs/) | Load a compiled module at runtime. The guest has separate memory; the host controls imported capabilities and must bound resource use. | Explicit data ABI and host-rendered UI. Cannot pass a live Go model or Go pointers. Not benchmarked. |

Compiled registration and a child process cover the two strongest alternatives: native UI fidelity versus install-without-rebuild. Yaegi adds runtime compatibility risk while retaining in-process trust, and WASM needs a separate declarative UI ABI before it can show the same customization.

## Measurements

A local prototype linked the same example package into the host and a test-only worker. Both implemented the same manifest, `refactor · inbox 2` footer, and `/inbox` command. The experiment checked results and command rejection across both loaders. The worker was a minimal, single-in-flight JSON-RPC subset, not a shippable plug-in manager.

Apple M3 Pro, macOS arm64, Go 1.27.4. Five 300 ms `go test` measurements per case; table gives the median and range of run means.

| Operation | Linked | Stdio worker |
| --- | ---: | ---: |
| Load: constructor and manifest validation vs process spawn through manifest handshake | 10.0 ns (9.91–10.28 ns) | 4.51 ms (4.15–4.97 ms) |
| One footer segment per frame | 26.2 ns (26.1–26.8 ns), 1 alloc | 12.8 µs (12.7–12.9 µs), 9 allocs |

The worker build, parent startup, shutdown, terminal paint and styling are excluded. Worker load starts a fresh process with the binary already built; filesystem caches were warm. These are single-segment costs, not p99 full-frame results. A single worker round trip is small relative to the 2.08 ms per-message frame budget when the worker is responsive, but has no bound suitable for `View`. Rendering more fragments or waiting on a busy worker would consume UI time. Compiled registration also supports native overlays without a second rendering protocol.

## Extension API constraints

- Version a public Go package, not a runtime ABI for shared libraries. Constructor registration must be explicit; compile against the chosen hush version. Native extensions have process privileges, so include only trusted code.
- Keep common hooks transportable: immutable context snapshots, stable IDs, declarative status segments/widgets/menu items, and typed actions. Never expose private TUI fields or mutable store references. Built-ins are not exceptions.
- Render callbacks must be pure and bounded; commands and other side effects return work to the host to run away from `Update` and `View`. Custom Bubble Tea overlays are an optional in-process capability, not a requirement of every extension.
- A future child or WASM loader can use the declarative subset. Send snapshots/actions asynchronously, cache contributions, and render the last available contribution. Put no synchronous RPC or guest I/O in the render loop. Negotiate future wire capabilities independently of the Go API version.
