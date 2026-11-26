# Hush UI style

This is the style contract for the production TUI. See [TUI rendering](tui-rendering.md) for frame geometry, terminal safety, and performance. [The UI audit](ui-audit.md) records current behavior and migration gaps. Do not change existing bindings, workflow, or runtime to resolve a migration gap without separate approval.

## Principles

- Calm by default. Show additional controls or metadata only when the user reaches for them.
- Use one left edge. Give user messages an accent bar; render agent text as plain prose.
- Yolo by default. Irreversible steps park and are released by typing in chat. No approval menus and no plan mode. A plan is a contract the agent fulfils; opening it changes nothing.
- Keyboard first. Give every action a key path and let the mouse reach the same actions.

## Layout

- Dock every panel, menu, and picker to the bottom. Its bottom edge sits on the input rule, it grows upward, and the prompt stays in place. No centered or floating modals, and no content hanging below the logo with a gap before the prompt.
- Grow to fit up to about 70% of the terminal height, then scroll internally. `f` zooms to full screen.
- Separate four zones with blank rows: list or body; consequence line and key line; live rule; prompt. Below about 24 rows, drop blank rows before content.
- Show live state on the rule above the prompt: state on the left (`● editing x · 12s`), changes count on the right (`± 4 files +87 -12`), and the model and thinking effort on the rule too.
- Use two navigation bars in order: repo bar, then thread bar. Thread tabs show a state icon and the worktree branch (`⎇ name`).

## Shared menu shell

Use one component for menus, pickers, and panels. Extend that component for variants; don't fork it.

1. **Header rule:** title and scope or revision on the left, progress or count on the right.
2. **Body:** list on the left, details for the focused row on the right. On narrow terminals stack details below the list. `tab` moves focus to details.
3. **Rows:** one line each, with a `›` cursor, selection only for multi-select, a status glyph, a readable title (never an internal ID), and one short state column. Put long reasons in details, not clipped in the list. Cursor, selection, and status are distinct and never share a glyph.
4. **Detail:** start with the title and a short description. Follow with conditions (needs, after, accept), thinking level (requested → effective), evidence, and findings, as applicable.
5. **Consequence line:** start with the primary action in Strong and say what Enter will do now: `↵ Run 2 @ r1  Save a profile → Restore saved profiles · 2 unselected`. If Enter can't work, say why and change nothing.
6. **Key line:** one line using the same vocabulary: `↑↓ move`, `space select`, `↵ primary action`, `/ filter`, `tab details`, `f full`, `esc back`. Show applicable actions only. Esc goes back exactly one level.

Use `✓` for verified, `◐` for running, `○` for pending, `⏸` for parked or blocked, and `✗` for failed. Amber means live or needs attention; red means failed.

Settings is the only variant without a detail pane. Cycle values with `←→` and apply immediately; `u` undoes; `tab` switches between this thread and the default. Show a one-line description for the focused setting.

## Leader menu

`ctrl+space` starts a which-key menu. Draw it after about 250 ms so a learned chord can run before it appears. Roots: `t` threads, `r` repos, `c` checkpoint, `w` worktree, `d` diff, `s` skills, `p` plan, `o` options, `/` search. `⌫` goes up; Esc closes. When a slash command or mouse action invokes a leader action, show its chord once (`next time: ctrl+space c r`). `/` in the leader opens semantic search over threads, actions, and files.

## Worktrees and checkpoints

The agent decides when to create a worktree by tool call. The UI shows a collapsed chat line, the branch on the thread tab, and the changes count. Land and discard are parked steps. Closing a thread hides the tab only; the thread and its worktree are kept.

## Typography

Use only these four levels:

| Level | Treatment | Use |
| --- | --- | --- |
| Strong | Bold, full brightness | Plan titles, focused row titles, Markdown emphasis, primary actions |
| Normal | Regular, full brightness | Agent answers and user messages |
| Quiet | Regular, dimmed | Tool summaries, reasoning, timestamps, key hints, secondary metadata |
| Live | Italic, accent color | One live line on screen |

Underline only openable links and paths. Reserve reverse video or a shaded row for the cursor. Never bold whole rows or rely on bold alone to distinguish states. Reasoning is quiet, never bold, and collapsed by default (`Thought · 46s · 12 steps ▸`).

## Themes

A theme is semantic roles and a surface mode, not a bag of colors. Its base colors are `accent`, `ok`, `warn`, and `err`. The surface is `transparent`, `tinted`, or `solid`. The base is `auto` (detect the terminal background with OSC 11), `dark`, or `light`.

Derive foreground levels, tints, diff colors, and dimming from the roles and detected background. In transparent mode, paint no backgrounds except for the cursor row; use the bar, rules, and brightness for structure. At startup, raise any role below about 4.5:1 against the detected background and report the adjustment once. Components use roles only: no raw hex colors or hard-coded ANSI attributes.

## Copy

Use sentence case. No trailing period on labels or headings. Use “your,” never “my.” Say what happens without “successfully,” “please,” or exclamation marks. Errors say what happened and what to do next.

## Open questions

- The guide doesn't define keyboard selection in prose or terminal image pixels. Preserve the existing copy-selection and image behavior; neither defines chrome styling.
- Extension custom models can supply sanitized foreground and emphasis; keep the extension API intact and apply the shared chrome around the child view.
- See the audit migration column for where the target conflicts with current behavior. These entries are not precedent for new UI.
