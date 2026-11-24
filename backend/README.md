# Reference backend

The installable `hush` binary [embeds the backend](../README.md#build-and-verify-the-installable-binary).
For development or a manually managed backend, build with `npm ci --ignore-scripts` and
`npm run backend:bundle`. Run the resulting single file with Node.js 22.19.0 or later:

```sh
node dist/hush-backend.mjs --data-dir /path/to/private/hush --cwd /path/to/project
```

The resulting Unix socket is `<data-dir>/hush.sock` (mode 0600). The directory must belong to the caller and have mode
0700; a missing directory is created with that mode. The default Hush profile is `~/.hush/` on
the backend host; `--data-dir` defaults to `~/.hush/runtime/`. The profile may hold
`credential-store`, `auth.json` and `models.json`; the files must be private and owned by the user. The default
does not load Pi resources or project-local `.hush/` settings. It does not require Pi installed.
Existing databases are not moved, and a Hush store and a Pi store cannot be opened interchangeably.
A manually started process prints one JSON ready record to stdout. The Go CLI can instead start a detached backend for
its default socket, with an internal startup pipe and no stdout/stderr inheritance. It waits for a compatible `hello`.
Normal storage is `history.sqlite`; the separate kernel-owned SQLite lock prevents simultaneous writers. A competing
launcher waits for the lock holder's socket, and a restart can recover work and remove a verified stale socket under the lock.
The app stays up while clients, runnable work, or deadlines exist. Once all three are absent for `--idle-ms`
(default 600000), it closes the Harness and releases the lock. SIGINT/SIGTERM close it explicitly. The negotiated
[`runtime:1` capability](../protocol/v1/runtime.md) provides the same orderly shutdown through `hush stop`; the next
managed CLI command can start the replacement. `hush.log` records fixed lifecycle/error labels, not prompts, provider
errors or credential material.

Flags include `--cwd`, `--profile hush|pi`, `--data-dir`, `--model provider/id`,
`--idle-ms`, and `--faux` (local scripted test model). `--config <file>` reads a JSON
object with the same option names without leading dashes; explicit flags win.
The Hush profile uses a pinned model default, the built-in coding tools, and no
project executable configuration. Tools run with the backend user's permissions;
a thread's working directory is not a sandbox.

## Standalone store compatibility and restore

A new standalone runtime initializes a `store-profile` JSON manifest with exactly
`profile: "hush"`, `format: 1`, `storeId`, `generation`, and decimal string `device`
and `inode` fields. `storeId` and `generation` are lowercase UUIDv4 values. The
manifest binds to `history.sqlite` by device/inode and a second hard link,
`history.identity`, which keeps the original inode reserved while the store is in
use. **Never open the alias in SQLite.** The history header carries SQLite
`application_id` `0x48555348` (`HUSH`), independent of Durable's schema bookkeeping.
This contract requires a private local filesystem supporting Unix file ownership,
SQLite locking, hard links, and stable device/inode numbers (the supported hosts
are macOS and Linux).

Hush checks the profile, format, file binding and header under the writer lock,
before SQLite opens existing history or Hush removes a stale socket. It will not
adopt a Pi database, a bare SQLite file, the old development `hush` marker, or an
incomplete bootstrap. It does not migrate, repair, or rewrite incompatible history.
Pi mode cannot open a marked Hush store. Keep rejected files for inspection; use a
separate fresh data directory if you don't need their history.

**Offline history restore:** Stop the backend and prevent autostart or other writers
throughout backup or restore. Take a backup of `history.sqlite` and `store-profile`
together after a clean shutdown with no `history.sqlite-wal`, `-shm`, or `-journal`
remaining. If sidecars remain, do not copy only the main file or delete them to force
a backup. Use only a known compatible, matched backup. To restore:

1. In a new private (0700) staging directory on the target local filesystem, copy
   the backup database to a **new** regular file `history.sqlite` with mode 0600.
   Create `history.identity` as a hard link to that new file. Do not copy socket,
   database sidecar or writer-lock files into staging.
2. Write a private `store-profile` from the backup's manifest. Keep `profile`,
   `format`, and `storeId`; replace `generation` with a **new** UUIDv4 every time.
   Set `device` and `inode` to the new file's device and inode as decimal strings
   (e.g. Python's `str(os.stat(path).st_dev)` and `str(os.stat(path).st_ino)`).
3. Move the stopped original runtime directory aside and put the staged directory
   in its place before permitting a new start. Keep the old directory separate as
   a fallback, not as a running clone. If you later restore that fallback, bind a
   new copy with a new generation then too. Hush does not restore credentials, Git
   worktree files, projects, or other external state. Start a **fresh attach**: do
   not carry over thread state, history anchors, or old submission keys.

An untouched same-store restart keeps both identity values. Replacing the store
with another independently initialized store changes the logical ID; restoring
an older checkpoint of the same store must change the generation. Copying or
editing live history, copying just WAL sidecars, and raw copies over the same
inode are unsupported. The file binding catches ordinary file replacement, not
in-place page rewrites or a filesystem/VM rollback that also rolls back the
binding and identity. Detecting arbitrary rollback needs a witness outside the
restored filesystem; Hush does not have one. This is not tamper protection from
other processes running as the same user.

The [store identity wire capability](../protocol/v1/store-identity.md) keeps
these storage identities separate from the backend's fresh `hello` epoch.

## Host-local Codex login

Use `hush auth login`, `hush auth status`, or `hush auth logout` on the backend host.
In a managed local Hush TUI, `/login` opens the same Codex sign-in flow.
Other providers need credentials configured separately on the backend host.
The equivalent bundle commands are `node dist/hush-backend.mjs auth login|status|logout`.
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
run `hush auth logout` to remove its Codex record. Do not remove the whole
`auth.json` if it contains credentials you need for other providers. Existing
backups are not scrubbed. Then select the new store and sign in again:

```sh
(umask 077; mkdir -p "$HOME/.hush"; printf 'keyring\n' > "$HOME/.hush/credential-store")
chmod 600 "$HOME/.hush/credential-store"
hush auth login
```

Use `file` in the same selector to switch back, with the same stop/logout steps.
There is no migration, mixing, or automatic fallback. A running backend
refuses new credential reads if the selection changes; restart it to adopt
the new setting. The Pi profile does not use this selector.

Keyring mode keeps the complete Codex OAuth record in one item scoped to the
Hush profile on the backend host. It uses the backend account's default
Keychain on macOS and an unlocked Secret Service in its local Linux D-Bus
session. Start Hush from that account/session. Hush does not start, unlock or
prompt for a keyring; when it is inaccessible, unlock or authorize access in
the OS and retry `hush auth status`. A manually started Node backend in
keyring mode needs `HUSH_KEYRING_HELPER` set to the trusted absolute path of
the installed `hush` binary. Managed starts and `hush auth` set this themselves.

A private SQLite lock coordinates sign-in, refresh and logout across processes
in the same profile. It contains no credentials. The keyring avoids plaintext
Hush token files and their backups; it is not a sandbox for tools running as
the backend user.

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

## Standalone system prompt

The backend bundle includes [`src/prompts/system.md`](src/prompts/system.md) as text. No prompt file is
required at startup. The default tells the model to answer questions after only the necessary checks,
without editing, and to keep explicitly requested changes scoped. Hush renders it first, followed by
instructions for the active tools and the thread's working directory, then optional personal instructions
from `~/.hush/APPEND_SYSTEM.md` on the **backend host**. It adds applicable `AGENTS.md` files in a separate
workspace-context section, ordered from the nearest Git root (`.git` directory or worktree marker file)
down to the working directory. Outside a repository, only the working directory's `AGENTS.md` applies.

Hush re-reads these optional files when rendering for the next provider request; no restart is needed.
It reads regular UTF-8 files up to 64 KiB each without following a symlink at the file itself, uses a
256 KiB workspace text budget, and
skips missing, unreadable, empty, invalid or oversized files. There is no full override, project `.hush/`
prompt, or Pi resource discovery in this profile. The text goes to the model provider, not into chat
history as a message. Prompt text guides the model; it cannot grant tool permissions or weaken host
policy. Loopback fixtures check the request contents, not whether a model follows the instructions.

## Opt-in Pi profile

Select `--profile pi`, `HUSH_PROFILE=pi`, or the legacy explicit `--agent-dir` or
`--pi-events` flags. Without `--agent-dir`, the Pi profile uses the effective Pi agent
directory (`PI_CODING_AGENT_DIR` or `~/.pi/agent`); its runtime data defaults to the
`hush/` subdirectory. Pi settings determine model, thinking, compaction, retry,
transport, and queue defaults for that profile only.

The Pi profile uses Pi 1.6's SettingsManager, ModelRuntime/ModelRegistry, resource loader and trust store. Models and
auth share the configured Pi files, and provider auth is resolved at request time. It reads available, installed Pi
resources/packages; it does not install or update packages, run Pi extension factories, or present a trust prompt.
Use Pi to make or revoke trust decisions. Untrusted project `.pi` settings, prompts, skills and MCP servers are
not loaded; the ancestor AGENTS.md context still follows Pi. Project resources and selected prompt sections are
cached per directory and invalidated by the environment watcher. Text starting with a prompt template name is expanded
before admission. The already-admitted key wins even if the template later changes. Pi's shipped documentation paths
can be selected with `PI_PACKAGE_DIR`; documentation files are not shipped in this JS artifact.

Configured MCP tools use Pi's config and transport/auth implementation backed by pi-mcp. The reference backend
exposes enabled non-hidden MCP tools directly to the model (no Pi codemode tool or MCP sign-in UI). Durable treats
these calls as replay-unsafe. A trusted local tool, model, or MCP service may deliberately return sensitive project
data. The raw core does not include provider auth/configuration or exception details; the local socket and coding
tools are **not** a sandbox from other processes running as the same user.

## Thread titles and worktrees

The default title is an excerpt of the first committed user message. For automatic
names, run a persistent backend with `--thread-title-model provider/small-model`
(or set `"thread-title-model"` in its config). This opts in to sending bounded excerpts
of the first prompt and answer to that configured provider after a committed answer.
No tools, system context, or image bytes are sent with that request. It is best-effort:
new native chats can claim one attempt in Durable, and do not replay it after restart.
Manual names win. Without the option, no title model is called.

In standalone Hush, grant consent explicitly on the backend host with
`hush trust grant --cwd PATH`. Use `status` to inspect it and `revoke` to remove it.
Records live in the private `~/.hush/workspace-trust-v1/` directory. Git grants
share the canonical common directory across linked checkouts; non-Git grants
apply to the exact resolved directory. Neither an attach nor repository files
nor Pi settings can grant Hush trust. The opt-in Pi profile uses Pi's own trust.
This grants consent for host operations, not a sandbox or file access. Checkout
rechecks the grant just before starting Git; revocation doesn't undo a checkout
already admitted or change files.

A native `thread new --worktree` request requires a trusted repository and a
committed HEAD. It allocates `worktrees/<unique-id>` inside
the private data directory, with a new `hush/<unique-id>` branch. The data directory
must be outside the source checkout and Git directory. Hush copies no uncommitted
files and does not switch the original branch. If Git or Durable fails mid-create,
inspect the directory before retrying; Hush will not delete potentially populated files.
Archive only changes visibility. Close/open and transcript forks never remove a Git
worktree, main or otherwise. See [the directory API](../protocol/v1/thread-directory.md).

## User-scheduled work

The standalone persistent backend runs explicit occurrences as Durable background
tasks. Busy targets receive a follow-up, not a steer. Late wake grace defaults to
15 minutes; set `--schedule-late-ms N` or `"schedule-late-ms": N` in backend config.
It applies only to user occurrences, not provider retries, tool polls, or other
Durable sleeps. When the backend is down, no prompt is dispatched until it returns.
An unattended restart may leave a late or unavailable occurrence waiting for a
decision. The service does not install or start an OS daemon.

## Checkpoints and file rewind

File checkpoints pin committed HEAD and require a Hush repository grant.
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
disconnects. Coding and MCP tools are disabled in this mode so there is no tool spool, image cache, or independent
tool process to leave behind. No transcript database, backup, or backend log is opened. A hard kill can leave the
empty socket file; the invoking client owns the private directory. Hush credentials are read without changes to the profile.
A private invocation requires a current OAuth token: it does not rotate and discard single-use
refresh tokens. Use a normal Hush run or sign in again if the token needs refresh. Explicit
Pi mode keeps the Pi profile's existing credential handling. With `--faux`, auth is always in memory.

The Go CLI starts its own bundle with `--parent-stdin`. This internal flag is valid only with `--no-session`.
The child holds the read end of a lifetime pipe; EOF shuts it down even if the CLI died before connecting.
The pipe never carries prompts or replies. The CLI reaps the child and removes the directory on normal exit.

## Ordered Pi events

With `--pi-events`, the reference host provides [`piEvents:1`](../protocol/v1/pi-1.6/pi-events.md)
for both SQLite and MemoryStorage. Without that flag, it serves the native protocol and its native capabilities. No changes
are required to upstream Pi Durable: the backend observes its public commit and task-hook
interfaces, wraps provider streams and tool output, and forwards these boundaries over the
socket. The Go client handles standard input/output and exit behavior.

A Pi binding exclusively owns a profile-created thread while attached and through its last
run's drain. Native state and history remain readable; native mutations of profile threads and
attachment to an already-active run are refused. After loss, a durable run can still finish;
reopening an idle profile thread starts a new event epoch and never replays the previous one.
Automatic compaction for these threads belongs to the profile, not the Durable background
compactor. The host deliberately projects only physical chat models and excludes private
headers and endpoint paths from model records. Images, Pi session import/tree operations,
and approvals are not advertised.

Run `npm run backend:check` under Node 22.19.0. It uses temporary Pi data, a loopback provider and stdio MCP fixture.
The backend also boots without node_modules under the official Node 22.19.0 Linux image with the network disabled.
