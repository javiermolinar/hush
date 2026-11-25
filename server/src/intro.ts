import { isAbsolute } from "node:path";
import { introGet as getShape, introRename as renameShape, introRecover as recoverShape, introSuccess, introSuccessV2 } from "./validators.js";
import { durableID, WireError } from "./wire.js";

export type IntroVersion = 1 | 2;
export type IntroQuery = { threadId?: string; cwd?: string; after?: number };
export type IntroRunIssue = { taskId: string; reason: string; detail?: string; retry: boolean; switchModel: boolean; endedAtMs?: number };
export type IntroThread = { id: string; title?: string; issue?: IntroRunIssue; dismissedFailureId?: string };
export type IntroAttention = { title: string; status: "parked" | "stopped"; reason?: string; untilMs?: number;
  threadId?: string; issue?: IntroRunIssue };
export type IntroAction = { kind: "rename"; threadId: string; title: string } |
  { kind: "retry" | "dismiss"; threadId: string; taskId: string };
export type IntroReport = {
  revision: number; startedAtMs: number; cwd: string;
  model?: { provider: string; modelId: string }; thinking?: string;
  sessions: { open: number; running: number; quiet: number; partial: boolean };
  attention: IntroAttention[]; moreAttention: number; thread?: IntroThread;
  skills: string[]; moreSkills?: number;
  context?: { window: number; renderedBytes: number; reportedTokens?: number };
};
export interface IntroHost {
  get(query: IntroQuery, signal: AbortSignal, version: IntroVersion): Promise<IntroReport>;
  act?(action: IntroAction, signal: AbortSignal): Promise<object>;
}

const envelopes = { 1: introSuccess, 2: introSuccessV2 };

export function parseIntroQuery(params: Record<string, unknown>): IntroQuery {
  if (!getShape(params) || (params.threadId !== undefined && !durableID(params.threadId)) ||
      (typeof params.cwd === "string" && (!isAbsolute(params.cwd) || params.cwd.includes("\0")))) {
    throw new WireError("invalid_request");
  }
  return params as IntroQuery;
}

export function parseIntroAction(kind: string, params: Record<string, unknown>): IntroAction {
  const { threadId, taskId, title } = params;
  if (kind !== "rename" && kind !== "retry" && kind !== "dismiss") throw new WireError("unknown_method");
  if (durableID(threadId)) {
    if (kind === "rename" && renameShape(params) && typeof title === "string") return { kind, threadId, title };
    if (kind !== "rename" && recoverShape(params) && durableID(taskId)) return { kind, threadId, taskId };
  }
  throw new WireError("invalid_request");
}

export function checkIntroReply(version: IntroVersion, frame: object) {
  if (!envelopes[version](frame)) throw new WireError("internal");
  return frame;
}
