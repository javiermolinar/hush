# TUI rendering and performance

The TUI uses Bubble Tea v2, Lip Gloss v2, and x/ansi. See [TUI design](tui-design.md) for layout and interaction rules and [Contributing](../CONTRIBUTING.md#visual-changes) for visual checks.

## Model and effects

- Use one top-level model. Components are plain structs, not nested `Update` loops.
- Only `Update` changes state. No I/O or sleeps in `Update`: do side effects in `tea.Cmd` and return messages. Commands and goroutines never touch the model.
- `View` only reads state, except for memoization behind pointers (render caches and the last frame for hit-testing). Animate with `tea.Tick`.

## Frame cost

`View` runs after every message, even with draws capped at 120 fps. `TestFrameBudget` enforces p99 `Update` + `View` under 2.08 ms per message. Its contract cases hold keystrokes and streaming patches to 1 ms: typing into 1, 4 and 16 KB drafts, streaming through a real `sdk.Replica` (`replica-streaming`) and typing in an image thread. `linked-streaming` holds `View` to 0.5 ms per patch as a linked answer grows to 64 KB. A case that misses today names its fixing step in `frameTargets`, and reports rather than fails until that step lands. Profile before changing the budget (`go test ./tui -run '^$' -bench Frame -cpuprofile /tmp/cpu.prof`). Add new hot paths and lists to `perf_test.go`.

- Render the transcript lazily from newest block back, stopping at the viewport (`cache.go`). Never compute total heights or re-wrap the whole list per frame.
- Cache blocks by all render inputs, including width, palette revision, expansion, image mode, and speaker breaks. Invalidate a burst's summary when a result arrives. Only live blocks render each frame.
- Composite overlays over just the rows they cover. In `motionFilter`, drop no-op input by effect, not by time, so the last position isn't lost.
- Use goldmark to parse Markdown once per text; only layout depends on width. Lay out just the streaming suffix after the last safe cut, or the current line in an open code block. `TestStreamingMatchesFullRender` covers equivalence. hush lays out Markdown and highlights code itself (`syntax.go`).

## Frame invariants

- Use exactly one line per terminal row, each at most the terminal width. Keep the footer in bounds. Below 40×12, show “terminal too small.” The same state must render the same frame.
- Render control characters in model and tool text as Control Pictures (including ESC as `␛`). Escape sequences in the frame come from the renderer, not untrusted text.
- Measure, truncate, cut, wrap, and strip styled strings with `x/ansi`, not with byte or rune counts. Style segments independently and concatenate them; avoid restyling across resets. Use SGR and OSC sequences for hot-path segments, not a Lip Gloss style per segment.
- Hit-test what was drawn: use `tabLayout` for tabs and OSC 8 links in the last frame for links. Don't duplicate layout math. Add new states, sheets, and overlays to `TestFrameGeometry`.
