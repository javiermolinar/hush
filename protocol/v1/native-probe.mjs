// Minimal fixture adapter over Durable 1.6. No transport, process state, or Pi event production.
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { diffRevisions } from "@earendil-works/chord/delta";
import { createModels, fauxProvider } from "@earendil-works/pi-ai";
import { createRegistry, defineExtension, defineTool, Harness, MemoryStorage } from "@earendil-works/pi-durable";

export { context };
export const id = (n) => String(n);
const field = (key, value) => value === undefined ? {} : { [key]: value };
const idField = (key, n) => field(key, n === undefined ? undefined : id(n));

export function projectThread(row) {
  return {
    id: id(row.id),
    ...(row.parent ? { parent: { threadId: id(row.parent.conversationId), atEntryId: id(row.parent.at) } } : {}),
    ...(row.owner ? { owner: { threadId: id(row.owner.conversationId), taskId: id(row.owner.taskId) } } : {}),
  };
}

function blocks(content) {
  if (typeof content === "string") return [{ type: "text", text: content }];
  return (content ?? []).map((item) => {
    switch (item.type) {
      case "text": return { type: "text", text: item.text };
      case "thinking": return item.redacted ? { type: "omitted", kind: "redactedThinking" }
        : { type: "thinking", text: item.thinking };
      case "toolCall": return { type: "toolCall", id: item.id, name: item.name, arguments: item.arguments };
      case "image": return { type: "omitted", kind: "image" };
      default: return { type: "omitted", kind: "other" };
    }
  });
}

function projectMessage(message) {
  const base = {
    role: message.role, timestampMs: message.timestamp,
    blocks: message.role === "system" ? [] : blocks(message.content),
  };
  switch (message.role) {
    case "user":
    case "system": return base;
    case "assistant": return { ...base, stopReason: message.stopReason, ...field("error", message.errorMessage) };
    case "toolResult": return { ...base, callId: message.toolCallId, name: message.toolName, isError: message.isError };
    default: throw new Error(`not a native message role: ${message.role}`);
  }
}

export function projectEntry(entry) {
  return {
    id: id(entry.id), threadId: id(entry.conversationId), kind: entry.kind,
    messages: (entry.model ?? []).map(projectMessage), ...idField("headEntryId", entry.head),
  };
}

const text = (content) => typeof content === "string" ? content : content
  .filter((block) => block.type === "text").map((block) => block.text).join("\n");

export function projectState(view) {
  const agent = view.docs["pi.agent"] ?? {};
  const live = view.docs["pi.live"] ?? {};
  const entries = view.entries.map(projectEntry);
  const ledger = view.docs["pi.usage"];
  const usage = { inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0, totalTokens: 0, costUsd: 0 };
  for (const record of [...Object.values(ledger?.models ?? {}), ...Object.values(ledger?.tools ?? {})]) {
    usage.inputTokens += record.input;
    usage.outputTokens += record.output;
    usage.cacheReadTokens += record.cacheRead;
    usage.cacheWriteTokens += record.cacheWrite;
    usage.totalTokens += record.totalTokens;
    usage.costUsd += record.cost.total;
  }
  return {
    thread: projectThread(view.conversation),
    agent: { ...field("model", agent.model), ...field("thinkingLevel", agent.thinkingLevel), ...field("cwd", agent.cwd),
      ...field("compactionEnabled", agent.compaction?.enabled) },
    usage,
    window: { entries, beforeEntryId: view.before === undefined ? null : id(view.before), lastEntryId: entries.at(-1)?.id ?? null },
    tools: (live.tools ?? []).map((slot) => ({
      callId: slot.callId, name: slot.name, status: slot.status, ...idField("taskId", slot.taskId),
      ...idField("resultEntryId", slot.entry), ...field("output", slot.output),
      ...field("droppedBytes", slot.droppedBytes), ...field("droppedLines", slot.droppedLines),
    })),
    queue: (view.docs["pi.inbox"]?.items ?? []).filter((item) => item.mode !== "write")
      .map((item) => ({ submissionId: id(item.id), mode: item.mode, text: text(item.content) })),
    compactions: (live.compactions ?? []).map((item) => ({
      taskId: id(item.taskId), reason: item.trigger, blocking: item.blocking, attempt: item.attempt,
    })),
    ...(live.compactionFailure ? { compactionFailure: {
      taskId: id(live.compactionFailure.taskId), reason: live.compactionFailure.trigger, detail: live.compactionFailure.error,
    } } : {}),
    ...(live.run ? { run: { taskId: id(live.run.taskId), submissionIds: live.run.inputs.map(id) } } : {}),
    ...(live.runFailure ? { failure: {
      taskId: id(live.runFailure.taskId), reason: live.runFailure.reason, ...field("detail", live.runFailure.detail),
    } } : {}),
    ...(live.generation ? { streaming: {
      attempt: live.generation.attempt, blocks: blocks(live.generation.message?.content),
      ...(live.generation.retry ? { retry: { atMs: live.generation.retry.at, detail: live.generation.retry.error } } : {}),
    } } : {}),
  };
}

