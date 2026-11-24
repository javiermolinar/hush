# TUI startup report

The interactive mode of `hush` and the attach example enable [`tui.IntroOptions`](../tui/intro.go). The report is local scrollback, not a Durable entry or a modal. The initial view starts connection work without waiting. By default the launcher starts a missing local runtime; an explicit socket stays attach-only. A startup failure shows its diagnostic in the report while connection attempts continue. Before the first submit, available observations update the report. Submission freezes the intro text in scrollback. A new thread created on first submit inherits the frozen block. The footer keeps following the active thread independently.

## Startup data and skills

The reference backend's optional [intro capability](../protocol/v1/intro.md) reports uptime, session activity, loaded skills and prompt sizing. The startup row shows the skill count; `/skills` shows the names. The backend shares one Durable task graph and reads `pi.live` for settled failures; the TUI opens no extra per-conversation watches. Without the capability, the TUI shows the first `thread.list` page as **listed**, not **open**, and otherwise uses the active thread's feed.

**Run status** distinguishes a run that ended without an answer from one still parked. It omits aborts, resets, and continues that found nothing to answer. Intro v2 gives actionable failures a thread title and, when recognizable from Durable's stored detail, a cause such as `rate limited (429)` or the unavailable model. Raw provider bodies stay in the runtime because they can contain credentials or request text.

Click **retry**, **switch model**, or **dismiss** on a current report. On the active thread, use `/retry` (`Ctrl+X B`), `/model` (`Ctrl+L` or `Ctrl+X M`), or `/dismiss` (`Ctrl+X X`). Retry continues the exact failed run; switching models does not retry it. Dismiss hides only that failure and does not change its settlement or delete its history. Actions depend on the failure and capabilities; hints that do not fit disappear whole. The frozen startup report retains the snapshot without its recovery buttons. `/threads` opens the thread list.

The backend saves a title from the first committed user message without calling a model. `Ctrl+R` (`Ctrl+X R`) or `/name` sets a Hush title without changing Pi's session name. The title appears in the active footer, tabs, switcher and run report.

Context starts as an estimate of the UTF-8 bytes in the rendered prompt and tool declarations (one token per four bytes). For an attached thread with a current same-model response, it uses reported response usage. It does not turn cumulative usage into context occupancy. No context row is shown without a known model window. An embedder can replace the SDK report with one cancellation-aware `IntroOptions.Watch` callback.

Use `/skills` or **List loaded skills** in the palette to browse the active workspace's trusted skill names even after the intro freezes. Choosing a skill stages `/skill:<name>`; a separate Enter submits it. The backend checks that the skill is still loaded before expanding it. The startup report itself never reads out skill bodies or submits anything.

## Rendering and shortcuts

The intro stays at the top as the first messages appear below it. Once the viewport fills, it scrolls away with the transcript. The fixed status line below the editor keeps showing the workspace, model and context. It stays visible below sheets and completion lists. A `~` marks an estimated context percentage. Narrow layouts omit optional statistics and the context gauge before dropping the workspace, model or percentage.

The cooldown timestamp is read and written off the UI loop. Only the banner waits for the cold-start decision; the prompt and report do not. Data appears immediately with a short fade and no stagger. Intro ticks reuse the composed view outside the intro block; Bubble Tea diffs the resulting terminal cells. Embedders using their own output/renderer get the static report because the client cannot verify their output's TTY or initial size.

The 64-column wave is clipped to the available width at 70 columns and up, and omitted below 70. RGB gradients require truecolor and the relevant palette reports. ANSI256 terminals use terminal tones and a stepped fade. Translucent terminals use stepped fading rather than blending toward an opaque background. `NO_COLOR` and low-color terminals make the intro monochrome.

Hints use existing bindings rather than assigning `Ctrl+K` (delete to end of line) or `Ctrl+G` (external editor). `/` lists commands, `?` opens the action palette from an empty prompt, and the threads hint respects multiplexer overrides.
