<p align="center">
  <img src="docs/assets/hush-banner.gif" alt="Hush" width="720">
</p>
<p align="center"><strong>A quiet TUI for durable agents</strong></p>

Closing a terminal shouldn’t cancel the run. Hush keeps the work in a separate
[Pi Durable](https://github.com/earendil-works/pi/tree/v1.6.0/packages/durable)
backend. Open `/threads` when you come back and pick up the same conversation.

<a name="build-and-verify-the-installable-binary"></a>

## Install

Unpack `hush-<os>-<arch>.tar.gz` for your machine. From the directory containing
`hush`:

```sh
mkdir -p "$HOME/.local/bin" && install -m 755 ./hush "$HOME/.local/bin/hush"
```

Run `~/.local/bin/hush` in your project. The local TUI asks before trusting a new
workspace for tools, `@` file browsing, and Git. You can choose **Not now** and
change your mind with `/trust`.
Type `/login` to choose an [OAuth provider](backend/README.md#host-local-provider-login), then use `/model` to select a model. The binary embeds the backend. You need Node.js 22.19.0 or newer, but not a separate Pi installation.
[Codemode](backend/README.md#standalone-codemode) is an optional host setting for batching workspace reads.

For a [release candidate](CONTRIBUTING.md#release-candidates), use the GitHub Actions
workflow or run `mise run release` locally with [Mise](https://mise.jdx.dev/) and Python 3.
The local build writes `dist/release/<os>-<arch>/hush`.

## Pick up a thread

Pass a prompt with `hush --print '...'` or
`hush -- '...'`, or pipe it to `hush` on stdin. Unknown commands aren’t sent
as prompts. `--json` formats native command output; Pi's JSON/RPC modes,
profile, and session files are not supported.

This is from a run against Hush’s scripted test model. I restarted the backend
between the first prompt and `thread list`.

```console
$ hush trust grant --cwd .
$ hush --print 'Check the release notes'
Faux: Check the release notes
$ hush thread list
7  done  "Check the release notes"
1  idle
$ hush --thread 7 --print 'Now check the install instructions'
Faux: Now check the install instructions
```

In the Go TUI, open the thread switcher with `Ctrl+X S` or `/threads`. It groups
threads under workspace names and shows which are running, need attention, or have stopped.
Type a workspace name, thread title, or phrase from a message to search. Chat search starts
at three characters. Local sessions start in the launch workspace; choose **Browse all
workspaces** or run `/threads-all` to search across them. To start a thread in a known
workspace, filter to it, then move up to **+ New thread here** and press Enter.
You can also press `Ctrl+X N` on a thread in that workspace. If there’s more than one
working directory, choose which one to use. At the prompt, `/new` still uses the launch
directory. Use `/close` to close the current tab without deleting its thread or stopping
its run.

The TUI caches transcript blocks and renders the visible window rather than rewrapping
the whole thread on each keystroke. Use `/config` to set the default model and thinking effort for new threads, and to show live and
historical reasoning without expanding tool output. `/model` still changes only the current thread.

On a supported standalone backend, click the `±` badge or use `/diff` or
`Ctrl+X I D` to review the active thread’s trusted Git worktree. The review
shows the thread’s working directory and, when available, the local Git branch.
Open a file with Enter, go back with Esc, or use Tab in the list to compare with
the last file checkpoint. Review is read-only; it does not stage or restore files.
Git review and `@` use the same [workspace grant](backend/README.md#workspace-trust)
as agent tools. Use `/trust` in the local TUI or `hush trust grant --cwd PATH`
on the backend host.

Type `@name` to fuzzy-search the active thread’s trusted workspace, including nested
files and the top-level `.github` directory. Other hidden names are excluded.
Use ↑/↓ to scroll through matches, or add a parent path term inside quotes
(e.g. `@"tempo.fork generator.go"`). Type `@` or `@dir/` to browse one directory at
a time. Tab or Enter completes the path without submitting. In a new local
session this works before your first message, without creating a thread. `@~/` browses the
backend’s home only after a separate `hush trust grant --scope home`, which does not
permit tool execution.

You can also type `~` or `~/` without `@` to browse the granted backend home.
Type `/` after other text (for example, `Read /`) to choose a granted location.
At the start of the prompt, `/` still opens the command menu; choose **Browse paths…**
to see the same locations. An absolute path inside the recorded workspace can
be completed directly. The `/` shortcut shows granted locations, not the
filesystem root, and browsing stays within those grants.

Open `?` → **Show loaded context** to see the instruction files the backend
resolves for the active thread’s next submission. The inspector lists paths,
not contents. Hush [discovers skills](backend/README.md#standalone-skills) from
personal roots and trusted workspaces. Use `/skills` to stage one without submitting.

## Request an independent code review

Use `hush review start` to review exact files in a trusted thread's dirty checkout:

```sh
hush review start --thread 7 --request-id api-review --tracked src/api.go \
  --untracked src/new_test.go --reviewer 'API=Check the error paths.' \
  --check 'errors=Inspect error handling on the pinned change' \
  --evidence 'tests:Local test excerpt=3 tests passed'
hush review status --thread 7 --request-id api-review
```

The backend pins the staged and unstaged versions and posts one advisory result
per worker in the parent thread. It does not start another parent turn by default.
Pass a second `--reviewer` (or use one or two `--brief` flags instead) to request
two reviews, and `--parallel` to let them overlap.
Pass `--prompt 'Launch the reviewers and join their results before answering'` to
start an explicit parent turn that can spawn and join the reserved workers.
Click the worker count or press `ctrl+x i j` in the parent thread to inspect
briefs, pinned evidence, and advisory reports. Enter opens a read-only transcript
from the evidence panel. Where Rex is available, `o` opens that transcript in a
right pane. Use `x` in the evidence panel or `hush review stop HANDLE --thread 7`
to stop just that worker. Closing a panel or pane does not stop work.
Retry a lost launch reply with the same request ID and arguments. Supplied test
excerpts are not rerun, and review does not verify or approve work. See the
[native review contract](protocol/v1/delegation.md).

## TUI themes

Hush uses the terminal palette by default. Open `/theme` (or `?` → **Themes**) and use ↑/↓ or
click a row to preview it. The sample shows the active accent, link, status, and syntax colors.
Press Enter or click the row again to apply and save locally, or Esc to restore the previous theme
without saving.

Built-in choices are `panther`, `gruvbox`, `keanu`, `kitt`, `rose-pine`, and `rose-pine-moon`.
They only change foreground colors, so each has a set for dark terminals and one for light
terminals, and Hush picks the set from the background the terminal reports. On a light terminal
both Rosé Pine presets use Dawn. Gruvbox and Rosé Pine use foreground adaptations of the MIT-licensed
[Gruvbox palette](https://github.com/ellisonleao/gruvbox.nvim) and
[Rosé Pine palette](https://github.com/rose-pine/rose-pine-palette)
([notices](app/redistribution/THIRD_PARTY_NOTICES/INDEX.tsv)). Panther, Keanu, and KITT are Hush
interpretations of the palettes previewed in Crush.

Custom themes live in `~/.hush/themes/<name>.json`. Version 2 declares four semantic
colors, a surface, and a base. For example:

```json
{"version":2,"base":"auto","surface":"tinted","colors":{"accent":"#8b9df5","ok":"#a6e3a1","warn":"#f9e2af","err":"#f38ba8"}}
```

Choose `transparent`, `tinted`, or `solid` for `surface`, and `auto`, `dark`, or `light` for
`base`. Auto uses the OSC 11 terminal background report. Solid panels use that background color;
Hush never changes the terminal background itself. Omit a color to use the terminal default.
The client raises measurable low-contrast roles to about 4.5:1 when it can emit RGB.

Use a lowercase name with letters, digits, hyphens or underscores (starting with a letter or
digit). Existing version 1 files with `base: "terminal"` and individual foreground roles still
load; **Copy starter JSON** copies the version 1 template.

Reopen `/theme` to load the file. A local JSON file takes precedence over a built-in theme
of the same name; `terminal` is reserved. `hush --theme NAME` overrides the saved choice for
that launch; `hush --theme terminal` always uses the terminal palette. If the
override is invalid, Hush reports the error and keeps the saved choice if it is valid.
Custom RGB needs truecolor and a reported background. If colors fall back to the terminal palette,
the picker title and the save notice say why. `NO_COLOR` suppresses custom colors and fills.

Fills can tint the tab band, focused rows, tool cards, inline code, and menu surfaces. In `/theme`, use ←/→ (or
click the **fills** row) to choose `auto` (on, except in Rex), `on`, or `off`. Enter saves the choice
with the theme in `~/.hush/fills`; `hush --fills MODE` overrides it for one launch. With fills off,
backgrounds stay unpainted.

## Restart the local backend

Run `hush restart` to shut down the local backend and wait for its replacement.
If it isn’t running, this starts it. `hush restart --open` also opens the TUI;
add `--thread ID` to reopen a specific thread. Restart disconnects existing
clients but keeps stored threads and history. Explicit sockets (`--socket` or
`HUSH_SOCKET`) are attach-only. For an installed user service, use
`hush service restart` instead.

## Keep the backend running after exit

Run `hush service install` as the desktop login user to opt in to a macOS
LaunchAgent or Linux systemd user unit. Stop any manually started backend first
with `hush stop`. The service pins the Hush, Node, bundle, and keyring helper
paths. It does not inherit shell-only credentials, change the credential store,
or enable Linux linger. Unlock the login Keychain or Secret Service before using
keyring credentials. Check `hush service status` and `~/.hush/supervisor.log` for
attention; reattach with `hush` or `hush --thread ID`.

Use `hush service restart` for a normal restart, `hush service pause` before a
supported store cutover, and `hush service resume` only after inspecting the
store. While installed or paused, Hush will not start a replacement on demand.
`hush service uninstall` removes the job without removing history or credentials.
Explicit sockets remain attach-only. To verify an install on the same format-1
store, keep its existing `allowLegacyStore` opt-in; service install does not
migrate the database. `hush service status --json` reports the same state for
scripts and status displays.

## Menu bar app (macOS)

`mise run app:macos` builds `dist/macos/Hush.app`, a menu bar app with the `hush`
CLI inside it. Copy it to `/Applications` and open it. The menu shows whether the
service is running, how many threads are active, and which threads need attention;
choose one to open it. It also opens Hush, restarts or resumes the backend, and
shows the service log. **Open In** picks Rex, Terminal, iTerm, Ghostty or the
default app for `.command` files. The menu reads state only when you open it.

To update a development install, let active runs finish, quit Hush from the menu
bar, and run `mise run app:macos:install` from the checkout. It builds the backend
and app, replaces `/Applications/Hush.app`, and restarts the login service if it
was running. This disconnects open clients. The task refuses to replace an app
with a service pinned elsewhere, and leaves an already paused service paused.
Reopen Hush after the update.

To register the login service for a development install, run
`/Applications/Hush.app/Contents/MacOS/hush service install --backend "$PWD/dist/hush-backend.mjs"`
from the checkout. The app is signed ad hoc, so Login Items shows a generic icon;
a Developer ID signature would show the Hush icon.

## What it won’t do

A scheduled run can’t fire while the backend is down. A new thread worktree starts
from committed `HEAD`, not your uncommitted edits. It’s not a sandbox: the agent’s
tools run with the backend user’s permissions.

[MIT license](LICENSE)
