import { createProvider, envApiKeyAuth, getModelType, type Api, type Model, type MutableModels, type Provider,
  type SamplingParamsByThinkingLevel } from "@earendil-works/pi-ai";
import { anthropicMessagesApi } from "@earendil-works/pi-ai/api/anthropic-messages.lazy";
import { azureOpenAIResponsesApi } from "@earendil-works/pi-ai/api/azure-openai-responses.lazy";
import { bedrockConverseStreamApi } from "@earendil-works/pi-ai/api/bedrock-converse-stream.lazy";
import { googleGenerativeAIApi } from "@earendil-works/pi-ai/api/google-generative-ai.lazy";
import { googleVertexApi } from "@earendil-works/pi-ai/api/google-vertex.lazy";
import { mistralConversationsApi } from "@earendil-works/pi-ai/api/mistral-conversations.lazy";
import { openAICodexResponsesApi } from "@earendil-works/pi-ai/api/openai-codex-responses.lazy";
import { openAICompletionsApi } from "@earendil-works/pi-ai/api/openai-completions.lazy";
import { openAIResponsesApi } from "@earendil-works/pi-ai/api/openai-responses.lazy";
import { piMessagesApi } from "@earendil-works/pi-ai/api/pi-messages.lazy";
import { ProviderCompatSchema, type ProviderCompat } from "@earendil-works/pi-ai/providers/compat-schema";
import { ModelCostTierSchema, ModelInputLimitsSchema, ModelInputModalitySchema, ModelPromptCacheSchema,
  ThinkingLevelMapSchema } from "@earendil-works/pi-ai/providers/model-schema";
import { arr, bool, compile, int, map, num, obj, opt, str, unknown } from "@earendil-works/pi-schema";
import { parseRecord } from "../../server/src/wire.js";
import { BackendError } from "./config.js";
import { configValue } from "./config-value.js";
import { readProfileText } from "./profile-json.js";

// Pi AI owns streaming, auth resolution and request normalization. Hush only
// maps its private models.json records onto these public chat APIs.
const apis = new Map([
  ["anthropic-messages", anthropicMessagesApi], ["azure-openai-responses", azureOpenAIResponsesApi],
  ["bedrock-converse-stream", bedrockConverseStreamApi], ["google-generative-ai", googleGenerativeAIApi],
  ["google-vertex", googleVertexApi], ["mistral-conversations", mistralConversationsApi],
  ["openai-codex-responses", openAICodexResponsesApi], ["openai-completions", openAICompletionsApi],
  ["openai-responses", openAIResponsesApi], ["pi-messages", piMessagesApi],
]);

const text = str({ minLength: 1, maxLength: 4096 });
const price = opt(num({ minimum: 0 }));
const cost = obj({ input: price, output: price, cacheRead: price, cacheWrite: price, tiers: opt(arr(ModelCostTierSchema)) });
const free = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
const levels = ["off", "minimal", "low", "medium", "high", "xhigh", "max"] as const;
const samplingByLevelFields = obj(Object.fromEntries(levels.map(level => [level, opt(map(unknown()))])));
const fields = obj({
  id: opt(text), name: opt(text), api: opt(text), baseUrl: opt(text), headers: opt(map(str())),
  input: opt(arr(ModelInputModalitySchema, { minItems: 1 })), reasoning: opt(bool()),
  contextWindow: opt(int({ minimum: 1, maximum: Number.MAX_SAFE_INTEGER })),
  maxTokens: opt(int({ minimum: 1, maximum: Number.MAX_SAFE_INTEGER })),
  cost: opt(cost), inputLimits: opt(ModelInputLimitsSchema), thinkingLevelMap: opt(ThinkingLevelMapSchema),
  promptCache: opt(ModelPromptCacheSchema), compat: opt(ProviderCompatSchema),
  samplingParams: opt(map(unknown())), samplingParamsByThinkingLevel: opt(samplingByLevelFields),
});
const providerFields = obj({
  name: opt(text), baseUrl: opt(text), api: opt(text), apiKey: opt(text), authHeader: opt(bool()), headers: opt(map(str())),
  compat: opt(ProviderCompatSchema), models: opt(arr(fields)), modelOverrides: opt(map(fields)),
});
const schema = obj({ $schema: opt(text), providers: opt(map(providerFields)) });
let validator: ReturnType<typeof compile<typeof schema>> | undefined;

export class ModelOverlayError extends BackendError {
  constructor(detail: string) {
    super("configuration");
    this.message = `Invalid ~/.hush/models.json: ${detail}. Fix the file and restart Hush.`;
  }
}

// Never print configured keys, provider/model IDs, URLs or raw validator messages.
function invalidField(path: string) {
  const [, root, provider, field, entry, modelField] = path.split("/");
  if (root !== "providers") return new ModelOverlayError("invalid top-level fields");
  if (!provider) return new ModelOverlayError("invalid providers object");
  if (field === "oauth") return new ModelOverlayError("providers.*.oauth is not supported; use a built-in Hush login or an API key");
  if (!field || !Object.hasOwn(providerFields.properties as object, field)) return new ModelOverlayError("invalid field under providers.*");
  const model = (field === "models" || field === "modelOverrides") && entry && modelField &&
    Object.hasOwn(fields.properties as object, modelField) ? `.*.${modelField}` : "";
  return new ModelOverlayError(`invalid providers.*.${field}${model}`);
}

