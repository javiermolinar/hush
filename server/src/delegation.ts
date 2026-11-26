import { durableID, object, token, WireError, type Thinking } from "./wire.js";

export const delegationVersion = 1, reviewEvidenceVersion = 1, workerActivityVersion = 1;
export const workerNoticeKind = "hush.worker.notice";
export const reviewParentKind = "hush.review.request";
export const spawnReviewsName = "hush_spawn_reviews", joinReviewsName = "hush_join_reviews";
export const workerTimeoutLimit = 180_000;
export const reviewTextLimit = 4_096;
export const reviewBriefLimit = 2_048;
export const reviewPathLimit = 16;
export type Selection = { model: { provider: string; modelId: string }; thinking: Thinking };
export type ReviewScope = { tracked: string[]; untracked: string[] };
export type ReviewCheck = { id: string; description: string };
export type SuppliedEvidence = { id: string; label: string; text: string };
export type ReviewContract = { checks?: ReviewCheck[]; evidence?: SuppliedEvidence[] };
export type ReviewLaunch = { threadId: string; requestId: string; scope: ReviewScope; assignments: { name: string; brief: string }[];
  mode: "sequential" | "parallel"; selection?: Selection; prompt?: string; workerTimeoutMs: number; assessment?: ReviewContract };
export type ReviewKey = Pick<ReviewLaunch, "threadId" | "requestId">;
export type WorkerKey = { threadId: string; handle: string };
export type WorkerState = "reserved" | "running" | "stopping" | "succeeded" | "failed" | "cancelled" | "timed_out" | "unresolved";
export type ReviewReceipt = { threadId: string; requestId: string; snapshotId: string; base: string; mode: ReviewLaunch["mode"];
  selection: Selection; workers: { handle: string; name: string }[]; submissionId?: string };
export type WorkerOutcome = { state: Extract<WorkerState, "succeeded" | "failed" | "cancelled" | "timed_out">; report?: string; detail?: string };
export type WorkerView = ReviewReceipt["workers"][number] & { state: WorkerState; report?: string; detail?: string; noticeId?: string };
export type ReviewView = { receipt: ReviewReceipt; workers: WorkerView[]; target: "current" | "stale" | "unavailable" };
export type WorkerActivityQuery = { threadId: string; after?: string };
export type WorkerActivityRow = ReviewReceipt["workers"][number] & { state: WorkerState; activity: string };
export type WorkerActivity = { threadId: string; cursor: string; workers: WorkerActivityRow[] };
export type WorkerInspectionQuery = WorkerKey & { transcript: boolean };
export type WorkerTranscript = { entries: { id: string; kind: "reviewer" | "activity"; text: string }[]; truncated: boolean };
export type WorkerInspection = { receipt: ReviewReceipt; worker: WorkerView; brief: string; scope: ReviewScope;
  target: ReviewView["target"]; assessment?: ReviewContract; transcript?: WorkerTranscript };
export interface DelegationHost {
  start(q: ReviewLaunch, signal: AbortSignal): Promise<ReviewReceipt>;
  get(q: ReviewKey, signal: AbortSignal): Promise<ReviewView>;
  stop(q: WorkerKey, signal: AbortSignal): Promise<{ worker: WorkerView }>;
  waitActivity?(q: WorkerActivityQuery, signal: AbortSignal): Promise<WorkerActivity>;
  inspect?(q: WorkerInspectionQuery, signal: AbortSignal): Promise<WorkerInspection>;
  evidence?: true;
}

export const validReviewText = (v: unknown, max: number, multiline = false): v is string => typeof v === "string" && v.isWellFormed() &&
  Buffer.byteLength(v) <= max && !!v.trim() && !/[\p{Zl}\p{Zp}\u202a-\u202e\u2066-\u2069]/u.test(v) &&
  !(multiline ? /\p{Cc}/u.test(v.replaceAll("\n", "").replaceAll("\t", "")) : /\p{Cc}/u.test(v));
const level = (v: unknown): v is Thinking => typeof v === "string" && ["off", "minimal", "low", "medium", "high", "xhigh", "max"].includes(v);
const keys = (p: Record<string, unknown>, names: string[]) => Object.keys(p).every(k => names.includes(k));
const invalid = () => { throw new WireError("invalid_request"); };
export const workerHandle = (v: unknown): v is string => typeof v === "string" && v.length === 32 && /^[a-f0-9]+$/.test(v);
const valid = validReviewText;
export const workerReport = (v: unknown): v is string => valid(v, reviewTextLimit, true);
export const reviewEvidenceID = (v: unknown): v is string => typeof v === "string" && /^[a-z][a-z0-9_-]{0,31}$/.test(v);

