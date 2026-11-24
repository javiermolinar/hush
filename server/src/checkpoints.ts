import { durableID, WireError } from "./wire.js";

export const checkpointsVersion = 1;
export const checkpointKind = "hush.checkpoint";
export const effectKind = "hush.known-effect";
export const importedLabelKind = "pi.label";
export const checkpointLimit = 128;
export const effectLimit = 128;
export type RewindMode = "files" | "transcript";

export type CheckpointItem = {
  id?: string; atEntryId: string; label: string; kind: "files" | "bookmark" | "imported_label";
};
export type EffectNote = { entryId: string; text: string };
export type FileRestore = { repository: string; commit: string; ref: string; cwd: string; branch: string };
export type RewindPreview = {
  id: string; threadId: string; atEntryId: string; mode: RewindMode;
  files?: FileRestore; reason?: string; effects: EffectNote[];
};
export type RewindResult = {
  id: string; threadId: string; atEntryId: string; mode: RewindMode;
  state: "staging" | "ready" | "failed"; forkThreadId?: string; reason?: string;
};
export type CheckpointRequest = { threadId: string } & (
  { op: "list" } |
  { op: "record"; kind: "files" | "bookmark"; label: string; atEntryId?: string } |
  { op: "effect"; text: string } |
  ({ op: "preview"; mode: RewindMode } & ({ checkpointId: string } | { atEntryId: string })) |
  { op: "confirm"; id: string; mode: RewindMode } | { op: "status"; id: string });

export interface CheckpointsHost {
  call(query: CheckpointRequest, signal: AbortSignal): Promise<
    CheckpointItem | { items: CheckpointItem[] } | RewindPreview | RewindResult | { entryId: string }
  >;
}

const uuid = (value: unknown): value is string => typeof value === "string" && value.length === 36 &&
  /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value);
const checkKeys = (p: Record<string, unknown>, ...keys: string[]) => {
  if (Object.keys(p).some(k => k !== "threadId" && !keys.includes(k))) throw new WireError("invalid_request");
};
const mode = (m: unknown): m is RewindMode => m === "files" || m === "transcript";
const label = (value: unknown, max: number) => typeof value === "string" &&
  value.isWellFormed() && Buffer.byteLength(value) <= max && !/[\p{Cc}\p{Cf}]/u.test(value);

export function parseCheckpoint(op: string, p: Record<string, unknown>): CheckpointRequest {
  const { threadId, kind, text, id, atEntryId, checkpointId } = p;
  if (!durableID(threadId)) throw new WireError("invalid_request");
  switch (op) {
    case "list":
      checkKeys(p);
      return { op, threadId };
    case "record":
      checkKeys(p, "kind", "label", "atEntryId");
      if ((kind !== "files" && kind !== "bookmark") || p.label !== undefined && !label(p.label, 192) ||
          atEntryId !== undefined && !durableID(atEntryId)) throw new WireError("invalid_request");
      return { op, threadId, kind, label: typeof p.label === "string" ? p.label.trim() : "",
        ...(atEntryId !== undefined && { atEntryId }) };
    case "effect":
      checkKeys(p, "text");
      if (!label(text, 192) || typeof text !== "string" || !text.trim()) throw new WireError("invalid_request");
      return { op, threadId, text: text.trim() };
    case "preview":
      checkKeys(p, "mode", "checkpointId", "atEntryId");
      if (!mode(p.mode)) throw new WireError("invalid_request");
      if (uuid(checkpointId) && atEntryId === undefined) return { op, threadId, mode: p.mode, checkpointId };
      if (durableID(atEntryId) && checkpointId === undefined) return { op, threadId, mode: p.mode, atEntryId };
      throw new WireError("invalid_request");
    case "confirm":
      checkKeys(p, "id", "mode");
      if (!uuid(id) || !mode(p.mode)) throw new WireError("invalid_request");
      return { op, threadId, id, mode: p.mode };
    case "status":
      checkKeys(p, "id");
      if (!uuid(id)) throw new WireError("invalid_request");
      return { op, threadId, id };
    default: throw new WireError("unknown_method");
  }
}
