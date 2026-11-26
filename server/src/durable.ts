// Pin the public Durable 1.6 adapter here. No host settings, provider credentials or Pi CLI imports.
import { withAbortSignal, BACKGROUND_CONTEXT, type Context } from "@earendil-works/chord/context";
import type { JsonValue } from "@earendil-works/chord/delta";
import {
  AgentDoc, ConversationBusy, ConversationIdle, ReadOnlySession, ResetEntry, configure,
  type AgentChange, type AgentState, type Conversation, type ConversationId, type ConversationRecord, type ConversationView,
  type Cursor, type EntryId, type EntryRecord, type Harness, type InboxState, type LiveState, type Observer,
  type SubmissionId, type SubmissionRecord, type Tx, type UsageState,
} from "@earendil-works/pi-durable";
import { Cursors } from "./cursor.js";
import { environmentEntry, environmentInputKind, type ClientEnvironment } from "./client-environment.js";
import { projectImageBytes, type ImageRef, type ImagesHost, type ImageSubmission } from "./images.js";
import { planReviewKind } from "./plan-review.js";
import { reviewParentKind, workerNoticeKind } from "./delegation.js";
import { planChatKind, planRunKind, planTaskKind, planResultKind, planPreviewKind, planPreviewSavedKind } from "./plan.js";
import { scheduleReportKind } from "./scheduling.js";
import { checkpointKind, effectKind } from "./checkpoints.js";
import type { CatalogCreation } from "./model-catalog.js";
import { cleanJSON, durableID, encode, entryShape, object, stateEnvelopeShape, WireError,
  type CoreRequest, type ExplicitRange, type Params } from "./wire.js";

const id = String;
const maybeID = (value: number | undefined) => value === undefined ? undefined : id(value);
const field = <K extends string, V>(key: K, value: V | undefined): Partial<Record<K, V>> =>
  value === undefined ? {} : { [key]: value } as Record<K, V>;
const threadID = (value: string) => Number(value) as ConversationId;
const entryID = (value: string) => Number(value) as EntryId;
const submissionID = (value: string) => Number(value) as SubmissionId;
const ownerless = { kind: "ownerless" } as const;
const notFound = () => new WireError("not_found");

export function requestError(error: unknown, readOnlyReason?: string) {
  if (error instanceof WireError) return error;
  if (error instanceof ReadOnlySession) {
    const refusal = new WireError("invalid_request");
    refusal.message = readOnlyReason ?? "The backend is read-only.";
    return refusal;
  }
  if (error instanceof ConversationBusy) return new WireError("busy");
  if (error instanceof ConversationIdle) return new WireError("idle");
  return new WireError("internal");
}

const projectThread = (row: ConversationRecord) => ({
  id: id(row.id),
  ...field("parent", row.parent && { threadId: id(row.parent.conversationId), atEntryId: id(row.parent.at) }),
  ...field("owner", row.owner && { threadId: id(row.owner.conversationId), taskId: id(row.owner.taskId) }),
});

type Message = NonNullable<EntryRecord["model"]>[number];
type Content = Exclude<Message["content"], string>;
const blocks = (content: string | readonly Content[number][] | undefined) =>
  typeof content === "string" ? [{ type: "text", text: content }] : (content ?? []).map(item => {
    switch (item.type) {
      case "text": return { type: "text", text: item.text };
      case "thinking": return item.redacted ? { type: "omitted", kind: "redactedThinking" } : { type: "thinking", text: item.thinking };
      case "toolCall": return { type: "toolCall", id: item.id, name: item.name, arguments: item.arguments };
      case "image": return { type: "omitted", kind: "image" };
      default: return { type: "omitted", kind: "other" };
    }
  });

