import type { JsonValue } from "@earendil-works/chord/delta";
import schema from "../../protocol/v1/core.schema.json" with { type: "json" };
import { requestShape } from "./validators.js";
export { entryShape, patchShape, stateEnvelopeShape, stateShape, successShape } from "./validators.js";

export const MAX_RECORD = 8 * 1024 * 1024;
export const object = (v: unknown): v is Record<string, unknown> => v !== null && typeof v === "object" && !Array.isArray(v);
export const token = (v: unknown): v is string => typeof v === "string" && /^[A-Za-z0-9_-]{1,160}$/.test(v) && !v.endsWith("\n");
export const durableID = (v: unknown): v is string =>
  typeof v === "string" && /^[1-9][0-9]{0,15}$/.test(v) && String(Number(v)) === v && Number.isSafeInteger(Number(v));

type Model = { provider: string; modelId: string };
export type Thinking = "off" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max";
type Thread = { threadId: string };
export type ExplicitRange = { kind: "before"; beforeEntryId: string | null }
  | { kind: "after"; afterEntryId: string | null; throughEntryId: string | null };
export type Params = {
  hello: { versions: number[]; capabilities?: Record<string, number[]> };
  subscribe: Thread & { window?: { min: number; max: number } };
  history: Thread & { limit: number; range: ExplicitRange | { kind: "cursor"; cursor: string } };
  "thread.list": { limit: number; cursor?: string };
  "thread.new": { cwd?: string; model?: Model };
  "thread.fork": Thread & { atEntryId: string };
  "thread.configure": Thread & { change: { model?: Model | null; thinkingLevel?: Thinking | null; compactionEnabled?: boolean | null } };
  "thread.abort": Thread;
  "thread.wait": Thread;
  "thread.compact": Thread & { instructions?: string };
  "thread.reset": Thread & { handoff?: string };
  submit: Thread & { requestId: string; text: string; whenBusy: "steer" | "followUp" | "reject" };
  continue: Thread;
  "submission.get": Thread & ({ requestId: string } | { submissionId: string });
  "submission.wait": Thread & { submissionId: string };
};
export type Method = keyof Params;
export type CoreRequest = { [M in Method]: { type: "request"; id: string; method: M; params: Params[M] } }[Method];
export type Envelope = { type: "request"; id: string; method: string; params?: unknown };

const allowedMethods = new Set(schema.$defs.request.oneOf.flatMap((arm) => arm.properties.method.const ?? []));

export const isCoreMethod = (method: string): method is Method => allowedMethods.has(method);
// Some cross-field restrictions are defined in the protocol prose, not the schema.
export function validRequest(raw: Envelope): raw is CoreRequest {
  if (!requestShape(raw)) return false;
  const p = raw.params as Record<string, unknown>;
  for (const key of ["threadId", "atEntryId", "submissionId"]) {
    if (Object.hasOwn(p, key) && !durableID(p[key])) return false;
  }
  return true;
}

type Code = "invalid_request" | "not_negotiated" | "unsupported_version" | "already_negotiated"
  | "unsupported_capability" | "unknown_method" | "not_found" | "invalid_cursor" | "busy" | "idle" | "limit" | "internal";
const messages: Record<Code, string> = {
  invalid_request: "Invalid request fields", not_negotiated: "Send hello first", unsupported_version: "No common wire major",
  already_negotiated: "This connection already negotiated a version", unsupported_capability: "Capability was not negotiated",
  unknown_method: "Unknown method", not_found: "No such record in this scope", invalid_cursor: "Invalid or expired cursor or anchor",
  busy: "The thread is busy", idle: "The thread is idle", limit: "Wire resource limit exceeded", internal: "Internal server error",
};
export class WireError extends Error {
  constructor(readonly code: Code, readonly capability?: string) { super(messages[code]); }
}
export const identity = ({ id, method }: Envelope) => ({ type: "response", id, method });

export function cleanJSON(value: unknown, depth = 0): value is JsonValue {
  if (typeof value === "string") return value.isWellFormed();
  if (typeof value === "number") return Number.isFinite(value);
  if (value === undefined) return false;
  if (depth > 256) return false;
  if (Array.isArray(value)) return value.every(v => v !== undefined && cleanJSON(v, depth + 1));
  if (object(value)) {
    for (const k of Object.keys(value)) if (!k.isWellFormed() || !cleanJSON(value[k], depth + 1)) return false;
  }
  return typeof value !== "bigint" && typeof value !== "symbol" && typeof value !== "function";
}

// Parse with V8 for syntax first, then check duplicate keys (including escaped spellings).
// JSON.parse alone has already discarded them. Each object owns its independent key set.
export function parseRecord(line: Buffer): unknown {
  const text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(line);
  const value: unknown = JSON.parse(text);
  if (!cleanJSON(value)) throw new SyntaxError("Non-JSON value");
  const stack: (Set<string> | null)[] = [];
  for (let i = 0; i < text.length; i++) {
    const c = text.charCodeAt(i);
    if (c === 123) stack.push(new Set());
    else if (c === 91) stack.push(null);
    else if (c === 125 || c === 93) stack.pop();
    else if (c === 34) {
      const start = i++;
      while (text.charCodeAt(i) !== 34) {
        if (text.charCodeAt(i++) === 92) i++;
      }
      let j = i + 1;
      while (text[j] === " " || text[j] === "\t" || text[j] === "\r" || text[j] === "\n") j++;
      if (text[j] === ":") {
        const keys = stack.at(-1)!;
        const k: string = JSON.parse(text.slice(start, i + 1));
        if (keys.has(k)) throw new SyntaxError("Duplicate object key");
        keys.add(k);
      }
    }
  }
  return value;
}

export function encode(value: object): string {
  if (!cleanJSON(value)) throw new WireError("internal");
  const data = JSON.stringify(value);
  if (Buffer.byteLength(data) > MAX_RECORD) throw new WireError("limit");
  return data + "\n";
}
