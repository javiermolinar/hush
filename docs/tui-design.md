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

- `/plan` opens a docked list of plans across the connected store. Put the thread’s active run first, even while it’s closing. Group the remaining closed plans after open ones. Show the current workspace first in each group, label read-only plans from other workspaces, and keep reported results distinct from verification. Browsing at startup doesn't create a thread. Enter opens the focused plan, Space selects tasks, and Enter runs the selection as one normal turn in the active thread. Neither browsing nor Run here pastes the plan into the composer. `w` offers a new worktree for the selected tasks, with separate trust before execution. `s` opts into scoped verification when offered. `h` opens run history, `v` opens an independent advisory review, and `c` closes or reopens the focused plan. Esc backs out without changing lifecycle.
- A plan run has a strip directly above the live rule. It shows a mark per step up to 20, then status counts. Run here tasks share one turn, so peek shows that turn’s activity once and distinguishes reported results from verification. On an empty prompt, ↑ peeks, Enter opens the plan from peek or an unread failure, and Esc backs out without arming Stop. Typing dismisses peek and goes straight into the prompt. `ctrl+x i k` or a click opens the current run without using the draft. Verification flashes briefly; failure stays highlighted until inspected.
- The `Subagents` badge on the input rule (`ctrl+x i g`) opens the latest delegated turn in the bottom dock. Click a row or press Enter to follow its public log. Esc returns to the roster. The usage footer adds recorded subagent tokens and cost to the parent’s spend; the `ctx` meter still shows only the parent’s context occupancy.
- An unsaved plan preview appears in the transcript. Click its title or use `ctrl+x i v` to inspect the draft. The current preview has a **Save to /plan** action (`ctrl+x i w`); `ok save it` confirms the same preview. Saving never selects or runs a task.

## Colors and contrast

- Follow the fixed [typography scale](ui-style.md#typography). Themes change foreground colors, not type levels. The terminal owns the background and the default foreground. A chosen [theme](../README.md#tui-themes) can override foreground roles with RGB; the reported background picks the built-in light or dark variant. The theme picker identifies overrides that fell back. [Theme version 2](../README.md#tui-themes) uses four base inks plus surface and base; existing version 1 files and `--fills` still work.
- Quiet uses regular text at 4.5:1 or better against the OSC 11 background when measurable. Derive it from that background and the foreground, or use the existing `muted` theme color if it meets contrast and does not outshine Normal. Without a reliable dim color, use the regular terminal foreground, not SGR faint. This also applies when fills are off.
- Soft fills mark the tab band, inline code, expanded tool cards, and key caps. Use a distinct state fill only for the focused row or active tab, and omit fills without sufficient contrast, truecolor, or a reported background. `auto` disables fills in Rex; the user can save `on` or `off` in the theme picker, and `--fills` overrides it for one launch. Without a fill, inline code stays regular and the cursor mark still identifies focus.
- Agent prose and user messages use the terminal foreground. Strong labels stand apart with a cursor or action mark; inline Markdown emphasis also gets higher-contrast ink when the terminal palette supports it. Only the pinned activity line uses Live. Code comments and frame rules are quiet. Diff removals use the minus sign, not an error color. Folded tool summaries stay on one quiet row with only the failure count in the error color.

Outside shared text roles, `theme_intro.go` emits RGB for animated gradients and `image_pixels.go` emits RGB and Kitty placeholder attributes directly. Custom extension dialogs retain sanitized, extension-supplied foreground and emphasis.

## Keys and mouse

- Give every action a `ctrl+x` leader binding and prefer it in hints when the direct chord is intercepted. Avoid `ctrl+k`, `ctrl+z`, `alt+[`, and Rex/Zellij defaults (`ctrl+p/t/n/g/h/s/b` and `alt` with arrows or `h/j/k/l`). Keep `ctrl+o` for Pi-style expand despite Zellij's binding. Use `ctrl+[` only with keyboard disambiguation; otherwise show `ctrl+x [`.
- With an empty prompt, `tab` toggles the latest visible thought. Click a thought summary to toggle it directly, or use `ctrl+o` to toggle all details. Tab still completes commands and paths.
- A second `esc` within 2 s stops a run and returns queued messages to the prompt. The fork picker uses `esc esc` within 450 ms only when idle with an empty prompt.
- Under mouse capture, click to switch tabs, open `+N` or a link, expand activity, or copy code from the rule above it. Middle-click closes a tab, drag selects and copies via OSC 52, and the wheel scrolls. Underlined links respond to hover, and folded activity uses a muted disclosure marker. Code copy shows `✓ copied` on the rule, with a brief green sweep when motion is enabled.
- Live effects tick at 20 fps only while visible. Reduced motion keeps effects still and refreshes elapsed labels once per second.

## Links

- Render URLs, `@` references, PR numbers, and tool-call paths as OSC 8 links. Use dotted underlines for paths and solid for web links. Exclude trailing punctuation; `@` and `#` start references only at a word boundary.
- In answers, link inline code that names a workspace file. Only turn Markdown links into hyperlinks for `http`, `https`, `mailto`, or workspace paths; leave other schemes as text.
- The TUI opens links itself under mouse capture. Open URLs in the browser, files in `$VISUAL`, `$EDITOR`, or the system default, at the line when known.
