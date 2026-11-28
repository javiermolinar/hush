import { isAbsolute } from "node:path";
import { parseCatalogCreation, type CatalogCreation } from "./model-catalog.js";
import { durableID, token, WireError } from "./wire.js";

export const planVersion = 2;
// Transcript entry kinds. Run inputs and results are current; the rest are
// display-only entries written by removed plan features.
export const planChatKind = "hush.plan.chat";
export const planResultKind = "hush.plan.result";
export const planRunKind = "hush.plan.run";
export const planTaskKind = "hush.plan.task";
export const planReviewKind = "hush.plan.review";
export const planPreviewKind = "hush.plan.preview";
export const planPreviewSavedKind = "hush.plan.preview-saved";

export const planListPageSize = 48;
export const planHistoryPageSize = 12;
export const planStepLimit = 24;
// A run can delegate at most this many steps: the per-turn subagent limit.
export const planDelegateLimit = 5;
export const planThinkingLevels = ["off", "minimal", "low", "medium", "high", "xhigh", "max"] as const;
export type PlanThinking = typeof planThinkingLevels[number];

export type PlanDelegate = { read_only?: boolean; worktree?: string; pwd?: string; thinking?: PlanThinking };
export type PlanStepDefinition = { id: string; title: string; prompt: string; requires?: string[]; delegate?: PlanDelegate };
export type PlanDefinition = { id: string; revision: number; title: string; steps: PlanStepDefinition[] };
export type PlanStepStatus = "pending" | "done" | "blocked";
export type PlanStep = PlanStepDefinition & { status: PlanStepStatus; reason?: string };
export type PlanActiveRun = { id: string; threadId: string; stepIds: string[] };
export type Plan = { id: string; revision: number; title: string; workspace: string; steps: PlanStep[]; runs: PlanActiveRun[];
  closed?: true; complete?: true };
export type PlanSnapshot = { revision: string; plan: Plan };
export type PlanSummary = { id: string; title: string; revision: number; workspace: string; total: number; done: number; blocked: number;
  running: boolean; closed: boolean };
export type PlanList = { revision: string; plans: PlanSummary[]; more: boolean };
export type PlanOutcome = { stepId: string; status: "done" | "blocked"; detail: string };
export type PlanRunState = "running" | "finished" | "failed";
export type PlanRunRecord = { id: string; revision: number; threadId: string; stepIds: string[]; state: PlanRunState; outcomes: PlanOutcome[];
  submissionId?: string; entryId?: string; failure?: string; worktree?: true };
export type PlanHistory = { runs: PlanRunRecord[]; next: string | null };
export type PlanActivityStep = { id: string; title: string; status: PlanStepStatus; reason?: string };
export type PlanActivityRun = PlanRunRecord & { planId: string; title: string; steps: PlanActivityStep[] };
// A thread's oldest active run, or else its most recent finished one.
export type PlanActivity = { revision: string; run: PlanActivityRun | null };

export type PlanListQuery = { cwd: string; page: number; after?: string };
export type PlanGetQuery = { cwd: string; planId: string; after?: string };
export type PlanHistoryQuery = { cwd: string; planId: string; before?: string };
export type PlanClosure = { cwd: string; planId: string; closed: boolean };
export type PlanActivityQuery = { threadId: string; after?: string };
export type PlanSelection = { planId: string; revision: number; stepIds: string[]; requestId: string };
export type PlanRunQuery = PlanSelection & { threadId: string };
export type PlanWorktreeQuery = PlanSelection & ({ threadId: string } |
  ({ cwd: string } & (Pick<CatalogCreation, "model" | "thinkingLevel"> | { model?: undefined; thinkingLevel?: undefined })));
export type PlanWorktreeRun = { runId: string; threadId: string; title: string; checkout: { cwd: string; root: string; branch: string } };

export interface PlanHost {
  list(query: PlanListQuery, signal: AbortSignal): Promise<PlanList>;
  get(query: PlanGetQuery, signal: AbortSignal): Promise<PlanSnapshot>;
  history(query: PlanHistoryQuery, signal: AbortSignal): Promise<PlanHistory>;
  close(query: PlanClosure, signal: AbortSignal): Promise<Record<string, never>>;
  activity(query: PlanActivityQuery, signal: AbortSignal): Promise<PlanActivity>;
  run(query: PlanRunQuery, signal: AbortSignal): Promise<{ runId: string }>;
  runInWorktree(query: PlanWorktreeQuery, signal: AbortSignal): Promise<PlanWorktreeRun>;
}

