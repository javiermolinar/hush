import type { PlanAttachment, PlanThinking } from "./plan.js";
import { durableID, token, WireError } from "./wire.js";

export const planReviewVersion = 1;
export const planReviewKind = "hush.plan.review";
export const planReviewAreas = ["requirements", "acceptance", "dependencies"] as const;
export type PlanReviewArea = typeof planReviewAreas[number];
export type PlanReviewAssessment = "addressed" | "concern" | "unverified";
export type PlanReviewSource = "captured" | "partial" | "missing";
export type PlanReviewReport = {
  summary: string;
  checks: Record<PlanReviewArea, PlanReviewAssessment>;
  findings: { severity: "blocking" | "advisory"; area: PlanReviewArea; stepId?: string; message: string }[];
  limitations: string[];
};
export type PlanReviewRecord = {
  id: string; threadId: string; reviewerThreadId?: string;
  model: { provider: string; modelId: string }; thinking: PlanThinking;
  state: "running" | "complete" | "failed";
  report?: PlanReviewReport; failure?: string;
};
export type PlanReviewTarget = PlanAttachment & { revision: number };
export type PlanReviewView = {
  cursor: string; title: string; revision: number; headRevision: number; source: PlanReviewSource; canStart: boolean;
  review: PlanReviewRecord | null;
};
export interface PlanReviewHost {
  wait(query: PlanReviewTarget & { after?: string }, signal: AbortSignal): Promise<PlanReviewView>;
  start(query: PlanReviewTarget & { requestId: string }, signal: AbortSignal): Promise<{ reviewId: string }>;
  cancel(query: PlanReviewTarget & { reviewId: string }, signal: AbortSignal): Promise<Record<string, never>>;
}

type ReviewRequest = { method: "wait"; query: PlanReviewTarget & { after?: string } }
  | { method: "start"; query: PlanReviewTarget & { requestId: string } }
  | { method: "cancel"; query: PlanReviewTarget & { reviewId: string } };

export function parsePlanReview(method: string | undefined, p: Record<string, unknown>): ReviewRequest {
  if (method !== "wait" && method !== "start" && method !== "cancel") throw new WireError("unknown_method");
  const extra = method === "wait" ? "after" : method === "start" ? "requestId" : "reviewId";
  if (Object.keys(p).some(k => k !== "threadId" && k !== "planId" && k !== "revision" && k !== extra) ||
      !durableID(p.threadId) || !token(p.planId) || typeof p.revision !== "number" ||
      !Number.isSafeInteger(p.revision) || p.revision < 1) throw new WireError("invalid_request");
  const target = { threadId: p.threadId, planId: p.planId, revision: p.revision };
  if (method === "wait" && (p.after === undefined || typeof p.after === "string" && p.after.length === 64 && /^[a-f0-9]+$/.test(p.after))) {
    return { method, query: { ...target, after: p.after } };
  }
  if (method === "start" && token(p.requestId)) return { method, query: { ...target, requestId: p.requestId } };
  if (method === "cancel" && durableID(p.reviewId)) return { method, query: { ...target, reviewId: p.reviewId } };
  throw new WireError("invalid_request");
}
