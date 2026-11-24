import { randomUUID } from "node:crypto";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { defineDoc, defineDocFamily, defineExtension, defineSignal, defineTask, SubmissionSettled,
  type ConversationId, type Harness, type SubmissionId, type TaskId, type Tx } from "@earendil-works/pi-durable";
import { defaultLateMs, scheduleLimit, scheduleReportKind, type ScheduleItem, type SchedulePrerequisite, type ScheduleRequest,
  type SchedulesHost } from "../../server/src/scheduling.js";
import { WireError } from "../../server/src/wire.js";
import { ThreadPresentationDoc } from "./thread-presentation.js";

const ScheduleIndex = defineDoc<{ ids: string[] }>({
  kind: "hush.schedule-index", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({ ids: [] }), checkpointWhen: () => true,
});
type Occurrence = { row?: ScheduleItem; text?: string; requestId?: string; task?: number; force?: true; wake?: string };
const Schedule = defineDocFamily<Occurrence, null>({
  kind: "hush.schedule", version: 1, family: true, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});
const Changed = defineSignal({ name: "hush.schedule.changed", version: 1, schema: { type: "boolean" } as const });
const settled = "settled";
const phase = { phase: "wake" } as const;
const ended = { status: "terminal", outcome: { status: "completed", result: null } } as const;

function sleep(record: Occurrence, until?: number) {
  // Durable signal keys are single-use, including when a deadline won.
  const key = record.wake = randomUUID();
  return { status: "sleeping" as const, checkpoint: phase,
    ...(until !== undefined && { until }), signals: [Changed.key(key)] };
}
const id = (s: string) => Number(s) as ConversationId;
const terminal = (row: ScheduleItem) => row.state === "done" || row.state === "failed" || row.state === "cancelled";
const copy = (row: ScheduleItem): ScheduleItem => JSON.parse(JSON.stringify(row)) as ScheduleItem;

async function prerequisite(tx: Tx, ref: SchedulePrerequisite | undefined):
    Promise<{ state: "waiting" | "failed" } | { state: "done"; at?: number }> {
  if (!ref) return { state: "done", at: 0 };
  const record = await tx.submission(Number(ref.submissionId) as SubmissionId);
  if (!record || record.conversationId !== id(ref.threadId) || record.type === "write" ||
      record.status === "unanswered") return { state: "failed" };
  if (record.status !== "done") return { state: "waiting" };
  const answer = await tx.entry(record.answer);
  const task = answer?.byTaskId === undefined ? undefined : await tx.task(answer.byTaskId);
  // Standalone selects no afterRun hooks. On the pinned generation implementation
  // the task that produced a standing answer becomes terminal in the settlement
  // commit, also if its onYield phase waited. Do not use the provider timestamp
  // (usually when streaming began), or when we happened to wake.
  if (answer?.conversationId !== record.conversationId || task?.kind !== "pi.generation" ||
      task.conversationId !== record.conversationId || task.state.status !== "terminal" ||
      task.endedAt === undefined || !Number.isSafeInteger(task.endedAt)) return { state: "done" };
  return { state: "done", at: task.endedAt };
}

async function report(tx: Tx, row: ScheduleItem, now: number, state: "done" | "failed" | "cancelled",
    detail: string, reason?: string) {
  const who = `Scheduled occurrence ${row.id} · thread #${row.targetThreadId}` +
    (row.submissionId ? ` · submission #${row.submissionId}` : "");
  const text = (who + "\n" + detail).slice(0, 8192).toWellFormed();
  const entry = await tx.appendEntry(id(row.threadId), { kind: scheduleReportKind, data: { text, timestampMs: now } });
  row.reportEntryId = String(entry.id);
  row.state = state;
  if (reason) row.reason = reason;
  else delete row.reason;
}

export class Scheduling implements SchedulesHost {
  readonly extension;
  readonly #task;