function projectMessage(message: Message) {
  const base = {
    role: message.role, timestampMs: message.timestamp,
    blocks: message.role === "system" ? [] : blocks(message.content),
  };
  switch (message.role) {
    case "user":
    case "system": return base;
    // Upstream error messages may contain raw request information. Only expose the stop reason.
    case "assistant": return { ...base, stopReason: message.stopReason };
    case "toolResult": return { ...base, callId: message.toolCallId, name: message.toolName, isError: message.isError };
  }
}
const projectEntry = (entry: EntryRecord) => ({
  id: id(entry.id), threadId: id(entry.conversationId),
  kind: entry.kind === environmentInputKind || entry.kind === reviewParentKind ? "pi.user" : entry.kind,
  messages: (entry.kind === scheduleReportKind || entry.kind === checkpointKind || entry.kind === effectKind ||
    entry.kind === planRunKind || entry.kind === planTaskKind || entry.kind === planChatKind || entry.kind === planResultKind || entry.kind === planReviewKind ||
    entry.kind === planPreviewKind || entry.kind === planPreviewSavedKind || entry.kind === workerNoticeKind) &&
    object(entry.data) && typeof entry.data.text === "string" && typeof entry.data.timestampMs === "number" ? [{ role: "assistant", timestampMs: entry.data.timestampMs,
      stopReason: "stop", blocks: [{ type: "text", text: entry.data.text }] }] :
    (entry.model ?? []).map(projectMessage),
  ...field("headEntryId", maybeID(entry.head)),
});

// Durable retains immutable EntryRecords across observations. Reuse only on
// object identity, not ID, and don't keep a record alive after Durable drops it.
// The resulting entries are shared read-only by the feed's diff and encoder.
type ProjectedEntry = ReturnType<typeof projectEntry>;
const projectedEntries = new WeakMap<EntryRecord, ProjectedEntry>();
function projectWindowEntry(entry: EntryRecord) {
  const cached = projectedEntries.get(entry);
  if (cached) return cached;
  const projected = projectEntry(entry);
  // Use the same depth limit as the whole state at window.entries[i].
  if (!cleanJSON(projected, 3) || !entryShape(projected)) throw new WireError("internal");
  projectedEntries.set(entry, projected);
  return projected;
}

function projectUsage(ledger: UsageState | undefined) {
  const usage = { inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0, totalTokens: 0, costUsd: 0 };
  for (const bucket of [ledger?.models, ledger?.tools]) {
    for (const row of Object.values(bucket ?? {})) {
      usage.inputTokens += row.input;
      usage.outputTokens += row.output;
      usage.cacheReadTokens += row.cacheRead;
      usage.cacheWriteTokens += row.cacheWrite;
      usage.totalTokens += row.totalTokens;
      usage.costUsd += row.cost.total;
    }
  }
  return usage;
}

type Builtins = { "pi.agent"?: AgentState; "pi.live"?: LiveState; "pi.usage"?: UsageState; "pi.inbox"?: InboxState };
export function projectView(view: ConversationView) {
  const docs: Builtins = view.docs;
  const agent = docs["pi.agent"] ?? {};
  const live = docs["pi.live"] ?? {};
  const entries: ProjectedEntry[] = [];
  for (const entry of view.entries) {
    if (!entry) throw new WireError("internal");
    entries.push(projectWindowEntry(entry));
  }
  const state = {
    thread: projectThread(view.conversation),
    agent: {
      ...field("model", agent.model && { provider: agent.model.provider, modelId: agent.model.modelId }),
      ...field("thinkingLevel", agent.thinkingLevel), ...field("cwd", agent.cwd),
      ...field("compactionEnabled", agent.compaction?.enabled),
    },
    usage: projectUsage(docs["pi.usage"]),
    window: { entries, beforeEntryId: maybeID(view.before) ?? null, lastEntryId: entries.at(-1)?.id ?? null },
    tools: (live.tools ?? []).map(slot => ({
      callId: slot.callId, name: slot.name, status: slot.status, ...field("taskId", maybeID(slot.taskId)),
      ...field("resultEntryId", maybeID(slot.entry)), ...field("output", slot.output),
      ...field("droppedBytes", slot.droppedBytes), ...field("droppedLines", slot.droppedLines),
    })),
    queue: (docs["pi.inbox"]?.items ?? []).flatMap(item => item.mode === "write" ? [] : [{
      submissionId: id(item.id), mode: item.mode,
      text: item.entry?.kind === planChatKind && object(item.entry.data) && typeof item.entry.data.text === "string" ? item.entry.data.text
        : typeof item.content === "string" ? item.content
        : item.content.filter(block => block.type === "text").map(block => block.text).join("\n"),
    }]),
    compactions: (live.compactions ?? []).map(item => ({
      taskId: id(item.taskId), reason: item.trigger, blocking: item.blocking, attempt: item.attempt,
    })),
    ...field("compactionFailure", live.compactionFailure && {
      taskId: id(live.compactionFailure.taskId), reason: live.compactionFailure.trigger, detail: "Compaction failed",
    }),
    ...field("run", live.run && { taskId: id(live.run.taskId), submissionIds: live.run.inputs.map(id) }),
    ...field("failure", live.runFailure && { taskId: id(live.runFailure.taskId), reason: live.runFailure.reason }),
    ...field("streaming", live.generation && {
      attempt: live.generation.attempt, blocks: blocks(live.generation.message?.content),
      ...field("retry", live.generation.retry && { atMs: live.generation.retry.at, detail: "Generation will retry" }),
    }),
  };
  // The envelope validator still sees the real array (including its size).
  // Only cleanJSON's recursive pass skips the already-checked entries.
  if (!cleanJSON({ ...state, window: { ...state.window, entries: [] } }) || !stateEnvelopeShape(state)) {
    throw new WireError("internal");
  }
  return state as typeof state & JsonValue;
}

