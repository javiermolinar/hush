import { durableID, object, WireError } from "./wire.js";

export const schedulesVersion = 1;
export const scheduleReportKind = "hush.schedule-report";
export const scheduleLimit = 128;
export const defaultLateMs = 15 * 60_000;
export type ScheduleTime = { dueMs: number; zone: string };
export type SchedulePrerequisite = { threadId: string; submissionId: string };
export type ScheduleState = "pending" | "needs_attention" | "admitted" | "done" | "failed" | "cancelled";
export type ScheduleItem = ScheduleTime & {
  id: string; threadId: string; targetThreadId: string; prerequisite?: SchedulePrerequisite;
  allowLate: boolean; state: ScheduleState; reason?: string; eligibleMs?: number; submissionId?: string; reportEntryId?: string;
};
export type ScheduleRequest = { threadId: string } & ({ op: "list" } |
  ({ op: "add"; targetThreadId: string; text: string; prerequisite?: SchedulePrerequisite; allowLate: boolean } & ScheduleTime) |
  { op: "run" | "cancel"; id: string } | ({ op: "reschedule"; id: string } & ScheduleTime));
export interface SchedulesHost {
  call(request: ScheduleRequest, signal: AbortSignal): Promise<ScheduleItem | { items: ScheduleItem[] }>;
}

const occurrenceID = (v: unknown): v is string => typeof v === "string" &&
  /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(v) && v.length === 36;
const invalid = (): never => { throw new WireError("invalid_request"); };
const fields = (p: Record<string, unknown>, keys: string[]) => {
  if (Object.keys(p).some(k => !keys.includes(k))) invalid();
};
function time(p: Record<string, unknown>): ScheduleTime {
  const { dueMs, zone } = p;
  if (typeof dueMs !== "number" || !Number.isSafeInteger(dueMs) || dueMs < 1 || dueMs > 253402300799999 ||
      typeof zone !== "string" || (zone !== "UTC" && (zone.length !== 6 || !/^[+-](?:[01]\d|2[0-3]):[0-5]\d$/.test(zone)))) {
    throw new WireError("invalid_request");
  }
  return { dueMs, zone };
}
export function parseSchedule(op: string, p: Record<string, unknown>): ScheduleRequest {
  const { threadId, targetThreadId, text, id, allowLate } = p;
  if (!durableID(threadId)) throw new WireError("invalid_request");
  switch (op) {
    case "list":
      fields(p, ["threadId"]);
      return { op, threadId };
    case "add": {
      fields(p, ["threadId", "targetThreadId", "dueMs", "zone", "text", "prerequisite", "allowLate"]);
      if (!durableID(targetThreadId) || typeof text !== "string" || !text.trim() || !text.isWellFormed() ||
          Buffer.byteLength(text) > 32 * 1024 || typeof allowLate !== "boolean" && allowLate !== undefined) {
        throw new WireError("invalid_request");
      }
      let prerequisite: SchedulePrerequisite | undefined;
      if (p.prerequisite !== undefined) {
        const v = p.prerequisite;
        if (!object(v) || !durableID(v.threadId) || !durableID(v.submissionId)) throw new WireError("invalid_request");
        fields(v, ["threadId", "submissionId"]);
        prerequisite = { threadId: v.threadId, submissionId: v.submissionId };
      }
      return { op, threadId, targetThreadId, text, ...time(p), allowLate: allowLate === true,
        ...(prerequisite && { prerequisite }) };
    }
    case "run": case "cancel":
      fields(p, ["threadId", "id"]);
      if (!occurrenceID(id)) throw new WireError("invalid_request");
      return { op, threadId, id };
    case "reschedule":
      fields(p, ["threadId", "id", "dueMs", "zone"]);
      if (!occurrenceID(id)) throw new WireError("invalid_request");
      return { op, threadId, id, ...time(p) };
    default: throw new WireError("unknown_method");
  }
}
