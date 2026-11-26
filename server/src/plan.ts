import { isAbsolute } from "node:path";
import { durableID, token, WireError } from "./wire.js";

export const planVersion = 1;
export const planWorkspaceVersion = 1;
export const planDraftVersion = 1;
export const planChatVersion = 1;
export const planPreviewKind = "hush.plan.preview";
export const planPreviewSavedKind = "hush.plan.preview-saved";
export const planWorkspacePageSize = 48;
export const planHistoryPageSize = 12;
export const planStepLimit = 24;
export const planRunKind = "hush.plan.run";
export const planTaskKind = "hush.plan.task";
export const planChatKind = "hush.plan.chat";
export const planResultKind = "hush.plan.result";
export const planThinkingLevels = ["off", "minimal", "low", "medium", "high", "xhigh", "max"] as const;
export type PlanThinking = typeof planThinkingLevels[number];

export const planFilePathPattern = "^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*){0,15}$";
const filePath = new RegExp(planFilePathPattern);
export const planFilePath = (path: string) => path.length <= 160 && !path.endsWith("\n") && filePath.test(path);
export type PlanFileEdit = { path: string; contains: string };
export const planScopeFileLimit = 8;
export const planScopeWriteLimit = 4;
export const planScriptLimit = 4096;
export type PlanFileScope = { read?: string[]; write: string[]; acceptance: { label: string; script: string } };

export type PlanStep = {
  id: string; title: string; description: string; checks: string[];
  requires?: string[]; after?: string; blocker?: string; thinking?: PlanThinking; edit?: PlanFileEdit; scope?: PlanFileScope;
};
export type PlanDefinition = { id: string; revision: number; title: string; steps: PlanStep[] };
export type PlanProgress = {
  stepId: string; state: "running" | "verified" | "failed"; effectiveThinking: PlanThinking;
  workerThreadId: string; entryId?: string; evidence?: string[];
};
export type PlanTaskReport = { stepId: string; state: "done" | "blocked" | "incomplete"; detail: string };
export type PlanRunState = "running" | "verified" | "failed" | "closed" | "done";
export type PlanRun = { id: string; stepIds: string[]; state: PlanRunState; ownerThreadId?: string; failure?: string; publicationUncertain?: true;
  chat?: true; submissionId?: string };
export type PlanStatus = "pending" | "running" | "attention" | "complete";
export type PlanSummary = { id: string; title: string; revision: number; verified: number; reportedDone?: number; total: number; status: PlanStatus; closed: boolean };
export type PlanWorkspace = { revision: string; plans: PlanSummary[]; more: boolean };
export type PlanRunRecord = { id: string; revision: number; threadId: string; stepIds: string[]; state: PlanRunState; failure?: string; publicationUncertain?: true;
  workers: { stepId: string; threadId: string; state: PlanProgress["state"] }[];
  turns?: { stepId: string; entryId: string; state: PlanProgress["state"] }[];
  chat?: true; entryId?: string; reports?: PlanTaskReport[] };
export type PlanHistory = { runs: PlanRunRecord[]; next: string | null };
export type PlanSnapshot = {
  revision: string; plan: PlanDefinition | null; progress: PlanProgress[]; run: PlanRun | null;
  runAvailability: "disabled" | "fixture" | "scoped" | "pinned" | "unbound";
  closed?: true; complete?: true; settling?: true;
  chat?: true; reports?: PlanTaskReport[];
};
export type PlanAttachment = { threadId: string; planId: string };
export type PlanClosure = PlanAttachment & { closed: boolean };
export type PlanWorkspaceQuery = { threadId: string; page: number; after?: string };
export type PlanHistoryQuery = PlanAttachment & { before?: string };
export type PlanSelection = { threadId: string; planId: string; revision: number; stepIds: string[]; requestId: string };

export type PlanDraftTarget = { threadId: string; previewId: string };
export type PlanDraftReceipt = { planId: string; revision: number };
export interface PlanDraftHost {
  save(target: PlanDraftTarget, signal: AbortSignal): Promise<PlanDraftReceipt>;
}

export function parsePlanDraftTarget(p: Record<string, unknown>): PlanDraftTarget {
  if (Object.keys(p).length !== 2 || !durableID(p.threadId) || !durableID(p.previewId)) throw new WireError("invalid_request");
  return { threadId: p.threadId, previewId: p.previewId };
}

export interface PlanHost {
  wait(threadId: string, after: string | undefined, signal: AbortSignal, planId?: string, chat?: boolean): Promise<PlanSnapshot>;
  workspace(query: PlanWorkspaceQuery, signal: AbortSignal, chat?: boolean): Promise<PlanWorkspace>;
  history(query: PlanHistoryQuery, signal: AbortSignal, chat?: boolean): Promise<PlanHistory>;
  setClosed(closure: PlanClosure, signal: AbortSignal): Promise<Record<string, never>>;
  attach(attachment: PlanAttachment, signal: AbortSignal, replace?: boolean): Promise<Record<string, never>>;
  run(selection: PlanSelection, signal: AbortSignal): Promise<{ runId: string }>;
}

