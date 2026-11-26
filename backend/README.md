# Reference backend

The installable `hush` binary [embeds the backend](../README.md#build-and-verify-the-installable-binary).
For development or a manually managed backend, build with `npm ci --ignore-scripts` and
`npm run backend:bundle`. Run the entry with Node.js 22.19.0 or later. Keep the generated
`hush-backend.mjs.wasm-pack` beside the entry; the installable Hush binary embeds both:

```sh
node dist/hush-backend.mjs --data-dir /path/to/private/hush --cwd /path/to/project
```

The resulting Unix socket is `<data-dir>/hush.sock` (mode 0600). The directory must belong to the caller and have mode
0700; a missing directory is created with that mode. The default Hush profile is `~/.hush/` on
the backend host; `--data-dir` defaults to `~/.hush/runtime/`. The profile may hold
`credential-store`, `auth.json` and `models.json`; the files must be private and owned by the user.
Hush uses Pi AI's built-in model catalog and OAuth flows with its own credential files.
It does not install Pi, read Pi resources or load project-local `.hush/` settings.
Hush does not read or migrate Pi stores or session files.
A manually started process prints one JSON ready record to stdout. The Go CLI can instead start a detached backend for
its default socket, with an internal startup pipe and no stdout/stderr inheritance. It waits for a compatible `hello`.
Managed Hush launches use a private Node compile cache under `~/.hush/backend-cache/`, separate
from the store. With the cache available, the launcher extracts fresh embedded bytes to a
stable path keyed by the bundle and Node version, then removes the extracted files after startup.
A successful managed start keeps the current cache generation and the two most recently used prior
generations. Older recognized entries are removed in bounded batches; unrelated files are left alone.
Hush falls back to temporary extraction if the cache is unavailable. Set `NODE_DISABLE_COMPILE_CACHE=1`
to disable caching.
Normal storage is `history.sqlite`; the separate kernel-owned SQLite lock prevents simultaneous writers. A competing
launcher waits for the lock holder's socket, and a restart can recover work and remove a verified stale socket under the lock.
The app stays up while clients, runnable work, or deadlines exist. Once all three are absent for `--idle-ms`
(default 600000), it closes the Harness and releases the lock. SIGINT/SIGTERM close it explicitly. The negotiated
[`runtime:1` capability](../protocol/v1/runtime.md) provides the same orderly shutdown through `hush stop`; the next
managed CLI command can start the replacement. `hush restart` does both for the managed local backend and waits for
`hello` from the replacement. Use `hush service restart` if the login supervisor owns the backend. `hush.log` records
fixed lifecycle/error labels, not prompts, provider errors or credential material.

Flags include `--cwd`, `--data-dir`, `--model provider/id`,
`--idle-ms`, and `--faux` (local scripted test model). `--config <file>` reads a JSON
object with the same option names without leading dashes; explicit flags win.
The Hush profile uses a pinned model default, the built-in coding tools, and no
project executable configuration. Tools run with the backend user's permissions;
a thread's working directory is not a sandbox.

## Pi Durable references

Use the pinned [spec](https://github.com/earendil-works/pi/blob/v1.6.0/packages/durable/docs/spec.md)
as the authority on Durable behavior. See the [README](https://github.com/earendil-works/pi/blob/v1.6.0/packages/durable/README.md)
for concepts and APIs, and the [changelog](https://github.com/earendil-works/pi/blob/v1.6.0/packages/durable/CHANGELOG.md)
for version changes. `node_modules/@earendil-works/pi-durable/` includes the matching README and changelog
after dependency setup. Update the links when bumping the pin.

The [delegation contract](../docs/delegation-contract.md) describes the parked join,
backend tool profiles, exact code review snapshots and recovery. See [the native capability](../protocol/v1/delegation.md)
for explicit launch, status and Stop.

## Standalone store compatibility and restore

A new standalone runtime initializes a format-2 `store-profile`. Its logical
store ID and generation are lowercase UUIDv4 values. The manifest binds to
`history.sqlite` by device and inode and to `history.identity`, a hard link that
reserves the inode. **Never open the alias in SQLite.** The database header
carries `application_id` `0x48555348` (`HUSH`), separate from Durable's schema.
Hush checks this binding under the writer lock, before SQLite opens history or
Hush removes a stale socket. It does not adopt a Pi database, a bare SQLite
file, or an incomplete bootstrap. There is no automatic upgrade.

To keep using an existing format-1 store with the current backend, add
`"allowLegacyStore": true` to the private backend-host `~/.hush/settings.json`
and restart. This opt-in applies only to the default managed standalone runtime
with no recorded cutover. Before opening history or removing a stale socket,
Hush checks the binding and validates the Durable schema, document versions and
persisted task definitions on a private temporary copy including any WAL. An
incompatible store is still refused. The original format, store ID and generation
stay the same; this is not a cutover or a guarantee that older builds can handle
work written by the current backend. Fresh stores still use format 2.

The managed cutover implementation has this compatibility matrix. `hush store
inspect` reports the exact target bundle SHA-256 and the on-disk phase; compare
that SHA-256 with the current candidate's release evidence. This is an
implementation matrix, not an independently verified release claim.

| Source build | Target build | Runtime and scope |
| --- | --- | --- |
| [Pinned standalone candidate](../docs/standalone-release-baseline.md), exact `eec512d5…c21f` Linux arm64 executable; Hush format 1 | Same target bundle for preparation, activation and subsequent managed startup; Hush format 2, registry `hush-standalone-2` | Node 22.19.0 for preparation, private local Linux arm64 default `~/.hush/runtime` only; Pi Durable 1.6.0 SQLite schema 2 on both sides |

Use the [operator runbook](../docs/store-cutover.md) to check the exact pair,
quiesce launchers, recover an interrupted cutover, and attach fresh clients.

The format-1 allowlist includes built-in `pi.generation` v3, `pi.tool` v4,
`pi.compaction` v1, and the pinned standalone coding-tool and document registry.
Unrecognized tasks, wake definitions, profiles, runtime artifacts, and managed
worktrees are not supported sources. Providing the pinned executable verifies
its bytes, not the provenance of an arbitrary database.

The writer interlock stays in place. An incompatible marker fences legacy
launchers; the target additionally requires a matched gate in
`~/.hush/store-cutover/`. The predecessor database snapshot is retained there
with its manifest, not as a runnable clone. It does not copy credential files,
Git worktrees, or other external files; it does not redact transcript contents.
The pinned predecessor has no `hush stop` command. Its writer must exit before
preparation; cutover refuses a live owner. Do not copy a database over the
active file, move the runtime aside, start a backup as another store, or edit the
gate. Before the gate a supported restore changes generation; after the gate
or if the phase is unknown, preserve both stores and recover forward. Normal
restart keeps both identity values; restoring never permits reuse of prior
history anchors or submission keys. This is not protection against a same-user
process rewriting files or a whole-filesystem rollback that also rolls back
the outside gate.

The [store identity wire capability](../protocol/v1/store-identity.md) keeps
these storage identities separate from the backend's fresh `hello` epoch.

## Host-local provider login

In a managed local Hush TUI, use `/login` to choose a provider, then `/model`
to select a model. From a shell on the backend host, use `hush auth login PROVIDER`,
`hush auth status PROVIDER`, or `hush auth logout PROVIDER`. Omitting `PROVIDER`
still targets `openai-codex` for all three commands.

| Provider ID | Sign-in |
| --- | --- |
| `openai-codex` | OpenAI Codex: browser or device code |
| `anthropic` | Anthropic (Claude): browser or copy code |
| `openai` | OpenAI (ChatGPT): browser |
| `github-copilot` | GitHub Copilot on GitHub.com: device code |
| `kimi-coding` | Kimi Code: device code |
| `meta` | Meta: device code |
| `openrouter` | OpenRouter: browser |
| `radius` | Radius public gateway: browser or device code |
| `xai` | xAI (Grok): device code |

These are the built-in OAuth flows from Pi AI 1.6.0. Configure API keys and other
credentials separately on the backend host. Standalone `models.json` supports
`providers` with a built-in provider ID, or custom chat models using a Pi AI API
and an HTTP(S) endpoint. A provider-level `api` applies only to entries in `models`;
it does not change the API of built-in models. `modelOverrides` adjusts individual entries
without dropping untouched thinking levels, image limits, cache lifetimes, or sampling settings.
Use literal values or `$VAR` / `${VAR}` for configured API keys and headers.

The private UTF-8 file accepts `//` and `/* ... */` comments. Token limits must be positive integers.
Hush does not allow executable `!command` values, Pi extension providers, or custom `oauth` entries
(including `"radius"`); the built-in Radius login is unaffected. If the file is invalid, Hush reports
the field where possible without logging values. An existing store opens read-only: browse history
and receipts, but fix the file and restart to resume work. Hush never sends requests to a fallback
endpoint in that state. On a new profile, startup refuses to create a store until the file is fixed.

The equivalent bundle commands are
`node dist/hush-backend.mjs auth login|status|logout [PROVIDER]`.
There is no socket auth method. Login requires a TTY and writes to the selected
Hush credential store. It shows browser or device instructions only on the login
terminal; manually pasted codes and redirect URLs are not echoed. Cancel before
the provider exchange commits to keep the previous login. Logout removes the
selected Hush credential locally; it does not revoke the provider session or
remove separately configured alternatives. Status does not validate tokens with the provider.

### Choose the store

The default for compatibility is `file`, which uses private `~/.hush/auth.json`.
To use the OS keyring, finish auth operations and stop the Hush daemon (SIGTERM
the matching backend or wait for idle shutdown). While still on the old store,
run `hush auth logout PROVIDER` for each credential you want to remove. Do not
remove the whole `auth.json` if it contains credentials you need to keep. Existing
backups are not scrubbed. Then select the new store and sign in again:

```sh
(umask 077; mkdir -p "$HOME/.hush"; printf 'keyring\n' > "$HOME/.hush/credential-store")
chmod 600 "$HOME/.hush/credential-store"
hush auth login
```

Use `file` in the same selector to switch back, with the same stop/logout steps.
There is no migration, mixing, or automatic fallback. A running backend
refuses new credential reads if the selection changes; restart it to adopt
the new setting.

Keyring mode keeps one complete OAuth record per provider and Hush profile on
the backend host. Existing Codex records remain readable without migration.
It uses the backend account's default Keychain on macOS and an unlocked Secret
Service in its local Linux D-Bus session. Start Hush from that account/session. Hush does not start, unlock or
prompt for a keyring; when it is inaccessible, unlock or authorize access in
the OS and retry `hush auth status`. A manually started Node backend in
keyring mode needs `HUSH_KEYRING_HELPER` set to the trusted absolute path of
the installed `hush` binary. Managed starts and `hush auth` set this themselves.

A private SQLite lock coordinates credential changes across processes in the same profile.
File-backed writers also take the legacy `auth.json.lock` used by older Hush builds, so an in-flight
refresh can finish before a newer writer reads or changes the record. Neither lock contains credentials.
The keyring avoids plaintext Hush token files and their backups; it is not a sandbox for tools
running as the backend user.

For standalone input admission, the active daemon checks the selected model against current
credentials, after checking for an existing keyed receipt. Missing auth returns a static
`invalid_request` diagnostic without admitting a new input. Continue and Intro retry also
check before admission. The provider runtime independently reads credentials when it starts
an actual request. Logout does not cancel a provider request whose auth was
already resolved. An invocation-private backend can read a current bearer but
does not refresh or write either store.

After `npm run backend:bundle`, run the loopback OAuth/generation and terminal fixture with
Python 3, Go and Node available:

```sh
(cd app && HUSH_TEST_BACKEND="$PWD/../dist/hush-backend.mjs" go test ./internal/headless -run 'Test(ParseLocalAuth|CodexAuthLoopback)$')
```

To exercise the real keyring from an unlocked local session, use the opt-in
fixture. It uses synthetic credentials, a loopback provider, temporary Hush
profiles, and removes the fixture keyring entries. Run it on macOS and Linux
(Secret Service available), with Node.js 22.19.0 and Python 3:

```sh
npm run backend:bundle
go build -o /tmp/hush-keyring-fixture ./app/cmd/hush
HUSH_TEST_KEYRING_BINARY=/tmp/hush-keyring-fixture node --test app/test/keyring.test.mjs
```

## Standalone configuration

`/config` opens the persistent settings modal on a standalone host. It is also available as
**Configuration** in the main command palette. **Default model** and
**Default thinking effort** apply to newly created threads; `/model` continues to change the current
thread. **Reasoning text** is independent: `expanded` shows supplied live and historical reasoning
while leaving tool output folded, and `folded` retains the Ctrl+O behavior. Changes take effect
without a backend restart and are stored in the backend host's private `~/.hush/settings.json`.

```json
{
  "defaultProvider": "openrouter",
  "defaultModel": "moonshotai/kimi-k3.1-pro",
  "defaultThinkingLevel": "high",
  "display": { "reasoning": "expanded" }
}
```

The file must remain owned by the user and mode `0600`. Existing unrelated keys are preserved.
Reasoning display cannot recover empty or redacted provider reasoning. Other already-connected
clients reload the setting on their next configuration read or restart rather than receiving a push.

## Standalone Codemode

Codemode is off by default. To enable it, add `"codemode": true` to the JSON object in
`~/.hush/settings.json` on the **backend host**. The file must be owned by your user and private
(`chmod 600 ~/.hush/settings.json`). Restart the backend (`hush restart --open` for a local
backend, or `hush service restart` for a supervised backend). Set the key to `false` or remove it
and restart to turn it off. The installable binary includes QuickJS; it does not need Pi or
adjacent Node packages. Project settings, prompts, and `--no-session` runs cannot enable Codemode.

Direct tools remain available. A `codemode` script can only call `tools.inspect_file({ path })`,
a read-only tool for regular, single-link UTF-8 files up to 32 KiB. Paths are relative to the thread's
working directory and cannot traverse `..` or follow symlinks. Use a physical working directory
(`pwd -P`); Linux requires `/proc/self/fd` to verify the file actually opened. This restriction
applies to scripts, not to the separate direct coding tools.

```js
const files = await Promise.all(["one.txt", "two.txt"].map(path => tools.inspect_file({ path })));
return files.map(file => file.split("\n").find(line => line.startsWith("REVIEW_KEY=")));
```

Each script has up to 16 calls and 10 seconds, including tool time. The result is capped at about
1,500 tokens (at most 16 KiB and 500 lines), with no spill files or image output. A first line of
`// @options: {"timeout_ms": 2000, "max_output_tokens": 500}` can lower those limits. Scripts cannot
call the direct shell, write, edit, or read tools, MCP tools, or model helpers. QuickJS isolates
JavaScript; the backend separately enforces the call allowlist and file checks.

With Codemode still enabled, [Pi Durable 1.6.0](https://github.com/earendil-works/pi/blob/v1.6.0/packages/durable/README.md#code-mode)
can replay an interrupted script within its original deadline. It reuses committed nested results;
an inspection that had not committed can run again. See [the controlled comparison](../docs/codemode-comparison.md)
for measured costs of filtering four independent reads in one script.

## Standalone system prompt

The backend bundle includes [`src/prompts/system.md`](src/prompts/system.md) as text. No prompt file is
required at startup. The default tells the model to answer questions after only the necessary checks,
without editing, and to keep explicitly requested changes scoped. Hush renders it first, followed by
instructions for the active tools and the thread's working directory, then instruction files on the
**backend host** in this order:

1. **Personal instructions**: `~/.hush/APPEND_SYSTEM.md`, `~/.hush/AGENTS.md`, then `~/.agents/AGENTS.md`.
2. **Workspace context**: from the nearest Git root (`.git` directory or worktree marker file) down to
   the working directory, `AGENTS.md` then `.agents/AGENTS.md` at each level. Outside a repository, only
   the working directory is checked. Persistent Hush requires [workspace trust](#workspace-trust) for
   workspace instructions.

These files are additive, and Hush includes each path at most once per prompt. Hush re-reads them for the
next provider request; no restart is needed. It reads regular UTF-8 files up to 64 KiB each without
following symlinks, uses a 256 KiB workspace text budget, and skips missing, unreadable, empty, invalid
or oversized files. There is no full override, project `.hush/` prompt, or Pi resource discovery in this
profile. The text goes to the model provider, not into chat history as a message. Prompt text guides the
model; it cannot grant tool permissions or weaken host policy. Loopback fixtures check the request
contents, not whether a model follows the instructions.

## Standalone skills

Hush discovers text skills automatically on the **backend host**, in this order:

1. `~/.hush/skills`, then `~/.agents/skills`.
2. Any explicitly configured root for the exact physical workspace.
3. `.agents/skills` in the working directory and its ancestors through the Git
   root, nearest first. Outside Git, only the working directory applies.

The first valid definition of a name wins, and the picker shows up to 64 skills.
Project roots require the [workspace grant](#workspace-trust) for the working
directory (`hush trust grant --cwd PATH`). There is no automatic scan of
`.claude/skills` or `.pi/skills`, and Hush does not import project settings,
hooks, or MCP servers. A skill is `NAME/SKILL.md` with matching `name`, a
`description`, and a nonempty text body within a 32 KiB file. Hush skips
unreadable files and symlinks.

No configuration file is needed. A private (mode 600) `~/.hush/skills.json`
can disable both personal roots with `"personal": false` or add per-workspace
relative paths with `"projectRoots": [{"workspace": "/absolute/physical/project",
"directory": "other-skills"}]`. Adding a root does not grant trust.

Persistent standalone model requests include a catalog of available
`/skill:NAME` commands and their authored descriptions, capped at 8 KiB. The catalog sends no skill
bodies, file paths, or selection tokens, and makes no extra model calls. Only
an explicit invocation loads the skill instructions.

Use `/skills` or choose a suggestion under `/` in an existing thread or at a
fresh local prompt. Selection only stages `/skill:NAME`; press Enter to submit.
Personal skills can be listed without workspace trust. Hush requires that trust
to submit and rechecks the selected source at admission. If a selection changes,
use the picker again instead of resending its stale token.

## Thread titles and worktrees

The backend derives a short topic from the first completed exchange, using the
first prompt as a provisional label until then. This runs locally and the settled
title is saved. For model-written titles, run a persistent backend with
`--thread-title-model provider/small-model` (or set `"thread-title-model"` in its config).
This opts in to sending bounded excerpts of the first prompt and answer to the
configured provider. No tools, system context, or image bytes are sent. The request
is best-effort: new native chats can claim one attempt in Durable, and do not replay
it after restart. Manual names win. Without the option, no title model is called.

A native `thread new --worktree` request requires a trusted source workspace
and a committed Git HEAD. It allocates `worktrees/<unique-id>` inside the private
data directory, with a new `hush/<unique-id>` branch. Hush rechecks trust before
starting Git; revocation doesn't undo a checkout already admitted. The new
checkout needs its own workspace grant. The data directory
must be outside the source checkout and Git directory. Hush copies no uncommitted
files and does not switch the original branch. If Git or Durable fails mid-create,
inspect the directory before retrying; Hush will not delete potentially populated files.
Archive only changes visibility. Close/open and transcript forks never remove a Git
worktree, main or otherwise. See [the directory API](../protocol/v1/thread-directory.md).

## Workspace trust

One workspace grant covers agent tools, `@` file browsing, and Git operations
such as diff, managed checkout, and file checkpoints/rewind. The managed local
TUI shows the exact resolved directory before granting it, including for plain
folders. **Not now** remembers the dismissal without granting anything. Use
`/trust` to review the active workspace later. Attached clients give instructions,
not a grant button. On the backend host, use:

```sh
hush trust status --cwd PATH
hush trust grant --cwd PATH
hush trust revoke --cwd PATH
```

`PATH` defaults to the current directory. Each grant is bound to one canonical
workspace directory and its filesystem identity. `@` lists below that directory;
Git operations use the whole associated checkout. Trust does not carry across
thread working directories, even if they're nested or share Git metadata. Home
browsing via `@~/` is a separate `hush trust grant --scope home` decision.
Socket peers, project files, and Pi settings cannot grant trust.

Existing tool grants and dismissals in `~/.hush/workspace-work-v1/` carry over;
`--scope work` remains an alias. Older Git-only and browse-only records do not
carry over: review the workspace and grant it explicitly to use the new policy.
The old `--scope git` and `--scope browse` flags are no longer accepted.

Persistent Hush checks workspace trust before accepting new agent input and again
before each tool attempt, including after a park or process restart. Headless
commands return a diagnostic instead of prompting. Previously admitted model
work may still finish without tools; revocation does not kill a command that
already started. Scheduled work without trust parks for an explicit retry.
Trust is consent to run with your user's permissions, **not a filesystem sandbox**.
Invocation-private `--no-session` runs remain tool-free. If upgrading a managed Hush backend, run `hush stop` before
opening the new client (or `hush service restart` for a supervised backend)
so the running host uses the new trust policy.

## User-scheduled work

The standalone persistent backend runs explicit occurrences as Durable background
tasks. Busy targets receive a follow-up, not a steer. Late wake grace defaults to
15 minutes; set `--schedule-late-ms N` or `"schedule-late-ms": N` in backend config.
It applies only to user occurrences, not provider retries, tool polls, or other
Durable sleeps. When the backend is down, no prompt is dispatched until it returns.
An unattended restart may leave a late or unavailable occurrence waiting for a
decision. The service does not install or start an OS daemon.

## Checkpoints and file rewind

File checkpoints pin committed HEAD and require a trusted Hush workspace.
A rewind preview identifies the commit, ref, exact new checkout/branch and
recorded external effects. Only confirmation admits the restore, and only a
verified checkout publishes a new fork. Existing checkouts are never reset.
If the backend is interrupted, it reuses a fully staged clean checkout on
reopen; incomplete or changed leftovers remain available for inspection under
`rewind-worktrees-v1/` in the private data directory. Check the receipt before
retrying. Trust revocation or a missing ref leaves a transcript-only option.
See [the checkpoint contract](../protocol/v1/checkpoints.md).

## Invocation-private backend

Run with `--no-session --socket /private/one-invocation/hush.sock`, in a fresh caller-owned directory with mode 0700.
Do not supply a data directory or reuse a socket path. It serves a new MemoryStorage and exits after the last client
disconnects. Coding tools are disabled in this mode so there is no tool spool, image cache, or independent
tool process to leave behind. No transcript database, backup, or backend log is opened. A hard kill can leave the
empty socket file; the invoking client owns the private directory. Hush credentials are read without changes to the profile.
A private invocation requires a current OAuth token: it does not rotate and discard single-use
refresh tokens. Use a normal Hush run or sign in again if the token needs refresh. With `--faux`, auth is always in memory.

The Go CLI starts its own bundle with `--parent-stdin`. This internal flag is valid only with `--no-session`.
The child holds the read end of a lifetime pipe; EOF shuts it down even if the CLI died before connecting.
The pipe never carries prompts or replies. The CLI reaps the child and removes the directory on normal exit.

Run `npm run backend:check` under Node 22.19.0. It uses temporary Hush profiles and local provider fixtures.
The backend also boots without node_modules under the official Node 22.19.0 Linux image with the network disabled.
