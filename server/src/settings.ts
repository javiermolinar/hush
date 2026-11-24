import { object, WireError } from "./wire.js";
import { publicCatalogText, thinkingLevels, type ModelRef } from "./model-catalog.js";

export const settingsVersion = 1;
export const reasoningModes = ["folded", "expanded"] as const;
export type ReasoningMode = typeof reasoningModes[number];
export type ThinkingLevel = typeof thinkingLevels[number];
export type SettingsView = { model: ModelRef; thinkingLevel: ThinkingLevel; reasoning: ReasoningMode };
export type SettingsChange = { model?: ModelRef; thinkingLevel?: ThinkingLevel; reasoning?: ReasoningMode };

export interface SettingsHost {
  get(): Promise<SettingsView>;
  set(change: SettingsChange): Promise<SettingsView>;
}

const exact = (v: Record<string, unknown>, keys: string[]) =>
  Object.keys(v).length === keys.length && keys.every(k => Object.hasOwn(v, k));
const ref = (v: unknown): v is ModelRef => object(v) && exact(v, ["provider", "modelId"]) &&
  publicCatalogText(v.provider) && publicCatalogText(v.modelId);
const thinking = (v: unknown): v is ThinkingLevel => thinkingLevels.some(level => level === v);
const reasoning = (v: unknown): v is ReasoningMode => reasoningModes.some(mode => mode === v);

export function parseSettingsChange(v: unknown): SettingsChange {
  if (!object(v)) throw new WireError("invalid_request");
  const keys = Object.keys(v);
  if (keys.length === 0 || keys.some(key => key !== "model" && key !== "thinkingLevel" && key !== "reasoning") ||
      Object.hasOwn(v, "model") && !ref(v.model) ||
      Object.hasOwn(v, "thinkingLevel") && !thinking(v.thinkingLevel) ||
      Object.hasOwn(v, "reasoning") && !reasoning(v.reasoning)) {
    throw new WireError("invalid_request");
  }
  return v as SettingsChange;
}

export function checkSettings(v: unknown): asserts v is SettingsView {
  if (!object(v) || !exact(v, ["model", "thinkingLevel", "reasoning"]) || !ref(v.model) ||
      !thinking(v.thinkingLevel) || !reasoning(v.reasoning)) {
    throw new WireError("internal");
  }
}
