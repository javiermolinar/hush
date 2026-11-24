// Pi-facing compaction result proof. This records source boundaries, not a live event stream.
import type { Context } from "@earendil-works/chord/context";
import { getCurrentSystemMessage, type Message } from "@earendil-works/pi-ai";
import { calculateContextTokens, estimateTokens, type CompactionResult as PiResult } from "@earendil-works/pi-coding-agent";
import {
  CompactionEntry, CompactionTask,
  type CompactionReason, type CompactionResult, type ContextView, type Conversation,
  type ConversationId, type EntryId, type EntryRecord, type Harness, type SummaryRequest, type TaskId, type TaskRecord,
} from "@earendil-works/pi-durable";

export type CompactionId = TaskId<CompactionResult>;
export type Selection = Pick<SummaryRequest, "tail" | "firstKept"> & { conversationId: ConversationId };
export type CompactionOutcome = { reason: CompactionReason } & (
  | { kind: "applied"; entryId: EntryId; result: PiResult & { estimatedTokensAfter: number } }
  | { kind: "aborted" }
  | { kind: "not_applied"; cause: "no_summary" | "stale" }
  | { kind: "failed" | "unavailable"; message: string }
);

const taskKind = CompactionTask.definition.name;

// Attach before scheduling. The task freezes the original range in a committed summarize/retry checkpoint.
// The sink must only record this data synchronously; the caller owns retention and detachment.
export function observeCompactionSelections(harness: Harness, record: (taskId: CompactionId, selection: Selection) => void) {
  return harness.observeCommits({
    advance(publication) {
      for (const change of publication.changes) {
        if (change.type !== "task" || change.value.kind !== taskKind) continue;
        const { id, conversationId, state } = change.value;
        const point = state.checkpoint;
        if (!point || typeof point !== "object" || Array.isArray(point)) continue;
        if (point.phase !== "summarize" && point.phase !== "retry") continue;
        if (!isEntryId(point.tail) || !isEntryId(point.firstKept) || point.firstKept > point.tail) continue;
        record(id as CompactionId, { conversationId, tail: point.tail, firstKept: point.firstKept });
      }
    },
    end() {},
  });
}

function isEntryId(value: unknown): value is EntryId {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}

function reasonOf(task: TaskRecord<unknown, unknown, unknown>): CompactionReason | undefined {
  if (task.kind !== taskKind || !task.input || typeof task.input !== "object" || !("reason" in task.input)) return undefined;
  const { reason } = task.input;
  return reason === "manual" || reason === "threshold" || reason === "overflow" ? reason : undefined;
}

// Durable 1.6 documents a wrapped summary in the model payload, not a plain summary field.
const summaryOpen = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n";
const summaryClose = "\n</summary>";
export function summaryOf(entry: EntryRecord | undefined): string | undefined {
  if (!CompactionEntry.is(entry) || entry.model?.length !== 1) return;
  const message = entry.model[0];
  if (message.role !== "user" || !Array.isArray(message.content) || message.content.length !== 1) return;
  const block = message.content[0];
  if (block.type !== "text" || !block.text.startsWith(summaryOpen) || !block.text.endsWith(summaryClose)) return;
  return block.text.slice(summaryOpen.length, -summaryClose.length);
}

type Estimator = (message: Message) => number;

// Pi counts a compaction summary as text only. Use the same text estimate without the Durable wrapper.
function estimator(view: ContextView): Estimator | undefined {
  if (!CompactionEntry.is(view.head)) return estimateTokens;
  const raw = view.head.model?.[0];
  const summary = summaryOf(view.head);
  if (summary === undefined) return undefined;
  return (message) => message === raw
    ? estimateTokens({ role: "user", content: summary, timestamp: message.timestamp })
    : estimateTokens(message);
}

