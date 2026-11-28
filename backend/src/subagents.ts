import { join } from "node:path";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";
import { AgentDoc, AssistantEntry, configure, defineDocFamily, defineExtension, defineTask, defineTool, GenerationTask, hook, LiveDoc,
  SubmissionSettled, ToolTask, UsageDoc, UserEntry, type Agent, type ConversationId, type Cursor, type EntryRecord, type Extension, type Harness,
  type SubmissionId, type TaskId, type Tx } from "@earendil-works/pi-durable";
import { environmentInputKind } from "../../server/src/client-environment.js";
import { projectUsage, type NativeHost } from "../../server/src/durable.js";
import { planChatKind, planThinkingLevels, type PlanThinking } from "../../server/src/plan.js";
import { subagentToolName, type SubagentEvent, type SubagentOverviewQuery, type SubagentQuery, type SubagentRow, type SubagentStatus, type SubagentsHost } from "../../server/src/subagents.js";
import { WireError } from "../../server/src/wire.js";
import { delegateGitName } from "./delegate-git.js";
import { planStepDelegate } from "./plan.js";
import { bindExecutionWorkspace, delegateDirectory, requireExecutionWorkspace, requireSubdirectory, sameExecutionWorkspace,
  type ExecutionWorkspace } from "./execution-workspace.js";
import { admitToolProfile, type ToolPolicy } from "./tool-policy.js";
import { ChangePulse, WorkspaceRevisions } from "./workspace-changes.js";
import type { WorkspaceTrust } from "./workspace-trust.js";
import { existingWorktree } from "./worktrees.js";

const workerKind = "hush.subagent";
const maxStarts = 5, maxPrompt = 8192, logBytes = 16 * 1024;
const childKey = (task: TaskId) => `subagent:${task}`;
const settledKey = "settled";
const modelHint = "Use subagent without waiting for the user to ask when delegating a focused task or getting an independent second opinion would improve the answer. " +
  "For example, a review might benefit from separate passes on security and design, or from a second look at one risky change. Choose the scope and number of workers based on the task. " +
  "Give it a self-contained prompt. Workers are read-only by default, with file reads and, when the repository root is trusted, read-only Git (status, diff, show, log); " +
  "set read_only: false for work that needs edits or shell tools. Set pwd to start in a subdirectory of the checkout, and thinking to change the reasoning effort. " +
  "For independent read-only tasks, issue the calls together in one response so they can run in parallel. Writable workers run one at a time. " +
  "The parent resumes after the results are ready. You can start at most five per user turn; handle quick checks yourself. " +
  "In a plan run, delegate a step that has a delegate specification by passing only its step ID.";

type Input = { prompt: string; read_only?: boolean; worktree?: string; pwd?: string; thinking?: PlanThinking };
type Args = Partial<Input> & { step?: string };
// `pwd` is relative to the workspace's cwd; omitted for the checkout itself.
type Intent = { prompt: string; read_only: boolean; worktree?: string; pwd?: string; turn: SubmissionId; source: ExecutionWorkspace;
  workspace: ExecutionWorkspace; model: NonNullable<Agent["model"]>; thinking: Agent["thinkingLevel"] };
type Validate = NonNullable<NativeHost["validateSelection"]>;
const delegateCwd = ({ workspace, pwd }: Intent) => pwd ? join(workspace.root, pwd) : workspace.root;
type Phase = { phase: "open" } | { phase: "submit"; child: ConversationId }
  | { phase: "settle"; child: ConversationId; submission: SubmissionId };
type Finish = { status: SubagentStatus; detail?: string };
type Spend = ReturnType<typeof projectUsage>;
type PastSpend = { through: number; usage: Spend };
type SpendCache = { past: PastSpend; latest: Map<string, Spend>; turn?: SubmissionId };

