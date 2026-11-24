import { durableID, object, WireError, type Params } from "./wire.js";

export const modelCatalogVersion = 1;
export const modelCatalogSearchVersion = 2;
export const modelCatalogDraftVersion = 3;
export type ModelCatalogVersion = typeof modelCatalogVersion | typeof modelCatalogSearchVersion | typeof modelCatalogDraftVersion;
export const modelCatalogLimit = 64;
const catalogQueryMaxBytes = 128;
export const thinkingLevels = ["off", "minimal", "low", "medium", "high", "xhigh", "max"] as const;
type Thinking = typeof thinkingLevels[number];
export type ModelRef = NonNullable<Params["thread.new"]["model"]>;
export type CatalogReason = "missing_model" | "credentials_required" | "availability_unknown";
export type CatalogRow = ModelRef & {
  label: string; available: boolean; reason: CatalogReason | null; thinkingLevels: readonly Thinking[];
};
export type Catalog = { current: ModelRef | null; default: ModelRef; models: CatalogRow[] };
export type CatalogCreation = { cwd?: string; model: ModelRef; thinkingLevel: Thinking };
export interface ModelCatalogHost {
  list(threadId: string, signal: AbortSignal): Promise<Catalog>;
  search?(threadId: string, query: string, signal: AbortSignal): Promise<Catalog>;
  draft?(query: string, signal: AbortSignal): Promise<Catalog>;
}

const exact = (v: Record<string, unknown>, keys: string[]) =>
  Object.keys(v).length === keys.length && keys.every(k => Object.hasOwn(v, k));
const cleanCatalogText = (v: string) => v.isWellFormed() && !/[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/u.test(v);
export const publicCatalogText = (v: unknown): v is string => typeof v === "string" && !!v.trim() &&
  v.length <= 512 && cleanCatalogText(v) && [...v].length <= 256;
const refFields = (v: Record<string, unknown>) => publicCatalogText(v.provider) && publicCatalogText(v.modelId);
const ref = (v: unknown): v is ModelRef => object(v) && exact(v, ["provider", "modelId"]) && refFields(v);
export const modelKey = (v: ModelRef) => JSON.stringify([v.provider, v.modelId]);

export function parseCatalogQuery(v: unknown, version: ModelCatalogVersion) {
  if (!object(v)) throw new WireError("invalid_request");
  const { threadId } = v;
  if (threadId !== undefined && !durableID(threadId)) throw new WireError("invalid_request");
  if (threadId === undefined && version !== modelCatalogDraftVersion) throw new WireError("invalid_request");
  const scope = threadId === undefined ? [] : ["threadId"];
  if (exact(v, scope)) return { threadId, query: "" };
  if (version === modelCatalogVersion || !exact(v, [...scope, "query"]) ||
      typeof v.query !== "string" || Buffer.byteLength(v.query) > catalogQueryMaxBytes || !cleanCatalogText(v.query)) {
    throw new WireError("invalid_request");
  }
  return { threadId, query: v.query.trim() };
}

export function parseCatalogCreation(v: unknown): CatalogCreation {
  if (!object(v) || !ref(v.model)) throw new WireError("invalid_request");
  const { cwd } = v;
  const thinkingLevel = thinkingLevels.find(level => level === v.thinkingLevel);
  if (!thinkingLevel || !exact(v, cwd === undefined ? ["model", "thinkingLevel"] : ["cwd", "model", "thinkingLevel"]) ||
      (cwd !== undefined && (typeof cwd !== "string" || !cwd.length || cwd.length > 8192 || [...cwd].length > 4096))) {
    throw new WireError("invalid_request");
  }
  return { ...(cwd === undefined ? {} : { cwd }), model: v.model, thinkingLevel };
}

export function checkCatalog(v: unknown, filtered: boolean): asserts v is Catalog {
  if (!object(v) || !exact(v, ["current", "default", "models"]) || !ref(v.default) ||
      (v.current !== null && !ref(v.current)) || !Array.isArray(v.models) || v.models.length > modelCatalogLimit) {
    throw new WireError("internal");
  }
  const seen = new Set<string>();
  for (const row of v.models) {
    if (!object(row) || !exact(row, ["provider", "modelId", "label", "available", "reason", "thinkingLevels"]) ||
        !refFields(row) || !publicCatalogText(row.label) || typeof row.available !== "boolean" ||
        !Array.isArray(row.thinkingLevels) || row.thinkingLevels.length > thinkingLevels.length ||
        Array.from(row.thinkingLevels).some(level => !thinkingLevels.includes(level)) || new Set(row.thinkingLevels).size !== row.thinkingLevels.length ||
        (row.available ? row.reason !== null || !row.thinkingLevels.length :
          row.reason === "missing_model" ? row.thinkingLevels.length !== 0 :
            (row.reason !== "credentials_required" && row.reason !== "availability_unknown") || !row.thinkingLevels.length)) {
      throw new WireError("internal");
    }
    const key = modelKey({ provider: String(row.provider), modelId: String(row.modelId) });
    if (seen.has(key)) throw new WireError("internal");
    seen.add(key);
  }
  if (!filtered && (!seen.has(modelKey(v.default)) || (v.current && !seen.has(modelKey(v.current))))) throw new WireError("internal");
}
