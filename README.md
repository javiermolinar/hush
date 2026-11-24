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

Run `~/.local/bin/hush` in your project and type `/login` to sign in with OpenAI Codex.
The binary embeds the backend. You need Node.js 22.19.0 or newer, but not a separate Pi installation.

For a [release candidate](CONTRIBUTING.md#release-candidates), use the GitHub Actions
workflow or run `mise run release` locally with [Mise](https://mise.jdx.dev/) and Python 3.
The local build writes `dist/release/<os>-<arch>/hush`.

## Pick up a thread

This is from a run against Hush’s scripted test model. I restarted the backend
between the first command and `thread list`.

```console
$ hush --print 'Check the release notes'
Faux: Check the release notes
$ hush thread list
7  done  "Check the release notes"
1  idle
$ hush --thread 7 --print 'Now check the install instructions'
Faux: Now check the install instructions
```

In the Go TUI, you can open threads in tabs. It caches transcript blocks and
renders the visible window, rather than rewrapping the whole thread on each keystroke.

## What it won’t do

A scheduled run can’t fire while the backend is down. A new thread worktree starts
from committed `HEAD`, not your uncommitted edits. It’s not a sandbox: the agent’s
tools run with the backend user’s permissions.

[MIT license](LICENSE)