function addSpend(total: Spend, spend: Spend) {
  for (const key of Object.keys(total) as (keyof Spend)[]) total[key] += spend[key];
}
type Worker = { task: TaskId<Finish>; caller: TaskId; child?: ConversationId; submission?: SubmissionId };
const Turns = defineDocFamily<{ workers: Worker[] }, null>({
  kind: "hush.subagent-turn", family: true, version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({ workers: [] }), checkpointWhen: () => true,
});
const complete = (status: SubagentStatus, detail?: string) => ({ status: "terminal", outcome: { status: "completed",
  result: { status, ...(detail && { detail }) } } } as const);
const unresolved = () => complete("failed", "Subagent records could not be matched.");
const output = (text: string, isError = false) => ({ output: [{ type: "text" as const, text }], ...(isError && { isError }) });

// Match a direct tool in an ordinary placed user turn or a plan run. A steer,
// tool round, inputless Continue, or nested call can't make a new budget.
async function parentTurn(tx: Tx, owner: ConversationId, caller: { tool: TaskId } | { generation: TaskId }) {
  const parent = await tx.conversation(owner);
  if (!parent || parent.owner) return;
  const { run } = await tx.doc(LiveDoc, owner);
  if (!run?.inputs.length) return;
  if ("tool" in caller) {
    const task = await tx.task(caller.tool);
    if (task?.kind !== ToolTask.definition.name || task.conversationId !== owner || task.owner !== run.taskId ||
        task.abortRequested || task.state.status === "terminal" || (task.input as { kind?: string })?.kind !== "model") return;
  } else if (run.taskId !== caller.generation) return;
  const input = await tx.submission(run.inputs[0]!);
  if (input?.type !== "input" || input.status !== "placed" || input.conversationId !== owner) return;
  const first = await tx.entry(input.entry);
  if (first?.conversationId !== owner || !UserEntry.is(first) && first.kind !== environmentInputKind && first.kind !== planChatKind) return;
  return { id: input.id, cwd: (await tx.doc(AgentDoc, owner)).cwd };
}

function validInput({ prompt, worktree, pwd, thinking }: Input) {
  return !!prompt.trim() && prompt.isWellFormed() && !prompt.includes("\0") && Buffer.byteLength(prompt) <= maxPrompt &&
    (worktree === undefined || worktree.isWellFormed() && Buffer.byteLength(worktree) <= 255 && !/[\s\p{Cc}]/u.test(worktree)) &&
    (pwd === undefined || pwd.isWellFormed() && Buffer.byteLength(pwd) <= 1024) &&
    (thinking === undefined || planThinkingLevels.includes(thinking));
}

function publicText(message: { content: readonly { type: string; text?: string }[] } | undefined) {
  return (message?.content ?? []).flatMap(block => block.type === "text" && block.text ? [block.text] : []).join("")
    .toWellFormed().replace(/[\p{Cc}\p{Zl}\p{Zp}\u202a-\u202e\u2066-\u2069]/gu, c => c === "\n" || c === "\t" ? c : " ");
}
function entryText(entry: EntryRecord, finalPartial: boolean) {
  const message = AssistantEntry.is(entry) ? entry.model?.[0] : undefined;
  return message?.role === "assistant" && (["stop", "toolUse", "length"].includes(message.stopReason) ||
    finalPartial && message.stopReason === "aborted") ? publicText(message) : "";
}

async function childSubmission(tx: Tx, parent: ConversationId, row: Worker) {
  if (!row.child || !row.submission) return;
  const child = await tx.conversation(row.child);
  if (child?.parent || child?.owner?.taskId !== row.task || child.owner.conversationId !== parent) return;
  const record = await tx.submission(row.submission);
  if (record?.conversationId === child.id && record.type === "input" && record.requestId === childKey(row.task)) return record;
}

