import { defineDoc, type ConversationId, type RunFailure, type RunFailureReason, type TaskGraph } from "@earendil-works/pi-durable";
import type { ThreadStatus } from "../../server/src/thread-directory.js";
import type { ManagedCheckout } from "./worktrees.js";

// Hush's display metadata is independent of Pi's session profile and does not
// change the Durable run, its inputs or their terminal outcomes.
type ThreadPresentation = {
  title?: string; autoTitle?: string; generatedTitle?: string; titleAttempted?: boolean;
  dismissedFailure?: number; archived?: boolean; checkout?: ManagedCheckout;
};
export const ThreadPresentationDoc = defineDoc<ThreadPresentation>({
  kind: "hush.thread-presentation", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

export function threadLabel(value: string, max: number) {
  const prefix = value.slice(0, 2048).toWellFormed().replace(/[\p{Cc}\p{Cf}\s]+/gu, " ").trim();
  const chars = Array.from(prefix);
  return chars.length > max ? chars.slice(0, max - 1).join("") + "…" : prefix;
}

export function displayTitle(doc: Readonly<ThreadPresentation> | undefined, piName?: string) {
  return doc?.title || (piName && threadLabel(piName, 96)) || doc?.generatedTitle || doc?.autoTitle || undefined;
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