export const planStepID = (v: unknown): v is string => typeof v === "string" && v.length <= 64 && token(v);

const watchToken = (s: unknown): s is string | undefined => s === undefined || typeof s === "string" && /^[a-f0-9]{64}$/.test(s);
const fields = (p: Record<string, unknown>, ...keys: string[]) => Object.keys(p).every(k => keys.includes(k));
const planPage = (v: unknown): v is number => typeof v === "number" && Number.isSafeInteger(v) && v >= 0 && v <= 1_000_000;
const invalid = () => new WireError("invalid_request");

export function validPlanCheckoutPath(cwd: unknown): cwd is string {
  return typeof cwd === "string" && isAbsolute(cwd) && Buffer.byteLength(cwd) <= 4096 && cwd.isWellFormed() && !/[\x00-\x1f\x7f-\x9f]/.test(cwd);
}

export function parsePlanList(p: Record<string, unknown>): PlanListQuery {
  if (!fields(p, "cwd", "page", "after") || !validPlanCheckoutPath(p.cwd) || !planPage(p.page) || !watchToken(p.after)) throw invalid();
  return { cwd: p.cwd, page: p.page, after: p.after };
}

export function parsePlanGet(p: Record<string, unknown>): PlanGetQuery {
  if (!fields(p, "cwd", "planId", "after") || !validPlanCheckoutPath(p.cwd) || !token(p.planId) || !watchToken(p.after)) throw invalid();
  return { cwd: p.cwd, planId: p.planId, after: p.after };
}

export function parsePlanHistory(p: Record<string, unknown>): PlanHistoryQuery {
  if (!fields(p, "cwd", "planId", "before") || !validPlanCheckoutPath(p.cwd) || !token(p.planId) ||
      p.before !== undefined && !durableID(p.before)) throw invalid();
  return { cwd: p.cwd, planId: p.planId, before: p.before };
}

export function parsePlanClosure(p: Record<string, unknown>): PlanClosure {
  if (!fields(p, "cwd", "planId", "closed") || !validPlanCheckoutPath(p.cwd) || !token(p.planId) || typeof p.closed !== "boolean") throw invalid();
  return { cwd: p.cwd, planId: p.planId, closed: p.closed };
}

export function parsePlanActivity(p: Record<string, unknown>): PlanActivityQuery {
  if (!fields(p, "threadId", "after") || !durableID(p.threadId) || !watchToken(p.after)) throw invalid();
  return { threadId: p.threadId, after: p.after };
}

function parseSelection(p: Record<string, unknown>): PlanSelection {
  if (Object.keys(p).length !== 4 || !token(p.planId) || !token(p.requestId) ||
      typeof p.revision !== "number" || !Number.isSafeInteger(p.revision) || p.revision < 1 ||
      !Array.isArray(p.stepIds) || p.stepIds.length < 1 || p.stepIds.length > planStepLimit ||
      !p.stepIds.every(planStepID) || new Set(p.stepIds).size !== p.stepIds.length) throw invalid();
  return { planId: p.planId, revision: p.revision, stepIds: p.stepIds, requestId: p.requestId };
}

export function parsePlanRun({ threadId, ...p }: Record<string, unknown>): PlanRunQuery {
  if (!durableID(threadId)) throw invalid();
  return { threadId, ...parseSelection(p) };
}

export function parsePlanWorktree({ threadId, cwd, model, thinkingLevel, ...p }: Record<string, unknown>): PlanWorktreeQuery {
  const staged = model !== undefined || thinkingLevel !== undefined;
  if (durableID(threadId) && cwd === undefined && !staged) return { threadId, ...parseSelection(p) };
  if (threadId !== undefined || !validPlanCheckoutPath(cwd)) throw invalid();
  const selection = parseSelection(p);
  if (!staged) return { cwd, ...selection };
  const creation = parseCatalogCreation({ model, thinkingLevel });
  return { cwd, ...selection, model: creation.model, thinkingLevel: creation.thinkingLevel };
}
