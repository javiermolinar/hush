import { durableID, WireError } from "./wire.js";

export const loadedContextVersion = 1;
export const loadedContextAgentsVersion = 2;
export type LoadedContextVersion = typeof loadedContextVersion | typeof loadedContextAgentsVersion;
export type InstructionSource = { kind: "personal" | "workspace"; path: string };
export interface LoadedContextHost {
  get(threadId: string, version: LoadedContextVersion, signal: AbortSignal): Promise<{ sources: InstructionSource[] }>;
}

export function parseLoadedContext(params: Record<string, unknown>) {
  if (Object.keys(params).length !== 1 || !durableID(params.threadId)) throw new WireError("invalid_request");
  return params.threadId;
}
