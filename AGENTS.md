# AGENTS.md

hush is a Go TUI and headless CLI backed by Pi Durable. Treat Pi and Crush as references, not code sources.

Build and test per [CONTRIBUTING.md](CONTRIBUTING.md). Read only the guides relevant to your change:
- [TUI model, rendering, and performance](docs/tui-rendering.md). Keep `Update` nonblocking; never send raw model or tool text to the terminal.
- [Existing TUI interactions and terminal links](docs/tui-design.md). Use the style contract below for new or migrated UI; preserve existing key bindings and workflows unless the task explicitly changes them.
- [TUI extension API](docs/tui-extensions.md).
- [Headless I/O](protocol/v1/README.md#headless-io).
- [Backend and pinned Durable references](backend/README.md#pi-durable-references).

## Hush UI style

Read docs/ui-style.md before changing any UI.

- Bottom-docked panels only. Use the shared menu shell; never add a new modal.
- Use theme roles only, with no raw colors, and only Strong, Normal, Quiet, and Live type levels.
- Esc goes back one level. Give every new action a key path and a leader-menu entry.
- Use one line per list row; move long reasons into details.

Pre-merge checklist:

- [ ] The panel anchors to the input rule without moving the prompt.
- [ ] Header, list, detail (except settings), consequence line, and key line are present.
- [ ] Cursor, selection, and status are separate.
- [ ] It works in transparent, tinted, and solid surfaces.
- [ ] It works at 80×24.
- [ ] It has a snapshot test.
