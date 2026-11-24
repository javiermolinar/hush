import { constants } from "node:fs";
import { lstat, open, readlink, type FileHandle } from "node:fs/promises";
import { isAbsolute, join } from "node:path";
import { CodemodeSandbox, type CodemodeRunOptions } from "@earendil-works/pi-codemode";
import { parseCodemodeSource } from "@earendil-works/pi-codemode/source";
import { defineTool, type Agent, type Extension, type ToolRegistration } from "@earendil-works/pi-durable";
import { CODEMODE_TOOL_NAME, createCodemodeExtension, type CodemodeToolInput } from "@earendil-works/pi-durable/codemode";

// Both assets are embedded by backend/bundle.mjs. The extracted backend file
// need not still exist when a later tool call starts a worker.
declare const HUSH_CODEMODE_WORKER: string;
declare const HUSH_QUICKJS_WASM: string;

const maxScriptBytes = 32 * 1024;
const maxFileBytes = 32 * 1024;
const maxOutputTokens = 1500; // Pi's estimate, backed by the byte limit below.
const maxTimeMs = 10_000;
const maxCalls = 16;
const maxWorkers = 4;
const readFlags = constants.O_RDONLY | constants.O_NONBLOCK;
// These flags are not exposed by Node. They are stable on the release targets.
const linuxOPath = 0o10000000; // Linux open(2): inspect an inode without opening it for I/O.
const darwinNoFollowAny = 0x20000000; // Darwin sys/fcntl.h: reject symlinks in *all* components.

const fileError = "Expected a readable, single-link regular UTF-8 file of at most 32 KiB inside the thread's working directory (no symlinks).";

async function openInspection(path: string, signal?: AbortSignal): Promise<FileHandle> {
  if (process.platform === "linux") {
    // Checking realpath and then opening by pathname would race a symlink swap
    // in a parent directory. Verify the kernel path of the actual inode, then
    // open that same inode for reading. Fail closed without /proc/self/fd.
    const ref = await open(path, linuxOPath | constants.O_NOFOLLOW);
    try {
      const info = await ref.stat();
      const fdPath = `/proc/self/fd/${ref.fd}`;
      if (!info.isFile() || info.nlink !== 1 || info.size > maxFileBytes || await readlink(fdPath) !== path)
        throw new Error(fileError);
      signal?.throwIfAborted();
      return await open(fdPath, readFlags);
    } finally { await ref.close(); }
  }
  if (process.platform === "darwin") {
    if (!(await lstat(path)).isFile()) throw new Error(fileError);
    signal?.throwIfAborted();
    return open(path, readFlags | darwinNoFollowAny);
  }
  throw new Error(fileError);
}

async function inspectText(cwd: string, path: string, signal?: AbortSignal) {
  // Require a physical workspace path too: resolving cwd with realpath here
  // would let a replaced workspace root redirect reads out of that workspace.
  if (!isAbsolute(cwd) || !path || Buffer.byteLength(path) > 1024 || isAbsolute(path) || path.includes("\0") ||
      path.includes("\\") || path.split("/").includes("..")) throw new Error(fileError);
  signal?.throwIfAborted();
  const file = await openInspection(join(cwd, path), signal);
  try {
    const info = await file.stat();
    if (!info.isFile() || info.nlink !== 1 || info.size > maxFileBytes) throw new Error(fileError);
    // Bound the read even if the file grows after stat(). Never read a FIFO.
    const data = Buffer.alloc(maxFileBytes + 1);
    let length = 0;
    while (length < data.length) {
      signal?.throwIfAborted();
      const { bytesRead } = await file.read(data, length, data.length - length, length);
      if (!bytesRead) break;
      length += bytesRead;
    }
    signal?.throwIfAborted();
    const bytes = data.subarray(0, length);
    if (length > maxFileBytes || bytes.includes(0)) throw new Error(fileError);
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } finally { await file.close(); }
}

const inspectFile = defineTool({
  name: "inspect_file",
  description: "Read one UTF-8 text file (up to 32 KiB, no links) relative to the thread's working directory. Returns its text.",
  callers: ["tools"],
  replay: "safe",
  parameters: {
    type: "object", properties: { path: { type: "string", maxLength: 1024 } }, required: ["path"], additionalProperties: false,
  },
  structuredOutputSchema: { type: "string" },
  async execute(args, api, ctx) {
    try {
      const { path } = args as { path: string };
      if (!api.env?.cwd) throw new Error(fileError);
      const text = await inspectText(api.env.cwd, path, ctx.abortSignal);
      return { output: [], structuredOutput: text };
    } catch {
      ctx.abortSignal?.throwIfAborted();
      // Do not expose host paths or platform filesystem error messages.
      return { output: [{ type: "text", text: fileError }], isError: true };
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

export async function workspaceCodemode(): Promise<{ sandbox: CodemodeSandbox; extension: Extension }> {
  const wasm = await WebAssembly.compile(Buffer.from(HUSH_QUICKJS_WASM, "base64"));
  let workers = 0;
  const sandbox = new WorkspaceSandbox({
    wasm, workerUrl: new URL(HUSH_CODEMODE_WORKER), memoryLimitBytes: 32 * 1024 * 1024,
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
    description: "Run raw JavaScript (an async function body, no code fence) in QuickJS to batch independent workspace reads. " +
      "Use Promise.all(paths.map(path => tools.inspect_file({ path }))), filter the results in the script, and return only the relevant text. " +
      `Direct tools are not callable from scripts. At most ${maxCalls} calls. A first line ` +
      `// @options: {\"max_output_tokens\": ${maxOutputTokens}, \"timeout_ms\": ${maxTimeMs}} can lower the host limits.`,
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
  return { sandbox, extension };
}
