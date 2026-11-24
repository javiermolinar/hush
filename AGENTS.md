# AGENTS.md

hush is a Go TUI client and headless CLI (Bubble Tea v2, Lip Gloss v2, x/ansi) for a Pi Durable backend. These are
constraints for `tui/` and `app/`. Tests enforce most of them: a change that breaks one is wrong until shown otherwise.
The optional local design mock and its sign-off notes in `tui/mock/` are not part of the published workspace.

## References

Links are pinned to the versions the project uses (Pi and Pi Durable 1.6.0). When a pin changes, update its links in
the same change.

- Pi Durable spec, the authority on its behavior:
  https://github.com/earendil-works/pi/blob/v1.6.0/packages/durable/docs/spec.md
- Pi Durable README, concepts and API overview:
  https://github.com/earendil-works/pi/blob/v1.6.0/packages/durable/README.md
- Pi Durable changelog, for API changes between versions:
  https://github.com/earendil-works/pi/blob/v1.6.0/packages/durable/CHANGELOG.md
- Offline copies of that README and changelog, matching the installed version:
  `node_modules/@earendil-works/pi-durable/` after `npm ci`.
- Pi's headless modes, which this CLI matches: `cli-integration.md`, `json.md`, `rpc.md`, and `rpc-commands.md` in
  https://github.com/earendil-works/pi/tree/v1.6.0/packages/coding-agent/docs

## Check every change

Build and test the npm and Go workspaces as described in [`CONTRIBUTING.md`](CONTRIBUTING.md). The full TUI suite
checks frame geometry, lazy render equal to full render, hit-testing, flows, golden frames, and the 120 fps frame
budget. Use `-race -short` for race runs; short runs skip the timing checks.

After a visual change, inspect light and dark captures. Regenerate golden frames (`go test -run Golden -update`)
only for an intended change. If you have the local mock, run its tools separately from the product workspace,
for example `cd tui/mock && GOWORK=off ./shots.sh`.

## Bubble Tea

- One top-level model. Components are plain structs with methods the model calls, as in Crush; no nested `Update`s.
- `Update` never blocks: no file, network, or process I/O, and no sleeps. Side effects run in a `tea.Cmd` and come
  back as messages, as pasted image paths and image decoding do.
- Commands never touch the model; only `Update` changes state.
- `View` reads the model. The only state it writes is memoization behind pointers: render caches, and the last frame
  for hit-testing.
- No goroutine shares the model. Animate with `tea.Tick`.
- Handle styled strings with `x/ansi` (`StringWidth`, `Truncate`, `Cut`, `Wrap`, `Strip`), never by bytes or runes.
- Style each segment on its own and concatenate the results. Restyling an already styled string loses its colors at
  every reset inside it. Draw segments with `x/ansi` SGR and hyperlink sequences; a lipgloss style per segment costs
  several times as much.

## 120 fps

Bubble Tea calls `View` after every message, not once per frame, and draws at most 120 frames a second with
`tea.WithFPS(120)`, its maximum (the default is 60). Several messages can arrive within one 8.33 ms frame.

- Budget: p99 `Update`+`View` under 2.08 ms per message, a quarter of a frame. `TestFrameBudget` enforces it.
  Profile before changing the budget (`go test -run '^$' -bench Frame -cpuprofile /tmp/cpu.prof`).
- A frame costs what the screen shows, not what the data holds. The transcript renders lazily from the newest block
  back, only as far as the screen needs (`cache.go`).
- Cache every rendered block, keyed by everything its output depends on: width, palette revision, expanded or not,
  image mode, and speaker breaks. Adding a result invalidates its burst's cached summary.
- Only live blocks (a running call, a streaming answer) render every frame.
- Never render or measure the whole list per frame: no total heights, no full re-wrap.
- Composite an overlay over the rows it covers, not over the whole screen.
- Drop input that changes nothing before `Update` (`motionFilter`). Filter by effect, not by time, so the last
  position is never lost.
- A new hot path or list gets a scenario in `perf_test.go`.
- Parse Markdown once per text; only its layout depends on the width. A streaming answer lays out only what follows
  its last safe cut, and an open code block only the line being written. `TestStreamingMatchesFullRender` checks
  that the stream draws what a full render draws.

## Every frame

- Exactly one line per terminal row: more lines scroll the terminal, fewer lift the footer off the bottom.
- No line wider than the terminal: a wider line wraps and pushes every row below it.
- No control characters other than escape sequences. Text from the model or a tool never reaches the terminal
  raw: its control characters, ESC among them, show as Control Pictures (`␛`).
- The same state renders the same frame; golden tests compare them.
- What you type is never cut. Hints drop from the right first; long input wraps, up to a quarter of the screen, and
  shows its end.
- The footer never pushes the frame past the screen.
- Below 40×12, show "terminal too small" instead of a broken layout.
- Mouse hit-testing comes from the code that draws: `tabLayout` for tabs, and the OSC 8 links in the last frame for
  links. Never keep a second copy of the layout math.
- A new state, sheet, or overlay goes into `TestFrameGeometry`.

## Colors and contrast

- Every color comes from the terminal: ANSI red, green, yellow, and blue carry meaning, faint text carries secondary
  information, and the terminal's own background, queried with OSC 11 and asked again when it does not answer, is the
  base for every background. Never hardcode a color.
