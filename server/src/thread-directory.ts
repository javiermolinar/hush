import AjvModule from "ajv/dist/2020.js";
import schema from "../../protocol/v1/thread-directory.schema.json" with { type: "json" };
import { durableID, WireError } from "./wire.js";

export const threadDirectoryVersion = 1;
export const threadManagementVersion = 2;
export const threadWorkspaceVersion = 1;
export const threadWorkspaceSearchVersion = 2;
export type ThreadDirectoryVersion = 1 | 2;
export type ThreadWorkspaceVersion = 1 | 2;
export type ThreadDirectoryQuery = { limit: number; cursor?: string; includeArchived?: boolean; workspace?: string; browseAll?: boolean; query?: string };
export type ThreadLookup = { threadIds: string[]; after?: number };
export type ThreadCreation = { cwd?: string; title?: string; worktree?: boolean };
export type ThreadArchive = { threadId: string; archived: boolean };
export type ThreadStatus = "running" | "parked" | "scheduled" | "done" | "idle" | "failed" | "stopped";
export type ThreadCheckout = { kind: "shared"; cwd?: string } | { kind: "managed"; cwd: string; branch: string };
export type ThreadDirectoryRow = { id: string; parentId?: string; title?: string; lastMessageMs?: number };
export type ManagedThreadRow = ThreadDirectoryRow & { status: ThreadStatus; archived: boolean; checkout: ThreadCheckout };
export type WorkspaceMatch = "inside" | "outside" | "unknown";
export type WorkspaceLabel = { id: string; name: string };
export type ThreadDirectoryPage = { threads: ThreadDirectoryRow[]; nextCursor: string | null; indexing: boolean;
  workspaceMatches?: WorkspaceMatch[]; workspaces?: Array<WorkspaceLabel | null> };
export type ThreadDirectoryReport = { revision: number; threads: ManagedThreadRow[] };

export interface ThreadDirectoryHost {
  workspace?: true | typeof threadWorkspaceSearchVersion;
  list(query: ThreadDirectoryQuery, signal: AbortSignal, version: ThreadDirectoryVersion, workspaceVersion?: ThreadWorkspaceVersion): Promise<ThreadDirectoryPage>;
  management?: {
    get(query: ThreadLookup, signal: AbortSignal): Promise<ThreadDirectoryReport>;
    create(query: ThreadCreation, signal: AbortSignal): Promise<ManagedThreadRow>;
    archive(query: ThreadArchive, signal: AbortSignal): Promise<void>;
  };
}

const ajv = new AjvModule.Ajv2020({ strict: false, ownProperties: true }).addSchema(schema);
const def = (name: keyof typeof schema.$defs) => ajv.compile({ $ref: schema.$id + "#/$defs/" + name });
const listShape = def("list"), listV2Shape = def("listV2"), getShape = def("get"), newShape = def("newThread"), archiveShape = def("archive");
const listWorkspaceShape = def("listWorkspace"), envelopeWorkspace = def("successWorkspace");
const listSearchShape = def("listWorkspaceSearch"), envelopeSearch = def("successWorkspaceSearch");
const envelope = def("successEnvelope"), envelopeV2 = def("successEnvelopeV2");

export function parseThreadDirectoryQuery(params: Record<string, unknown>, version: ThreadDirectoryVersion, workspace?: ThreadWorkspaceVersion): ThreadDirectoryQuery {
  const shape = workspace === threadWorkspaceSearchVersion ? listSearchShape : workspace && Object.hasOwn(params, "workspace") ?
    listWorkspaceShape : version === 2 ? listV2Shape : listShape;
  if (!shape(params)) throw new WireError("invalid_request");
  return params as ThreadDirectoryQuery;
}

export function parseThreadLookup(params: Record<string, unknown>): ThreadLookup {
  if (!getShape(params) || !(params.threadIds as unknown[]).every(durableID)) throw new WireError("invalid_request");
  return params as ThreadLookup;
}

export function parseThreadCreation(params: Record<string, unknown>): ThreadCreation {
  if (!newShape(params)) throw new WireError("invalid_request");
  return params as ThreadCreation;
}

export function parseThreadArchive(params: Record<string, unknown>): ThreadArchive {
  if (!archiveShape(params) || !durableID(params.threadId)) throw new WireError("invalid_request");
  return params as ThreadArchive;
}

export function checkThreadDirectoryReply(version: ThreadDirectoryVersion, frame: object, rows: readonly ThreadDirectoryRow[], workspace?: ThreadWorkspaceVersion) {
  const shape = workspace === threadWorkspaceSearchVersion ? envelopeSearch : workspace ? envelopeWorkspace : version === 2 ? envelopeV2 : envelope;
  if (!shape(frame) || rows.some(row => !durableID(row.id) || (row.parentId && !durableID(row.parentId)))) {
    throw new WireError("internal");
  }
  return frame;
}
