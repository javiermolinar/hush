import { isAbsolute } from "node:path";
import { durableID, WireError } from "./wire.js";
import { watchRevision } from "./workspace-watch.js";

export const workspaceFilesVersion = 1;
export const workspaceScopeWatchVersion = 1;
export const workspaceFileSearchVersion = 2;
export const workspaceFileProjectPathsVersion = 3;
export const githubDirectory = ".github";
const maxSearchResults = 32;
// A persisted thread or, from version 2, a prospective thread: the absolute
// directory a new thread would record. The host resolves either to a
// workspace and applies the same grant; neither creates a conversation.
export type WorkspaceFileTarget = { threadId: string } | { cwd: string };
export type WorkspaceFileScope = { workspace: boolean; home: boolean };
export type WorkspaceScopeWatch = { target: WorkspaceFileTarget; after?: string };
export type WorkspaceScopeUpdate = WorkspaceFileScope & { revision: string };
export type WorkspaceFileEntry = { name: string; directory: boolean };
export type WorkspaceFilePage = { entries: WorkspaceFileEntry[]; nextCursor?: string };
export type WorkspaceFileQuery = { kind: "scope"; target: WorkspaceFileTarget } | {
  kind: "list"; target: WorkspaceFileTarget; root: "workspace" | "home"; directory: string; limit: number; cursor?: string; includeGitHub?: true;
};
export type WorkspaceFileSearch = { target: WorkspaceFileTarget; query: string; limit: number; includeGitHub?: true };
export type WorkspaceFileSearchResult = { matches: { path: string; directory: boolean }[]; partial: boolean };
export interface WorkspaceFilesHost {
  readonly projectPaths?: true;
  call(query: WorkspaceFileQuery, signal: AbortSignal): Promise<WorkspaceFileScope | WorkspaceFilePage>;
  search?(query: WorkspaceFileSearch, signal: AbortSignal): Promise<WorkspaceFileSearchResult>;
  waitScope?(query: WorkspaceScopeWatch, signal: AbortSignal): Promise<WorkspaceScopeUpdate>;
}

export const browseName = (name: unknown, includeGitHub = false): name is string => typeof name === "string" && name.length > 0 &&
  Buffer.byteLength(name) <= 255 && name.isWellFormed() && (!name.startsWith(".") || includeGitHub && name === githubDirectory) &&
  !/[/\\"@:\x00-\x1f\x7f-\x9f]/.test(name);

const browsePath = (path: unknown, includeGitHub: boolean): path is string => {
  if (typeof path !== "string" || Buffer.byteLength(path) > 2048) return false;
  const names = path.split("/");
  return names.length <= 32 && names.every((name, i) => browseName(name, includeGitHub && i === 0));
};

export const searchPath = (path: unknown, includeGitHub: boolean): path is string =>
  browsePath(path, includeGitHub) && path !== "~" && !path.startsWith("~/");

export const workspaceDirectory = (cwd: unknown): cwd is string => typeof cwd === "string" && isAbsolute(cwd) &&
  cwd.isWellFormed() && Buffer.byteLength(cwd) <= 4096 && !/[\x00-\x1f\x7f-\x9f]/.test(cwd);

// Exactly one of threadId or (version 2) cwd. Version 1 frames are unchanged.
function parseTarget({ threadId, cwd }: Record<string, unknown>, version: number): WorkspaceFileTarget {
  if (cwd === undefined && durableID(threadId)) return { threadId };
  if (threadId === undefined && version >= workspaceFileSearchVersion && workspaceDirectory(cwd)) return { cwd };
  throw new WireError("invalid_request");
}

export function parseWorkspaceScopeWatch(fields: Record<string, unknown>, version: number): WorkspaceScopeWatch {
  const { after } = fields;
  if (after !== undefined && !watchRevision(after) ||
      Object.keys(fields).some(key => key !== "threadId" && key !== "cwd" && key !== "after")) throw new WireError("invalid_request");
  return { target: parseTarget(fields, version), after: after as string | undefined };
}

export function parseWorkspaceFileSearch(fields: Record<string, unknown>, version: number): WorkspaceFileSearch {
  const { query, limit } = fields;
  if (typeof query !== "string" || !query.isWellFormed() || Buffer.byteLength(query) > 255 ||
      !/[^\p{White_Space}]/u.test(query) || /[/\\"@:\x00-\x1f\x7f-\x9f]/.test(query) ||
      !Number.isInteger(limit) || (limit as number) < 1 || (limit as number) > maxSearchResults ||
      Object.keys(fields).some(k => !["threadId", "cwd", "query", "limit"].includes(k))) throw new WireError("invalid_request");
  return { target: parseTarget(fields, version), query, limit: limit as number,
    ...(version === workspaceFileProjectPathsVersion && { includeGitHub: true }) };
}

export function parseWorkspaceFiles(method: string, fields: Record<string, unknown>, version: number): WorkspaceFileQuery {
  const { root, directory, limit, cursor } = fields;
  if (method !== "scope" && method !== "list") throw new WireError("unknown_method");
  const target = parseTarget(fields, version);
  if (method === "scope") {
    if (Object.keys(fields).length !== 1) throw new WireError("invalid_request");
    return { kind: "scope", target };
  }
  const includeGitHub = version === workspaceFileProjectPathsVersion && root === "workspace";
  if ((root !== "workspace" && root !== "home") || directory !== "" && !browsePath(directory, includeGitHub) ||
      !Number.isInteger(limit) || (limit as number) < 1 || (limit as number) > 100 ||
      cursor !== undefined && (typeof cursor !== "string" || !/^[A-Za-z0-9_-]{44,512}$/.test(cursor) || cursor.endsWith("\n")) ||
      Object.keys(fields).some(k => !["threadId", "cwd", "root", "directory", "limit", "cursor"].includes(k))) {
    throw new WireError("invalid_request");
  }
  return { kind: "list", target, root, directory, limit: limit as number, cursor: cursor as string | undefined,
    ...(includeGitHub && { includeGitHub: true }) };
}
