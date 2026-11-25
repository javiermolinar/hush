import { stat } from "node:fs/promises";
import { BACKGROUND_CONTEXT, withAbortSignal, type Context } from "@earendil-works/chord/context";
import type { Models } from "@earendil-works/pi-ai";
import { calculateContextTokens } from "@earendil-works/pi-ai/utils/estimate";
import { toToolDeclaration } from "@earendil-works/pi-ai/utils/transcript";
import { AgentDoc, LiveDoc, configure, type CommitChange, type Conversation, type ConversationId, type Cursor,
  type Agent, type AgentChange, type Harness, type LiveState, type RunFailure, type TaskGraph } from "@earendil-works/pi-durable";
import type { IntroAction, IntroAttention, IntroHost, IntroQuery, IntroReport, IntroRunIssue, IntroThread,
  IntroVersion, NativeHost } from "../../server/src/index.js";
import { checkPreparedModel } from "../../server/src/durable.js";
import { WireError } from "../../server/src/wire.js";
import type { StandaloneResources } from "./resources.js";
import { actionableFailure, displayTitle, failureDetail, ThreadPresentationDoc, threadLabel } from "./thread-presentation.js";

const MAX_SESSIONS = 256;
const PAGE = 64;
const bounded = (s: string, n: number) => Array.from(s.toWellFormed()).slice(0, n).join("");
const uint = (v: number) => Number.isSafeInteger(v) && v >= 0;

function rosterChange(change: CommitChange) {
  if (change.type === "conversation") return true;
  if (change.type !== "document" && change.type !== "document.copy") return false;
  return change.record.kind === ThreadPresentationDoc.definition.kind ||
    change.record.kind === LiveDoc.definition.kind && (change.type !== "document" || change.kind !== "change" || !change.ops.length ||
    change.ops.some(op => op[0] === "r" || Array.isArray(op[1]) && op[1][0] === "runFailure"));
}

function activity(graph: TaskGraph) {
  const byThread = new Map<ConversationId, { runnable: boolean; parked?: { reason?: string; untilMs?: number } }>();
  for (const node of Object.values(graph.tasks)) {
    let owner = node;
    let background = node.background;
    while (!background && owner.owner !== undefined) {
      const upstream = graph.tasks[String(owner.owner)];
      if (!upstream) break;
      owner = upstream;
      background = owner.background;
    }
    if (background) continue;
    const item = byThread.get(owner.conversationId) ?? { runnable: false };
    if (node.blocked) item.parked ??= { reason: node.blocked.replaceAll("_", " ") };
    else if (node.state.status === "running" || node.state.status === "pending") item.runnable = true;
    else if (node.state.status === "sleeping" && node.kind !== "pi.generation") {
      const until = node.state.until;
      if (until !== undefined && uint(until) && (!item.parked || item.parked.untilMs === undefined || until < item.parked.untilMs)) {
        item.parked = { untilMs: until };
      } else item.parked ??= {};
    }
    byThread.set(owner.conversationId, item);
  }
  return byThread;
}

type PreviewAgent = Pick<Agent, "model" | "thinkingLevel" | "tools" | "instructions">;

type Inventory = Pick<IntroReport, "sessions" | "moreAttention"> & {
  attention: Array<IntroAttention & { legacyTitle: string }>;
};
type Roster = { rows: { id: ConversationId; failure?: Readonly<RunFailure> }[]; partial: boolean };

function runIssue(live: Readonly<LiveState> | undefined) {
  const f = live?.runFailure;
  return f && actionableFailure(f.reason) ? f : undefined;
}

function busy(live: Readonly<LiveState> | undefined) {
  return !!(live?.run || live?.generation || live?.compactions?.length);
}

// One shared graph per host. Peer waits are cancellation-bound and only get
// replacement reports. The observer just invalidates a revision; none of the
// projection, snapshots, resource loading, or transport runs on the commit line.
export class IntroReports implements IntroHost {
  readonly #lifetime = new AbortController();
  readonly #waiters = new Set<() => void>();
  #graph?: TaskGraph;
  #watch?: Promise<void>;
  #failure = false;
  #detach?: () => void;
  #revision = 1;
  #rosterRevision = 1;
  #roster?: { revision: number; value: Promise<Roster> };
  #inventory?: { revision: number; value: Promise<Inventory> };

