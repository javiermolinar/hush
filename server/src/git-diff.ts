import { durableID, object, WireError } from "./wire.js";
import { watchRevision } from "./workspace-watch.js";

export const gitDiffVersion = 1;
export const gitDiffWatchVersion = 1;
export const gitDiffFiles = 128;
export const gitDiffLines = 1500;
export type GitDiffRange = "head" | "checkpoint";
export type GitDiffRequest =
  | { op: "summary"; threadId: string; range: GitDiffRange }
  | { op: "file"; threadId: string; revision: string; file: number };

export type GitDiffFile = {
  path: string; status: "A" | "D" | "M" | "T" | "U" | "?";
  added?: number; removed?: number; staged: boolean; text: boolean;
};
export type GitDiffLine = {
  kind: "hunk" | "context" | "add" | "remove" | "note";
  oldLine?: number; newLine?: number; text: string;
};
export type GitDiffSummary = {
  revision: string; range: GitDiffRange; commit: string; checkpoint: boolean; branch?: string; files: GitDiffFile[];
};
export type GitDiffPatch = { revision: string; file: number; lines: GitDiffLine[]; truncated: boolean };
export type GitDiffWatch = { threadId: string; after?: string };
export type GitDiffUpdate = { revision: string } & (
  { summary: GitDiffSummary } | { error: { code: string; message: string } }
);
export interface GitDiffHost {
  call(query: GitDiffRequest, signal: AbortSignal): Promise<GitDiffSummary | GitDiffPatch>;
  wait?(query: GitDiffWatch, signal: AbortSignal): Promise<GitDiffUpdate>;
}

export function parseGitDiffWatch(fields: Record<string, unknown>): GitDiffWatch {
  const { threadId, after } = fields;
  if (!durableID(threadId) || after !== undefined && !watchRevision(after) ||
      Object.keys(fields).some(key => key !== "threadId" && key !== "after")) throw new WireError("invalid_request");
  return { threadId, after: after as string | undefined };
}

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
export function parseGitDiff(op: string, params: unknown): GitDiffRequest {
  if (op !== "summary" && op !== "file") throw new WireError("unknown_method");
  if (!object(params) || !durableID(params.threadId)) throw new WireError("invalid_request");
  const { threadId, range, revision, file } = params;
  if (op === "summary" && Object.keys(params).length === 2 && (range === "head" || range === "checkpoint")) {
    return { op, threadId, range };
  }
  if (op === "file" && Object.keys(params).length === 3 && typeof revision === "string" && revision.length === 36 && uuid.test(revision) &&
      typeof file === "number" && Number.isInteger(file) && file >= 0 && file < gitDiffFiles) {
    return { op, threadId, revision, file };
  }
  throw new WireError("invalid_request");
}
