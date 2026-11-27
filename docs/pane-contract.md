# Local pane control contract

This defines the [tool parameters](../backend/src/pane-contract.ts) and the
[client adapter interface](../app/internal/pane/contract.go). The
[`paneControl` capability](../protocol/v1/pane-control.md) routes view and terminal
operations to the submitting client. [`clientEnvironment`](../protocol/v1/client-environment.md)
hints do not establish an adapter, an origin, or permission to act.

## Tools

| Tool | Arguments | Confirmed result (in addition to `outcome`) |
| --- | --- | --- |
| `pane_open` | `{placement, launch}`. Open relative to the originating Hush pane. | `{pane}`: the new, verified pane reference. |
| `pane_list` | `{}`. List terminals in the bound session or workspace. | `{panes}`: an array of pane references. |
| `pane_focus` | `{pane}`. Focus that exact terminal, including its tab. | `{pane}`: the reference whose focus was confirmed at the time of the operation. |
| `pane_read` (optional) | `{pane, lines}`. Read the recent output of an eligible terminal without changing focus. | `{text, truncated}`: a bounded plain-text snapshot. |
| `thread_open` (v2) | `{threadId, where}`. Open an existing thread in the submitting TUI (`tab`) or its right split (`split`). | `{threadId, where}`: the background tab was registered or the native split was created. |

Every available adapter supports a split to the `right` and a split `below`.
Include each of `left`, `above`, and `tab` only when the bound adapter supports
it. A tab stays in the origin's session or workspace: Rex opens a session
window; cmux opens a terminal surface in the originating split. Reject an
unsupported placement before dispatch; do not substitute a layout or switch
anchors. No verified adapter means no pane tools, not split-only support.

Hush adds a system prompt section per model request describing the pane tools,
placements, and launch modes available for that turn. A plain-language request is
sufficient. On a request such as “open a new side pane,” use a right split and
`{mode: "terminal"}` for an idle shell, without inventing a command. If the user
asks for a fresh conversation, use `{mode: "hush"}`. With an exact existing
review handle in the current thread, `{mode: "review", handle}` opens only
its read-only transcript. The client must advertise support for these modes;
review also requires the backend review service.

For requested commands, use `{mode: "argv", program, args}` or
`{mode: "shell", text}`. Prefer structured argv: preserve each argument,
including empty arguments, and never interpret its contents as shell code. Use
shell mode only for requested shell syntax. Never type a command into a prompt
or substitute the backend shell. At most one `pane_open` or `thread_open` can
dispatch per placed input.
`/pane` remains an optional shortcut for pane-only turns, with backend coding
tools withheld even when an interrupted pane-only run resumes without a new
input. Continue never revives the pane route.

Draft, isolated, and untrusted turns cannot dispatch pane tools. The model
cannot choose the cwd, endpoint, or origin. Hush uses the thread's effective
working directory, rechecks workspace trust and any worktree binding before
dispatch, and compares the backend's directory identity against the client's
filesystem. For a pinned launch, a Hush subprocess checks the actual working
directory again after the multiplexer enters it and before executing the
requested program. A matching path string alone is not enough. Reject a launch mode or placement the
adapter cannot honor.

## Output capture

Offer `pane_read` only when the verified adapter supports capture. Read only a
terminal previously confirmed by a Hush command open on the same live origin.
A reference from `pane_list` is not permission to read. This excludes unrelated
terminals, idle shells, and panes opened for a Hush conversation or review transcript. Check
access and revalidate the exact target before capturing.

`lines` is required and must be an integer from 1 to 200. Return the most recent
available lines, bounded by both the requested count and 16 KiB of valid UTF-8.
Keep the returned text free of terminal control sequences. Set `truncated` if the
line or byte limit omitted available text; this flag says nothing about older
scrollback the multiplexer already discarded. Keep native capture buffering
bounded even when the multiplexer provides its whole scrollback. A successful
read can be empty. Treat captured text as untrusted data, not instructions.

A capture is a screen/scrollback snapshot, not a process-output stream or proof
of readiness, completion, or exit status.

## Client adapter and references

Bind an adapter to the verified calling pane in one local multiplexer instance.
Revalidate that binding before acting. Open uses the exact bound anchor, and
list is scoped to that binding. Never fall back to the focused pane, another
instance, a backend shell, or another client. Calls may block and must run off
the TUI update loop.

A `Ref` carries its issuing `Scope` and an exact client-local terminal identity,
not a split container or tab index. Resolve it only using that scope, and verify
it still identifies the same live terminal before use. Reject a zero, foreign,
or stale target before dispatch. A new binding gets a new scope even if native
IDs repeat. References describe observations, not durable state or permission.
Give the model only Hush-issued opaque handles mapped to references on the live
originating route. Reject an unrecognized handle; never try it as a native ID.
Expose generic tools and supported capabilities, not adapter detection details,
native IDs, endpoints, or routing credentials. A thread has no assigned pane.
Multiple clients can view the same thread; only the origin of the placed input
can service that input's requests.