function projectSubmission(row: SubmissionRecord | undefined, thread: ConversationId) {
  // Passive writes (including a host's internal work) have no native input settlement contract.
  if (!row || row.conversationId !== thread || row.type === "write") throw notFound();
  const base = { threadId: id(thread), submissionId: id(row.id), status: row.status, ...field("entryId", maybeID(row.entry)) };
  switch (row.status) {
    case "queued":
    case "placed": return base;
    case "done": return { ...base, answerEntryId: id(row.answer) };
    case "unanswered": return { ...base, reason: row.reason };
  }
}

export function visibleEntry(entry: EntryRecord, owner: ConversationId, delegation: boolean) {
  return entry.kind !== workerNoticeKind || delegation && entry.conversationId === owner;
}

async function visible(conv: Conversation, at: string, ctx: Context, delegation = false) {
  const bound = entryID(at);
  const exact = await conv.entries({ minEntryId: bound, maxEntryId: bound }, 1, undefined, ctx);
  const entry = exact.items[0];
  return entry?.id === bound && visibleEntry(entry, conv.id, delegation);
}

async function visibleEntries(conv: Conversation, query: Parameters<Conversation["entries"]>[0], limit: number,
    delegation: boolean, ctx: Context) {
  const found: EntryRecord[] = [];
  let cursor: Cursor | undefined;
  // A series of display-only outcomes cannot make an old peer page over an
  // invisible anchor. Bound pathological histories (e.g. very deep forks).
  let scanned = 0;
  do {
    const page = await conv.entries(query, limit - found.length, cursor, ctx);
    for (const entry of page.items) if (visibleEntry(entry, conv.id, delegation)) found.push(entry);
    cursor = page.next;
    scanned += page.items.length;
    if (scanned > 2_048) throw new WireError("limit");
  } while (cursor !== undefined && found.length < limit);
  return found;
}

type Call = Exclude<CoreRequest, { method: "hello" | "subscribe" }>;
export type StructuralView = ConversationView;
export type StructuralObserver = Observer<StructuralView>;
export type Admission = { result: object } | { wait: Promise<object> };
export const nativeStopVersion = 1;
export const stopCandidateLimit = 8;
export type StopQuery = { threadId: string; submissionIds: string[] };
type Withdrawal = "withdrawn" | "not_withdrawn" | "unknown";
type StopResult = { stopped: boolean; candidates: { submissionId: string; outcome: Withdrawal }[] };

export function parseStop(params: Record<string, unknown>): StopQuery {
  const { threadId, submissionIds } = params;
  if (Object.keys(params).length !== 2 || !durableID(threadId) || !Array.isArray(submissionIds) ||
      submissionIds.length > stopCandidateLimit || !submissionIds.every(durableID) ||
      new Set(submissionIds).size !== submissionIds.length) throw new WireError("invalid_request");
  return { threadId, submissionIds };
}

/** Local customization of base admission, not a negotiated wire extension. */
export interface NativeHost {
  defaults?(cwd: string | undefined, context: Context): Promise<AgentChange>;
  /** Hosts can keep internal worker conversations inspectable but not mutable or forkable by peers. */
  checkMutation?(conv: Conversation, context: Context): Promise<void>;
  /** A host can keep a delegated transcript private while projecting its committed public notice. */
  privateConversation?(id: ConversationId, context: Context): Promise<boolean>;
  /** Guard native input and settings admission. Manual compaction is checked before asking Durable to start it. */
  checkAdmission?(tx: Tx, threadId: ConversationId, operation: "input" | "continue" | "configure" | "compact" | "reset"): Promise<void>;
  prepare?(input: Readonly<{ threadId?: string; cwd?: string; text?: string; skillToken?: string }>, context: Context):
    Promise<{ text?: string; agent?: AgentChange; confirm?: (signal?: AbortSignal) => Promise<void> }>;
  /** Check the selected model outside the transaction. Never resolve or carry request credentials. */
  requireModel?(model: AgentState["model"]): Promise<void>;
  validateSelection?(model: AgentState["model"], level: NonNullable<AgentState["thinkingLevel"]>): Promise<void>;
}

