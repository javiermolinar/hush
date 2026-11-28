import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";
import type { Context } from "@earendil-works/chord/context";
import { AgentDoc, configure, defineDoc, defineDocFamily, defineExtension, defineTool, GenerationTask, hook, LiveDoc, ToolTask, UserEntry,
  type AgentChange, type ConversationId, type Cursor, type EntryId, type Harness, type SubmissionId, type TaskId, type Tx } from "@earendil-works/pi-durable";
import { environmentInputKind } from "../../server/src/client-environment.js";
import type { NativeHost } from "../../server/src/durable.js";
import { threadStartNoticeKind } from "../../server/src/pane-control.js";
import { WireError } from "../../server/src/wire.js";
import { paneOnlyTurn } from "./pane-intent.js";
import { bindExecutionWorkspace, type ExecutionWorkspace } from "./execution-workspace.js";
import type { PaneRouting } from "./pane-routing.js";
import { markCheckoutReady, ThreadPresentationDoc } from "./thread-presentation.js";
import type { ToolPolicy } from "./tool-policy.js";
import { workspaceGit, type WorkspaceTrust } from "./workspace-trust.js";
import { existingWorktree, type ThreadWorktrees } from "./worktrees.js";

export const threadStartName = "thread_start";
// Scoped to the new thread. Ordinary client inputs never acquire this pin.
export const threadStartRequest = "hush.thread-start";
export const StartedThread = defineDoc<{ first?: { submission: SubmissionId; workspace: ExecutionWorkspace } }>({
  kind: "hush.thread-start-input", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

const maxPrompt = 8192;
const genericError = new WireError("invalid_request").message;
const newWorktree = "new";
type Checkout = { cwd: string; branch: string };
// `head` is the new thread's latest entry when allocated. Any later entry
// means someone already used the thread, so its first prompt is not admitted.
type Start = { caller: TaskId; child: ConversationId; submission?: SubmissionId; checkout?: Checkout; forked?: boolean; head?: EntryId };
const Starts = defineDocFamily<{ start?: Start }, null>({
  kind: "hush.thread-start", family: true, version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

async function placedInput(tx: Tx, owner: ConversationId, caller: { tool: TaskId } | { generation: TaskId }) {
  const { run, tools } = await tx.doc(LiveDoc, owner);
  if (!run || run.afterRun || !run.inputs.length || (await tx.conversation(owner))?.owner) return;
  const generation = await tx.task(run.taskId);
  if (!generation || generation.abortRequested || generation.state.status === "terminal") return;
  if ("tool" in caller) {
    const task = await tx.task(caller.tool);
    if (task?.kind !== ToolTask.definition.name || task.conversationId !== owner || task.owner !== run.taskId ||
        task.abortRequested || task.state.status === "terminal" ||
        !tools?.some(t => t.taskId === caller.tool && t.name === threadStartName && !t.unoffered && t.status !== "done")) return;
  } else if (run.taskId !== caller.generation) return;
  // A steer or additional tool round does not create a new budget. The
  // generated first prompt cannot start another thread, even on a steer.
  const source = await tx.submission(run.inputs[0]!);
  if (source?.type !== "input" || source.status !== "placed" || source.conversationId !== owner) return;
  if (source.requestId === threadStartRequest && (await tx.doc(StartedThread, owner)).first?.submission === source.id) return;
  const entry = await tx.entry(source.entry);
  if (entry?.conversationId === owner && (UserEntry.is(entry) || entry.kind === environmentInputKind) && !await paneOnlyTurn(tx, owner))
    return { id: source.id, entry: source.entry };
}

const result = (value: object, isError = false) => ({ output: [{ type: "text" as const, text: JSON.stringify(value) }], isError });
const failure = (reason: string, child?: ConversationId) => result({ reason, ...(child !== undefined && { threadId: String(child) }) }, true);
const validBranch = (name: string) => name.isWellFormed() && Buffer.byteLength(name) <= 255 && !/[\s\p{Cc}]/u.test(name);
const slugSource = (prompt: string) => prompt.split(/\s+/).filter(Boolean).slice(0, 6).join(" ");

// History is the read-only recovery path even if this tool never reports
// back. Write each notice atomically with the effect it describes, without
// adding the notice or the child's first prompt to the parent's model context.
function recordStart(tx: Tx, owner: ConversationId, input: SubmissionId, { child, submission, checkout, forked }: Start) {
  const where = (checkout ? ` in worktree ${checkout.branch}` : "") + (forked ? " with this conversation's history" : "");
  const text = submission === undefined
    ? `Thread ${child} allocated${where} for input ${input}; first prompt not yet admitted.`
    : `Thread ${child} admitted its first prompt (${submission})${where} for input ${input}.`;
  return tx.appendEntry(owner, { kind: threadStartNoticeKind, data: { text, timestampMs: Date.now() } });
}

// The entry before the current input, across fork ancestry. A page can stop
// short at a fork boundary, so follow its cursor.
async function priorEntry(h: Harness, owner: ConversationId, before: EntryId, ctx: Context) {
  const conversation = await h.conversation(owner, ctx);
  let cursor: Cursor | undefined;
  do {
    const page = await conversation!.entries({ order: "descending", maxEntryId: before }, 2, cursor, ctx);
    const found = page.items.find(entry => entry.id !== before);
    if (found) return found.id;
    cursor = page.next;
  } while (cursor !== undefined);
}

// Uncommitted paths stay in the source; report how many were not copied.
async function uncommittedPaths(trust: WorkspaceTrust, cwd: string, signal: AbortSignal) {
  try {
    const out = await trust.withGit(cwd, signal, git =>
      workspaceGit(git.root.path, signal, "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=normal"));
    return out.split("\0").filter(Boolean).length;
  } catch { signal.throwIfAborted(); }
}

export type ThreadStartDeps = {
  harness: () => Harness; policy: ToolPolicy; trust: WorkspaceTrust;
  defaults: (cwd: string, ctx: Context) => Promise<AgentChange>;
  requireModel: () => NativeHost["requireModel"];
  worktrees: () => ThreadWorktrees | undefined;
  routing?: PaneRouting;
};

const moveHint = "A thread works in one checkout. To work in another checkout, do not cd in shell commands: call thread_start with " +
  `worktree ("${newWorktree}" creates a checkout from committed HEAD; a branch name selects an existing trusted checkout of this repository) ` +
  "and history: true to move this conversation there. Hush switches the user to the new thread. Uncommitted changes are not copied.";

export function threadStart({ harness, policy, trust, defaults, requireModel, worktrees, routing }: ThreadStartDeps) {
  return defineExtension({ name: "hush-thread-start", tools: [defineTool({ name: threadStartName,
    description: "When asked to start work in a separate conversation, create a durable thread and submit a self-contained first prompt. " +
      `Omit worktree for the current checkout; worktree "${newWorktree}" creates a checkout from committed HEAD; a branch name selects an existing ` +
      "trusted checkout of this repository. history: true continues this conversation there (a move) and switches the user to it. Returns " +
      "threadId and submissionId after admission; does not wait. For a delegated subtask that returns to this turn, use subagent instead.",
    callers: ["model"], replay: "unsafe", executionMode: "sequential", outputLimits: { maxBytes: 1024, maxLines: 1 },
    parameters: { type: "object", properties: { prompt: { type: "string", minLength: 1, maxLength: maxPrompt },
      worktree: { type: "string", minLength: 1, maxLength: 255 }, history: { type: "boolean" } },
      required: ["prompt"], additionalProperties: false } as const,
    async execute({ prompt, worktree, history = false }, api, ctx) {
      if (!prompt.trim() || !prompt.isWellFormed() || prompt.includes("\0") || Buffer.byteLength(prompt) > maxPrompt)
        return failure("The first prompt must be nonblank text of at most 8192 UTF-8 bytes.");
      if (worktree !== undefined && !validBranch(worktree))
        return failure(`The worktree must be "${newWorktree}" or a branch name without spaces of at most 255 bytes.`);
      const owner = api.conversationId, caller = { tool: api.taskId }, signal = ctx.abortSignal ?? new AbortController().signal;
      const input = await api.commit(tx => placedInput(tx, owner, caller), ctx);
      if (input === undefined) return failure("Starting a thread requires an ordinary placed user input.");
      const source = await policy.threadStartWorkspace(api, ctx);
      if (!source) return failure("The current checkout is not authorized for a new thread.");
      const h = harness();
      const forkAt = history ? await priorEntry(h, owner, input.entry, ctx) : undefined;
      // A moved conversation keeps its current model; a fresh thread uses the defaults.
      const parent = await api.agent(ctx);
      const agent: AgentChange = history ? { model: parent.model ?? null, thinkingLevel: parent.thinkingLevel ?? null } : await defaults(source.cwd, ctx);
      try { await requireModel()?.(agent.model ?? undefined); }
      catch {
        ctx.abortSignal?.throwIfAborted();
        return failure(history ? "This thread's model is unavailable. Check your Hush model and credentials."
          : "The default model is unavailable. Check your Hush model and credentials.");
      }
      let target: ExecutionWorkspace | undefined = worktree === undefined ? source : undefined;
      if (worktree !== undefined && worktree !== newWorktree) {
        try { target = await bindExecutionWorkspace(trust, await existingWorktree(trust, source.root, worktree, signal), signal); }
        catch {
          ctx.abortSignal?.throwIfAborted();
          return failure("The worktree must be an existing, trusted checkout of a branch in this repository.");
        }
      }
      ctx.abortSignal?.throwIfAborted();
      const authorized = async (tx: Tx) => (await placedInput(tx, owner, caller))?.id === input.id &&
        await policy.confirmDefaultWorkspace(tx, api, source, ctx);
      // Durable needs a committed conversation handle before it can admit
      // input. Reserve exactly one ID per source input, never a replacement.
      const allocate = (cwd: string, checkout?: Checkout & { repository: string }) => api.commit(async tx => {
        if (!await authorized(tx)) return;
        const row = await tx.doc(Starts, owner, String(input.id), null);
        if (row.start) return { ...row.start, ...(row.start.checkout && { checkout: { ...row.start.checkout } }) };
        const ownership = { kind: "ownerless" } as const;
        const child = forkAt === undefined ? await tx.createConversation({ ownership }) : await tx.forkConversation(owner, forkAt, { ownership });
        await configure(tx, child.id, { ...agent, cwd });
        if (checkout) (await tx.doc(ThreadPresentationDoc, child.id)).checkout = { ...checkout };
        // A fork's latest entry is its anchor; a new conversation has none.
        const allocated: Start = { caller: caller.tool, child: child.id, ...(checkout && { checkout: { cwd, branch: checkout.branch } }),
          ...(forkAt !== undefined && { forked: true, head: forkAt }) };
        row.start = allocated;
        await recordStart(tx, owner, input.id, allocated);
        return allocated;
      }, ctx);
      // Copy inside the transaction; a document value is unusable after it.
      let start = await api.commit(async tx => {
        const stored = (await tx.doc(Starts, owner, String(input.id), null)).start;
        return stored && { ...stored, ...(stored.checkout && { checkout: { ...stored.checkout } }) };
      }, ctx), uncommitted: number | undefined;
      if (!start && worktree === newWorktree) {
        const checkouts = worktrees();
        if (!checkouts) return failure("Managed worktrees are unavailable on this host.");
        const held: { start?: Start; reserving?: true } = {};
        try {
          // Publish a discoverable thread before Git writes the checkout.
          const created = await checkouts.create(source.cwd, signal, { name: async () => ({ fallback: slugSource(prompt) || "thread" }),
            grant: source.grant,
            reserve: async planned => {
              held.reserving = true;
              const allocated = held.start = await allocate(planned.cwd, { cwd: planned.cwd, branch: planned.branch, repository: planned.repository });
              if (allocated?.caller !== caller.tool || allocated.checkout?.cwd !== planned.cwd) throw new Error("not reserved");
            },
            // Not tied to this call: a stopped tool still records the finished checkout.
            created: async planned => {
              const child = held.start?.child;
              if (child !== undefined) await h.commit(tx => markCheckoutReady(tx, child, planned.cwd), BACKGROUND_CONTEXT);
            } });
          // A checkout Hush creates inherits the source's grant.
          target = await bindExecutionWorkspace(trust, created.cwd, signal);
          uncommitted = await uncommittedPaths(trust, source.cwd, signal);
        } catch (error) {
          ctx.abortSignal?.throwIfAborted();
          const reserved = held.start;
          if (!held.reserving) return failure("Could not create the worktree" + (error instanceof WireError && error.message !== genericError
            ? `: ${error.message}` : ". This checkout needs a Git repository with a committed HEAD."));
          if (reserved === undefined) return failure("The turn changed before a thread was allocated.");
          if (reserved.caller !== caller.tool) return failure("A thread was already started for this input.", reserved.child);
          return failure("Could not finish the worktree. The reserved thread's checkout may be incomplete; review it in /threads.", reserved.child);
        }
        start = held.start;
      } else if (!start) start = await allocate(target!.cwd);
      if (!start) return failure("The turn changed before a thread was allocated.");
      if (start.caller !== caller.tool) return result({ reason: "A thread was already started for this input.",
        state: start.submission === undefined ? "allocated" : "admitted", threadId: String(start.child),
        ...(start.submission !== undefined && { submissionId: String(start.submission) }) }, true);
      if (start.submission !== undefined) return receipt(start, start.submission);
      if (!target) return failure("The new thread exists but its first prompt was not submitted.", start.child);
      const child = await h.conversation(start.child, ctx);
      if (!child) return failure("The new thread exists but its first prompt was not submitted.", start.child);
      const workspace = target, reserved = start;
      try {
        const submitted = await api.commit(async tx => {
          if (!await authorized(tx)) return;
          const row = await tx.doc(Starts, owner, String(input.id), null);
          const stored = row.start;
          if (stored?.caller !== caller.tool || stored.child !== child.id) return;
          if (stored.submission !== undefined) return stored.submission;
          const selected = await tx.doc(AgentDoc, child.id);
          if (selected.cwd !== workspace.cwd || selected.model?.provider !== agent.model?.provider || selected.model?.modelId !== agent.model?.modelId ||
              selected.thinkingLevel !== (agent.thinkingLevel ?? undefined) || await tx.submissionByRequest(child.id, threadStartRequest) ||
              await tx.latestEntryId(child.id) !== stored.head) return;
          // A different checkout needs its own live grant at admission, and a
          // selected branch must still be checked out there.
          if (workspace !== source && !await policy.confirmWorkspace(workspace, ctx)) return;
          if (worktree !== undefined && worktree !== newWorktree &&
              await existingWorktree(trust, source.root, worktree, signal).catch(() => undefined) !== workspace.cwd) return "moved" as const;
          const pin = await tx.doc(StartedThread, child.id);
          // Literal text: this is not a client slash command or a routed input.
          const submission = await child.submit(tx, { type: "input", whenBusy: "reject", requestId: threadStartRequest, content: prompt });
          pin.first = { submission, workspace };
          stored.submission = submission;
          await recordStart(tx, owner, input.id, { ...stored });
          return submission;
        }, ctx);
        if (submitted === "moved") return failure(`The worktree is no longer on branch ${worktree}. The new thread has no first prompt.`, reserved.child);
        if (submitted === undefined) return failure("The new thread exists but its first prompt was not submitted.", reserved.child);
        h.resume();
        // A move switches the submitting TUI to the new thread. This is a
        // view change only; a missing or declined route leaves it unopened.
        // An unknown outcome may have opened it, so don't report either way.
        const outcome = history && routing ? (await routing.dispatchRouted({ op: "thread_open", threadId: String(reserved.child), where: "focus" }, api, ctx,
          tx => policy.confirmDefaultWorkspace(tx, api, source, ctx), threadStartName))?.outcome : undefined;
        const opened = outcome === undefined ? undefined : outcome === "unknown" ? "unknown" as const : outcome === "confirmed";
        return receipt(reserved, submitted, uncommitted, opened);
      } catch {
        ctx.abortSignal?.throwIfAborted();
        return failure("Could not confirm the first submission. Check this thread before starting another.", reserved.child);
      }
    },
  })], hooks: [hook(GenerationTask, { async beforeRequest({ messages }, api, ctx) {
    const tools = getCurrentTools(messages);
    if (!tools.some(t => t.name === threadStartName)) return;
    const index = messages.findLastIndex(m => m.role === "system"), system = messages[index];
    if (system?.role !== "system") return;
    const allowed = await harness().commit(async tx => {
      const input = await placedInput(tx, api.conversationId, { generation: api.taskId });
      return input !== undefined && !(await tx.doc(Starts, api.conversationId, String(input.id), null)).start;
    }, ctx);
    if (allowed) {
      const profile = await policy.prepare(api.conversationId, api.taskId, ctx);
      if (profile.kind === "default" && profile.workspace !== null) {
        const open = tools.some(t => t.name === "thread_open")
          ? " If the user asks to start work in a fresh thread and open it without moving this conversation, first call thread_start, then pass the returned threadId to thread_open." : "";
        return { messages: messages.with(index, { ...system, sections: { ...system.sections, hushThreadStart: moveHint + open } }) };
      }
    }
    return { messages: messages.with(index, { ...system, toolsAdded: system.toolsAdded?.filter(t => t.name !== threadStartName),
      toolsRemoved: [...system.toolsRemoved ?? [], { name: threadStartName }] }) };
  } })] });
}

function receipt(start: Start, submission: SubmissionId, uncommitted?: number, opened?: boolean | "unknown") {
  return result({ threadId: String(start.child), submissionId: String(submission), requestId: threadStartRequest,
    ...(start.checkout && { worktree: { ...start.checkout } }), ...(start.forked && { history: true }),
    ...(uncommitted && { uncommittedNotCopied: uncommitted }), ...(opened !== undefined && { opened }) });
}