async function readLog(tx: Tx, parent: ConversationId, row: Worker, live: boolean) {
  const input = await childSubmission(tx, parent, row);
  if (!input?.entry || !row.child) return { text: "", truncated: false };
  const page = await tx.scanEntries({ conversationId: row.child, minEntryId: input.entry,
    ...(input.status === "done" && { maxEntryId: input.answer }), order: "descending" }, 64);
  const lines = page.items.toReversed().filter(entry => entry.conversationId === row.child)
    .map(entry => entryText(entry, input.status === "unanswered" && entry.id === page.items[0]?.id));
  if (live && input.status === "placed") lines.push(publicText((await tx.doc(LiveDoc, row.child)).generation?.message));
  const bytes = Buffer.from(lines.filter(Boolean).join("\n"));
  let offset = Math.max(0, bytes.length - logBytes);
  while (offset && (bytes[offset]! & 0xc0) === 0x80) offset++;
  return { text: bytes.toString("utf8", offset), truncated: !!page.next || offset > 0 };
}

export class Subagents implements SubagentsHost {
  readonly #revisions = new WorkspaceRevisions();
  // Rebuild from Durable on the first observation after restart or eviction.
  // Only terminal workers contribute to a cached amount.
  readonly #spend = new Map<ConversationId, SpendCache>();

  #spendFor(owner: ConversationId) {
    let cache = this.#spend.get(owner);
    if (!cache) {
      if (this.#spend.size >= 32) this.#spend.delete(this.#spend.keys().next().value!);
      this.#spend.set(owner, cache = { past: { through: 0, usage: projectUsage(undefined) }, latest: new Map() });
    }
    return cache;
  }
  readonly #worker;
  readonly extension: Extension;

  constructor(private readonly harness: () => Harness, private readonly trust: WorkspaceTrust,
      private readonly policy: ToolPolicy, private readonly promptExtension: Extension,
      private readonly coding: Extension, private readonly git?: Extension, private readonly validate?: Validate) {
    this.#worker = this.#definition();
    this.extension = this.#extension();
  }