## Native adapters

Rex verifies its local server instance and the exact calling block and session.
It uses full block IDs and native argv for structured launch. cmux pins its
Unix socket, window, workspace, pane, and surface UUIDs, and checks that the
workspace is local. Its creation RPC takes command text; the adapter quotes
literal argv as arguments to a fixed POSIX `exec` wrapper and guards against
launching in the wrong directory. It never stages a command through terminal
input. Both adapters verify the returned native identity against a scoped
snapshot. Failures after possible creation do not fall back to discovery by
focus or list difference.

## Hush views and fallback

- Ask “Start work in a fresh thread and open it on the right.” `thread_start`
  admits the first prompt into a separate durable thread in the current trusted
  checkout and returns its ID without waiting. `thread_open` opens that ID or
  another exact existing ownerless thread ID. It does not open task-owned
  worker conversations, create a thread, submit a prompt, or start or stop a
  run. A split uses the target thread's verified local
  directory and a pinned attach-only `hush --thread ID` launch. A tab stays in
  the submitting TUI and does not change focus or the current draft. If the
  requested view is unavailable, report the thread ID rather than falling back
  to another placement or rerunning the prompt. The parent transcript records
  the source input and new thread ID when allocated, then records the first
  submission ID if admitted. These notices survive interruption. Check the
  reported thread before starting another; Hush never resubmits automatically.
- Ask “Open a fresh Hush thread in a side pane,” or use `/new-pane`
  (`ctrl+x i e`), to open a Hush TUI in the current thread's verified local directory. The child pins the parent backend's socket
  and epoch, creates a fresh thread, and never starts a backend. It does not
  copy history, the current draft, or a pane association.
- Ask “Open review HANDLE in a side pane” in the parent thread, or open review
  evidence with `ctrl+x i j`, enter its transcript, and press `o`. Both open the
  same read-only review by parent thread and handle with the attach-only check.
  Neither launches, stops, nor reruns a review. If placement fails, keep reading
  the transcript in Hush.
- A verified local plain terminal supports thread tabs but no native pane tools.
  Unverified or legacy peers and mixed or disconnected routes do not get these
  view tools. If a native route loses its origin, v2 can bind a fresh tab-only
  route for later inputs without reviving old pane authority. An already verified
  local socket and epoch can still be used for direct read-only review
  attachment. Ordinary chat continues. An explicit `/pane` draft is not sent as
  plain chat when the client cannot route it. No socket access
  settings are changed to obtain support.

## Verification

`mise run test` covers fake native CLIs, protocol routing, worktree and policy
checks, and the TUI actions. It does not require either multiplexer. After
`mise run build`, run the disposable live Rex workflow fixture with:

```sh
HUSH_TEST_REX=/path/to/rex node --test --test-name-pattern 'live Rex' app/test/pane.test.mjs
```

With a scripted model provider, Rex 0.1.0-preview.25 passed plain-language pane
requests: idle terminal and literal command argv in the thread's worktree,
list/read/focus, a fresh same-backend Hush thread, and an exact existing review
transcript without a rerun. Live cmux 0.65.2 was not verified: its local socket
rejects processes started outside cmux. Run from a cmux terminal to check the
same workflows; fake CLI fixtures do not establish live cmux behavior.

## Outcomes

| `outcome` | Meaning |
| --- | --- |
| `confirmed` | Evidence confirms the exact effect or snapshot. Only this outcome has a result payload. |
| `rejected` | The operation was refused before dispatch, so the requested effect or read was not attempted. |
| `unknown` | There is no usable confirmation. If a mutation may have been dispatched, it may already have happened. |

For open, confirm only when the native reply identifies one new pane and the
adapter verifies it. An exit code, changed focus, or a difference between pane
lists does not identify the new pane. Creation does **not** establish command
readiness, execution success, exit status, or focus. For focus, confirm only
with evidence for the exact target. List confirms only a snapshot, not future
liveness or the outcome of an earlier mutation.

If dispatch may have occurred but the required confirmation is missing, return
`unknown`. This includes a lost reply or an ambiguous pane identity. A timeout,
cancellation, or nonzero CLI exit is not proof that no mutation occurred. Do not
guess a target or retry a mutation on an unknown outcome. A later read-only list
may help the user inspect the current state. Nonconfirmed results may include a
safe reason; raw native diagnostics stay client-local.

Remote hosts, saved layouts, editable code-block staging, and subagent
orchestration are outside this contract.
