# Durable scheduled occurrences, version 1

Offer `"schedules":[1]` in hello. The persistent standalone Hush host selects it.
Pi, invocation-private, bare and unoffered hosts do not. Each request has the
requesting `threadId`; the target is fixed at creation, not inferred from the
client's selection. IDs for threads, submissions and entries are decimal Durable
IDs; an occurrence has a lowercase UUID v4.

| Request | Additional fields | Result |
| --- | --- | --- |
| `ext.schedules.add` | `targetThreadId`, `dueMs`, `zone`, `text`, optional `prerequisite` and `allowLate` | occurrence |
| `ext.schedules.list` | none | `{items: occurrence[]}`, newest first |
| `ext.schedules.run` | `id` | occurrence |
| `ext.schedules.reschedule` | `id`, `dueMs`, `zone` | occurrence |
| `ext.schedules.cancel` | `id` | occurrence |

`dueMs` is the positive absolute Unix time in integer milliseconds, no later
than the end of year 9999. `zone` is `UTC` or an explicit RFC3339 offset
(`+01:00`, for example); it is for display, never to reinterpret the instant.
`text` is nonblank and at most 32 KiB UTF-8. A prerequisite is
`{threadId,submissionId}` and must be the exact successful input/continue on
that thread. Idle, a passive write and an unavailable/unsuccessful dependency
do not count. `allowLate` defaults to false.

A result holds `id`, `threadId`, `targetThreadId`, `dueMs`, `zone`, `allowLate`,
`state`, optionally `prerequisite`, `eligibleMs`, `reason`, `submissionId` and
`reportEntryId`. The list retains the latest 128 rows per requesting thread.
Only terminal rows can roll out; the underlying occurrence and receipts remain.

States: `pending`, `needs_attention`, `admitted`, `done`, `failed`, `cancelled`.
Eligibility starts at the later of the due instant and the prerequisite's
committed success. A background Durable task parks late occurrences after the
configured window (15 minutes by default), unless late admission was explicit.
An archived or unavailable target also parks. A target without workspace tool
trust parks with reason `target_untrusted` and no submission. A manual run is for
a parked item: it bypasses lateness, not ownership, target availability,
workspace trust or prerequisite checks.
Reschedule changes only time and zone. Neither operation replays admitted work.
Once admitted, cancel returns the existing receipt and does not withdraw/stop it.
The admission and receipt are one Durable commit. IDs remain fixed across restart.

A terminal occurrence appends one display-only `hush.schedule-report` entry to
the requesting thread in the same commit as its report receipt. It is projected
as attributed output, not a user prompt or model-context input. It can be seen
by a core-only reader. The result is bounded text from the target's committed
answer; it doesn't copy tool results, images, or hidden reasoning. Failures report
fixed diagnostic reasons. The backend must be running to wake on time; no OS
service is installed. One target admission and one report do not give arbitrary
provider or tool effects exactly-once semantics.
