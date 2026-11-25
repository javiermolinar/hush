import type { CredentialStore } from "@earendil-works/pi-ai";
import { builtinModels } from "@earendil-works/pi-ai/providers/all";
import { loadModelOverlays } from "./model-overlays.js";

export async function createHushModels(credentials: CredentialStore, modelsPath?: string) {
  const runtime = builtinModels({ credentials });
  if (modelsPath) await loadModelOverlays(runtime, modelsPath);
  return runtime;
}