- Soft backgrounds are one OKLab lightness step from the terminal's background, −0.03 on light themes and +0.045 on
  dark ones, and 1.8 steps for key caps. They mark your messages, the tab band, and key caps.
- When that step is indistinguishable (contrast below 0.015), the background is unknown, or there is no truecolor,
  drop the soft backgrounds: your messages get a left bar and keys become plain text. The layout never changes with
  the theme.
- A reported background is not always what shows: translucent or blended panes (Rex) show the window behind them,
  where every fill derived from the report reads as a dark slab. Fills are a setting (`auto|on|off`, off in Rex);
  with fills off nothing paints a background or blends toward one, and the fallbacks above apply.
- Strong backgrounds mark state and nothing else, as blends with the terminal's background: the active tab (blue
  0.26), a parked tab (amber 0.20), diff lines (green and red 0.20), and the selected row (blue 0.15). Without a known
  background, show state with color alone.
- Check both themes in the shots (Catppuccin Latte and Mocha) before calling a color change done.
- No `▕ ▏` bars as outlines: they look broken in Rex. Keys are key caps on a soft background.
- Activity is unboxed; an active sleep is reported on the live line in amber. Quotes and tables are unboxed.
  Code blocks have faint rules above and below, sized to the code.
- Code takes ANSI magenta (keywords) and cyan (strings, numbers, inline code), which mark nothing else; comments are
  faint. Red and green appear in code only on a diff's lines.

## Layout

- The UI uses the full terminal width.
- In production, prompt, status, user and assistant text, folded tool summaries, and live activity start two cells in.
  User tint or a fallback bar extends left of that edge. Only expanded tool children indent.
- One blank transcript row separates speakers. Tool results between pieces of assistant prose (or a new user
  turn) form one summary. Count committed results and errors; put the timestamp at the right edge.
- The live state is one separate line above the prompt, even when scrolled back. Merge thread name into the
  status line with a single open thread; show the tab bar only when at least two are open.
- Wrap messages and detailed output. One-line summaries and chrome may ellipsize.
- Hints show only the keys that apply now, most important first.
- Lists open under the prompt (slash commands, `@` files, the switcher, the fork picker, models). The only floating
  elements are the tab popover and the overlay stack (the command palette and its dialogs), drawn over the
  transcript, never over the footer. The top overlay gets every key and click; `esc` closes only it.

## Keys and mouse

- Avoid `ctrl+k` (delete to end of line), `ctrl+z` (suspend), and `alt+[` (it starts escape sequences). Avoid the
  keys multiplexers keep by default too: Zellij's and Rex's modal keymaps take `ctrl+p/t/n/g/h/s/b` (Zellij also
  `ctrl+o`, which hush keeps for Pi's expand), and `alt` with arrows and `h/j/k/l`. A key the terminal keeps never
  reaches hush, so every action has a leader form, and hints show the leader form where the terminal's keymap is
  known to take the chord (Rex: `rex keymap --json`).
- Offer `ctrl+[` only where the terminal reports key disambiguation (kitty keyboard protocol), and `ctrl+x [`
  elsewhere. Every action is reachable through the `ctrl+x` leader, since many macOS terminals do not send Option as
  Meta.
- Two presses of `esc` within 2 s stop a run, and queued messages come back to the prompt; `esc esc` within 450 ms
  opens the fork picker only when the thread is idle and the prompt is empty, as in Pi.
- The TUI captures the mouse, so it handles clicks itself: a tab opens, `+N` opens the switcher, a link opens, a
  middle click closes a tab, a click on an activity row expands it, a click on the line above a code block copies the
  block, and a drag selects and copies (OSC 52). The wheel scrolls.
- What a click does shows before and after the click. Links use the accent and answer hover; folded activity
  uses a muted disclosure marker rather than blue. Copy feedback shows where you clicked (`✓ copied`).
- Live effects run on their own 20 fps `tea.Tick`, only while one is on screen. Reduced motion keeps effects
  still and refreshes elapsed labels once a second.

## Headless

The binary also runs without the TUI, in Pi's print, JSON, and RPC modes, so other tools can drive it.

- Headless paths never start Bubble Tea or write to the terminal: no alternate screen, no mouse mode, no terminal
  queries.
- stdout carries only the requested output: the final answer in print mode, JSONL in the JSON and RPC modes, or a
  command's `--json` result. Everything else goes to stderr.
- Output formats and exit statuses match Pi's exactly, because existing tools parse them. Golden streams and Pi's own
  `RpcClient` test that.
- Headless never prompts. A run that needs an approval exits with its own status and stays parked for another client
  to decide.

## Links

- URLs, `@` references, PR numbers, and the paths in tool calls render as OSC 8 hyperlinks in the accent color.
- Trailing punctuation is not part of a link, and `@` or `#` starts a reference only at the start of a word.
- In answers, inline code that names a file in the thread's workspace links to it, and Markdown links become links
  only for `http`, `https`, `mailto`, and workspace paths. Any other scheme stays text.
- Under mouse capture the terminal never sees a plain click, so the TUI opens links: URLs in the browser, and files in
  `$VISUAL`, `$EDITOR`, or the system's default app, at the line when known.

## Code provenance

- Crush is FSL-1.1-MIT: read it for patterns, never copy its code. Crush and Pi are references, not sources.
- goldmark (MIT) parses Markdown; hush lays it out and tokenizes code itself (`syntax.go`). Not glamour, whose styles
  fix their colors, and not chroma, which is too slow to highlight a streaming block on every frame.
