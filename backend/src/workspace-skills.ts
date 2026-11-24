import { createHash } from "node:crypto";
import { opendir, realpath } from "node:fs/promises";
import { isAbsolute, join, normalize, resolve } from "node:path";
import { parseFrontmatter } from "@earendil-works/pi-coding-agent";
import type { Harness } from "@earendil-works/pi-durable";
import { Cursors } from "../../server/src/cursor.js";
import { object, WireError } from "../../server/src/wire.js";
import { skillCommand, skillName, type WorkspaceSkill, type WorkspaceSkillsHost } from "../../server/src/workspace-skills.js";
import { threadWorkspace } from "./thread-workspace.js";
import { readStoreJSON, statStorePath } from "./store-files.js";
import { readScopedText } from "./workspace-file.js";
import type { WorkspaceWorkTrust } from "./workspace-trust.js";

const sourceLimit = 32 * 1024;
const maxDirectoryEntries = 64;
const hash = (value: unknown) => createHash("sha256").update(JSON.stringify(value)).digest("hex");
const safe = (v: unknown): v is string => typeof v === "string" && v.length > 0 && v.isWellFormed() && !/[\x00-\x1f\x7f-\x9f]/.test(v);
const fileStamp = (v: Awaited<ReturnType<typeof statStorePath>>) => v && `${v.dev}:${v.ino}:${v.birthtimeNs}:${v.ctimeNs}`;
const configPath = (agent: string) => join(agent, "skills.json");

async function settings(agent: string) {
  try {
    const path = configPath(agent), stamp = fileStamp(await statStorePath(path));
    if (!stamp) return;
    const raw = await readStoreJSON(path);
    if (!object(raw) || Object.keys(raw).some(k => k !== "personal" && k !== "projectRoots") ||
        raw.personal !== undefined && typeof raw.personal !== "boolean" ||
        raw.projectRoots !== undefined && (!Array.isArray(raw.projectRoots) || raw.projectRoots.length > 16)) return;
    const projects = new Map<string, string>();
    for (const row of (raw.projectRoots ?? []) as unknown[]) {
      if (!object(row) || Object.keys(row).length !== 2 || !safe(row.workspace) || !isAbsolute(row.workspace) ||
          normalize(row.workspace) !== row.workspace || !safe(row.directory) || Buffer.byteLength(row.directory) > 512 ||
          !row.directory.split("/").every(dir => dir && ![".", "..", ".pi", ".hush", ".git"].includes(dir.toLowerCase()) && !dir.includes("\\")) ||
          projects.has(row.workspace)) return;
      projects.set(row.workspace, row.directory);
    }
    if (!(raw.personal || projects.size) || fileStamp(await statStorePath(path)) !== stamp) return;
    return { personal: raw.personal === true, projects, stamp: hash([stamp, raw]) };
  } catch { /* Invalid/unsafe private config does not opt in. */ }
}

export const standaloneSkillsConfigured = async (agent: string) => !!await settings(agent);

type Root = { cwd: string; directory: string; source: WorkspaceSkill["source"]; grant: string };
type Scope = { cwd: string; stamp: string; roots: Root[] };
type Skill = Omit<WorkspaceSkill, "token"> & { fingerprint: string; body: string; path: string; base: string };

export function skillUnavailable() {
  const error = new WireError("invalid_request");
  error.message = "Standalone skill unavailable or changed. Use /skills to review the current workspace skills and check Hush trust on the backend host.";
  return error;
}

const xml = (s: string) => s.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");

export class StandaloneSkills implements WorkspaceSkillsHost {
  readonly #cursors = new Cursors();
  #pending = 0;

  constructor(private readonly h: () => Harness, private readonly agent: string, private readonly trust: WorkspaceWorkTrust) {}

