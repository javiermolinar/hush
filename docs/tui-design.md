# TUI design

## Layout

- Use the full width. Prompt, status, user and assistant text, folded tool summaries, and live activity start two cells in. The user bar extends left of that edge; only expanded tool children indent farther.
- Separate speakers and set off tool bursts with one blank row. Group results between pieces of assistant prose or before the next user turn. Count committed results and errors, with the timestamp at the right edge.
- Put live state and applicable hints on the rule above the input, even when scrolled back. Order hints by importance and drop them from the right rather than clipping input. Long input wraps up to a quarter of the screen, then scrolls to show its end.
- The main prompt intentionally folds pasted text above 10 lines or 1,000 Unicode code points. The marker shows the draft's paste number and either the line count (`[paste #1 +29 lines]`) or character count (`[paste #1 1001 chars]`); the line count takes precedence. Typed lookalikes are plain text. The cursor steps over a marker, selection includes it as a unit, and Backspace or Delete removes both marker and payload. Draft and history recall keep the folded view and its payload. Sending, copying the marker and opening the draft in an external editor use the full paste text after normal input sanitization. A changed external edit uses the same folding limits and can retain a marker for a shortened folded draft that still contains tabs. Pasted text or editor output that exceeds the 32 KiB expanded prompt limit is rejected without changing the draft.
- Below the input show directory, branch, model, current context, and spend when available. Cumulative tokens belong in usage details. A lone thread's title goes in status; the tab band starts at two threads.
- Wrap messages and detailed output. One-line summaries and chrome may ellipsize.
- Dock command panels such as `/plan` to the input rule: the rule is the bottom edge, the panel grows upward, and the prompt stays put. Empty space belongs above the panel. Keep live state and the changes count on the rule. Inline completions and existing under-prompt sheets still open below the prompt. Floating dialogs cover the transcript, never the footer. The top overlay owns all keys and clicks; `esc` closes only it.

- `/plan` opens a docked workspace list. Show status and verified counts, with closed plans grouped after open ones. Enter opens the focused plan; Enter in its task picker admits the run and returns to the initiating thread to show task progress and verification. `h` opens run history, `v` opens advisory review for the displayed revision, and `c` closes or reopens the focused plan. Esc backs out without changing lifecycle.
- An unsaved plan preview appears in the transcript. Click its title or use `ctrl+x i v` to inspect the draft. The current preview has a **Save to /plan** action (`ctrl+x i w`); `ok save it` confirms the same preview. Saving never selects or runs a task.

## Colors and contrast

- Use the terminal palette by default, with semantic ANSI colors and faint secondary text. A chosen [theme](../README.md#tui-themes), built-in or client-local JSON, can override foreground roles with RGB. Built-in presets carry dark and light sets, picked from the reported background, because the terminal keeps its own background. Fall back to terminal roles without truecolor, a reported background, or sufficient contrast, and say in the theme picker which roles fell back and why. Derive backgrounds from OSC 11 (retry if unanswered), never hardcode them. `auto` disables fills in Rex; the user can save `on` or `off` from the theme picker, and `--fills` overrides it for one launch. With fills off, don't paint or blend toward a background. Themes must not change layout.
- Soft fills mark the tab band, inline code, expanded tool cards, and key caps. Step OKLab lightness by −0.03 on light or +0.045 on dark, with 1.8 steps for caps. Omit soft fills when contrast is below 0.015, the background is unknown, or truecolor is unavailable. Without soft fills use emphasis for inline code and plain text for keys.
- Strong fills mark state only: active tab blue 0.26, parked tab amber 0.20, diff lines green/red 0.20, selected row blue 0.15. Without a known background, use foreground color alone.
- Blue foreground marks live activity and the user bar, not links, tab titles, or input labels. Agent prose uses the terminal foreground. Code uses ANSI magenta for keywords, cyan for strings and numbers, and faint comments; red/green are reserved for diffs.
- Folded activity, quotes, and tables are unboxed. Expanded results use a tinted card, failures first. Active sleep is amber on the input rule. Code block rules are faint and sized to the code. No `▕ ▏` outlines.

## Keys and mouse

- Give every action a `ctrl+x` leader binding and prefer it in hints when the direct chord is intercepted. Avoid `ctrl+k`, `ctrl+z`, `alt+[`, and Rex/Zellij defaults (`ctrl+p/t/n/g/h/s/b` and `alt` with arrows or `h/j/k/l`). Keep `ctrl+o` for Pi-style expand despite Zellij's binding. Use `ctrl+[` only with keyboard disambiguation; otherwise show `ctrl+x [`.
- A second `esc` within 2 s stops a run and returns queued messages to the prompt. The fork picker uses `esc esc` within 450 ms only when idle with an empty prompt.
- Under mouse capture, click to switch tabs, open `+N` or a link, expand activity, or copy code from the rule above it. Middle-click closes a tab, drag selects and copies via OSC 52, and the wheel scrolls. Underlined links respond to hover, and folded activity uses a muted disclosure marker. Code copy shows `✓ copied` on the rule, with a brief green sweep when motion is enabled.
- Live effects tick at 20 fps only while visible. Reduced motion keeps effects still and refreshes elapsed labels once per second.

## Links

- Render URLs, `@` references, PR numbers, and tool-call paths as OSC 8 links. Use dotted underlines for paths and solid for web links. Exclude trailing punctuation; `@` and `#` start references only at a word boundary.
- In answers, link inline code that names a workspace file. Only turn Markdown links into hyperlinks for `http`, `https`, `mailto`, or workspace paths; leave other schemes as text.
- The TUI opens links itself under mouse capture. Open URLs in the browser, files in `$VISUAL`, `$EDITOR`, or the system default, at the line when known.
