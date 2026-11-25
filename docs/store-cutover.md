# Managed store cutover and recovery

These instructions apply only to the pinned Linux arm64 migration into
[`v0.1.0-rc.1`](https://github.com/javiermolinar/hush/releases/tag/v0.1.0-rc.1).
Follow this procedure only after checking both executables and the target
bundle against the release evidence.

## Check the source and target

| | Verified configuration |
| --- | --- |
| Source | [Standalone candidate `standalone-rc-2026-11-23`](standalone-release-baseline.md), exact Linux arm64 executable `eec512d5bb9755f97f927ffcc2d8da77d59b834e1c5d3d32255ca82a1f17c21f`; Hush format 1 |
| Target | The `hush-linux-arm64.tar.gz` release asset. Check its archive and executable digests and the embedded bundle digest listed in the release notes. Hush format 2 (`hush-standalone-2`). |
| Runtime | Node.js **22.19.0** on the backend host. The general binary requires 22.19.0 or later; `store prepare` requires the exact pinned version. Both stores use Pi Durable 1.6.0 and SQLite schema 2. |
| Storage | Owner-private default `~/.hush/runtime` and `~/.hush/store-cutover` on the same supported local Linux arm64 filesystem (ext, XFS, Btrfs or overlayfs). |

The candidate pins Go, the npm lockfile (including Pi libraries and Chord), the
wire profiles and the embedded backend. A build of the same source commit is not
a substitute for the pinned binary. Check the archive and the extracted
executable against release evidence. Confirm the source executable's digest and
provenance separately: `--from` checks the executable, not the origin of an
arbitrary database. Common installed-artifact tests also ran on Linux amd64 and
macOS arm64/amd64; they do not establish migration on those targets. Tests used
isolated homes and scripted providers, not live provider authentication.

There is no automatic migration. Pi databases, arbitrary SQLite, custom data
paths, unfamiliar tasks or wake definitions, and source managed worktrees are
not eligible. A live writing backend or a source with a running tool is refused.
`store inspect` lists the supported source tasks in `sourceTasks`:
`pi.compaction` v1, `pi.generation` v3, and `pi.tool` v4. This list is a
version check, not approval to admit every task of those kinds. The actual
predecessor produced the queued steer and in-flight compaction in the tested
migration. The parked `read` was a separate synthetic seed. The due schedule
was created only after activation by the target; it is not a format-1 schedule
import. None of these fixtures establishes product approvals, arbitrary task
import, or exactly-once tool effects.

## Quiesce, prepare, activate

1. Work on the backend host as the user who owns the store. Keep separate
   absolute paths to the pinned `PREDECESSOR` and `TARGET` binaries; do not
   replace the old executable in place. Use the same home as the source. Unset
   `HUSH_SOCKET`, `HUSH_PROFILE` and `HUSH_BACKEND`; do not pass a bundle or socket
   override. Use Node 22.19.0 and check both executables against the release notes.
2. Pause anything that launches or attaches to this store, including supervision,
   frontends and scripts. Commands such as managed `hush status` may start a
   backend. Reconcile active work and let tools finish; wait for the matching
   predecessor backend to exit or stop it under its supervisor with `SIGTERM`.
   The pinned predecessor does **not** have `hush stop`. Preserve the runtime
   directory and its writer lock. The cutover commands take exclusive locks and
   fail if the predecessor still owns the store.
3. Inspect without starting the backend. Expect `phase: "not-prepared"` and
   `format: 1`. Compare the reported source SHA and target bundle SHA before
   continuing. Prepare, then re-inspect:

   ```sh
   "$TARGET" store inspect
   "$TARGET" store prepare --from "$PREDECESSOR"
   "$TARGET" store inspect
   ```

   Expect `phase: "prepared"` and `rollbackEligible: true`. Save the transaction,
   snapshot digest and source identity. Both executables are fenced from opening
   the store while prepared. A successful process exit alone does not establish
   the phase. Inspect on any interruption; do not skip to activation.
4. If proceeding, commit the gate and inspect again:

   ```sh
   "$TARGET" store activate
   "$TARGET" store inspect
   ```

   Wait for `phase: "committed"`, `format: 2`, and `rollbackEligible: false`.
   Activation replaces the bound database and changes the store generation. It
   does not start a listener. Start the **same verified target** from the normal
   working directory and open fresh clients. Starting the backend can resume
   admitted and due work. Review state and history before accepting new work;
   do not resend an unresolved tool call or scheduled action as a smoke test.

## If interrupted

Inspect using the same target with clients and launchers still paused:

| Observation | Action |
| --- | --- |
| `not-prepared`, `format: 1` | Nothing crossed the gate. Once the verified source and original binding are known intact, restart that source or prepare again deliberately. |
| `prepared`, `rollbackEligible: true` | Either activate the verified target or explicitly run `"$TARGET" store rollback`. Inspect for `rolled-back`, `format: 1`, then reopen with the exact predecessor and fresh clients. |
| `restored` | A prior rollback already made its restore decision. Rerun `store rollback` with the same target only to finish publishing the verified source binding. Wait for `rolled-back`; if refused, stop and preserve the files. |
| `committed`, format not yet `2` | Rollback is forbidden. `store activate` with the same target can finish publishing the manifest only if the installed database, binding and gate still match. If refused, preserve the state and escalate. |
| `committed`, `format: 2` | Recover forward with the exact activated bundle and existing store. Inspect durable history and submission receipts, schedule reports, interrupted tools and actual external effects. An old launcher is not a fallback writer. |
| `unknown`, missing/unreadable state, ineligible prepared archive, or a refusal that cannot be explained by an active writer | Keep launchers paused. Preserve the live runtime, outside gate, matched archive and evidence together. Use the transaction and exact artifacts to investigate offline with the maintainer. There is no automatic restore or supported in-place repair for this state. |

Do not open `history.identity` in SQLite, rename the runtime to make an
executable initialize a new store, copy a snapshot over history, modify the gate,
or start a backup as another runnable store. Do not pair this runtime with a
stale VM or filesystem snapshot that also rewinds its gate. This release binds
managed startup to the activated bundle SHA; a different or newer bundle is
not a recovery procedure. If the exact bundle and on-disk state cannot be
matched, preserve them for investigation rather than bypassing the fence.

## What is stored and what is not restored

- `~/.hush/runtime/history.sqlite` holds durable records. Its `-wal`/`-shm`
  sidecars, `history.identity`, `store-profile`, and stable `owner.sqlite` belong
  to the runtime. The socket and backend log are also local to the runtime.
- `~/.hush/store-cutover/state.json` records the outside decision. The private
  transaction directory holds `snapshot.sqlite` and `source-manifest.json`.
  The snapshot includes committed WAL content at prepare time and needs no
  sidecars. It is sensitive transcript data, not a runnable store or a general
  backup. It does not include provider credentials or settings, the writer
  interlock, logs, worktrees, workspace files, or external tool effects.
- Archive/unarchive changes thread presentation. It neither deletes nor scrubs
  the transcript, Git files, backups, or service-side data. Restore does not undo
  an external action. A replay-unsafe tool can have committed an external effect
  before an interruption was recorded; confirm the effect before acting again.

## Reattach and reconcile

A normal backend restart gets a new server epoch but retains store ID and
generation. Activation or supported rollback changes generation. Discard the
old client's per-thread state, history anchors, and submission keys on a
continuity error. Open a fresh TUI or SDK client, rediscover the intended thread
and read current durable history and receipts before making a new admission. Do
not copy previously admitted input into a new draft for automatic submission.

Two contracts apply independently:

1. **Native state** — patches apply only to their subscription, epoch and base
   revision. A gap, stale base or disconnect requires a fresh snapshot, not
   replay of patches. A snapshot describes current state, not every event.
2. **Durable history** — page committed entries by a scoped cursor or entry
   anchor after verifying [store identity](../protocol/v1/store-identity.md).
   The live window is bounded and is not the full transcript. Fill gaps with
   history queries, not with live revisions. A submission receipt confirms
   admission, not placement or settlement. Use `submission.get` or
   `submission.wait` for the exact input. Same-key native retry is valid only
   within verified continuity with the same payload; never use an old key after
   restore.

## Headless modes, private runs and remote attachment

`store` refusal exits 1 with a diagnostic. `store inspect` emits JSON and can
exit 0 with `phase: "unknown"`; check the fields. Native `--print` exits 0 on a
completed answer, or 1 on a failed invocation; `--no-wait` can succeed at
admission without an answer. Transport failure exits 1; a caught signal exits
`128 + signal` (for example, SIGINT 130).

`--no-session` starts a tool-free in-memory host tied to one CLI invocation. It
will not continue a durable thread, create a transcript database, payload log,
backup or media cache, or leave detached work after the client dies. A hard kill
can leave an empty socket path; normal exit reaps the host and cleans up. This
is a local persistence guarantee, not a claim about what providers retain.

`--socket` and `HUSH_SOCKET` are attach-only; they do not start a remote backend
or change its credentials. Use an authorized Unix socket on the backend host
(or a private, authenticated tunnel to it). Run `hush auth login|status|logout`
on that host as its Hush user; credentials never travel over the Hush protocol.
Store admin commands are similarly host-local. Native Hush uses its own profile
without requiring a Pi installation or borrowing Pi credentials.