  async #scope(thread: string, signal: AbortSignal): Promise<Scope> {
    const cwd = await threadWorkspace(this.h(), thread, signal);
    const cfg = await settings(this.agent), roots: Root[] = [];
    if (!cfg) return { cwd, stamp: "", roots };
    if (cfg.personal) roots.push({ cwd: await realpath(this.agent), directory: "skills", source: "personal", grant: "" });
    if (cfg.projects.size) {
      const binding = await this.trust.binding(cwd, signal).catch(() => undefined);
      signal.throwIfAborted();
      const directory = binding && cfg.projects.get(binding.path);
      if (binding && directory) roots.push({ cwd: binding.path, directory, source: "workspace", grant: binding.stamp });
    }
    return { cwd, stamp: cfg.stamp, roots };
  }

  async #read(root: Root, name: string, signal: AbortSignal): Promise<Skill | undefined> {
    const base = join(root.cwd, root.directory, name);
    const relative = `${root.directory}/${name}/SKILL.md`;
    try {
      const file = await readScopedText(root.cwd, relative, sourceLimit, signal);
      const { frontmatter: data, body } = parseFrontmatter(file.text);
      if (data.name !== name || typeof data.description !== "string" || !body.trim()) return;
      const description = data.description.replace(/[\x00-\x1f\x7f-\x9f]/g, " ").trim().slice(0, 160).toWellFormed();
      if (!description) return;
      return { name, description, source: root.source, path: join(base, "SKILL.md"), base, body: body.trim(),
        fingerprint: hash([root, name, file.stamp, file.text]) };
    } catch { signal.throwIfAborted(); return undefined; }
  }

  async #names(root: Root, signal: AbortSignal) {
    const names: string[] = [];
    const path = join(root.cwd, root.directory);
    if (await realpath(path).catch(() => undefined) !== path) return names;
    const dir = await opendir(path).catch(() => undefined);
    if (!dir) return names;
    let count = 0;
    for await (const entry of dir) {
      signal.throwIfAborted();
      if (++count > maxDirectoryEntries) throw new WireError("limit");
      if (entry.isDirectory() && skillName(entry.name)) names.push(entry.name);
    }
    return names;
  }

  #binding(thread: string, scope: Scope, skill: Skill) {
    return hash([thread, scope.cwd, scope.stamp, skill.fingerprint]);
  }

  async list(threadId: string, peer: AbortSignal) {
    if (this.#pending >= 4) throw new WireError("limit");
    this.#pending++;
    try {
      const signal = AbortSignal.any([peer, AbortSignal.timeout(3500)]);
      const scope = await this.#scope(threadId, signal);
      const candidates = new Map<string, Skill | null>();
      for (const root of scope.roots) {
        const names = await this.#names(root, signal);
        // Bound per-list disk concurrency as well as total bytes. A directory
        // of many small skills should not pay each read's latency in series.
        for (let offset = 0; offset < names.length; offset += 4) {
          const batch = await Promise.all(names.slice(offset, offset + 4).map(name => this.#read(root, name, signal)));
          for (const skill of batch) if (skill) candidates.set(skill.name, candidates.has(skill.name) ? null : skill);
        }
      }
      if (hash(await this.#scope(threadId, signal)) !== hash(scope)) throw skillUnavailable();
      const skills = Array.from(candidates.values()).filter((skill): skill is Skill => !!skill)
        .sort((a, b) => a.name.localeCompare(b.name)).slice(0, 64)
        .map(skill => ({ name: skill.name, description: skill.description, source: skill.source,
          token: this.#cursors.encode("workspaceSkill", this.#binding(threadId, scope, skill)) }));
      return { skills };
    } finally { this.#pending--; }
  }

  async expand(thread: string, cwd: string | undefined, input: string, token: string | undefined, peer?: AbortSignal) {
    const match = skillCommand(input);
    if (!match) throw skillUnavailable();
    const signal = AbortSignal.any([...(peer ? [peer] : []), AbortSignal.timeout(3500)]);
    const scope = await this.#scope(thread, signal);
    if (!cwd || resolve(cwd) !== resolve(scope.cwd)) throw skillUnavailable();
    const name = match[1]!;
    const choices = (await Promise.all(scope.roots.map(root => this.#read(root, name, signal)))).filter((value): value is Skill => !!value);
    if (choices.length !== 1 || hash(await this.#scope(thread, signal)) !== hash(scope)) throw skillUnavailable();
    const skill = choices[0]!, binding = this.#binding(thread, scope, skill);
    if (token) {
      try { if (this.#cursors.decode<string>("workspaceSkill", token) !== binding) throw skillUnavailable(); }
      catch { throw skillUnavailable(); }
    }
    const expected = token ?? this.#cursors.encode("workspaceSkill", binding);
    const args = match[2]?.trim();
    return {
      text: `<skill name="${name}" location="${xml(skill.path)}">\nReferences are relative to ${JSON.stringify(skill.base)}.\n\n${skill.body}\n</skill>` + (args ? `\n\n${args}` : ""),
      confirm: async (signal?: AbortSignal) => { await this.expand(thread, cwd, input, expected, signal); },
    };
  }
}
