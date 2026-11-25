import { open, realpath } from "node:fs/promises";
import { constants } from "node:fs";
import { basename, dirname, join, resolve } from "node:path";
import { BACKGROUND_CONTEXT as context, type Context } from "@earendil-works/chord/context";
import { ModelRegistry, DefaultResourceLoader, ProjectTrustStore, SettingsManager, stripFrontmatter } from "@earendil-works/pi-coding-agent";
import { NodeExecutionEnv } from "@earendil-works/pi-durable/env/node";
import type { FileWatcher, ShellExecOptions, WatchTarget } from "@earendil-works/pi-durable/env";
import { defineExtension, section, type Extension, type Harness, type Registry, type ToolRegistration, type PromptSection } from "@earendil-works/pi-durable";
import { environmentPrompt } from "./client-environment.js";
import { buildSystemPromptSections, createCodingToolDefinitions, expandPromptTemplate } from "./pi-source.js";
import { McpScopes } from "./mcp.js";
import { ProfileDoc } from "./pi/projection.js";
import { Rejected } from "./pi/commands.js";
import { readScopedText } from "./workspace-file.js";
import type { InstructionSource } from "../../server/src/loaded-context.js";
import { workspaceAncestors } from "./workspace-ancestors.js";
import type { StandaloneSkills } from "./workspace-skills.js";

export type Warn = (code: "resource_watch_failed" | "mcp_unavailable" | "harness_report" | "closing" | "listening") => void;
const keys = ["preamble", "tools", "rules", "docs", "addendum", "project_context", "skills", "cwd"] as const;
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
  constructor(cwd: string, shellPath?: string, readonly requestedCwd = cwd) { super({ cwd, shellPath }); }

  override exec(command: string | readonly string[], options: ShellExecOptions | undefined, ctx: typeof context) {
    return super.exec(command, { ...options, inheritEnv: false, env: { ...safeEnv, ...options?.env } }, ctx);
  }
}

function watchTargets(cwd: string, agentDir: string): WatchTarget[] {
  function paths(root: string): WatchTarget[] {
    return [...["SYSTEM.md", "APPEND_SYSTEM.md", "settings.json", "mcp.json"].map(path => ({ path: join(root, path) })),
      ...["skills", "prompts"].map(path => ({ path: join(root, path), recursive: true }))];
  }
  const targets: WatchTarget[] = [
    ...paths(agentDir), ...paths(join(cwd, ".pi")), { path: join(agentDir, "trust.json") },
    { path: join(agentDir, "AGENTS.md") }, { path: join(agentDir, "CLAUDE.md") },
  ];
  for (let dir = cwd;; dir = dirname(dir)) {
    targets.push({ path: join(dir, "AGENTS.md") }, { path: join(dir, "CLAUDE.md") },
      { path: join(dir, ".agents", "skills"), recursive: true });
    if (dirname(dir) === dir) break;
  }
  return targets;
}

type Loaded = {
  trusted: boolean;
  settings: SettingsManager;
  loader?: DefaultResourceLoader;
  pref: { snippet: Record<string, string>; rules: Record<string, string[]> };
  prompt: Map<string, Record<string, string>>;
  mcp?: Extension;
};
type Cache = {
  cwd: string;
  watcher?: FileWatcher;
  generation: number;
  loaded?: { generation: number; value: Loaded };
  metadata?: { generation: number; value: Loaded };
  inFlight?: Promise<Loaded>;
  reading?: Promise<Loaded>;
  watching?: Promise<void>;
};

export class ProjectResources {
  readonly #trust?: ProjectTrustStore;
  readonly #standalone?: Loaded;
  readonly #scopes: McpScopes | undefined;
  readonly #cache = new Map<string, Cache>();
  readonly #changed = new Set<() => void>();
  #closed = false;

