import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { getSupportedThinkingLevels } from "@earendil-works/pi-ai/models";
import type { ModelRegistry, ModelRuntime } from "@earendil-works/pi-coding-agent";
import { AgentDoc, type ConversationId, type Harness } from "@earendil-works/pi-durable";
import type { NativeHost } from "../../server/src/index.js";
import { modelCatalogLimit, modelKey, publicCatalogText, type Catalog, type CatalogRow, type ModelCatalogHost,
  type CatalogReason, type ModelRef } from "../../server/src/model-catalog.js";
import { WireError } from "../../server/src/wire.js";

const projectRef = ({ provider, modelId }: ModelRef): ModelRef => ({ provider, modelId });
const publicRef = (ref: ModelRef) => publicCatalogText(ref.provider) && publicCatalogText(ref.modelId);

export function nativeModelCatalog(h: Harness, runtime: ModelRuntime, registry: ModelRegistry,
    defaultModel: ModelRef): ModelCatalogHost {
  async function list(threadId: string | undefined, signal: AbortSignal, query = ""): Promise<Catalog> {
    let current: ModelRef | null = null;
    if (threadId !== undefined) {
      const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
      const id = Number(threadId) as ConversationId;
      if (!await h.conversation(id, ctx)) throw new WireError("not_found");
      const stored = (await h.snapshot(AgentDoc, id, ctx))?.model;
      if (stored) current = projectRef(stored);
    }
    const fallback = projectRef(defaultModel);
    const priority: CatalogRow[] = [], usable: CatalogRow[] = [], disabled: CatalogRow[] = [];
    const seen = new Set<string>();
    const auth = new Map<string, CatalogReason | null>();
    const budget = AbortSignal.any([signal, AbortSignal.timeout(2_000)]);
    const term = query.toLowerCase();
    const matches = (ref: ModelRef, label: string) => {
      if (!term) return true;
      const value = `${ref.provider}/${ref.modelId} ${label}`.toLowerCase();
      let offset = 0;
      for (const char of term) {
        const at = value.indexOf(char, offset);
        if (at < 0) return false;
        offset = at + char.length;
      }
      return true;
    };
    const add = async (ref: ModelRef, pinned: boolean) => {
      signal.throwIfAborted();
      const key = modelKey(ref);
      if (seen.has(key) || !publicRef(ref)) return;
      seen.add(key);
      const physical = runtime.getPhysicalModel(ref.provider, ref.modelId) && runtime.getModel(ref.provider, ref.modelId);
      const label = publicCatalogText(physical?.name) ? physical.name : ref.modelId;
      if (!matches(ref, label)) return;
      if (!physical) {
        if (pinned) priority.push({ ...ref, label, available: false, reason: "missing_model", thinkingLevels: [] });
        return;
      }
      let reason = auth.get(ref.provider);
      if (reason === undefined) {
        reason = "availability_unknown";
        try {
          if (!budget.aborted) reason = await runtime.checkAuth(ref.provider, { signal: budget }) ? null : "credentials_required";
        } catch { /* Do not expose provider or store diagnostics. */ }
        auth.set(ref.provider, reason);
      }
      const available = reason === null;
      if (pinned || available || disabled.length < modelCatalogLimit) {
        const row: CatalogRow = { ...ref, label, available, reason, thinkingLevels: getSupportedThinkingLevels(physical) };
        (pinned ? priority : available ? usable : disabled).push(row);
      }
    };
    if (current) await add(current, true);
    await add(fallback, true);
    // Cap matching rows, not the inventory. Check credentials only for
    // matching providers and preserve runtime order within each bucket.
    for (const model of registry.getAll()) {
      if (priority.length + usable.length >= modelCatalogLimit) break;
      await add({ provider: model.provider, modelId: model.id }, false);
    }
    signal.throwIfAborted();
    return { current, default: fallback, models: [...priority, ...usable, ...disabled].slice(0, modelCatalogLimit) };
  }
  return { list, search: (threadId, query, signal) => list(threadId, signal, query),
    draft: (query, signal) => list(undefined, signal, query) };
}

export function nativeSelectionValidator(runtime: ModelRuntime, requireModel: NativeHost["requireModel"]):
    NonNullable<NativeHost["validateSelection"]> {
  return async (ref, level) => {
    const model = ref && runtime.getPhysicalModel(ref.provider, ref.modelId) && runtime.getModel(ref.provider, ref.modelId);
    if (!model || !getSupportedThinkingLevels(model).some(tier => tier === level)) {
      const error = new WireError("invalid_request");
      error.message = model ? "Thinking level is not supported by the selected model. Reopen /thinking or /model." :
        "Selected model is not installed. Select an available model with /model.";
      throw error;
    }
    await requireModel?.(ref);
  };
}
