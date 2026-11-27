import type { projectUsage } from "./durable.js";
import { durableID, WireError } from "./wire.js";

export const subagentToolName = "subagent";
export const subagentsVersion = 1;
export const subagentOverviewVersion = 2;

export type SubagentQuery = { threadId: string; submissionId: string; after?: string };
export type SubagentStatus = "ok" | "failed" | "cancelled" | "timed_out";
export type SubagentEvent =
  | { type: "started"; workerId: string; cwd: string; read_only: boolean }
  | { type: "text"; workerId: string; text: string; truncated?: true }
  | { type: "finished"; workerId: string; status: SubagentStatus; detail?: string };
export type SubagentLog = { cursor: string; events: SubagentEvent[]; settled: boolean };
export type SubagentOverviewQuery = { threadId: string; after?: string };
export type SubagentUsage = ReturnType<typeof projectUsage>;
export type SubagentRow = {
  id: string; title: string; cwd: string; readOnly: boolean;
  status: SubagentStatus | "starting" | "waiting" | "reading" | "working" | "retrying" | "responding" | "stopping";
  usage: SubagentUsage; text: string; truncated: boolean; detail?: string;
};
export type SubagentOverview = { threadId: string; cursor: string; workers: SubagentRow[]; usage: SubagentUsage };
export interface SubagentsHost {
  wait(query: SubagentQuery, signal: AbortSignal): Promise<SubagentLog>;
  overview(query: SubagentOverviewQuery, signal: AbortSignal): Promise<SubagentOverview>;
}
const validAfter = (after: unknown): after is string | undefined =>
  after === undefined || typeof after === "string" && after.length === 64 && /^[a-f0-9]+$/.test(after);

export function parseSubagentOverview({ threadId, after, ...extra }: Record<string, unknown>): SubagentOverviewQuery {
  if (Object.keys(extra).length || !durableID(threadId) || !validAfter(after)) throw new WireError("invalid_request");
  return { threadId, after };
}

export function parseSubagentQuery(p: Record<string, unknown>): SubagentQuery {
  const { threadId, submissionId, after } = p;
  if (Object.keys(p).some(k => k !== "threadId" && k !== "submissionId" && k !== "after") ||
      !durableID(threadId) || !durableID(submissionId) ||
      !validAfter(after)) {
    throw new WireError("invalid_request");
  }
  return { threadId, submissionId, after };
}