  constructor(private readonly cwd: string, private readonly agentDir: string, registry: Registry,
    models: ModelRegistry, private readonly warn: Warn, ephemeral: boolean, tapTool?: (tool: ToolRegistration) => ToolRegistration,
    standalone = false, private readonly catalog?: Pick<StandaloneSkills, "promptCatalog">) {
    if (standalone) {
      this.#standalone = { trusted: false, settings: SettingsManager.inMemory(), pref: { snippet: {}, rules: {} }, prompt: new Map() };
    } else {
      this.#trust = new ProjectTrustStore(agentDir);
      if (!ephemeral) this.#scopes = new McpScopes(registry, agentDir, models, cwd => this.trusted(cwd), warn, tapTool);
    }
  }

  trusted(cwd: string) {
    if (!this.#trust) return false;
    const explicit = this.#trust.get(cwd);
    if (explicit !== null) return explicit;
    return SettingsManager.create(cwd, this.agentDir, { projectTrusted: false }).getDefaultProjectTrust() === "always";
  }

  directory(cwd?: string) { return resolve(this.cwd, cwd ?? "."); }

  onChange(listener: () => void) {
    this.#changed.add(listener);
    return () => this.#changed.delete(listener);
  }

  #directoryCache(cwd: string) {
    let cache = this.#cache.get(cwd);
    if (this.#closed) throw new Error("Resource loader is closed");
    if (!cache) {
      if (this.#cache.size >= 256) throw new Error("Resource directory limit");
      this.#cache.set(cwd, cache = { cwd, generation: 0 });
    }
    return cache;
  }

  async load(input?: string): Promise<Loaded> {
    if (this.#standalone) return this.#standalone;
    const cwd = this.directory(input);
    const trusted = this.trusted(cwd);
    const cache = this.#directoryCache(cwd);
    if (cache.inFlight) {
      const loaded = await cache.inFlight;
      return this.trusted(cwd) === loaded.trusted ? loaded : this.load(cwd);
    }
    if (cache.loaded?.generation === cache.generation && cache.loaded.value.trusted === trusted && cache.watcher) {
      return cache.loaded.value;
    }
    const current = cache;
    cache.inFlight = this.#load(current, trusted);
    try {
      const value = await cache.inFlight;
      if (this.trusted(cwd) === value.trusted) return value;
    } finally { current.inFlight = undefined; }
    return this.load(cwd);
  }

  async #watch(cache: Cache) {
    if (cache.watcher) return;
    if (cache.watching) return cache.watching;
    const cwd = cache.cwd;
    cache.watching = (async () => {
      const result = await new NodeExecutionEnv({ cwd }).watch(watchTargets(cwd, this.agentDir), change => {
        cache.generation++;
        if (change.kind === "failed") {
          cache.watcher = undefined;
          this.warn("resource_watch_failed");
        }
        if (cache.loaded?.value.trusted && !this.trusted(cwd)) void this.#scopes?.remove(cwd);
        for (const notify of this.#changed) notify();
      }, context);
      if (result.ok) cache.watcher = result.value;
      else this.warn("resource_watch_failed");
    })();
    try { await cache.watching; } finally { cache.watching = undefined; }
  }

  // This uses exactly the request loader's settings/trust, but never opens an
  // MCP scope, installs an extension or changes a conversation.
  async read(input?: string): Promise<Loaded> {
    if (this.#standalone) return this.#standalone;
    const cwd = this.directory(input);
    const cache = this.#directoryCache(cwd);
    if (cache.reading) return cache.reading;
    cache.reading = (async () => {
      await this.#watch(cache);
      for (;;) {
        if (this.#closed) throw new Error("Resource loader is closed");
        const trusted = this.trusted(cwd), generation = cache.generation;
        for (const source of [cache.loaded, cache.metadata]) {
          if (cache.watcher && source?.generation === generation && source.value.trusted === trusted) return source.value;
        }
        const value = await this.#read(cwd, trusted);
        if (this.trusted(cwd) === trusted) {
          cache.metadata = { generation, value };
          return value;
        }
      }
    })();
    try { return await cache.reading; } finally { cache.reading = undefined; }
  }

  async #load(cache: Cache, trusted: boolean): Promise<Loaded> {
    const cwd = cache.cwd;
    await this.#watch(cache);
    const generation = cache.generation;
    const metadata = await this.#read(cwd, trusted);
    const mcp = this.#closed || trusted !== this.trusted(cwd) ? undefined : await this.#scopes?.load(cwd);
    const value = { ...metadata, mcp };
    cache.loaded = { generation, value };
    return value;
  }

  async #read(cwd: string, trusted: boolean): Promise<Loaded> {
    const settings = SettingsManager.create(cwd, this.agentDir, { projectTrusted: trusted });
    const loader = new DefaultResourceLoader({
      cwd, agentDir: this.agentDir, settingsManager: settings, noExtensions: true, noThemes: true,
      packageCommandOutput: "capture",
    });
    await loader.reload(); // delegates package/skill/prompt discovery and loadProjectContextFiles to Pi
    const defs = createCodingToolDefinitions(cwd);
    const pref = {
      snippet: Object.fromEntries(defs.flatMap(d => d.promptSnippet ? [[d.name, d.promptSnippet]] : [])),
      rules: Object.fromEntries(defs.flatMap(d => d.promptGuidelines ? [[d.name, [...d.promptGuidelines]]] : [])),
    };
    return { trusted, loader, settings, pref, prompt: new Map() };
  }

  expand(loaded: Loaded, text?: string) {
    return loaded.loader && text?.startsWith("/") ? expandPromptTemplate(text, loaded.loader.getPrompts().prompts) : text;
  }

  skillNames(loaded: Loaded) {
    if (!loaded.loader) return [];
    return [...new Set(loaded.loader.getSkills().skills.map(skill => skill.name).filter(name => name.trim()))];
  }

  commands(loaded: Loaded) {
    if (!loaded.loader) return [];
    return [
      ...loaded.loader.getPrompts().prompts.map(p => ({ name: p.name, description: p.description, source: "prompt" as const, sourceInfo: p.sourceInfo })),
      ...loaded.loader.getSkills().skills.map(s => ({ name: `skill:${s.name}`, description: s.description, source: "skill" as const, sourceInfo: s.sourceInfo })),
    ];
  }

  async expandSkill(loaded: Loaded, input: string, requireLoaded = false) {
    const match = /^\/skill:([^ ]+)(?: ([\s\S]*))?$/.exec(input);
    const skill = match && loaded.loader?.getSkills().skills.find(s => s.name === match[1]);
    if (requireLoaded && !skill) throw new Rejected("Skill is not loaded");
    if (skill) {
      try {
        const file = await open(skill.filePath, constants.O_RDONLY | constants.O_NONBLOCK);
        const size = 1024 * 1024;
        const data = Buffer.alloc(size + 1);
        let offset = 0;
        try {
          if (!(await file.stat()).isFile()) throw new Error("not a regular file");
          while (offset <= size) {
            const { bytesRead } = await file.read(data, offset, data.length - offset, offset);
            if (!bytesRead) break;
            offset += bytesRead;
          }
        } finally { await file.close(); }
        if (offset > size) throw new Error("skill exceeds limit");
        const body = stripFrontmatter(new TextDecoder("utf-8", { fatal: true }).decode(data.subarray(0, offset))).trim();
        const args = match[2]?.trim();
        input = `<skill name="${skill.name}" location="${skill.filePath}">\nReferences are relative to ${skill.baseDir}.\n\n${body}\n</skill>` +
          (args ? `\n\n${args}` : "");
      } catch { throw new Rejected("Could not read the selected skill"); }
    }
    return input;
  }

  async expandPi(loaded: Loaded, input: string) {
    return this.expand(loaded, await this.expandSkill(loaded, input)) ?? input;
  }

  async instructionSources(cwd: string, includeWorkspace: boolean, includeExtended: boolean, signal: AbortSignal): Promise<InstructionSource[]> {
    if (!this.#standalone) return [];
    const sources = await standaloneInstructions(this.agentDir, cwd, includeWorkspace, signal);
    signal.throwIfAborted();
    return sources.filter(source => includeExtended || source.legacy).map(({ kind, path }) => ({ kind, path }));
  }

  async promptSections(loaded: Loaded, cwd: string, selectedTools: string[],
      overrides?: { systemPrompt?: string; appendSystemPrompt?: string }, includeWorkspace = true, signal?: AbortSignal, workspaceAuthorized = false): Promise<Record<string, string>> {
    if (this.#standalone) {
      const physical = includeWorkspace && !workspaceAuthorized ? await realpath(cwd).catch(() => undefined) : cwd;
      return standalonePromptSections(this.agentDir, cwd, includeWorkspace ? physical : undefined, signal, this.catalog);
    }
    const loader = loaded.loader;
    if (!loader) return {};
    const hasOverrides = overrides?.systemPrompt !== undefined || overrides?.appendSystemPrompt !== undefined;
    const signature = JSON.stringify(selectedTools);
    let prompt = hasOverrides ? undefined : loaded.prompt.get(signature);
    if (!prompt) {
      prompt = buildSystemPromptSections({
        cwd, selectedTools, toolSnippets: loaded.pref.snippet, toolGuidelines: loaded.pref.rules,
        customPrompt: overrides?.systemPrompt ?? loader.getSystemPrompt(),
        appendSystemPrompt: [...loader.getAppendSystemPrompt(), overrides?.appendSystemPrompt].filter(Boolean).join("\n\n"),
        contextFiles: loader.getAgentsFiles().agentsFiles, skills: loader.getSkills().skills,
      });
      // Do not reuse invocation-specific prompt content across threads.
      if (!hasOverrides) {
        if (loaded.prompt.size >= 16) loaded.prompt.clear();
        loaded.prompt.set(signature, prompt);
      }
    }
    return prompt;
  }

  extension(getHarness: () => Harness | undefined, requireWorkspaceEnv = false) {
    type Input = Parameters<PromptSection["render"]>[0];
    const prepared = new WeakMap<Input, Promise<Record<string, string>>>();
    const render = async (input: Input, ctx: Context) => {
      const cwd = requireWorkspaceEnv && input.env?.cwd || this.directory(input.agent.cwd);
      const loaded = await this.load(cwd);
      const overrides = loaded.loader ? await input.read.snapshot(ProfileDoc, input.conversationId, ctx) : undefined;
      // Generation can continue without an env after revocation, but must not
      // read fresh workspace instructions on that path.
      return this.promptSections(loaded, cwd, input.agent.tools.map(t => t.name), overrides, !requireWorkspaceEnv || !!input.env, ctx.abortSignal, requireWorkspaceEnv);
    };
    return defineExtension({
      name: this.#standalone ? "hush-standalone-prompt" : "hush-pi-prompt",
      ...(this.#standalone && { hooks: [environmentPrompt(getHarness)] }),
      // Durable shares the render input across sections. Memoize only on that
      // input, never on cwd, thread, or the standalone resource object.
      sections: (this.#standalone ? standaloneKeys : keys).map(key => section(key, async (input, ctx) => {
        let sections = prepared.get(input);
        if (!sections) prepared.set(input, sections = render(input, ctx));
        return (await sections)[key];
      }, { tag: false })),
    });
  }

  async close() {
    this.#closed = true;
    await this.#scopes?.close();
    await Promise.allSettled([...this.#cache.values()].map(async c => {
      await Promise.allSettled([c.inFlight, c.reading]);
      await c.watcher?.stop(context);
    }));
    this.#cache.clear();
  }
}