  constructor(private readonly harness: () => Harness, private readonly lateMs = defaultLateMs) {
    this.#task = defineTask<{ threadId: string; occurrence: string }, typeof phase, null>({
      name: "hush.scheduled-occurrence", version: 1, initial: () => phase,
      phases: { wake: async (task, runtime, ctx) => {
        const h = this.harness(), owner = id(task.input.threadId);
        const current = await h.snapshot(Schedule, owner, task.input.occurrence, ctx);
        // An occurrence's target is immutable. Obtain the handle outside the
        // commit; check liveness/visibility/archive again on the session line.
        const targetId = current?.row ? id(current.row.targetThreadId) : undefined;
        const target = targetId === undefined || current?.row?.submissionId || current?.row && terminal(current.row)
          ? undefined : await h.conversation(targetId, ctx);
        await runtime.commit(async tx => {
          const record = await tx.doc(Schedule, owner, task.input.occurrence, null);
          const row = record.row;
          if (!row || terminal(row)) return ended;
          delete record.wake;
          const now = runtime.now();
          if (row.submissionId) {
            const input = await tx.submission(Number(row.submissionId) as SubmissionId);
            if (!input || input.conversationId !== targetId || input.type !== "input" || input.status === "unanswered") {
              await report(tx, row, now, "failed", "Target input did not receive an answer.", "target_unanswered");
              return ended;
            }
            if (input.status !== "done") return { status: "sleeping", checkpoint: phase, signals: [SubmissionSettled.key(settled)] };
            const answer = await tx.entry(input.answer);
            if (!answer || answer.conversationId !== targetId) {
              await report(tx, row, now, "failed", "Target answer is unavailable.", "answer_missing");
              return ended;
            }
            const text = answer.model?.flatMap(message => message.role === "assistant"
              ? message.content.flatMap(block => block.type === "text" ? [block.text] : []) : []).join("\n");
            await report(tx, row, now, "done", text || "Target input settled without text.");
            return ended;
          }
          if (row.state === "needs_attention") return sleep(record);
          const dep = await prerequisite(tx, row.prerequisite);
          if (dep.state === "failed") {
            await report(tx, row, now, "failed", "Required submission did not succeed.", "prerequisite_failed");
            return ended;
          }
          if (row.dueMs > now) return sleep(record, row.dueMs);
          // A short timer checks a late prerequisite without allocating a
          // signal key per poll. Changes are already committed on the same line.
          if (dep.state === "waiting") return { status: "sleeping", checkpoint: phase, until: now + 500 };
          const park = (reason: string) => {
            row.state = "needs_attention";
            row.reason = reason;
            return sleep(record);
          };
          const at = "at" in dep ? dep.at : undefined;
          if (at !== undefined) {
            row.eligibleMs = Math.max(row.dueMs, at);
            if (row.eligibleMs > now) return sleep(record, row.eligibleMs);
          }
          if (!target || !targetId || target.id !== targetId) return park("target_missing");
          const thread = await tx.conversation(targetId);
          if (!thread || thread.owner) return park("target_missing");
          if ((await tx.doc(ThreadPresentationDoc, targetId)).archived) return park("target_archived");
          if (!record.force && !row.allowLate) {
            if (row.eligibleMs === undefined) return park("settlement_time_unknown");
            if (now - row.eligibleMs > this.lateMs) return park("late");
          }
          if (!record.requestId || !record.text) throw new Error("Incomplete occurrence");
          const submission = await target.submit(tx, { type: "input", whenBusy: "followUp",
            requestId: record.requestId, content: record.text, notify: { taskId: runtime.taskId, key: settled } });
          row.submissionId = String(submission);
          row.state = "admitted";
          delete row.reason;
          delete record.force;
          return { status: "sleeping", checkpoint: phase, signals: [SubmissionSettled.key(settled)] };
        }, ctx);
      } },
      abort: async (task, runtime, ctx) => {
        await runtime.commit(async tx => {
          const record = await tx.doc(Schedule, id(task.input.threadId), task.input.occurrence, null);
          if (record.row && !terminal(record.row) && !record.row.submissionId)
            await report(tx, record.row, runtime.now(), "cancelled", "Occurrence cancelled.", "cancelled");
          return { status: "terminal", outcome: { status: "aborted" } };
        }, ctx);
      },
    });
    this.extension = defineExtension({ name: "hush.scheduling", tasks: [this.#task] });
  }

  async call(request: ScheduleRequest, signal: AbortSignal) {
    const h = this.harness();
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const source = await h.conversation(id(request.threadId), ctx);
    if (!source) throw new WireError("not_found");
    const result = await source.commit(async tx => {
      const owner = source.id;
      const thread = await tx.conversation(owner);
      if (!thread || thread.owner) throw new WireError("not_found");
      const index = await tx.doc(ScheduleIndex, owner);
      if (request.op === "list") {
        const items = [];
        for (const key of index.ids) {
          const row = (await tx.doc(Schedule, owner, key, null)).row;
          if (row) items.push(copy(row));
        }
        return { items };
      }
      if (request.op === "add") {
        const target = await tx.conversation(id(request.targetThreadId));
        if (!target || target.owner) throw new WireError("not_found");
        if (index.ids.length >= scheduleLimit) {
          // Keep a bounded recent list, but do not discard individual durable receipts.
          let retire = -1;
          for (let i = index.ids.length - 1; i >= 0; i--) {
            const row = (await tx.doc(Schedule, owner, index.ids[i]!, null)).row;
            if (row && terminal(row)) { retire = i; break; }
          }
          if (retire < 0) throw new WireError("limit");
          index.ids.splice(retire, 1);
        }
        const occurrence = randomUUID();
        const row: ScheduleItem = { id: occurrence, threadId: request.threadId, targetThreadId: request.targetThreadId,
          dueMs: request.dueMs, zone: request.zone, allowLate: request.allowLate, state: "pending",
          ...(request.prerequisite && { prerequisite: request.prerequisite }) };
        const doc = await tx.doc(Schedule, owner, occurrence, null);
        doc.row = row;
        doc.text = request.text;
        doc.requestId = "hush-schedule-" + randomUUID();
        index.ids.unshift(occurrence);
        doc.task = await tx.createTask(this.#task, { threadId: request.threadId, occurrence },
          { ownership: { kind: "conversation" }, background: true });
        return row;
      }
      const record = await tx.doc(Schedule, owner, request.id, null);
      const row = record.row;
      if (!row || record.task === undefined) throw new WireError("not_found");
      // Don't withdraw or stop a target input even if it's still queued.
      if (terminal(row) || row.submissionId) return copy(row);
      if (request.op === "run" && row.state !== "needs_attention") throw new WireError("invalid_request");
      const task = await tx.task(record.task as TaskId);
      if (!task || task.state.status === "terminal") {
        await report(tx, row, Date.now(), "failed", "Occurrence task cannot resume.", "task_unavailable");
        return copy(row);
      }
      if (record.wake) await tx.signal(record.task as TaskId, Changed, record.wake, true);
      if (request.op === "cancel") {
        await report(tx, row, Date.now(), "cancelled", "Occurrence cancelled.", "cancelled");
      } else {
        if (request.op === "reschedule") {
          row.dueMs = request.dueMs;
          row.zone = request.zone;
          delete row.eligibleMs;
          delete record.force;
        } else record.force = true;
        row.state = "pending";
        delete row.reason;
      }
      return copy(row);
    }, ctx);
    if (request.op !== "list") h.resume();
    return result;
  }
}