  #extension() {
    const service = this;
    const tool = defineTool({ name: subagentToolName, description: "Run one task in a fresh backend subagent and wait for its public log. " +
      "read_only defaults to true. worktree is the branch of an existing trusted checkout in the same repo; pwd is a directory inside the checkout; " +
      "thinking overrides this thread's thinking level. In a plan run, pass only step to run a selected step's delegate specification.",
      callers: ["model"], replay: "unsafe", executionMode: "parallel", outputLimits: { maxBytes: logBytes + 512, maxLines: logBytes },
      parameters: { type: "object", properties: { prompt: { type: "string", minLength: 1, maxLength: maxPrompt },
        read_only: { type: "boolean" }, worktree: { type: "string", minLength: 1, maxLength: 255 },
        pwd: { type: "string", minLength: 1, maxLength: 1024 }, thinking: { type: "string", enum: planThinkingLevels },
        step: { type: "string", pattern: "^[A-Za-z0-9_-]+$", maxLength: 64 } },
        additionalProperties: false } as const,
      async execute(given: Args, api, ctx) {
        const owner = api.conversationId, caller = { tool: api.taskId };
        let args: Input;
        if (given.step !== undefined) {
          // The step's specification comes from the run's pinned revision.
          if (Object.keys(given).length !== 1) return output("Pass only step to delegate a plan step.", true);
          const step = given.step, spec = await api.commit(tx => planStepDelegate(tx, owner, step), ctx);
          if ("error" in spec) return output(spec.error, true);
          args = spec;
        } else if (given.prompt === undefined) return output("Pass a prompt, or a step in a plan run.", true);
        else args = { ...given, prompt: given.prompt };
        if (!validInput(args)) return output("Invalid subagent prompt, worktree branch, pwd or thinking level.", true);
        const readOnly = args.read_only ?? true;
        const first = await api.commit(tx => parentTurn(tx, owner, caller), ctx);
        if (!first) return output("Subagent requires an ordinary placed user turn.", true);
        const agent = await api.agent(ctx);
        if (!agent.model) return output("The parent has no available model selection.", true);
        if (args.thinking !== undefined && args.thinking !== agent.thinkingLevel) {
          try {
            if (!service.validate) throw new Error("unavailable");
            await service.validate(agent.model, args.thinking);
          } catch {
            ctx.abortSignal?.throwIfAborted();
            return output(`Thinking level ${args.thinking} is not available for this thread's model.`, true);
          }
        }
        const source = await service.policy.subagentWorkspace(api, ctx);
        if (!source) return output("The parent workspace is not authorized for subagents.", true);
        let workspace: ExecutionWorkspace;
        try {
          const cwd = args.worktree ? await existingWorktree(service.trust, source.root, args.worktree, ctx.abortSignal!) : source.cwd;
          workspace = cwd === source.cwd ? source : await bindExecutionWorkspace(service.trust, cwd, ctx.abortSignal);
          await requireExecutionWorkspace(service.trust, source, ctx.abortSignal);
        } catch {
          ctx.abortSignal?.throwIfAborted();
          return output("Subagent requires a trusted workspace and an existing trusted worktree in the same repository.", true);
        }
        const pwd = args.pwd === undefined ? "" : delegateDirectory(workspace, args.pwd);
        try {
          if (pwd === undefined) throw new Error("outside");
          await requireSubdirectory(workspace, pwd);
        } catch {
          ctx.abortSignal?.throwIfAborted();
          return output("pwd must be an existing directory inside the subagent's checkout.", true);
        }
        const intent: Intent = { prompt: args.prompt, read_only: readOnly, ...(args.worktree && { worktree: args.worktree }),
          ...(pwd && { pwd }), turn: first.id, source, workspace, model: { ...agent.model }, thinking: args.thinking ?? agent.thinkingLevel };
        const task = await api.commit(async tx => {
          const current = await parentTurn(tx, owner, caller);
          if (current?.id !== first.id || current.cwd !== first.cwd || !await service.policy.matchesSubagentWorkspace(tx, api, source)) return;
          const turn = await tx.doc(Turns, owner, String(first.id), null);
          const prior = turn.workers.find(w => w.caller === api.taskId);
          if (prior) return prior.task;
          if (turn.workers.length >= maxStarts) return "limit" as const;
          const id = await tx.createTask(service.#worker, intent, { ownership: { kind: "task", taskId: api.taskId }, abandonOnRestart: true });
          turn.workers.push({ task: id, caller: api.taskId });
          return id;
        }, ctx);
        if (task === "limit") return output("Subagent start limit reached: five per user turn.", true);
        if (task === undefined) return output("The parent turn or workspace changed.", true);
        // Don't park this replay-unsafe call. Durable aborts this owned worker
        // on restart, so neither its provider request nor writes are replayed.
        const { state: { outcome } } = await api.waitForTask(task, ctx);
        const row = (await service.harness().snapshot(Turns, owner, String(first.id), ctx))?.workers
          .find(w => w.task === task && w.caller === api.taskId);
        if (!row) return output("Subagent records could not be matched.", true);
        const result = outcome.result ?? { status: "failed", detail: "The subagent did not settle normally." };
        const log = await api.commit(tx => readLog(tx, owner, row, false), ctx);
        const text = (log.truncated ? "[Earlier subagent output omitted]\n" : "") + log.text;
        return result.status === "ok" ? output(text) : output([result.detail, text].filter(Boolean).join("\n"), true);
      },
    });
    return defineExtension({ name: "hush-subagents", tools: [tool], tasks: [this.#worker], hooks: [
      hook(GenerationTask, { async beforeRequest({ messages }, api, ctx) {
        const tools = getCurrentTools(messages);
        if (!tools.some(t => t.name === subagentToolName)) return;
        const last = messages.findLastIndex(m => m.role === "system"), msg = messages[last];
        if (msg?.role !== "system") return;
        const allowed = await service.harness().commit(tx => parentTurn(tx, api.conversationId, { generation: api.taskId }), ctx);
        if (allowed) return { messages: messages.with(last, { ...msg, sections: { ...msg.sections, hushSubagents: modelHint } }) };
        return { messages: messages.with(last, { ...msg, toolsAdded: msg.toolsAdded?.filter(t => t.name !== subagentToolName),
          toolsRemoved: [...msg.toolsRemoved ?? [], { name: subagentToolName }] }) };
      } }),
    ] });
  }

