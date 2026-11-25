import { inspectWorkspaceText, workspaceFileError } from "./workspace-file.js";
import { LocalEnvironment } from "./resources.js";
import { CodemodeSandbox, type CodemodeRunOptions } from "@earendil-works/pi-codemode";
import { parseCodemodeSource } from "@earendil-works/pi-codemode/source";
import { defineTool, type Agent, type Extension, type ToolRegistration } from "@earendil-works/pi-durable";
import { CODEMODE_TOOL_NAME, createCodemodeExtension, type CodemodeToolInput } from "@earendil-works/pi-durable/codemode";

import type { WasmAssets } from "./wasm-assets.js";

declare const HUSH_CODEMODE_WORKER: string | undefined;

const maxScriptBytes = 32 * 1024;
const maxOutputTokens = 1500; // Pi's estimate, backed by the byte limit below.
const maxTimeMs = 10_000;
const maxCalls = 16;
const maxWorkers = 4;

const inspectFile = defineTool({
  name: "inspect_file",
  description: "Read a workspace-relative UTF-8 file (up to 32 KiB, no links).",
  callers: ["tools"],
  replay: "safe",
  parameters: {
    type: "object", properties: { path: { type: "string", maxLength: 1024 } }, required: ["path"], additionalProperties: false,
  },
  structuredOutputSchema: { type: "string" },
  async execute(args, api, ctx) {
    try {
      const { path } = args as { path: string };
      // General tool trust resolves aliases, but Codemode still requires a
      // physical cwd as well as a no-link path within it.
      const env = api.env;
      if (!(env instanceof LocalEnvironment) || env.requestedCwd !== env.cwd) throw new Error(workspaceFileError);
      const text = await inspectWorkspaceText(env.cwd, path, ctx.abortSignal);
      return { output: [], structuredOutput: text };
    } catch {
      ctx.abortSignal?.throwIfAborted();
      // Do not expose host paths or platform filesystem error messages.
      return { output: [{ type: "text", text: workspaceFileError }], isError: true };
    }
  },
});

const discovery = new Set(["searchTools", "describeTool", "describeNamespace"]);
class WorkspaceSandbox extends CodemodeSandbox {
  override start(code: string, options: CodemodeRunOptions) {
    const run = super.start(code, { ...options, onCall(call) {
      // Enforce the capability at the bridge, including during replay: the
      // persisted tool discovery list from an earlier attempt is not authority.
      if (call.target === "tool" ? call.name !== inspectFile.name : !discovery.has(call.name))
        throw new Error("Call is not allowed in Hush Codemode");
      if (call.id > maxCalls) throw new Error(`Codemode permits at most ${maxCalls} calls per script`);
      if (Buffer.byteLength(JSON.stringify(call.args) ?? "") > 4096) throw new Error("Codemode call arguments are too large");
      options.onCall(call);
    } });
    return {
      get finished() { return run.finished; },
      deliver: run.deliver,
      // Drop synthetic image output before Durable persists or renders it.
      result: run.result.then(result => ({ ...result, output: result.output.filter(item => item.type === "text") })),
    };
  }
}

function scriptAgent(agent: Agent): Agent {
  return { ...agent, callable: agent.callable.filter(tool => tool.name === inspectFile.name), extensions: [] };
}

export function workspaceCodemode(assets: WasmAssets | undefined) {
  let wasm: Promise<WebAssembly.Module> | undefined;
  let workers = 0;
  const sandbox = new WorkspaceSandbox({
    // The worker launcher accesses this getter on the first execution, not
    // when Durable registers the tool. Deduplicate concurrent first calls.
    get wasm() { return assets && (wasm ??= assets.read("quickjs").then(bytes => WebAssembly.compile(bytes))); },
    workerUrl: typeof HUSH_CODEMODE_WORKER === "undefined" ? undefined : new URL(HUSH_CODEMODE_WORKER),
    memoryLimitBytes: 32 * 1024 * 1024,
    acquireWorker() {
      if (workers >= maxWorkers) throw new Error("Codemode worker limit reached");
      workers++;
      return () => { workers--; };
    },
  });
  const upstream = createCodemodeExtension({ sandbox, models: false });
  const tool = upstream.tools?.find(tool => tool.name === CODEMODE_TOOL_NAME);
  if (!tool) throw new Error("Durable did not register Codemode");
  const boundedCodemode: ToolRegistration = {
    ...tool,
    description: "Raw async JavaScript body, no fences. Batch reads with Promise.all and return excerpts. " +
      `Only tools.inspect_file({ path }) is callable (max ${maxCalls} calls). Lower limits with a first line ` +
      `// @options: {\"max_output_tokens\": ${maxOutputTokens}, \"timeout_ms\": ${maxTimeMs}}.`,
    outputLimits: { maxBytes: 16 * 1024, maxLines: 500 },
    async execute(args, api, ctx) {
      const { code: source } = args as CodemodeToolInput;
      if (Buffer.byteLength(source) > maxScriptBytes)
        return { output: [{ type: "text", text: "Codemode source exceeds 32 KiB" }], isError: true };
      const { code, options } = parseCodemodeSource(source);
      const limits = {
        timeout_ms: Math.min(options.timeoutMs ?? maxTimeMs, maxTimeMs),
        max_output_tokens: Math.min(options.maxOutputTokens ?? maxOutputTokens, maxOutputTokens),
      };
      // Supply a deadline even if the model omits one; Durable saves it in
      // the replay journal. Hide env from Codemode itself so it cannot spill
      // output to disk. Nested tools get their own unmodified execution API.
      return tool.execute({ code: `// @options: ${JSON.stringify(limits)}\n${code}` }, {
        ...api, env: undefined,
        agent: async context => scriptAgent(await api.agent(context)),
      }, ctx);
    },
  };
  const extension: Extension = {
    ...upstream,
    tools: [boundedCodemode, inspectFile],
    sections: upstream.sections?.map(section => ({ ...section, render: (input, ctx) =>
      section.render({ ...input, agent: scriptAgent(input.agent) }, ctx),
    })),
  };
  return { sandbox, extension, toolName: CODEMODE_TOOL_NAME };
}
