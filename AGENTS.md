# AGENTS.md

hush is a Go TUI and headless CLI backed by Pi Durable. Treat Pi and Crush as references, not code sources.

Build and test per [CONTRIBUTING.md](CONTRIBUTING.md). Read only the guides relevant to your change:
- [TUI model, rendering, and performance](docs/tui-rendering.md). Keep `Update` nonblocking; never send raw model or tool text to the terminal.
- [TUI layout, colors, keys, and links](docs/tui-design.md). Keep the default palette terminal-derived. Themes the user picks, built-in presets or client-local JSON, may override foreground roles with RGB; they never own the terminal background.
- [TUI extension API](docs/tui-extensions.md).
- [Headless I/O](protocol/v1/README.md#headless-io).
- [Backend and pinned Durable references](backend/README.md#pi-durable-references).
