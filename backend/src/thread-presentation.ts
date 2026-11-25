import { defineDoc, type ConversationId, type RunFailure, type RunFailureReason, type TaskGraph } from "@earendil-works/pi-durable";
import type { ThreadStatus } from "../../server/src/thread-directory.js";
import type { ManagedCheckout } from "./worktrees.js";

// Display metadata does not change the Durable run, its inputs or their terminal outcomes.
type ThreadPresentation = {
  title?: string; autoTitle?: string; topicTitle?: string; generatedTitle?: string; titleAttempted?: boolean;
  dismissedFailure?: number; archived?: boolean; checkout?: ManagedCheckout;
};
export const ThreadPresentationDoc = defineDoc<ThreadPresentation>({
  kind: "hush.thread-presentation", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

function cleanLabel(value: string) {
  return value.slice(0, 2048).toWellFormed().replace(/[\p{Cc}\p{Cf}\s]+/gu, " ").trim();
}

export function threadLabel(value: string, max: number) {
  const prefix = cleanLabel(value);
  const chars = Array.from(prefix);
  return chars.length > max ? chars.slice(0, max - 1).join("") + "…" : prefix;
}

// Only automatic titles use these limits. A deliberate user name is kept as-is.
export function shortTitle(value: string) {
  const words = cleanLabel(value).replace(/^[#*`"' ]+|[*`"']+$/g, "").split(/ +/u, 4);
  while (words.length >= 2) {
    const title = words.join(" ");
    if (Array.from(title).length <= 96 && /\p{L}/u.test(title)) return title;
    words.pop();
  }
  return undefined;
}

const filler = new Set(("a an and are as at be been being by can can't cannot could did do does for from had has have hello hey hi how i i'd i'll i'm in is it it's just " +
  "me more my of on or our other please should some so than that the their them there these they this those to up us use using was we were what when where " +
  "which who will with would you your about again also check could details fix fixed help maybe need review seems think trying want way work").split(" "));
const generic = new Set(["bug", "issue", "problem", "change", "changes", "thing", "thread", "new", "start", "global"]);
const titleVerbs = new Set(["load", "start", "reload", "show", "build", "link", "test", "update"]);
const titleWord = /[\p{L}\p{N}][\p{L}\p{M}\p{N}]*(?:[._'’-][\p{L}\p{M}\p{N}]+)*/gu;

function topicWords(text: string) {
  // A bounded excerpt may end in the middle of a word. Don't turn that fragment into a name.
  return [...text.replace(/\S+…$/u, "").matchAll(titleWord)].flatMap(match => {
    const word = match[0], key = word.toLowerCase();
    const aliases = [key, ...key.split("-")].map(part => part.replace(/(?:ing|ed)$/, "").replace(/s$/, ""));
    return filler.has(key) || !/\p{L}/u.test(word) || Array.from(word).length > 32 ? [] :
      [{ word, key, aliases, end: match.index + word.length, start: match.index }];
  });
}

// A local extractive title: prefer neighboring topic words, especially ones
// supported by both sides of the exchange. Don't require a second model call.
export function topicTitle(user: string, answer = "") {
  if (user.startsWith("/skill:")) return answer ? shortTitle(user.slice(7) + " skill") ?? "Skill invocation" : user;
  const prompt = topicWords(user), reply = topicWords(answer);
  let best = "", score = -Infinity;
  for (const [words, support, text, fromPrompt] of [[prompt, reply, user, true], [reply, prompt, answer, false]] as const) {
    const matching = new Set(support.flatMap(w => w.aliases));
    for (let i = 0; i + 1 < words.length; i++) {
      const phrase = words.slice(i, i + 2);
      if (phrase[0]!.key === phrase[1]!.key) continue;
      const shared = phrase.filter(w => w.aliases.some(a => matching.has(a)));
      if (!fromPrompt && prompt.length && !shared.length) continue;
      const adjacent = /^\s+$/.test(text.slice(phrase[0]!.end, phrase[1]!.start));
      const rank = (fromPrompt ? 1 : 0) - Math.min(1, i / 20) + (adjacent ? 2 : 0) +
        shared.reduce((sum, w) => sum + (generic.has(w.aliases[0]!) ? 1 : 3), 0) +
        phrase.reduce((sum, w) => sum + (generic.has(w.aliases[0]!) ? -.5 : 1) + (w.key.includes("-") ? 2.5 : 0), 0) +
        (!fromPrompt && adjacent && /ing$/.test(phrase[1]!.key) && titleVerbs.has(phrase[1]!.aliases[0]!) ? 1.5 : 0);
      if (rank <= score) continue;
      const title = shortTitle(phrase.map(w => w.word).join(" "));
      if (title) {
        best = title;
        score = rank;
      }
    }
  }
  return best || (prompt[0] ? `${prompt[0].word} discussion` : "Quick chat");
}

export function displayTitle(doc: Readonly<ThreadPresentation> | undefined) {
  return doc?.title || (doc?.generatedTitle && shortTitle(doc.generatedTitle)) ||
    doc?.topicTitle || (doc?.autoTitle && topicTitle(doc.autoTitle)) || undefined;
}

export function actionableFailure(reason: RunFailureReason) {
  switch (reason) {
    case "model_error": case "no_model": case "faulted": case "orphaned": case "interrupted": case "restart_limit": case "turn_limit":
      return true;
    default: // aborted, reset and nothing_to_answer don't need a recovery action
      return false;
  }
}

// Use the live task graph only for current activity. A foreground retry is
// still a run, a sleeping tool is parked, and a future background wake is
// scheduled. A blocked parent can't mask a runnable foreground child.
export function threadActivity(graph: TaskGraph) {
  const states = new Map<ConversationId, "running" | "parked" | "scheduled">();
  const ranks: Record<string, number> = { running: 3, parked: 2, scheduled: 1 };
  const roots = new Map<number, { thread: ConversationId; background: boolean }>();
  for (const node of Object.values(graph.tasks)) {
    let root = { thread: node.conversationId, background: node.background };
    let cursor = node;
    while (cursor.owner !== undefined) {
      const cached = roots.get(cursor.owner);
      if (cached) { root = { thread: cached.thread, background: root.background || cached.background }; break; }
      const parent = graph.tasks[String(cursor.owner)];
      if (!parent) break;
      root = { thread: parent.conversationId, background: root.background || parent.background };
      cursor = parent;
    }
    roots.set(node.id, root);
    const state = node.state;
    const status = node.blocked ? (root.background ? undefined : "parked")
      : state.status === "pending" || state.status === "running" ? "running"
      : state.status === "sleeping" ? (root.background ? (state.until === undefined ? undefined : "scheduled")
        : node.kind === "pi.generation" && state.until !== undefined ? "running" : "parked")
      : root.background ? undefined : "parked";
    const previous = states.get(root.thread);
    if (status && (!previous || ranks[status]! > ranks[previous]!)) states.set(root.thread, status);
  }
  return states as ReadonlyMap<ConversationId, ThreadStatus>;
}

// Durable owns the actual cause. Never put arbitrary provider bodies or task
// exceptions on the wire: they can echo credentials, headers or prompt data.
// Project only recognizable diagnostics, not substrings from an error payload.
export function failureDetail({ reason, detail }: RunFailure, faux: boolean): string | undefined {
  if (!detail) return;
  if (reason === "no_model") {
    if (detail === "No model is configured") return "no model configured";
    const ref = /^Model ([\w.:-]{1,64}\/[\w.:-][\w./:-]{0,128}) is not available$/.exec(detail)?.[1];
    if (ref) return threadLabel(`${ref} is unavailable`, 160);
  } else if (reason === "model_error") {
    if (faux && detail === "synthetic failure") return detail;
    const text = detail.slice(0, 4096);
    const status = /(?:^\s*|\b(?:HTTP|status(?: code)?)[ :]+|\()([45]\d\d)\b/i.exec(text)?.[1];
    if (/\binsufficient[_ ]quota\b|\bquota exceeded\b|\bcredit balance is too low\b/i.test(text)) return "provider quota exhausted";
    if (status === "429") return "rate limited (429)";
    if (/\brate[_ -]?limit|\btoo many requests\b/i.test(text)) return "provider rate limited the request";
    if (/\bcontext[_ ]length[_ ]exceeded\b|\bmaximum context length\b|\bprompt is too long\b/i.test(text)) return "model context limit exceeded";
    if (/\bno (?:api key|credentials|authentication)\b/i.test(text)) return "provider credentials not configured";
    if (status === "401" || /\binvalid[_ ]api[_ ]key\b|\bunauthorized\b/i.test(text)) return "provider authentication failed";
    if (status === "403" || /\bforbidden\b|\bpermission denied\b/i.test(text)) return "provider denied access";
    if (/\bmodel[_ ]not[_ ]found\b/i.test(text)) return "provider could not find the model";
    if (/\b(?:timed? ?out|timeout|ETIMEDOUT)\b/i.test(text)) return "model request timed out";
    if (/\b(?:fetch failed|connection refused|ECONNRESET|ECONNREFUSED|ENOTFOUND)\b/i.test(text)) return "could not reach the model provider";
    if (status?.startsWith("5") || /\boverloaded\b/i.test(text)) return "model provider unavailable";
  } else if (reason === "turn_limit" && /^\d{1,8} turns?$/.test(detail)) {
    return detail;
  } else if (reason === "orphaned") {
    switch (detail) {
      case "missing_task": return "task definition missing";
      case "task_too_old": return "task version no longer supported";
      case "migration_failed": return "task migration failed";
    }
  }
}