function estimateBefore(view: ContextView, estimate: Estimator): number {
  // A cut or edit invalidates provider usage from earlier context. Associate the current projected
  // message with its source entry before using its usage; an edited/reordered view need not be raw history.
  const fence = view.entries.reduce((latest, entry) =>
    entry.edits?.length ? Math.max(latest, entry.id) : latest, view.head?.id ?? 0);
  for (let i = view.messages.length - 1; i >= 0; i--) {
    const message = view.messages[i];
    if (message.role !== "assistant" || message.stopReason === "error" || message.stopReason === "aborted") continue;
    const tokens = calculateContextTokens(message.usage);
    if (tokens <= 0) continue;
    if (view.contributions.some((source, j) => view.entries[j].id > fence && source.includes(message))) {
      return view.messages.slice(i + 1).reduce((count, trailing) => count + estimate(trailing), tokens);
    }
    break;
  }
  const system = getCurrentSystemMessage(view.messages);
  return view.messages.reduce((count, message) => count + (message.role === "system" ? 0 : estimate(message)),
    system === undefined ? 0 : estimateTokens(system));
}

export function estimateCompactionContext(view: ContextView): number | undefined {
  const estimate = estimator(view);
  return estimate && estimateBefore(view, estimate);
}

export async function resolveCompaction(
  harness: Harness, conversation: Conversation, taskId: CompactionId,
  getSelection: (taskId: CompactionId) => Selection | undefined, context: Context,
  placement: "wait" | "committed" = "wait",
): Promise<CompactionOutcome> {
  const task = await harness.getTask(taskId, context);
  const reason = task && reasonOf(task);
  if (task?.conversationId !== conversation.id || reason === undefined) {
    throw new Error("No such compaction task in this conversation");
  }
  const { state: { outcome } } = await harness.waitForTask(taskId, context);
  if (outcome.status === "aborted") return { kind: "aborted", reason };
  if (outcome.status !== "completed") {
    return { kind: "failed", reason, message: outcome.error?.message ?? outcome.reason ?? outcome.status };
  }
  let { entryId } = outcome.result;
  if (outcome.result.submissionId !== undefined) {
    const write = await harness.submission(outcome.result.submissionId, context);
    if (!write) return { kind: "unavailable", reason, message: "Compaction write receipt is missing" };
    // A beforeRequest hook may only verify placement at an already committed boundary.
    // Waiting on an unplaced write here would block the very generation that places it.
    const receipt = placement === "committed" ? await write.status(context) : await write.wait(context);
    if (receipt.type !== "write" || receipt.conversationId !== conversation.id) {
      return { kind: "unavailable", reason, message: "Compaction write receipt does not match this conversation" };
    }
    if (receipt.status === "queued") {
      return { kind: "unavailable", reason, message: "Summary write was not placed at the required boundary" };
    }
    if (receipt.status === "unanswered") {
      if (receipt.reason === "stale") return { kind: "not_applied", reason, cause: "stale" };
      return { kind: "failed", reason, message: receipt.reason };
    }
    entryId = receipt.entry;
  }
  if (entryId === undefined) return { kind: "not_applied", reason, cause: "no_summary" };

  const selection = getSelection(taskId);
  if (selection?.conversationId !== conversation.id || selection.tail >= entryId) {
    return { kind: "unavailable", reason, message: "Compaction selection boundary was not captured" };
  }
  const [before, after] = await Promise.all([
    conversation.context(context, { at: selection.tail }),
    conversation.context(context, { at: entryId }),
  ]);
  const summary = summaryOf(after.head);
  if (!CompactionEntry.is(after.head) || after.head.id !== entryId || after.head.head !== selection.firstKept ||
      after.head.data.reason !== reason || summary === undefined ||
      !before.entries.some((entry) => entry.id === selection.firstKept)) {
    return { kind: "unavailable", reason, message: "Placed summary does not match its captured selection" };
  }
  const beforeEstimate = estimator(before);
  const afterEstimate = estimator(after);
  if (!beforeEstimate || !afterEstimate) {
    return { kind: "unavailable", reason, message: "Unrecognized Durable compaction summary format" };
  }
  return {
    kind: "applied", reason, entryId,
    result: {
      summary,
      firstKeptEntryId: String(selection.firstKept),
      tokensBefore: estimateBefore(before, beforeEstimate),
      estimatedTokensAfter: after.messages.reduce((count, message) => count + afterEstimate(message), 0),
    },
  };
}
