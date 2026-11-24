import { durableID, WireError } from "./wire.js";

export const workspaceFilesVersion = 1;
export type WorkspaceFileScope = { workspace: boolean; home: boolean };
export type WorkspaceFileEntry = { name: string; directory: boolean };
export type WorkspaceFilePage = { entries: WorkspaceFileEntry[]; nextCursor?: string };
export type WorkspaceFileQuery = { kind: "scope"; threadId: string } | {
  kind: "list"; threadId: string; root: "workspace" | "home"; directory: string; limit: number; cursor?: string;
};
export interface WorkspaceFilesHost {
  call(query: WorkspaceFileQuery, signal: AbortSignal): Promise<WorkspaceFileScope | WorkspaceFilePage>;
}

export const browseName = (name: unknown): name is string => typeof name === "string" && name.length > 0 &&
  Buffer.byteLength(name) <= 255 && name.isWellFormed() && !name.startsWith(".") && !/[/\\"@:\x00-\x1f\x7f-\x9f]/.test(name);

export function parseWorkspaceFiles(method: string, fields: Record<string, unknown>): WorkspaceFileQuery {
  const { threadId, root, directory, limit, cursor } = fields;
  if (method !== "scope" && method !== "list") throw new WireError("unknown_method");
  if (!durableID(threadId)) throw new WireError("invalid_request");
  if (method === "scope") {
    if (Object.keys(fields).length !== 1) throw new WireError("invalid_request");
    return { kind: "scope", threadId };
  }
  if ((root !== "workspace" && root !== "home") || typeof directory !== "string" || Buffer.byteLength(directory) > 2048 ||
      directory !== "" && (directory.split("/").length > 32 || !directory.split("/").every(browseName)) ||
      !Number.isInteger(limit) || (limit as number) < 1 || (limit as number) > 100 ||
      cursor !== undefined && (typeof cursor !== "string" || !/^[A-Za-z0-9_-]{44,512}$/.test(cursor) || cursor.endsWith("\n")) ||
      Object.keys(fields).some(k => !["threadId", "root", "directory", "limit", "cursor"].includes(k))) {
    throw new WireError("invalid_request");
  }
  return { kind: "list", threadId, root, directory, limit: limit as number, cursor: cursor as string | undefined };
}
