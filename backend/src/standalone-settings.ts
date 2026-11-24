import { constants } from "node:fs";
import { open } from "node:fs/promises";
import { join } from "node:path";
import type { ModelThinkingLevel } from "@earendil-works/pi-ai";
import { getSupportedThinkingLevels } from "@earendil-works/pi-ai/models";
import { publicCatalogText } from "../../server/src/model-catalog.js";
import type { ModelRuntime } from "@earendil-works/pi-coding-agent";
import { BackendError } from "./config.js";
import { hushDefaultModel, privateFile } from "./profile.js";

const levels = new Set(["off", "minimal", "low", "medium", "high", "xhigh", "max"]);

// Pi-compatible global keys. Standalone never reads project .pi settings or
// writes Pi's settings file; existing threads retain their committed model.
export async function standaloneModelDefault(agentDir: string, runtime: ModelRuntime) {
  const path = join(agentDir, "settings.json");
  if (!await privateFile(path)) return { model: hushDefaultModel, thinkingLevel: "off" as ModelThinkingLevel };
  let value: unknown;
  try {
    const file = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW);
    try {
      const info = await file.stat();
      if (!info.isFile() || info.nlink !== 1 || info.uid !== process.getuid?.() || (info.mode & 0o077) !== 0) {
        throw new BackendError("configuration");
      }
      value = JSON.parse(await file.readFile("utf8"));
    } finally { await file.close(); }
  } catch { throw new BackendError("configuration"); }
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new BackendError("configuration");
  const settings = value as Record<string, unknown>;
  const provider = settings.defaultProvider, modelId = settings.defaultModel;
  const thinking = settings.defaultThinkingLevel;
  if ((provider !== undefined && !publicCatalogText(provider)) ||
      (modelId !== undefined && !publicCatalogText(modelId)) ||
      (thinking !== undefined && (typeof thinking !== "string" || !levels.has(thinking)))) {
    throw new BackendError("configuration");
  }
  if ((provider === undefined) !== (modelId === undefined)) throw new BackendError("configuration");
  const model = provider === undefined ? hushDefaultModel : { provider: provider as string, modelId: modelId as string };
  const physical = runtime.getPhysicalModel(model.provider, model.modelId) && runtime.getModel(model.provider, model.modelId);
  if (!physical || thinking !== undefined &&
      !getSupportedThinkingLevels(physical).includes(thinking as ModelThinkingLevel)) {
    throw new BackendError("configuration");
  }
  return { model, thinkingLevel: (thinking ?? "off") as ModelThinkingLevel };
}
