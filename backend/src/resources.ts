import { realpath } from "node:fs/promises";
import { basename, dirname, join, resolve } from "node:path";
import type { Context } from "@earendil-works/chord/context";
import { NodeExecutionEnv } from "@earendil-works/pi-durable/env/node";
import type { ShellExecOptions } from "@earendil-works/pi-durable/env";
import { defineExtension, section, type Harness, type Extension, type PromptSection } from "@earendil-works/pi-durable";
import type { InstructionSource } from "../../server/src/loaded-context.js";
import { environmentPrompt } from "./client-environment.js";
import { readScopedText } from "./workspace-file.js";
import { workspaceAncestors } from "./workspace-ancestors.js";
import type { StandaloneSkills } from "./workspace-skills.js";

export type Warn = (code: "harness_report" | "closing" | "listening") => void;
const safeEnv = Object.fromEntries(Object.entries(process.env).filter(([k, v]) =>
  v !== undefined && /^(PATH|HOME|LANG|LC_[A-Z_]+|TMPDIR|TERM|USER|LOGNAME|SHELL)$/.test(k))) as Record<string, string>;

// Inlined by backend/bundle.mjs; no runtime prompt asset is needed.
declare const HUSH_SYSTEM_PROMPT: string;
const standaloneKeys = ["preamble", "cwd", "addendum", "project_context", "skills"] as const;
const promptFileLimit = 64 * 1024;
const workspaceLimit = 256 * 1024;

type Instruction = InstructionSource & { text: string; length: number; legacy: boolean };

async function readSupplement(dir: string, name: string, kind: InstructionSource["kind"], limit: number, signal?: AbortSignal): Promise<Instruction | undefined> {
  const path = join(dir, name);
  if (Buffer.byteLength(path) > 4096 || !path.isWellFormed() || /[\x00-\x1f\x7f-\x9f]/.test(path)) return;
  signal?.throwIfAborted();
  try {
    const { text, length } = await readScopedText(dir, name, limit, signal);
    // The v1 context inspector only accepts these legacy locations.
    const legacy = name === (kind === "personal" ? "APPEND_SYSTEM.md" : "AGENTS.md");
    if (text.trim()) return { kind, path, text: text.trim(), length, legacy };
  } catch { signal?.throwIfAborted(); /* Missing, unsafe, or invalid text. */ }
}

async function personalContext(profileDir: string, signal?: AbortSignal) {
  const profile = await realpath(profileDir).catch(() => undefined);
  if (!profile) return [];
  const files = await Promise.all([
    readSupplement(profile, "APPEND_SYSTEM.md", "personal", promptFileLimit, signal),
    readSupplement(profile, "AGENTS.md", "personal", promptFileLimit, signal),
    readSupplement(dirname(profile), ".agents/AGENTS.md", "personal", promptFileLimit, signal),
  ]);
  return files.filter(file => file !== undefined);
}

// Both generation and the inspector project this same ordered resolution. Do
// not read cached prompt bytes, Pi resources or private provider configuration.
async function standaloneInstructions(profileDir: string, cwd: string, includeWorkspace: boolean, signal?: AbortSignal) {
  const [files, parents] = await Promise.all([
    personalContext(profileDir, signal),
    includeWorkspace ? workspaceAncestors(cwd, signal) : [],
  ]);
  const seen = new Set(files.map(file => file.path));
  let remaining = workspaceLimit;
  for (const dir of parents.reverse()) {
    for (const name of ["AGENTS.md", ".agents/AGENTS.md"]) {
      if (!remaining) return files;
      if (seen.has(join(dir, name))) continue;
      const file = await readSupplement(dir, name, "workspace", Math.min(promptFileLimit, remaining), signal);
      if (!file) continue;
      remaining -= file.length;
      seen.add(file.path);
      files.push(file);
    }
  }
  return files;
}

function renderInstruction({ path, text }: Instruction) {
  return `### ${JSON.stringify(path)}\n\n${text}`;
}

async function standalonePromptSections(profileDir: string, cwd: string, workspace: string | undefined, signal?: AbortSignal,
    catalog?: Pick<StandaloneSkills, "promptCatalog">): Promise<Record<string, string>> {
  const [sources, skills] = await Promise.all([
    standaloneInstructions(profileDir, workspace ?? cwd, workspace !== undefined, signal),
    catalog?.promptCatalog(workspace ?? cwd, workspace !== undefined, signal),
  ]);
  const personal = sources.filter(s => s.kind === "personal").map(s => basename(s.path) === "APPEND_SYSTEM.md" ? s.text : renderInstruction(s));
  const files = sources.filter(s => s.kind === "workspace").map(renderInstruction);
  return {
    preamble: HUSH_SYSTEM_PROMPT.trimEnd(),
    cwd: `Working directory: ${JSON.stringify(cwd)}`,
    ...(personal.length && { addendum: `## Personal instructions\n\n${personal.join("\n\n")}` }),
    ...(files.length && { project_context: `## Workspace context\n\n${files.join("\n\n")}` }),
    ...(skills && { skills }),
  };
}

export class LocalEnvironment extends NodeExecutionEnv {
  constructor(cwd: string, readonly requestedCwd = cwd) { super({ cwd }); }

  override exec(command: string | readonly string[], options: ShellExecOptions | undefined, ctx: Context) {
    return super.exec(command, { ...options, inheritEnv: false, env: { ...safeEnv, ...options?.env } }, ctx);
  }
}

export class StandaloneResources {
  constructor(private readonly cwd: string, private readonly agentDir: string,
      private readonly catalog?: Pick<StandaloneSkills, "promptCatalog">) {}

  directory(cwd?: string) { return resolve(this.cwd, cwd ?? "."); }

  async instructionSources(cwd: string, includeWorkspace: boolean, includeExtended: boolean, signal: AbortSignal): Promise<InstructionSource[]> {
    const sources = await standaloneInstructions(this.agentDir, cwd, includeWorkspace, signal);
    signal.throwIfAborted();
    return sources.filter(source => includeExtended || source.legacy).map(({ kind, path }) => ({ kind, path }));
  }

  async promptSections(cwd: string,
      includeWorkspace = true, signal?: AbortSignal, workspaceAuthorized = false) {
    const physical = includeWorkspace && !workspaceAuthorized ? await realpath(cwd).catch(() => undefined) : cwd;
    return standalonePromptSections(this.agentDir, cwd, includeWorkspace ? physical : undefined, signal, this.catalog);
  }

  extension(getHarness: () => Harness | undefined, requireWorkspaceEnv = false) {
    type Input = Parameters<PromptSection["render"]>[0];
    const prepared = new WeakMap<Input, Promise<Record<string, string>>>();
    const render = (input: Input, ctx: Context) => {
      const cwd = requireWorkspaceEnv && input.env?.cwd || this.directory(input.agent.cwd);
      // Generation can continue without an env after revocation, but must not
      // read fresh workspace instructions on that path.
      return this.promptSections(cwd, !requireWorkspaceEnv || !!input.env, ctx.abortSignal, requireWorkspaceEnv);
    };
    return defineExtension({
      name: "hush-standalone-prompt",
      hooks: [environmentPrompt(getHarness)],
      sections: standaloneKeys.map(key => section(key, async (input, ctx) => {
        let sections = prepared.get(input);
        if (!sections) prepared.set(input, sections = render(input, ctx));
        return (await sections)[key];
      }, { tag: false })),
    });
  }
}
