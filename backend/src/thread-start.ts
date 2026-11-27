import { getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";
import type { Context } from "@earendil-works/chord/context";
import { AgentDoc, configure, defineDoc, defineDocFamily, defineExtension, defineTool, GenerationTask, hook, LiveDoc, ToolTask, UserEntry,
  type AgentChange, type ConversationId, type Harness, type SubmissionId, type TaskId, type Tx } from "@earendil-works/pi-durable";
import { environmentInputKind } from "../../server/src/client-environment.js";
import type { NativeHost } from "../../server/src/durable.js";
import { threadStartNoticeKind } from "../../server/src/pane-control.js";
import { paneOnlyTurn } from "./pane-intent.js";
import type { ExecutionWorkspace } from "./execution-workspace.js";
import type { ToolPolicy } from "./tool-policy.js";

export const threadStartName = "thread_start";
// Scoped to the new thread. Ordinary client inputs never acquire this pin.
export const threadStartRequest = "hush.thread-start";
export const StartedThread = defineDoc<{ first?: { submission: SubmissionId; workspace: ExecutionWorkspace } }>({
  kind: "hush.thread-start-input", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

const maxPrompt = 8192;
type Start = { caller: TaskId; child: ConversationId; submission?: SubmissionId };
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
  if (entry?.conversationId === owner && (UserEntry.is(entry) || entry.kind === environmentInputKind) && !await paneOnlyTurn(tx, owner)) return source.id;
}

const result = (value: object, isError = false) => ({ output: [{ type: "text" as const, text: JSON.stringify(value) }], isError });
const failure = (reason: string, child?: ConversationId) => result({ reason, ...(child !== undefined && { threadId: String(child) }) }, true);
const receipt = (child: ConversationId, submission: SubmissionId) => result({
  threadId: String(child), submissionId: String(submission), requestId: threadStartRequest,
});

// History is the read-only recovery path even if this tool never reports
// back. Write each notice atomically with the effect it describes, without
// adding the notice or the child's first prompt to the parent's model context.
function recordStart(tx: Tx, owner: ConversationId, input: SubmissionId, { child, submission }: Start) {
  const text = submission === undefined
    ? `Thread ${child} allocated for input ${input}; first prompt not yet admitted.`
    : `Thread ${child} admitted its first prompt (${submission}) for input ${input}.`;
  return tx.appendEntry(owner, { kind: threadStartNoticeKind, data: { text, timestampMs: Date.now() } });
}

export function threadStart(harness: () => Harness, policy: ToolPolicy, defaults: (cwd: string, ctx: Context) => Promise<AgentChange>,
    requireModel: () => NativeHost["requireModel"]) {
  return defineExtension({ name: "hush-thread-start", tools: [defineTool({ name: threadStartName,
    description: "When explicitly asked to start work in a separate conversation, create a durable thread in the current checkout and submit a self-contained first prompt. Returns threadId and submissionId after admission; does not wait or open a view. For an ordinary delegated subtask that returns to this turn, use subagent instead.",
    callers: ["model"], replay: "unsafe", executionMode: "sequential", outputLimits: { maxBytes: 1024, maxLines: 1 },
    parameters: { type: "object", properties: { prompt: { type: "string", minLength: 1, maxLength: maxPrompt } },
      required: ["prompt"], additionalProperties: false } as const,
    async execute({ prompt }, api, ctx) {
      if (!prompt.trim() || !prompt.isWellFormed() || prompt.includes("\0") || Buffer.byteLength(prompt) > maxPrompt)
        return failure("The first prompt must be nonblank text of at most 8192 UTF-8 bytes.");
      const owner = api.conversationId, caller = { tool: api.taskId };
      const input = await api.commit(tx => placedInput(tx, owner, caller), ctx);
      if (input === undefined) return failure("Starting a thread requires an ordinary placed user input.");
      const workspace = await policy.threadStartWorkspace(api, ctx);
      if (!workspace) return failure("The current checkout is not authorized for a new thread.");
      const agent = await defaults(workspace.cwd, ctx);
      try { await requireModel()?.(agent.model ?? undefined); }
      catch {
        ctx.abortSignal?.throwIfAborted();
        return failure("The default model is unavailable. Check your Hush model and credentials.");
      }
      ctx.abortSignal?.throwIfAborted();
      const authorized = async (tx: Tx) => await placedInput(tx, owner, caller) === input &&
        await policy.confirmDefaultWorkspace(tx, api, workspace, ctx);
      // Durable needs a committed conversation handle before it can admit
      // input. Reserve exactly one ID per source input, never a replacement.
      const start = await api.commit(async tx => {
        if (!await authorized(tx)) return;
        const row = await tx.doc(Starts, owner, String(input), null);
        if (row.start) return { ...row.start };
        const child = await tx.createConversation({ ownership: { kind: "ownerless" } });
        await configure(tx, child.id, agent);
        const allocated: Start = { caller: caller.tool, child: child.id };
        row.start = allocated;
        await recordStart(tx, owner, input, allocated);
        return allocated;
      }, ctx);
      if (!start) return failure("The turn changed before a thread was allocated.");
      if (start.caller !== caller.tool) return result({ reason: "A thread was already started for this input.",
        state: start.submission === undefined ? "allocated" : "admitted", threadId: String(start.child),
        ...(start.submission !== undefined && { submissionId: String(start.submission) }) }, true);
      if (start.submission !== undefined) return receipt(start.child, start.submission);
      const h = harness(), child = await h.conversation(start.child, ctx);
      if (!child)
        return failure("The new thread exists but its first prompt was not submitted.", start.child);
      try {
        const submitted = await api.commit(async tx => {
          if (!await authorized(tx)) return;
          const row = await tx.doc(Starts, owner, String(input), null);
          const stored = row.start;
          if (stored?.caller !== caller.tool || stored.child !== child.id) return;
          if (stored.submission !== undefined) return stored.submission;
          const selected = await tx.doc(AgentDoc, child.id);
          if (selected.cwd !== agent.cwd || selected.model?.provider !== agent.model?.provider || selected.model?.modelId !== agent.model?.modelId ||
              selected.thinkingLevel !== (agent.thinkingLevel ?? undefined) || await tx.latestEntryId(child.id) !== undefined ||
              await tx.submissionByRequest(child.id, threadStartRequest)) return;
          const pin = await tx.doc(StartedThread, child.id);
          // Literal text: this is not a client slash command or a routed input.
          const submission = await child.submit(tx, { type: "input", whenBusy: "reject", requestId: threadStartRequest, content: prompt });
          pin.first = { submission, workspace };
          stored.submission = submission;
          await recordStart(tx, owner, input, { ...stored });
          return submission;
        }, ctx);
        if (submitted === undefined) return failure("The new thread exists but its first prompt was not submitted.", start.child);
        h.resume();
        return receipt(start.child, submitted);
      } catch {
        ctx.abortSignal?.throwIfAborted();
        return failure("Could not confirm the first submission. Check this thread before starting another.", start.child);
      }
    },
  })], hooks: [hook(GenerationTask, { async beforeRequest({ messages }, api, ctx) {
    const tools = getCurrentTools(messages);
    if (!tools.some(t => t.name === threadStartName)) return;
    const index = messages.findLastIndex(m => m.role === "system"), system = messages[index];
    if (system?.role !== "system") return;
    const allowed = await harness().commit(async tx => {
      const input = await placedInput(tx, api.conversationId, { generation: api.taskId });
      return input !== undefined && !(await tx.doc(Starts, api.conversationId, String(input), null)).start;
    }, ctx);
    if (allowed) {
      const profile = await policy.prepare(api.conversationId, api.taskId, ctx);
      if (profile.kind === "default" && profile.workspace !== null) {
        if (!tools.some(t => t.name === "thread_open")) return;
        return { messages: messages.with(index, { ...system, sections: { ...system.sections,
          hushThreadStart: "If the user asks to start work in a fresh thread and open it, first call thread_start, then pass the returned threadId to thread_open." } }) };
      }
    }
    return { messages: messages.with(index, { ...system, toolsAdded: system.toolsAdded?.filter(t => t.name !== threadStartName),
      toolsRemoved: [...system.toolsRemoved ?? [], { name: threadStartName }] }) };
  } })] });
}
