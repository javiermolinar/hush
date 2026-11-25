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

In the default Hush profile, pass a prompt with `hush --print '...'` or
`hush -- '...'`, or pipe it to `hush` on stdin. Unknown commands aren’t sent
as prompts.

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

Open `?` → **Show loaded context** to see the instruction files the backend
resolves for the active thread’s next submission. The inspector lists paths,
not contents. Hush [discovers skills](backend/README.md#standalone-skills) from
personal roots and trusted workspaces. Use `/skills` to stage one without submitting.

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
migrate the database.

## What it won’t do

A scheduled run can’t fire while the backend is down. A new thread worktree starts
from committed `HEAD`, not your uncommitted edits. It’s not a sandbox: the agent’s
tools run with the backend user’s permissions.

[MIT license](LICENSE)
