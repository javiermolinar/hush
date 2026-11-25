import { CodemodeSandbox } from "@earendil-works/pi-codemode";
import { planScriptLimit } from "../../server/src/plan.js";
import { PlanFileError } from "./plan-files.js";
import type { WasmAssets } from "./wasm-assets.js";

declare const HUSH_CODEMODE_WORKER: string | undefined;

export type PlanAcceptance = ReturnType<typeof planAcceptance>;

// A guest gets a bounded copy of declared file text and no host calls. Store
// writes and output never become evidence; only an exact boolean does.
export function planAcceptance(assets: WasmAssets | undefined) {
  let wasm: Promise<WebAssembly.Module> | undefined;
  let active = 0;
  const sandbox = new CodemodeSandbox({
    get wasm() { return assets && (wasm ??= assets.read("quickjs").then(bytes => WebAssembly.compile(bytes))); },
    workerUrl: typeof HUSH_CODEMODE_WORKER === "undefined" ? undefined : new URL(HUSH_CODEMODE_WORKER),
    memoryLimitBytes: 16 * 1024 * 1024,
    acquireWorker() {
      if (active >= 2) throw new Error("Acceptance worker is still settling");
      active++;
      return () => { active--; };
    },
  });
  return {
    async check(script: string, files: ReadonlyMap<string, string>, signal: AbortSignal) {
      if (!script.trim() || Buffer.byteLength(script) > planScriptLimit) throw new PlanFileError("Invalid acceptance script.");
      signal.throwIfAborted();
      // File data is marshaled separately, never interpolated into guest code.
      // Both snapshots run in fresh VMs with the same fixed clock, random
      // seed and empty bridge. Do not serialize arbitrary return objects.
      const run = sandbox.start(`const files = Object.freeze(Object.assign(Object.create(null), load("files")));
const predicate = Object.getPrototypeOf(async function() {}).constructor("files", ${JSON.stringify(script)});
const verdict = await predicate(files);
return typeof verdict === "boolean" ? verdict : null;`, {
        startedAt: 0, seed: 0, timeoutMs: 1500, signal, tools: [], globals: [],
        store: { files: Object.fromEntries(files) },
        onCall() { throw new Error("Acceptance cannot call host functions"); },
      });
      const result = await run.result;
      signal.throwIfAborted();
      if (!result.ok) {
        throw new PlanFileError(result.error.kind === "timeout"
          ? "Acceptance script timed out. No files were published."
          : "Isolated acceptance could not run. No files were published.");
      }
      // The sandbox already caps guest output and store size. Neither is
      // used as evidence, and a check must not emit any output or host calls.
      if (result.output.length || result.calls.length) throw new PlanFileError("Acceptance script emitted output or attempted a host call. No files were published.");
      if (typeof result.value !== "boolean") throw new PlanFileError("Acceptance script did not return a boolean. No files were published.");
      return result.value;
    },
    close: () => sandbox.close(),
  };
}