// Replace comments with whitespace, not with nothing (which could join tokens).
// Quoted strings and their escapes are copied byte for byte.
function withoutComments(source: string) {
  const chunks: string[] = [];
  let start = 0;
  for (let i = 0; i < source.length; i++) {
    if (source[i] === '"') {
      while (++i < source.length && source[i] !== '"') if (source[i] === "\\") i++;
      continue;
    }
    if (source[i] !== "/" || source[i + 1] !== "/" && source[i + 1] !== "*") continue;
    chunks.push(source.slice(start, i), " ");
    if (source[i + 1] === "/") {
      while (++i < source.length && source[i] !== "\n" && source[i] !== "\r") { /* advance */ }
      start = i--;
    } else {
      const end = source.indexOf("*/", i + 2);
      if (end < 0) throw new SyntaxError("unterminated comment");
      i = end + 1;
      start = i + 1;
    }
  }
  return chunks.length ? chunks.join("") + source.slice(start) : source;
}

function endpoint(value: string | undefined, field: string): string | undefined {
  if (value === undefined) return;
  const url = URL.parse(value);
  if (!url || url.protocol !== "https:" && url.protocol !== "http:" || url.username || url.password)
    throw new ModelOverlayError(`${field} must be an HTTP(S) URL without credentials`);
  return value;
}

function configuredValue(input: string, field: string) {
  if (!input.startsWith("!")) {
    const value = configValue(input);
    if (value === undefined || !/[\r\n\0]/.test(value)) return value;
  }
  throw new ModelOverlayError(`${field} must resolve to a single-line value without shell commands`);
}

function headers(input: Record<string, string> | undefined, field: string) {
  if (!input) return;
  return Object.fromEntries(Object.entries(input).flatMap(([name, input]) => {
    if (!/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(name)) throw new ModelOverlayError(`invalid ${field} name`);
    const value = configuredValue(input, field);
    return value === undefined ? [] : [[name, value]];
  }));
}

function merge<T extends object>(...layers: (T | undefined)[]): T | undefined {
  return layers.reduce<T | undefined>((base, next) => next === undefined ? base : { ...base, ...next }, undefined);
}

function inputLimits(...layers: Model<Api>["inputLimits"][]): Model<Api>["inputLimits"] {
  const merged = merge(...layers);
  const images = merge(...layers.map(layer => layer?.images));
  if (merged && images) merged.images = { ...images, resize: merge(...layers.map(layer => layer?.images?.resize)) };
  return merged;
}

function compat(...layers: (ProviderCompat | undefined)[]): ProviderCompat | undefined {
  const merged = merge(...layers);
  if (!merged) return;
  for (const key of ["openRouterRouting", "vercelGatewayRouting", "chatTemplateKwargs", "chatTemplateArgs"] as const) {
    const field = merge(...layers.map(layer => layer?.[key]));
    if (field) Object.assign(merged, { [key]: field });
  }
  return merged;
}

function samplingByLevel(...layers: (SamplingParamsByThinkingLevel | undefined)[]): SamplingParamsByThinkingLevel | undefined {
  if (!layers.some(layer => layer !== undefined)) return;
  const merged: SamplingParamsByThinkingLevel = {};
  for (const level of levels) {
    const params = merge(...layers.map(layer => layer?.[level]));
    if (params) merged[level] = params;
  }
  return merged;
}

