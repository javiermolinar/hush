# TUI implementation notes

Read [Hush UI style](ui-style.md) for the style contract. These notes describe existing behavior that must be preserved until its migration is approved. See [the audit](ui-audit.md#behavior-conflicts) for known gaps.

## Layout

- Use the full width. Prompt, status, user and assistant text, folded tool summaries, and live activity start two cells in. The user bar extends left of that edge; only expanded tool children indent farther.
- Separate speakers and set off tool bursts with one blank row. Group results between pieces of assistant prose or before the next user turn. Count committed results and errors, with the timestamp at the right edge.
- Put live state and applicable hints on the rule above the input, even when scrolled back. Order hints by importance and drop them from the right rather than clipping input. Long input wraps up to a quarter of the screen, then scrolls to show its end.
- The main prompt intentionally folds pasted text above 10 lines or 1,000 Unicode code points. The marker shows the draft's paste number and either the line count (`[paste #1 +29 lines]`) or character count (`[paste #1 1001 chars]`); the line count takes precedence. Typed lookalikes are plain text. The cursor steps over a marker, selection includes it as a unit, and Backspace or Delete removes both marker and payload. Draft and history recall keep the folded view and its payload. Sending, copying the marker and opening the draft in an external editor use the full paste text after normal input sanitization. A changed external edit uses the same folding limits and can retain a marker for a shortened folded draft that still contains tabs. Pasted text or editor output that exceeds the 32 KiB expanded prompt limit is rejected without changing the draft.
- The live rule shows model and thinking effort alongside run state and the changes count. Below the prompt, show the workspace or repo and available context/spend; the next row shows open threads, including a lone active thread, with status and available branches. Cumulative tokens belong in usage details.
- Wrap messages and detailed output. One-line summaries and chrome may ellipsize.
- Panels and completions share a full-width dock above the input rule. Only the focused panel is drawn. Esc closes that layer; services that replace a sheet on acceptance still do so.

- `/plan` opens a docked list of plans across the connected store. Show task counts, keeping reported results distinct from verification, and group closed plans after open ones. Show the current workspace first in each group and label read-only plans from other workspaces. Browsing at startup doesn't create a thread. Enter opens the focused plan, Space selects tasks, and Enter runs the selection as one normal turn in the active thread. Neither browsing nor Run here pastes the plan into the composer. `w` offers a new worktree for the selected tasks, with separate trust before execution. `s` opts into scoped verification when offered. `h` opens run history, `v` opens an independent advisory review, and `c` closes or reopens the focused plan. Esc backs out without changing lifecycle.
- An unsaved plan preview appears in the transcript. Click its title or use `ctrl+x i v` to inspect the draft. The current preview has a **Save to /plan** action (`ctrl+x i w`); `ok save it` confirms the same preview. Saving never selects or runs a task.

## Colors and contrast

- The terminal owns its background. Hush detects the background with OSC 11 and derives foreground roles and panel surfaces. When measured roles are below 4.5:1, correct them on truecolor terminals and show a startup notice. Unknown terminal colors cannot be measured.
- [Theme version 2](../README.md#tui-themes) uses four base inks plus surface and base; existing version 1 files and `--fills` still work. With fills off, don't paint backgrounds. Keep surface changes independent of geometry.
- Terminal image pixels are not UI chrome. Custom extension models can still supply sanitized foreground and emphasis within their body.

## Keys and mouse

- Give every action a `ctrl+x` leader binding and prefer it in hints when the direct chord is intercepted. Avoid `ctrl+k`, `ctrl+z`, `alt+[`, and Rex/Zellij defaults (`ctrl+p/t/n/g/h/s/b` and `alt` with arrows or `h/j/k/l`). Keep `ctrl+o` for Pi-style expand despite Zellij's binding. Use `ctrl+[` only with keyboard disambiguation; otherwise show `ctrl+x [`.
- A second `esc` within 2 s stops a run and returns queued messages to the prompt. The fork picker uses `esc esc` within 450 ms only when idle with an empty prompt.
- Under mouse capture, click to switch tabs, open `+N` or a link, expand activity, or copy code from the rule above it. Middle-click closes a tab, drag selects and copies via OSC 52, and the wheel scrolls. Underlined links respond to hover, and folded activity uses a muted disclosure marker. Code copy shows `✓ copied` on the rule, with a brief green sweep when motion is enabled.
- Live effects tick at 20 fps only while visible. Reduced motion keeps effects still and refreshes elapsed labels once per second.

## Links

- Render URLs, `@` references, PR numbers, and tool-call paths as OSC 8 links. Use dotted underlines for paths and solid for web links. Exclude trailing punctuation; `@` and `#` start references only at a word boundary.
- In answers, link inline code that names a workspace file. Only turn Markdown links into hyperlinks for `http`, `https`, `mailto`, or workspace paths; leave other schemes as text.
- The TUI opens links itself under mouse capture. Open URLs in the browser, files in `$VISUAL`, `$EDITOR`, or the system default, at the line when known.