function parseContract(v: unknown): ReviewContract {
  if (!object(v) || !keys(v, ["checks", "evidence"])) return invalid();
  const checks: ReviewCheck[] = [], evidence: SuppliedEvidence[] = [];
  if (v.checks !== undefined) {
    if (!Array.isArray(v.checks) || v.checks.length > 6) return invalid();
    for (const c of v.checks) {
      if (!object(c) || !keys(c, ["id", "description"]) || !reviewEvidenceID(c.id) || !valid(c.description, 160) ||
          checks.some(prior => prior.id === c.id)) return invalid();
      checks.push({ id: c.id, description: c.description });
    }
  }
  if (v.evidence !== undefined) {
    if (!Array.isArray(v.evidence) || v.evidence.length > 4) return invalid();
    for (const e of v.evidence) {
      if (!object(e) || !keys(e, ["id", "label", "text"]) || !reviewEvidenceID(e.id) || !valid(e.label, 80) || !valid(e.text, 640, true) ||
          evidence.some(prior => prior.id === e.id)) return invalid();
      evidence.push({ id: e.id, label: e.label, text: e.text });
    }
  }
  return { ...(checks.length && { checks }), ...(evidence.length && { evidence }) };
}

export function parseWorkerActivity(p: Record<string, unknown>): WorkerActivityQuery {
  const { threadId, after } = p;
  if (!keys(p, ["threadId", "after"]) || !durableID(threadId) ||
      after !== undefined && (typeof after !== "string" || !/^[a-f0-9]{64}$/.test(after))) return invalid();
  return { threadId, after };
}

export function parseWorkerInspection(p: Record<string, unknown>): WorkerInspectionQuery {
  const { threadId, handle, transcript = false } = p;
  if (!keys(p, ["threadId", "handle", "transcript"]) || !durableID(threadId) || !workerHandle(handle) || typeof transcript !== "boolean") return invalid();
  return { threadId, handle, transcript };
}

export function parseReviewKey(p: Record<string, unknown>): ReviewKey {
  if (!keys(p, ["threadId", "requestId"]) || !durableID(p.threadId) || !token(p.requestId)) return invalid();
  return { threadId: p.threadId, requestId: p.requestId };
}

export function parseWorkerKey(p: Record<string, unknown>): WorkerKey {
  if (!keys(p, ["threadId", "handle"]) || !durableID(p.threadId) || !workerHandle(p.handle)) return invalid();
  return { threadId: p.threadId, handle: p.handle };
}

export function parseReviewLaunch(p: Record<string, unknown>): ReviewLaunch {
  const { threadId, requestId, scope, assignments, mode = "sequential", prompt, workerTimeoutMs = 120_000, selection } = p;
  if (!keys(p, ["threadId", "requestId", "scope", "assignments", "mode", "selection", "prompt", "workerTimeoutMs", "assessment"]) ||
      !durableID(threadId) || !token(requestId) || !object(scope) || !keys(scope, ["tracked", "untracked"]) ||
      !Array.isArray(scope.tracked) || !Array.isArray(scope.untracked) ||
      scope.tracked.length + scope.untracked.length < 1 || scope.tracked.length + scope.untracked.length > reviewPathLimit ||
      !Array.isArray(assignments) || assignments.length < 1 || assignments.length > 2 || (mode !== "sequential" && mode !== "parallel") ||
      prompt !== undefined && !valid(prompt, reviewBriefLimit, true) || typeof workerTimeoutMs !== "number" ||
      !Number.isInteger(workerTimeoutMs) || workerTimeoutMs < 100 || workerTimeoutMs > workerTimeoutLimit) return invalid();
  const paths = [...scope.tracked, ...scope.untracked];
  if (!paths.every(path => valid(path, 1024)) || new Set(paths).size !== paths.length) return invalid();
  const list: ReviewLaunch["assignments"] = [];
  for (const a of assignments) {
    if (!object(a) || !keys(a, ["name", "brief"]) || !valid(a.name, 64) || !valid(a.brief, reviewBriefLimit, true) ||
        list.some(prior => prior.name === a.name)) return invalid();
    list.push({ name: a.name, brief: a.brief });
  }
  let pick: Selection | undefined;
  if (selection !== undefined) {
    if (!object(selection) || !keys(selection, ["model", "thinking"]) || !object(selection.model) ||
        !keys(selection.model, ["provider", "modelId"]) || !valid(selection.model.provider, 256) ||
        !valid(selection.model.modelId, 256) || !level(selection.thinking)) return invalid();
    pick = { model: { provider: selection.model.provider, modelId: selection.model.modelId }, thinking: selection.thinking };
  }
  return { threadId, requestId, scope: { tracked: scope.tracked.toSorted(), untracked: scope.untracked.toSorted() }, assignments: list,
    mode, ...(prompt !== undefined && { prompt }), ...(pick && { selection: pick }), workerTimeoutMs,
    ...(p.assessment !== undefined && { assessment: parseContract(p.assessment) }) };
}