export async function loadModelOverlays(models: MutableModels, path: string) {
  const content = await readProfileText(path, 1024 * 1024).catch(() => {
    throw new ModelOverlayError("expected a private UTF-8 file (maximum 1 MiB)");
  });
  if (content === undefined) return;
  let input: unknown;
  try { input = parseRecord(Buffer.from(withoutComments(content))); }
  catch { throw new ModelOverlayError("malformed JSON"); }
  const result = (validator ??= compile(schema)).validate(input);
  if (!result.ok) throw invalidField(result.errors[0]?.path ?? "");
  const providers: Provider[] = [];
  for (const [id, config] of Object.entries(result.value.providers ?? {})) {
    const builtin = models.getProvider(id);
    const configuredBaseUrl = endpoint(config.baseUrl, "providers.*.baseUrl");
    const baseUrl = configuredBaseUrl ?? builtin?.baseUrl;
    const configuredHeaders = headers(config.headers, "providers.*.headers");
    const configuredKey = config.apiKey === undefined ? undefined : configuredValue(config.apiKey, "providers.*.apiKey");
    const apiKey = builtin?.auth.apiKey ?? envApiKeyAuth(`${id} API key`, []);
    const auth: Provider["auth"] = { ...builtin?.auth, apiKey: {
      ...apiKey,
      async resolve(options) {
        const result = await apiKey.resolve({ ...options, credential: configuredKey && !options.credential?.key ?
          { ...options.credential, type: "api_key", key: configuredKey } : options.credential });
        if (!config.authHeader || !result?.auth.apiKey) return result;
        return { ...result, auth: { ...result.auth,
          headers: { ...result.auth.headers, Authorization: `Bearer ${result.auth.apiKey}` } } };
      },
      check: configuredKey ? async () => ({ type: "api_key" }) : apiKey.check,
    } };
    const name = config.name ?? builtin?.name ?? id;
    const mergedHeaders = { ...builtin?.headers, ...configuredHeaders };
    const builtinApis = new Set(builtin?.getModels().map(model => model.api));
    const custom = new Map((config.models ?? []).map(model => {
      if (!model.id) throw new ModelOverlayError("providers.*.models.*.id is required");
      return [model.id, model];
    }));
    if (custom.size !== (config.models?.length ?? 0)) throw new ModelOverlayError("duplicate providers.*.models.*.id");
    // Resolve config once. Dynamic built-in catalogs can still change after
    // loading, but reading them must not reparse a user's overlay fields.
    const modelFields = <T extends { baseUrl?: string; headers?: Record<string, string> }>(model: T, kind: string) => ({ ...model,
      baseUrl: endpoint(model.baseUrl, `providers.*.${kind}.*.baseUrl`),
      headers: headers(model.headers, `providers.*.${kind}.*.headers`),
    });
    for (const [id, model] of custom) custom.set(id, modelFields(model, "models"));
    const overrides = new Map(Object.entries(config.modelOverrides ?? {}).map(([id, model]) => [id, modelFields(model, "modelOverrides")]));
    const getModels = (): Model<Api>[] => {
      const catalog = new Map(builtin?.getModels().map(model => [model.id, model]));
      for (const modelId of new Set([...catalog.keys(), ...custom.keys()])) {
        const original = catalog.get(modelId);
        const override = overrides.get(modelId);
        const added = custom.get(modelId);
        if (!added && !override && !configuredBaseUrl && !configuredHeaders && !config.compat) continue;
        const model = { ...original, ...added, ...override };
        const api = override?.api ?? (added ? added.api ?? config.api : undefined) ?? original?.api;
        const url = override?.baseUrl ?? added?.baseUrl ?? configuredBaseUrl ?? original?.baseUrl ?? baseUrl;
        if (!api || !apis.has(api) && !builtinApis.has(api)) {
          throw new ModelOverlayError("set a supported api in providers.*.models or modelOverrides");
        }
        if (!url) throw new ModelOverlayError("set baseUrl for custom models");
        const modelHeaders = { ...mergedHeaders, ...original?.headers, ...added?.headers, ...override?.headers };
        catalog.set(modelId, { ...model, id: modelId, name: model.name ?? modelId, provider: id, api, baseUrl: url,
          reasoning: model.reasoning ?? false, input: model.input ?? ["text"],
          contextWindow: model.contextWindow ?? 128_000, maxTokens: model.maxTokens ?? 16_384,
          cost: { ...free, ...original?.cost, ...added?.cost, ...override?.cost },
          thinkingLevelMap: merge(original?.thinkingLevelMap, added?.thinkingLevelMap, override?.thinkingLevelMap),
          inputLimits: inputLimits(original?.inputLimits, added?.inputLimits, override?.inputLimits),
          promptCache: merge(original?.promptCache, added?.promptCache, override?.promptCache),
          samplingParams: merge(original?.samplingParams, added?.samplingParams, override?.samplingParams),
          samplingParamsByThinkingLevel: samplingByLevel(original?.samplingParamsByThinkingLevel,
            added?.samplingParamsByThinkingLevel, override?.samplingParamsByThinkingLevel),
          compat: compat(original?.compat, config.compat, added?.compat, override?.compat),
          headers: Object.keys(modelHeaders).length ? modelHeaders : undefined,
        } as Model<Api>);
      }
      return [...catalog.values()];
    };
    const snapshot = getModels(); // validate before publishing the provider
    const customApis = Object.fromEntries([...apis].map(([api, factory]) => [api, factory()]));
    if (!builtin) {
      if (snapshot.length) providers.push(createProvider({ id, name, baseUrl, headers: mergedHeaders, auth, models: snapshot, api: customApis }));
      continue;
    }
    const fallback = createProvider({ id, baseUrl, auth, models: [], api: customApis });
    providers.push({ ...builtin, name, baseUrl, headers: mergedHeaders, auth, getModels,
      getAllModels: () => [...getModels(), ...(builtin.getAllModels?.() ?? []).filter(m => getModelType(m) !== "chat")],
      stream: (model, ctx, options) => (builtinApis.has(model.api) ? builtin : fallback).stream(model, ctx, options),
      streamSimple: (model, ctx, options) => (builtinApis.has(model.api) ? builtin : fallback).streamSimple(model, ctx, options),
    });
  }
  // A malformed overlay never leaves part of the file applied.
  for (const provider of providers) models.setProvider(provider);
}