export type PlanWorkspaceAt = Omit<PlanWorkspaceQuery, "threadId"> & { cwd: string };
export type PlanInspectAt = { cwd: string; planId: string; after?: string };
export interface PlanChatHost {
  runInChat(selection: PlanSelection, signal: AbortSignal): Promise<{ runId: string }>;
  workspaceAt(query: PlanWorkspaceAt, signal: AbortSignal): Promise<PlanWorkspace>;
  inspectAt(query: PlanInspectAt, signal: AbortSignal): Promise<PlanSnapshot>;
}

export const planStepID = (v: unknown): v is string => typeof v === "string" && v.length <= 64 && token(v);

const watchToken = (s: unknown): s is string | undefined => s === undefined || typeof s === "string" && /^[a-f0-9]{64}$/.test(s) && s.length === 64;
const fields = (p: Record<string, unknown>, ...keys: string[]) => Object.keys(p).every(k => keys.includes(k));
const planPage = (v: unknown): v is number => typeof v === "number" && Number.isSafeInteger(v) && v >= 0 && v <= 1_000_000;

export function parsePlanWait(p: Record<string, unknown>, named = false) {
  if (!fields(p, ...(named ? ["threadId", "after", "planId"] : ["threadId", "after"])) || !durableID(p.threadId) ||
      !watchToken(p.after) || named && !token(p.planId)) throw new WireError("invalid_request");
  return { threadId: p.threadId, after: p.after, planId: named ? p.planId as string : undefined };
}

export function parsePlanWorkspace(p: Record<string, unknown>): PlanWorkspaceQuery {
  if (!fields(p, "threadId", "page", "after") || !durableID(p.threadId) || !watchToken(p.after) ||
      !planPage(p.page)) throw new WireError("invalid_request");
  return { threadId: p.threadId, page: p.page, after: p.after };
}

export function parsePlanClosure(p: Record<string, unknown>): PlanClosure {
  if (!fields(p, "threadId", "planId", "closed") || !durableID(p.threadId) || !token(p.planId) || typeof p.closed !== "boolean") {
    throw new WireError("invalid_request");
  }
  return { threadId: p.threadId, planId: p.planId, closed: p.closed };
}

export function parsePlanHistory(p: Record<string, unknown>): PlanHistoryQuery {
  if (!fields(p, "threadId", "planId", "before") || !durableID(p.threadId) || !token(p.planId) ||
      p.before !== undefined && !durableID(p.before)) throw new WireError("invalid_request");
  return { threadId: p.threadId, planId: p.planId, before: p.before };
}

export function parsePlanAttach(p: Record<string, unknown>): PlanAttachment {
  if (Object.keys(p).length !== 2 || !durableID(p.threadId) || !token(p.planId)) throw new WireError("invalid_request");
  return { threadId: p.threadId, planId: p.planId };
}

export function parsePlanAt(p: Record<string, unknown>, inspect: true): PlanInspectAt;
export function parsePlanAt(p: Record<string, unknown>, inspect: false): PlanWorkspaceAt;
export function parsePlanAt({ cwd, ...p }: Record<string, unknown>, inspect: boolean): PlanInspectAt | PlanWorkspaceAt {
  if (typeof cwd !== "string" || !isAbsolute(cwd) || Buffer.byteLength(cwd) > 4096 || !cwd.isWellFormed() || /[\x00-\x1f\x7f-\x9f]/.test(cwd)) {
    throw new WireError("invalid_request");
  }
  const { after, planId, page } = p;
  if (!watchToken(after)) throw new WireError("invalid_request");
  if (inspect) {
    if (!fields(p, "planId", "after") || !token(planId)) throw new WireError("invalid_request");
    return { cwd, planId, after };
  }
  if (!fields(p, "page", "after") || !planPage(page)) throw new WireError("invalid_request");
  return { cwd, page, after };
}

export function parsePlanRun(p: Record<string, unknown>): PlanSelection {
  if (Object.keys(p).length !== 5 || !durableID(p.threadId) || !token(p.planId) || !token(p.requestId) ||
      typeof p.revision !== "number" || !Number.isSafeInteger(p.revision) || p.revision < 1 ||
      !Array.isArray(p.stepIds) || p.stepIds.length < 1 || p.stepIds.length > planStepLimit ||
      !p.stepIds.every(planStepID) || new Set(p.stepIds).size !== p.stepIds.length) throw new WireError("invalid_request");
  return { threadId: p.threadId, planId: p.planId, revision: p.revision, stepIds: p.stepIds, requestId: p.requestId };
}