const sameModelRef = (a: AgentState["model"], b: AgentState["model"]) =>
  a?.provider === b?.provider && a?.modelId === b?.modelId;

// Capture the before/after selection so a peer reconfiguration cannot admit
// against a different model. Defer rejection until after any raced receipt.
export async function checkPreparedModel(host: NativeHost["requireModel"], model: AgentState["model"], agent?: AgentChange) {
  if (!host) return;
  const before = model && { provider: model.provider, modelId: model.modelId };
  const selected = agent?.model === undefined ? before : agent.model ?? undefined;
  let failure: WireError | undefined;
  try { await host(selected); } catch (error) { failure = requestError(error); }
  return (current: AgentState["model"]) => {
    if (!sameModelRef(before, current)) throw new WireError("busy");
    if (failure) throw failure;
  };
}

export class DurableAdapter {
  readonly #cursors = new Cursors();

  constructor(private readonly harness: Harness, private readonly host?: NativeHost) {}

  context(signal: AbortSignal) { return withAbortSignal(signal, BACKGROUND_CONTEXT); }

  async stop(query: StopQuery, ctx: Context): Promise<StopResult> {
    const conv = await this.#get(query.threadId, ctx);
    const record = await this.harness.commit(tx => tx.conversation(conv.id), ctx);
    if (!record || record.owner) throw notFound();
    const candidates: StopResult["candidates"] = [];
    for (const value of query.submissionIds) {
      let outcome: Withdrawal = "unknown";
      try {
        const sub = await this.harness.submission(submissionID(value), ctx);
        const before = await sub?.status(ctx);
        if (sub && before && before.conversationId === conv.id && before.type === "input") {
          if (before.status === "placed" || before.status === "done" || before.status === "unanswered") outcome = "not_withdrawn";
          else if (before.status === "queued" && before.entry === undefined) {
            const raw: unknown = await sub.abort(ctx);
            const result = raw && typeof raw === "object" && "kind" in raw ? raw.kind : raw;
            if (result === "already_placed" || result === "settled") outcome = "not_withdrawn";
            else if (result === "aborted") {
              const after = await sub.status(ctx);
              if (after?.conversationId === conv.id && after.status === "unanswered" &&
                  after.reason === "aborted" && after.entry === undefined) outcome = "withdrawn";
            }
          }
        }
      } catch { /* No proof for this candidate; continue to ordinary abort. */ }
      candidates.push({ submissionId: value, outcome });
    }
    // Keep proven withdrawals even if the subsequent ordinary stop fails.
    let stopped = false;
    try { await conv.abort(ctx); stopped = true; } catch { /* Not confirmed. */ }
    return { stopped, candidates };
  }

  async observe(params: Params["subscribe"], observer: StructuralObserver, ctx: Context) {
    const conv = await this.#get(params.threadId, ctx);
    const window = params.window ?? { min: 50, max: 100 };
    if (window.min > window.max) throw new WireError("invalid_request");
    return this.harness.observeConversation(conv.id, observer, ctx, window);
  }

  get filtersWorkerNotices() { return this.host?.privateConversation !== undefined; }

  async windowPredecessors(owner: ConversationId, at: EntryId, limit: number, delegation: boolean, ctx: Context) {
    const conv = await this.#get(String(owner), ctx);
    return visibleEntries(conv, { maxEntryId: at, order: "descending" }, limit, delegation, ctx);
  }

  get supportsCatalogCreation() { return typeof this.host?.validateSelection === "function"; }

  async createCatalogThread(params: CatalogCreation, ctx: Context) {
    if (!this.host?.validateSelection) throw new WireError("unsupported_capability", "modelCatalog");
    await this.host.validateSelection(params.model, params.thinkingLevel);
    return this.#createThread(params, ctx);
  }

