import type { Context } from "@earendil-works/chord/context";
import type { Api, Message, Model, Usage, UserMessage } from "@earendil-works/pi-ai";
import type { SessionStats } from "@earendil-works/pi-coding-agent";
import { CompactionEntry, UsageDoc, UserEntry, defineDoc, defineDocFamily, type ContextView, type Conversation, type Cursor,
  type EntryId, type EntryRecord, type Harness } from "@earendil-works/pi-durable";
import { estimateCompactionContext, summaryOf } from "../../../protocol/v1/pi-1.6/compaction-result.mts";
import { QueueDelivery } from "../../../protocol/v1/pi-1.6/local-queue.mts";
import { Rejected } from "./commands.js";

export const ProfileDoc = defineDoc<{
  id?: string;
  timestamp?: string;
  cwd?: string;
  name?: string;
  parentSession?: string;
  autoCompaction?: boolean;
  systemPrompt?: string;
  appendSystemPrompt?: string;
}>({
  kind: "hush.pi-profile", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

export const SummaryDoc = defineDocFamily<{ tokensBefore: number }, number>({
  kind: "hush.pi-summary", version: 1, family: true, scope: "conversation", history: "latest", fork: "initial",
  initial: tokensBefore => ({ tokensBefore }), checkpointWhen: () => true,
});

export async function sessionMessages(h: Harness, view: ContextView, ctx: Context) {
  const head = view.head;
  if (!CompactionEntry.is(head)) return [...view.messages];
  const raw = head.model?.[0];
  const summary = summaryOf(head);
  const metadata = await h.snapshot(SummaryDoc, head.conversationId, String(head.id), ctx);
  if (!raw || !view.messages.includes(raw) || summary === undefined || !metadata) {
    // A restart may leave a native summary placed without any verified Pi estimate.
    // Never fill the gap with a guessed size, count or boundary.
    throw new Rejected("Verified Pi compaction metadata is unavailable");
  }
  return view.messages.map(m => m === raw ? {
    role: "compactionSummary" as const, summary, timestamp: raw.timestamp, tokensBefore: metadata.tokensBefore,
  } : m);
}

export function publicModel(model: Model<Api>): Model<Api> {
  // Do not spread raw model configuration. Request headers, endpoint paths and
  // query parameters can contain credentials. An origin is enough to identify it.
  let baseUrl = "";
  try { const url = new URL(model.baseUrl); if (url.protocol === "http:" || url.protocol === "https:") baseUrl = url.origin; }
  catch { /* non-HTTP provider */ }
  return {
    id: model.id, name: model.name, provider: model.provider, api: model.api, baseUrl,
    input: [...model.input], reasoning: model.reasoning, contextWindow: model.contextWindow, maxTokens: model.maxTokens,
    cost: { input: model.cost.input, output: model.cost.output, cacheRead: model.cost.cacheRead, cacheWrite: model.cost.cacheWrite },
  };
}

const isUser = (entry: EntryRecord) => UserEntry.is(entry) || entry.kind === QueueDelivery;
function userText(message: UserMessage) {
  return typeof message.content === "string" ? message.content : message.content.flatMap(b => b.type === "text" ? [b.text] : []).join("");
}

// Scan fork-aware immutable history through one observed tail. Active model context
// may discard earlier turns after compaction; it is not a source of fork IDs or spend.
export async function* branch(conv: Conversation, ctx: Context, at?: EntryId) {
  const tail = at ?? (await conv.entries({}, 1, undefined, ctx)).items[0]?.id;
  if (tail === undefined) return;
  let cursor: Cursor | undefined;
  do {
    const page = await conv.entries({ order: "ascending", maxEntryId: tail }, 100, cursor, ctx);
    yield* page.items;
    cursor = page.next;
  } while (cursor);
}

export async function forkPoints(conv: Conversation, ctx: Context) {
  const messages: { entryId: string; text: string }[] = [];
  for await (const entry of branch(conv, ctx)) {
    const text = forkText(entry);
    if (text) messages.push({ entryId: String(entry.id), text });
  }
  return messages;
}

export function forkText(entry: EntryRecord) {
  const user = isUser(entry) && entry.model?.find((m): m is UserMessage => m.role === "user");
  return user ? userText(user) : undefined;
}

// Use the active projection and never confuse a normal textless reply with a
// tool-only/failed update. Durable's model context excludes failed attempts.
export function lastText(view: ContextView) {
  for (const message of [...view.messages].reverse()) {
    if (message.role !== "assistant") continue;
    const text = message.content.flatMap(block => block.type === "text" ? [block.text] : []).join("").trim();
    if (text || message.stopReason === "stop") return text || undefined;
  }
}

export async function stats(harness: Harness, conv: Conversation, sessionId: string, model: Model<Api>, ctx: Context): Promise<SessionStats> {
  const tokens = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 };
  let userMessages = 0, assistantMessages = 0, toolCalls = 0, toolResults = 0, totalMessages = 0, cost = 0;
  const add = (usage: Usage) => {
    tokens.input += usage.input;
    tokens.output += usage.output;
    tokens.cacheRead += usage.cacheRead;
    tokens.cacheWrite += usage.cacheWrite;
    cost += usage.cost.total;
  };
  for await (const entry of branch(conv, ctx)) {
    if (CompactionEntry.is(entry)) continue;
    for (const message of entry.model ?? []) {
      totalMessages++;
      if (message.role === "user") userMessages++;
      else if (message.role === "assistant") {
        assistantMessages++;
        toolCalls += message.content.filter(x => x.type === "toolCall").length;
        if (entry.conversationId !== conv.id) add(message.usage);
      } else if (message.role === "toolResult") {
        toolResults++;
        if (entry.conversationId !== conv.id && message.usage) add(message.usage);
      }
    }
  }
  const usage = await harness.snapshot(UsageDoc, conv.id, ctx);
  for (const report of [...Object.values(usage?.models ?? {}), ...Object.values(usage?.tools ?? {})]) add(report);
  tokens.total = tokens.input + tokens.output + tokens.cacheRead + tokens.cacheWrite;
  const view = await conv.context(ctx);
  const postCut = !CompactionEntry.is(view.head) || view.entries.some((entry, i) => entry.id > view.head!.id &&
    view.contributions[i].some((m: Message) => m.role === "assistant" && (m.usage.input + m.usage.output + m.usage.cacheRead + m.usage.cacheWrite) > 0));
  const count = postCut ? estimateCompactionContext(view) ?? null : null;
  return {
    sessionId, sessionFile: undefined, userMessages, assistantMessages, toolCalls, toolResults,
    totalMessages, cost, tokens,
    contextUsage: model.contextWindow > 0 ? { tokens: count, contextWindow: model.contextWindow,
      percent: count === null ? null : count * 100 / model.contextWindow } : undefined,
  };
}