export async function observeNative(h, threadId, epoch, subscriptionId, { window, pendingLimit = 256 } = {}) {
  let pending = [];
  let failure;
  let ended = false;
  const observation = await h.observeConversation(threadId, {
    advance(call) {
      if (failure || ended) return;
      if (call.kind !== "value" || pending.length >= pendingLimit) {
        failure = call.kind === "value" ? "overflow" : "source_failed";
        pending = [];
      } else pending.push(call.value);
    },
    end() { if (!ended) { failure ??= "source_failed"; pending = []; } },
  }, context, window);
  let rev = 0;
  let state = projectState(observation.value);
  const identity = { epoch, subscriptionId };
  return {
    snapshot: { ...identity, rev, state },
    flush() {
      if (ended) return [];
      if (failure) {
        ended = true;
        observation.detach();
        return [{ type: "state_end", ...identity, reason: failure }];
      }
      const frames = [];
      for (const native of pending) {
        const next = projectState(native);
        const ops = diffRevisions(state, next);
        if (ops.length > 0) frames.push({ type: "patch", ...identity, baseRev: rev, nextRev: ++rev, ops });
        state = next;
      }
      pending = [];
      return frames;
    },
    detach: () => observation.detach(),
  };
}

// Fixture pagination produced by exact native scans. Continuation ranges stand in for opaque server tokens.
export async function historyPage(conv, range, limit) {
  const keys = range.kind === "after" ? [range.afterEntryId, range.throughEntryId] : [range.beforeEntryId];
  const lower = range.kind === "after" ? range.afterEntryId : null;
  const upper = range.kind === "after" ? range.throughEntryId : range.beforeEntryId;
  const ascending = range.kind === "after";
  if (ascending && lower === null && upper === null) return { entries: [], next: null };
  if ((ascending && upper === null) || (lower !== null && Number(lower) > Number(upper))) throw new Error("invalid_cursor");
  for (const key of new Set(keys)) {
    if (key === null) continue;
    const bound = Number(key);
    const exact = await conv.entries({ minEntryId: bound, maxEntryId: bound }, 1, undefined, context);
    if (exact.items[0]?.id !== bound) throw new Error("invalid_cursor");
  }
  const page = await conv.entries({
    order: ascending ? "ascending" : "descending",
    ...field("minEntryId", lower === null ? undefined : Number(lower)),
    ...field("maxEntryId", upper === null ? undefined : Number(upper)),
  }, limit + 2, undefined, context);
  const eligible = page.items.filter((entry) => ascending ? id(entry.id) !== lower : id(entry.id) !== upper);
  const selected = eligible.slice(0, limit);
  const last = selected.at(-1)?.id;
  const next = eligible.length > limit ? (ascending ? { ...range, afterEntryId: id(last) }
    : { ...range, beforeEntryId: id(last) }) : null;
  return { entries: (ascending ? selected : selected.reverse()).map(projectEntry), next };
}

export async function openNative({ storage = new MemoryStorage(), beforeToolResult } = {}) {
  const faux = fauxProvider({ api: "fixture-core-faux", tokenSize: { min: 4, max: 4 } });
  const models = createModels();
  models.setProvider(faux.provider);
  const registry = createRegistry();
  registry.install(defineExtension({ name: "fixture-core", tools: [defineTool({
    name: "fixture_echo", description: "Record a synthetic tool result.",
    parameters: { type: "object", properties: {}, additionalProperties: false },
    replay: "unsafe", // proof fixture only: the tool does no external work
    async execute(_args, api) {
      api.output("fixture output");
      await beforeToolResult?.();
      return {};
    },
  })] }));
  const h = await Harness.open(storage, { models, registry, settings: {
    compaction: { enabled: false }, progress: { partialIntervalMs: 1, outputIntervalMs: 1 },
  } }, context);
  const conv = await h.root(context, { agent: { cwd: "/fixture", model: { provider: "faux", modelId: "faux-1" } } });
  return { h, conv, faux, close: () => h.close(context) };
}