  async #createThread(params: Params["thread.new"] | CatalogCreation, ctx: Context) {
    const defaults = await this.host?.defaults?.(params.cwd, ctx);
    const conv = await this.harness.createConversation({ ownership: ownerless, agent: { ...defaults, ...params } }, ctx);
    return { id: id(conv.id) };
  }

  async admit(request: Call, ctx: Context, catalog = false, environment?: ClientEnvironment, skillToken?: string, delegation = false): Promise<Admission> {
    switch (request.method) {
      case "thread.new": return { result: await this.#createThread(request.params, ctx) };
      case "thread.list": {
        const { limit, cursor } = request.params;
        let next = cursor === undefined ? undefined : this.#cursors.decode<Cursor>("list", cursor);
        const threads: ReturnType<typeof projectThread>[] = [];
        for (let i = 0; i < 64; i++) {
          const page = await this.harness.commit(tx => tx.scanConversations({ order: "ascending" }, limit - threads.length, next), ctx);
          for (const row of page.items) if (!row.owner || !await this.host?.privateConversation?.(row.id, ctx)) threads.push(projectThread(row));
          next = page.next;
          if (!next || threads.length === limit) break;
        }
        return { result: { threads, nextCursor: next ? this.#cursors.encode("list", next) : null } };
      }
      case "submission.get": {
        const params = request.params;
        const tid = threadID(params.threadId);
        const row = "requestId" in params
          ? await this.harness.commit(tx => tx.submissionByRequest(tid, params.requestId), ctx)
          : await (await this.harness.submission(submissionID(params.submissionId), ctx))?.status(ctx);
        if (row?.conversationId === tid && await this.host?.privateConversation?.(tid, ctx)) throw notFound();
        return { result: projectSubmission(row, tid) };
      }
      case "submission.wait": {
        const tid = threadID(request.params.threadId);
        const sub = await this.harness.submission(submissionID(request.params.submissionId), ctx);
        if (!sub) throw notFound();
        projectSubmission(await sub.status(ctx), tid);
        if (await this.host?.privateConversation?.(tid, ctx)) throw notFound();
        return { wait: sub.wait(ctx).then(row => projectSubmission(row, tid)) };
      }
    }

    const conv = await this.#get(request.params.threadId, ctx);
    if (request.method !== "history" && request.method !== "thread.wait") await this.host?.checkMutation?.(conv, ctx);
    switch (request.method) {
      case "history": return { result: await this.#history(conv, request.params, ctx, delegation) };
      case "thread.fork": {
        const { threadId, atEntryId } = request.params;
        if (!(await visible(conv, atEntryId, ctx))) throw notFound();
        const child = await conv.fork(entryID(atEntryId), { ownership: ownerless }, ctx);
        return { result: { id: id(child.id), parent: { threadId, atEntryId } } };
      }
      case "thread.configure": {
        const { compactionEnabled, ...change } = request.params.change;
        const validate = catalog && this.host?.validateSelection;
        const check = change.model != null || change.thinkingLevel != null ? validate : undefined;
        const before = check ? await this.harness.snapshot(AgentDoc, conv.id, ctx) : undefined;
        if (check) {
          await check(change.model === undefined ? before?.model : change.model ?? undefined,
            (change.thinkingLevel === undefined ? before?.thinkingLevel : change.thinkingLevel) ?? "off");
        }
        if (!this.host?.checkAdmission && !check && compactionEnabled === undefined) await conv.configure(change, ctx);
        else await conv.commit(async tx => {
          await this.host?.checkAdmission?.(tx, conv.id, "configure");
          const doc = await tx.doc(AgentDoc, conv.id);
          if (check && ((change.model === undefined && !sameModelRef(before?.model, doc.model)) ||
              (change.thinkingLevel === undefined && before?.thinkingLevel !== doc.thinkingLevel))) throw new WireError("busy");
          if (compactionEnabled === undefined) {
            await configure(tx, conv.id, change);
            return;
          }
          const { enabled: _, ...rest } = doc.compaction ?? {};
          const compaction = compactionEnabled !== null ? { ...rest, enabled: compactionEnabled }
            : Object.keys(rest).length > 0 ? rest : null;
          await configure(tx, conv.id, { ...change, compaction });
        }, ctx);
        return { result: {} };
      }
      case "thread.abort":
        await conv.abort(ctx);
        return { result: {} };
      case "thread.wait": return { wait: conv.waitForIdle(ctx).then(() => ({})) };
      case "thread.compact": {
        const check = this.host?.checkAdmission;
        if (check) await conv.commit(tx => check(tx, conv.id, "compact"), ctx);
        return { result: { taskId: id(await conv.compact(request.params.instructions, ctx)) } };
      }
      case "thread.reset": {
        const check = this.host?.checkAdmission;
        if (!check) await conv.reset(request.params.handoff, ctx);
        else {
          const { handoff } = request.params;
          await conv.commit(async tx => {
            await check(tx, conv.id, "reset");
            return conv.submit(tx, { type: "write", entry: { kind: ResetEntry.kind, head: "self",
              ...(handoff === undefined ? {} : { model: [{ role: "user" as const, content: handoff, timestamp: Date.now() }] }),
            } });
          }, ctx);
          this.harness.resume();
        }
        return { result: {} };
      }
      case "submit": {
        const { requestId, text, whenBusy } = request.params;
        const draft = { type: "input", content: text, requestId, whenBusy, entry: environmentEntry(environment) } as const;
        if (skillToken && !this.host?.prepare) throw new WireError("unsupported_capability", "workspaceSkills");
        if (!this.host?.prepare && !this.host?.checkAdmission) return { result: { submissionId: id((await conv.submit(draft, ctx)).id) } };
        // Return the saved receipt before loading resources or running host admission hooks.
        const previous = await conv.commit(tx => tx.submissionByRequest(conv.id, requestId), ctx);
        if (previous) return { result: { submissionId: id(previous.id) } };
        const prepared = this.host?.prepare ? await this.#prepare(conv, ctx, text, skillToken) : undefined;
        if (prepared?.text !== undefined && prepared.text !== text) {
          encode({ ...request, params: { ...request.params, text: prepared.text } });
        }
        await prepared?.confirm?.(ctx.abortSignal);
        const submission = await conv.commit(async tx => {
          const raced = await tx.submissionByRequest(conv.id, requestId);
          if (raced) return raced.id;
          await this.host?.checkAdmission?.(tx, conv.id, "input");
          if (prepared?.verify) prepared.verify(await tx.doc(AgentDoc, conv.id));
          if (prepared?.agent) await configure(tx, conv.id, prepared.agent);
          return conv.submit(tx, { ...draft, content: prepared?.text ?? text });
        }, ctx);
        this.harness.resume(); // transactional admission does not resume the scheduler
        return { result: { submissionId: id(submission) } };
      }
      case "continue": {
        const draft = { type: "continue" } as const;
        if (!this.host?.prepare && !this.host?.checkAdmission) return { result: { submissionId: id((await conv.submit(draft, ctx)).id) } };
        const prepared = this.host?.prepare ? await this.#prepare(conv, ctx) : undefined;
        await prepared?.confirm?.(ctx.abortSignal);
        const submission = await conv.commit(async tx => {
          await this.host?.checkAdmission?.(tx, conv.id, "continue");
          if (prepared?.verify) prepared.verify(await tx.doc(AgentDoc, conv.id));
          if (prepared?.agent) await configure(tx, conv.id, prepared.agent);
          return conv.submit(tx, draft);
        }, ctx);
        this.harness.resume();
        return { result: { submissionId: id(submission) } };
      }
    }
  }

  async getImage(ref: ImageRef, ctx: Context) {
    const conv = await this.#get(ref.threadId, ctx);
    const bound = entryID(ref.entryId);
    const { items } = await conv.entries({ minEntryId: bound, maxEntryId: bound }, 1, undefined, ctx);
    // Only the given conversation's visible ancestry, never a global blob lookup.
    const msg = items[0]?.id === bound ? items[0].model?.[ref.messageIndex] : undefined;
    const content = msg?.role !== "system" && msg?.content;
    const image = Array.isArray(content) ? content[ref.blockIndex] : undefined;
    if (image?.type !== "image") throw notFound();
    return projectImageBytes(image);
  }

  async submitImages(input: ImageSubmission, imagesHost: ImagesHost, signal: AbortSignal, ctx: Context,
      environment?: ClientEnvironment, skillToken?: string) {
    if (skillToken && !this.host?.prepare) throw new WireError("unsupported_capability", "workspaceSkills");
    const conv = await this.#get(input.threadId, ctx);
    await this.host?.checkMutation?.(conv, ctx);
    const { requestId, text, whenBusy } = input;
    const start = await conv.commit(async tx => {
      const previous = await tx.submissionByRequest(conv.id, requestId);
      if (previous) return { submissionId: id(previous.id) } as const;
      const model = (await tx.doc(AgentDoc, conv.id)).model;
      return { model: model ? { provider: model.provider, modelId: model.modelId } : undefined };
    }, ctx);
    if (start.submissionId) return { submissionId: start.submissionId };

    const prepared = this.host?.prepare ? await this.#prepare(conv, ctx, text, skillToken) : undefined;
    const body = prepared?.text ?? text;
    if (Buffer.byteLength(body) > 128 * 1024 || !body.isWellFormed()) throw new WireError("limit");
    const fitted = await imagesHost.fit(start.model, input.images, signal);
    if (fitted.images.length !== input.images.length) throw new WireError("internal");
    await prepared?.confirm?.(ctx.abortSignal);
    // Apply the same atomic admission/receipt rule as a text submission. Do
    // not commit fitted content if another client changed the model meanwhile.
    const submission = await conv.commit(async tx => {
      const previous = await tx.submissionByRequest(conv.id, requestId);
      if (previous) return previous.id;
      await this.host?.checkAdmission?.(tx, conv.id, "input");
      const current = await tx.doc(AgentDoc, conv.id);
      prepared?.verify?.(current);
      const model = prepared?.agent?.model === undefined ? current.model : prepared.agent.model;
      if (model?.provider !== start.model?.provider || model?.modelId !== start.model?.modelId) throw new WireError("busy");
      if (prepared?.agent) await configure(tx, conv.id, prepared.agent);
      return conv.submit(tx, {
        type: "input", whenBusy, requestId, entry: environmentEntry(environment),
        content: [...(body ? [{ type: "text" as const, text: body }] : []), ...fitted.images, ...(fitted.notes ?? [])],
      });
    }, ctx);
    this.harness.resume();
    return { submissionId: id(submission) };
  }

  async #prepare(conv: Conversation, ctx: Context, text?: string, skillToken?: string) {
    const doc = await this.harness.snapshot(AgentDoc, conv.id, ctx);
    const prepared = await this.host!.prepare!({ threadId: id(conv.id), cwd: doc?.cwd, text, skillToken }, ctx);
    const model = await checkPreparedModel(this.host?.requireModel, doc?.model, prepared.agent);
    const verify = (model || prepared.confirm) ? (current: AgentState) => {
      if (prepared.confirm && current.cwd !== doc?.cwd) throw new WireError("busy");
      model?.(current.model);
    } : undefined;
    return { ...prepared, verify };
  }

  async #get(id: string, ctx: Context) {
    const conv = await this.harness.conversation(threadID(id), ctx);
    if (!conv || await this.host?.privateConversation?.(conv.id, ctx)) throw notFound();
    return conv;
  }

  async #history(conv: Conversation, { threadId, range: input, limit }: Params["history"], ctx: Context, delegation: boolean) {
    const namespace = "history:" + threadId + (delegation ? ":delegation" : "");
    const range = input.kind === "cursor" ? this.#cursors.decode<ExplicitRange>(namespace, input.cursor) : input;
    const ascending = range.kind === "after";
    const lower = range.kind === "after" ? range.afterEntryId : null;
    const upper = range.kind === "after" ? range.throughEntryId : range.beforeEntryId;
    if (ascending && lower === null && upper === null) return { threadId, entries: [], nextCursor: null };
    if ((ascending && upper === null) || (lower !== null && Number(lower) > Number(upper))) throw new WireError("invalid_cursor");
    for (const bound of new Set([lower, upper])) {
      if (bound !== null && (!durableID(bound) || !(await visible(conv, bound, ctx, delegation)))) throw new WireError("invalid_cursor");
    }
    const entries = await visibleEntries(conv, {
      order: ascending ? "ascending" : "descending",
      minEntryId: lower === null ? undefined : entryID(lower),
      maxEntryId: upper === null ? undefined : entryID(upper),
    }, limit + 2, delegation, ctx);
    const eligible = entries.filter(entry => id(entry.id) !== (ascending ? lower : upper));
    const selected = eligible.slice(0, limit);
    const last = selected.at(-1);
    const next: ExplicitRange | null = eligible.length > limit && last
      ? (range.kind === "after" ? { ...range, afterEntryId: id(last.id) } : { ...range, beforeEntryId: id(last.id) }) : null;
    return {
      threadId, entries: (ascending ? selected : selected.reverse()).map(projectEntry),
      nextCursor: next ? this.#cursors.encode(namespace, next) : null,
    };
  }
}