  // Git reads history and objects for the whole repository, so only a grant
  // for the repository root gets the git tool.
  async #rootGrant(w: ExecutionWorkspace, signal: AbortSignal) {
    try { return await this.trust.withGit(w.root, signal, identity => identity.root.path === identity.cwd.path); }
    catch { signal.throwIfAborted(); return false; }
  }

  async #workspaceFailure({ source, workspace, worktree, pwd }: Intent, signal: AbortSignal) {
    try {
      if (worktree && await existingWorktree(this.trust, source.root, worktree, signal) !== workspace.root)
        return complete("failed", "The selected worktree changed.");
      // The Git probe checks current trust, not the original grant or directory pins.
      await requireExecutionWorkspace(this.trust, source, signal);
      if (!sameExecutionWorkspace(source, workspace)) await requireExecutionWorkspace(this.trust, workspace, signal);
      await requireSubdirectory(workspace, pwd);
    } catch {
      signal.throwIfAborted();
      return complete("failed", "The workspace or worktree is no longer trusted or available.");
    }
  }

  #definition() {
    const rowFor = async (tx: Tx, owner: ConversationId, task: TaskId, caller: TaskId | undefined, turn: SubmissionId) =>
      (await tx.doc(Turns, owner, String(turn), null)).workers.find(row => row.task === task && row.caller === caller);
    return defineTask<Intent, Phase, Finish>({ name: workerKind, version: 1, initial: () => ({ phase: "open" }), phases: {
      open: async (task, runtime, ctx) => {
        const { input } = task, owner = runtime.conversationId;
        // Admission appends in one transaction, so dependencies only point
        // backward. Writers are exclusive within this parent's batch; readers
        // can overlap each other but not an earlier writer. Wait for terminal
        // tasks, not merely for answers: their owned work may still be draining.
        const blockers = await this.harness().commit(async tx => {
          const { workers } = await tx.doc(Turns, owner, String(input.turn), null);
          const index = workers.findIndex(row => row.task === task.id && row.caller === task.owner);
          if (index < 0) return;
          const waiting: TaskId[] = [];
          for (const row of workers.slice(0, index)) {
            const prior = await tx.task(row.task);
            const pin = prior?.input as Intent | undefined;
            if (prior?.kind !== workerKind || prior.conversationId !== owner || prior.owner !== row.caller || pin?.turn !== input.turn) return;
            if (prior.state.status !== "terminal" && (!input.read_only || !pin.read_only)) waiting.push(prior.id);
          }
          return waiting;
        }, ctx);
        if (!blockers) return runtime.commit(unresolved, ctx);
        await Promise.all(blockers.map(id => runtime.waitForTask(id, ctx)));
        const failure = await this.#workspaceFailure(input, runtime.signal);
        if (failure) return runtime.commit(() => failure, ctx);
        const offerGit = input.read_only && !!this.git && await this.#rootGrant(input.workspace, runtime.signal);
        return runtime.commit(async tx => {
          const row = await rowFor(tx, owner, task.id, task.owner, input.turn);
          if (!row || row.child !== undefined || row.submission !== undefined) return unresolved();
          const child = await tx.createConversation({ ownership: { kind: "task", taskId: task.id } });
          await admitToolProfile(tx, child, { kind: input.read_only ? "read-only" : "subagent-write", workspace: input.workspace,
            ...(input.pwd && { pwd: input.pwd }) });
          const git = offerGit ? [this.git!] : [];
          const tools = [...this.coding.tools?.filter(t => !input.read_only || t.name === "read") ?? [], ...git.flatMap(ext => ext.tools ?? [])];
          await configure(tx, child.id, { extensions: [this.promptExtension, this.coding, ...git], tools, modelTools: tools,
            cwd: input.pwd ? join(input.workspace.cwd, input.pwd) : input.workspace.cwd, model: input.model, thinkingLevel: input.thinking, maxTurns: null, compaction: { enabled: false },
            instructions: "You are a subagent. Do just the requested task in the displayed working directory. Keep progress and conclusions public and concise. Do not delegate. Report actions and limitations. Do not merge the worktree." });
          row.child = child.id;
          return { status: "running", checkpoint: { phase: "submit", child: child.id } };
        }, ctx);
      },
      submit: async (task, runtime, ctx) => {
        const c = task.state.checkpoint, input = task.input, owner = runtime.conversationId;
        const child = await runtime.conversation(c.child, ctx);
        return runtime.commit(async tx => {
          const row = await rowFor(tx, owner, task.id, task.owner, input.turn);
          const record = await tx.conversation(c.child);
          if (!child || !row || row.child !== c.child || row.submission !== undefined || record?.parent ||
              record?.owner?.taskId !== task.id || record.owner.conversationId !== owner) return unresolved();
          // Opening the child does not authorize submitting the prompt later.
          const failure = await this.#workspaceFailure(input, runtime.signal);
          if (failure) return failure;
          const submission = await child.submit(tx, { type: "input", content: input.prompt, requestId: childKey(task.id),
            whenBusy: "reject", notify: { taskId: task.id, key: settledKey } });
          row.submission = submission;
          return { status: "sleeping", signals: [SubmissionSettled.key(settledKey)],
            checkpoint: { phase: "settle", child: c.child, submission } };
        }, ctx);
      },
      settle: (task, runtime, ctx) => runtime.commit(async tx => {
        const c = task.state.checkpoint, owner = runtime.conversationId;
        const row = await rowFor(tx, owner, task.id, task.owner, task.input.turn);
        const input = row && row.child === c.child && row.submission === c.submission ? await childSubmission(tx, owner, row) : undefined;
        if (!input) return unresolved();
        if (input.status === "done") {
          const answer = await tx.entry(AssistantEntry, input.answer);
          const msg = answer?.conversationId === c.child ? answer.model?.[0] : undefined;
          if (msg?.role === "assistant" && msg.stopReason === "stop") return complete("ok");
          return complete("failed", "The subagent did not finish its answer normally.");
        }
        if (input.status === "unanswered") return complete("failed", input.reason === "model_error" ? "The model request failed."
          : input.reason === "turn_limit" ? "The worker reached its tool-round limit." : "The subagent ended without an answer.");
        return unresolved();
      }, ctx),
    }, abort: (task, runtime, ctx) => runtime.commit(() => task.abortReason === "restart"
      ? complete("failed", "Backend restart interrupted the subagent; it was not replayed.")
      : complete("cancelled", "The parent turn was stopped."), ctx) });
  }

  async overview({ threadId, after }: SubagentOverviewQuery, signal: AbortSignal) {
    const h = this.harness(), owner = Number(threadId) as ConversationId;
    const closed = new AbortController(), abort = AbortSignal.any([signal, closed.signal]);
    const ctx = withAbortSignal(abort, BACKGROUND_CONTEXT), pulse = new ChangePulse();
    const children = new Set<ConversationId>(), roster = new Set<ConversationId>();
    const observation = h.observeCommits({ advance: ({ changes }) => {
      if (changes.some(c => c.type === "task" && c.value.kind === workerKind && c.value.conversationId === owner ||
          c.type === "conversation" && c.value.owner?.conversationId === owner ||
          c.type === "entry" && roster.has(c.value.conversationId) ||
          c.type === "document" && c.conversationId !== undefined && (
            c.record.kind === UsageDoc.definition.kind && children.has(c.conversationId) ||
            c.record.kind === LiveDoc.definition.kind && roster.has(c.conversationId)))) pulse.emit();
    }, end: () => closed.abort(new WireError("internal")) });
    try {
      for (;;) {
        const version = pulse.generation;
        const cache = this.#spendFor(owner), { past, turn: cachedTurn, latest: cachedLatest } = cache;
        const { workers, owned, retired, through, allTerminal, turn } = await h.commit(async tx => {
          const parent = await tx.conversation(owner);
          if (!parent || parent.owner) throw new WireError("not_found");
          const workers: Omit<SubagentRow, "usage">[] = [];
          const owned = new Map<string, { child: ConversationId; terminal: boolean }>();
          const retired: ConversationId[] = [];
          children.clear();
          roster.clear();
          const query = { conversationId: owner, kind: workerKind, order: "descending" } as const;
          const { items: [latest] } = await tx.scanTasks(query, 1);
          const turn = (latest?.input as Intent | undefined)?.turn;
          let through = past.through, allTerminal = true;
          // Reuse the settled prefix while text streams. A new delegated turn
          // folds the preceding one into it without walking the older prefix.
          if (turn !== cachedTurn && latest) {
            let cursor: Cursor | undefined;
            scan: do {
              const page = await tx.scanTasks(query, 128, cursor);
              for (const task of page.items) {
                if (task.id <= past.through) break scan;
                if ((task.input as Intent).turn === turn) continue;
                through = Math.max(through, task.id);
                allTerminal &&= task.state.status === "terminal";
                const { items: [child], next } = await tx.scanConversations({ ownerTaskId: task.id }, 1);
                if (!next && !child?.parent && child?.owner?.conversationId === owner) {
                  children.add(child.id);
                  retired.push(child.id);
                }
              }
              cursor = page.next;
            } while (cursor);
          }
          if (turn !== undefined) for (const row of (await tx.doc(Turns, owner, String(turn), null)).workers) {
            const task = await tx.task(row.task);
            if (task?.kind !== workerKind || task.conversationId !== owner || task.owner !== row.caller) continue;
            const pin = task.input as Intent;
            if (pin.turn !== turn) continue;
            const input = await childSubmission(tx, owner, row);
            if (row.child && input) roster.add(row.child);
            const terminal = task.state.status === "terminal";
            if (row.child && input) {
              children.add(row.child);
              owned.set(String(task.id), { child: row.child, terminal });
            }
            const live = !terminal && row.child && input?.status === "placed" ? await tx.doc(LiveDoc, row.child) : undefined;
            const result = terminal ? (task.state.outcome.result as Finish | undefined) ?? { status: "failed" as const, detail: "The subagent did not settle normally." } : undefined;
            const tools = live?.tools?.filter(t => t.status !== "done") ?? [];
            const status = result?.status ?? (task.abortRequested ? "stopping" : !input ? "starting" : live?.generation?.retry ? "retrying"
              : tools.some(t => t.name === "read" || t.name === delegateGitName) ? "reading" : tools.length ? "working"
              : publicText(live?.generation?.message) ? "responding" : "waiting");
            const title = pin.prompt.split("\n", 1)[0]!.replace(/\s+/g, " ").trim().slice(0, 128).toWellFormed() || "Subagent";
            const log = await readLog(tx, owner, row, !terminal);
            workers.push({ id: String(task.id), title, cwd: delegateCwd(pin), readOnly: pin.read_only, status,
              ...log, ...(result?.detail && { detail: result.detail }) });
          }
          return { workers, owned, retired, through, allTerminal, turn };
        }, ctx);
        // Non-creating snapshots: a worker may not have used its model yet.
        const history = { ...past.usage };
        for (const child of retired) addSpend(history, projectUsage(await h.snapshot(UsageDoc, child, ctx)));
        const usage = { ...history }, finalized = new Map<string, Spend>();
        const rows: SubagentRow[] = [];
        for (const row of workers) {
          const target = owned.get(row.id);
          const previous = target?.terminal ? cachedLatest.get(row.id) : undefined;
          const spend = previous ?? projectUsage(target && await h.snapshot(UsageDoc, target.child, ctx));
          if (target?.terminal) finalized.set(row.id, spend);
          addSpend(usage, spend);
          rows.push({ ...row, usage: spend });
        }
        if (version !== pulse.generation) continue;
        // Another observer may have advanced this shared cache while we read.
        if (cache.past === past && allTerminal) {
          if (cache.turn !== turn) cache.past = { through, usage: history };
          cache.turn = turn;
          cache.latest = finalized;
        }
        const view = { workers: rows, usage };
        const cursor = this.#revisions.of([threadId, view]);
        if (cursor !== after) return { threadId, cursor, ...view };
        await pulse.wait(version, abort);
      }
    } finally { observation.detach(); }
  }

  async wait(query: SubagentQuery, signal: AbortSignal) {
    const h = this.harness(), owner = Number(query.threadId) as ConversationId, turn = Number(query.submissionId) as SubmissionId;
    const closed = new AbortController(), abort = AbortSignal.any([signal, closed.signal]);
    const ctx = withAbortSignal(abort, BACKGROUND_CONTEXT), pulse = new ChangePulse();
    const children = new Set<ConversationId>(), workers = new Set<TaskId>();
    const observation = h.observeCommits({ advance: ({ changes }) => {
      if (changes.some(c => c.type === "document" && (c.conversationId === owner && c.record.kind === Turns.definition.kind &&
          c.record.key === query.submissionId || c.conversationId !== undefined && children.has(c.conversationId) && c.record.kind === LiveDoc.definition.kind) ||
          c.type === "entry" && children.has(c.value.conversationId) || c.type === "task" && workers.has(c.value.id) ||
          c.type === "submission" && c.value.id === turn)) pulse.emit();
    }, end: () => closed.abort(new WireError("internal")) });
    try {
      for (;;) {
        const version = pulse.generation;
        const ledger = await h.snapshot(Turns, owner, query.submissionId, ctx);
        const view = await h.commit(async tx => {
          const input = await tx.submission(turn), parent = await tx.conversation(owner);
          if (!parent || parent.owner || input?.conversationId !== owner || input.type !== "input") throw new WireError("not_found");
          const events: SubagentEvent[] = [];
          for (const row of ledger?.workers ?? []) {
            workers.add(row.task);
            if (row.child) children.add(row.child);
            const task = await tx.task(row.task);
            if (task?.kind !== workerKind || task.conversationId !== owner || task.owner !== row.caller) continue;
            const pin = task.input as Intent;
            if (pin.turn !== turn) continue;
            const workerId = String(row.task);
            events.push({ type: "started", workerId, cwd: delegateCwd(pin), read_only: pin.read_only });
            const terminal = task.state.status === "terminal";
            const { text, truncated } = await readLog(tx, owner, row, !terminal);
            if (text) events.push({ type: "text", workerId, text, ...(truncated && { truncated: true }) });
            if (terminal) {
              const result = (task.state.outcome.result as Finish | undefined) ?? { status: "failed", detail: "The subagent did not settle normally." };
              events.push({ type: "finished", workerId, ...result });
            }
          }
          return { events, settled: input.status === "done" || input.status === "unanswered" };
        }, ctx);
        if (version !== pulse.generation) continue;
        const cursor = this.#revisions.of([query.threadId, query.submissionId, view]);
        if (view.settled || query.after !== cursor) return { cursor, ...view };
        await pulse.wait(version, abort);
      }
    } finally { observation.detach(); }
  }
}