  constructor(private readonly harness: Harness, private readonly root: Conversation,
      private readonly resources: StandaloneResources, private readonly runtime: Models, private readonly startedAtMs: number,
      private readonly previewAgent: () => PreviewAgent,
      private readonly prepare: NonNullable<NativeHost["prepare"]>,
      private readonly requireModel?: NativeHost["requireModel"]) {}

  #advance = () => {
    this.#revision++;
    for (const wake of this.#waiters) wake();
  };

  async #observe() {
    const signal = this.#lifetime.signal;
    signal.throwIfAborted();
    this.#detach = this.harness.observeCommits({
      advance: event => {
        const roster = event.changes.some(rosterChange);
        if (roster) this.#rosterRevision++;
        if (roster || event.changes.some(change => change.type === "entry" ||
            (change.type === "document" || change.type === "document.copy") &&
            (change.record.kind === AgentDoc.definition.kind ||
             change.record.kind === ThreadPresentationDoc.definition.kind))) {
          this.#advance();
        }
      },
      end: () => { this.#failure = true; this.#advance(); },
    }).detach;
    const graph = await this.harness.watchTaskGraph(withAbortSignal(signal, BACKGROUND_CONTEXT));
    this.#graph = graph.value;
    graph.start(async call => {
      if (call.kind === "value") {
        this.#graph = call.value;
        this.#advance();
      }
    });
    void graph.closed.then(() => { this.#failure = true; this.#advance(); });
  }

  close() {
    this.#lifetime.abort();
    this.#detach?.();
    this.#advance();
  }

  async get(query: IntroQuery, peer: AbortSignal, version: IntroVersion): Promise<IntroReport> {
    const signal = AbortSignal.any([peer, this.#lifetime.signal]);
    signal.throwIfAborted();
    await (this.#watch ??= this.#observe());
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    // Validate the scope even for a passive wait at the current revision.
    const conv = query.threadId === undefined ? this.root
      : await this.harness.conversation(Number(query.threadId) as ConversationId, ctx);
    if (!conv) throw new WireError("not_found");
    if (query.cwd !== undefined && !(await stat(query.cwd).catch(() => undefined))?.isDirectory()) throw new WireError("not_found");
    if (query.after !== undefined && query.after > this.#revision) throw new WireError("invalid_request");
    if (!this.#failure && query.after === this.#revision) await this.#wait(signal);
    signal.throwIfAborted();
    if (this.#failure) throw new WireError("internal");
    const revision = this.#revision;
    const [inventory, project] = await Promise.all([
      this.#viewInventory(revision), this.#project(conv, query, ctx, version),
    ]);
    signal.throwIfAborted();
    const attention = inventory.attention.map(({ legacyTitle, ...row }) => version === 2 ? row : {
      title: legacyTitle, status: row.status,
      ...(row.issue ? { reason: row.issue.reason.replaceAll("_", " ") } : row.reason && { reason: row.reason }),
      ...(row.untilMs !== undefined && { untilMs: row.untilMs }),
    });
    return { revision, startedAtMs: this.startedAtMs, ...inventory, attention, ...project };
  }

  async act(action: IntroAction, peer: AbortSignal): Promise<object> {
    const ctx = withAbortSignal(AbortSignal.any([peer, this.#lifetime.signal]), BACKGROUND_CONTEXT);
    const id = Number(action.threadId) as ConversationId;
    const conv = await this.harness.conversation(id, ctx);
    if (!conv || (await this.harness.commit(tx => tx.conversation(id), ctx))?.owner) throw new WireError("not_found");
    if (action.kind === "rename") {
      const title = threadLabel(action.title, 96);
      if (!title) throw new WireError("invalid_request");
      await conv.commit(async tx => { (await tx.doc(ThreadPresentationDoc, id)).title = title; }, ctx);
      return { title };
    }
    const taskId = Number(action.taskId);
    const check = (live: Readonly<LiveState> | undefined) => {
      if (busy(live)) throw new WireError("busy");
      if (runIssue(live)?.taskId !== taskId) throw new WireError("not_found");
    };
    let agent: AgentChange | undefined;
    let verify: Awaited<ReturnType<typeof checkPreparedModel>>;
    if (action.kind === "retry") {
      check(await this.harness.snapshot(LiveDoc, id, ctx));
      const [doc, current] = await Promise.all([
        this.harness.snapshot(ThreadPresentationDoc, id, ctx),
        this.harness.snapshot(AgentDoc, id, ctx),
      ]);
      if (doc?.dismissedFailure === taskId) throw new WireError("not_found");
      if (!this.requireModel && !this.#modelAvailable(current?.model)) throw new WireError("invalid_request");
      agent = (await this.prepare({ cwd: current?.cwd }, ctx)).agent;
      verify = await checkPreparedModel(this.requireModel, current?.model, agent);
    }
    const submission = await conv.commit(async tx => {
      // Check the exact failure in the same transaction as Continue or Dismiss.
      // A stale click must not restart or hide a different run.
      check(await tx.doc(LiveDoc, id));
      const doc = await tx.doc(ThreadPresentationDoc, id);
      if (doc.dismissedFailure === taskId) throw new WireError("not_found");
      if (action.kind === "dismiss") {
        doc.dismissedFailure = taskId;
        return undefined;
      }
      const current = (await tx.doc(AgentDoc, id)).model;
      if (verify) verify(current);
      else if (!this.#modelAvailable(current)) throw new WireError("invalid_request");
      if (agent) await configure(tx, id, agent);
      return conv.submit(tx, { type: "continue" });
    }, ctx);
    if (submission === undefined) return {};
    this.harness.resume();
    return { submissionId: String(submission) };
  }

  #modelAvailable(model: Agent["model"]) {
    return !!(model && this.runtime.getModel(model.provider, model.modelId));
  }

  async #issue(failure: Readonly<RunFailure>, model: Agent["model"], ctx: Context): Promise<IntroRunIssue> {
    const detail = failureDetail(failure, model?.provider === "faux");
    const endedAtMs = (await this.harness.commit(tx => tx.task(failure.taskId), ctx))?.endedAt;
    return { taskId: String(failure.taskId), reason: failure.reason, ...(detail && { detail }),
      // Keep the recovery affordance for a known model; auth can change on
      // disk without a report revision. The retry action checks it afresh.
      retry: this.#modelAvailable(model),
      switchModel: failure.reason === "no_model" || failure.reason === "model_error",
      ...(endedAtMs !== undefined && uint(endedAtMs) && { endedAtMs }) };
  }

  #wait(signal: AbortSignal) {
    signal.throwIfAborted();
    return new Promise<void>((resolve, reject) => {
      const done = () => { cleanup(); resolve(); };
      const abort = () => { cleanup(); reject(signal.reason); };
      const cleanup = () => { this.#waiters.delete(done); signal.removeEventListener("abort", abort); };
      this.#waiters.add(done);
      signal.addEventListener("abort", abort, { once: true });
    });
  }

  #viewInventory(revision: number) {
    if (this.#inventory?.revision === revision) return this.#inventory.value;
    const value = this.#collect(this.#graph!, withAbortSignal(this.#lifetime.signal, BACKGROUND_CONTEXT));
    this.#inventory = { revision, value };
    void value.catch(() => { if (this.#inventory?.value === value) this.#inventory = undefined; });
    return value;
  }

  #readRoster(ctx: Context) {
    if (this.#roster?.revision === this.#rosterRevision) return this.#roster.value;
    const value = this.#snapshotRoster(ctx);
    this.#roster = { revision: this.#rosterRevision, value };
    void value.catch(() => { if (this.#roster?.value === value) this.#roster = undefined; });
    return value;
  }

  async #snapshotRoster(ctx: Context): Promise<Roster> {
    const rows: Roster["rows"] = [];
    let cursor: Cursor | undefined;
    for (let scanned = 0; scanned < MAX_SESSIONS; scanned += PAGE) {
      const page = await this.harness.commit(tx => tx.scanConversations({ order: "descending" }, PAGE, cursor), ctx);
      for (const row of page.items) {
        if (row.owner) continue; // a task-owned child is not an independent session
        const [live, presentation] = await Promise.all([
          this.harness.snapshot(LiveDoc, row.id, ctx), this.harness.snapshot(ThreadPresentationDoc, row.id, ctx),
        ]);
        if (!presentation?.archived) rows.push({ id: row.id, failure: runIssue(live) });
      }
      cursor = page.next;
      if (!cursor) break;
    }
    return { rows, partial: cursor !== undefined };
  }

  async #collect(graph: TaskGraph, ctx: Context): Promise<Inventory> {
    const { rows, partial } = await this.#readRoster(ctx);
    const groups = activity(graph);
    let running = 0, quiet = 0, moreAttention = 0;
    const attention: Inventory["attention"] = [];
    for (const { id, failure } of rows) {
      const state = groups.get(id);
      if (state?.runnable || state && !state.parked) { running++; continue; }
      if (!state?.parked && !failure) { quiet++; continue; }
      const presentation = await this.harness.snapshot(ThreadPresentationDoc, id, ctx);
      if (!state?.parked && presentation?.dismissedFailure === failure?.taskId) { quiet++; continue; }
      if (attention.length === 3) { moreAttention++; continue; }
      const fallback = id === this.root.id ? "main" : `thread #${id}`;
      const common = { threadId: String(id), title: displayTitle(presentation) || fallback,
        legacyTitle: presentation?.title || fallback };
      if (state?.parked) attention.push({ ...common, status: "parked", ...state.parked });
      else if (failure) {
        const model = (await this.harness.snapshot(AgentDoc, id, ctx))?.model;
        attention.push({ ...common, status: "stopped", issue: await this.#issue(failure, model, ctx) });
      }
    }
    return { sessions: { open: rows.length, running, quiet, partial }, attention, moreAttention };
  }

  async #thread(conv: Conversation, model: Agent["model"], ctx: Context): Promise<IntroThread> {
    const id = String(conv.id);
    const [doc, live, record] = await Promise.all([
      this.harness.snapshot(ThreadPresentationDoc, conv.id, ctx), this.harness.snapshot(LiveDoc, conv.id, ctx),
      this.harness.commit(tx => tx.conversation(conv.id), ctx),
    ]);
    const title = displayTitle(doc);
    const failure = runIssue(live);
    const presentation: IntroThread = { id, ...(title && { title }) };
    if (!record || record.owner || !failure || busy(live)) return presentation;
    if (doc?.dismissedFailure === failure.taskId) presentation.dismissedFailureId = String(failure.taskId);
    else presentation.issue = await this.#issue(failure, model, ctx);
    return presentation;
  }

  async #project(conv: Conversation, query: IntroQuery, ctx: Context, version: IntroVersion) {
    const bound = query.threadId !== undefined;
    const current = query.cwd === undefined ? await conv.agent(ctx) : undefined;
    const cwd = this.resources.directory(query.cwd ?? current?.cwd);
    const agent = current ?? this.previewAgent();
    const ref = agent.model;
    const model = ref && this.runtime.getModel(ref.provider, ref.modelId);
    let context: IntroReport["context"];
    if (model?.contextWindow && uint(model.contextWindow)) {
      const sections = await this.resources.promptSections(cwd, true, ctx.abortSignal);
      const prompt = Object.values(sections).filter(text => text !== undefined).join("\n\n");
      const instructions = agent.instructions ? `\n\n<instructions>\n${agent.instructions}\n</instructions>` : "";
      const tools = JSON.stringify(agent.tools.map(toToolDeclaration));
      context = { window: model.contextWindow, renderedBytes: Buffer.byteLength(prompt + instructions) + Buffer.byteLength(tools) };
      // One bounded read, not a history scan. A newer reset, compaction,
      // edit, user message, or tool result makes the prior count inapplicable.
      if (bound) {
        const latest = (await conv.entries({}, 1, undefined, ctx)).items[0];
        const message = latest?.model?.at(-1);
        if (!latest?.head && !latest?.edits?.length && message?.role === "assistant" && message.model === ref!.modelId &&
            message.provider === ref!.provider && message.stopReason !== "error" && message.stopReason !== "aborted") {
          const used = calculateContextTokens(message.usage);
          if (uint(used) && used > 0) context.reportedTokens = used;
        }
      }
    }
    const thread = bound && version === 2 ? await this.#thread(conv, ref, ctx) : undefined;
    return { cwd: bounded(cwd, 4096), ...(ref && { model: { provider: bounded(ref.provider, 160), modelId: bounded(ref.modelId, 160) } }),
      thinking: agent.thinkingLevel, skills: [], ...(thread && { thread }),
      ...(context && { context }) };
  }
}
