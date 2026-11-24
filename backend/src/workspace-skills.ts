import { createHash } from "node:crypto";
import { opendir, realpath } from "node:fs/promises";
import { dirname, isAbsolute, join, normalize, resolve } from "node:path";
import { parseFrontmatter } from "@earendil-works/pi-coding-agent";
import type { Harness } from "@earendil-works/pi-durable";
import { Cursors } from "../../server/src/cursor.js";
import { object, WireError } from "../../server/src/wire.js";
import { skillCommand, skillName, type WorkspaceSkill, type WorkspaceSkillScope, type WorkspaceSkillsHost } from "../../server/src/workspace-skills.js";
import { workspaceAncestors } from "./workspace-ancestors.js";
import { threadWorkspace } from "./thread-workspace.js";
import { readStoreJSON, statStorePath } from "./store-files.js";
import { readScopedText } from "./workspace-file.js";
import type { WorkspaceTrust } from "./workspace-trust.js";

const sourceLimit = 32 * 1024;
const maxDirectoryEntries = 64;
const maxSkillReads = 256;
const maxSkills = 64;
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
    if (fileStamp(await statStorePath(path)) !== stamp) return;
    return { personal: raw.personal !== false, projects, stamp: hash([stamp, raw]) };
  } catch { /* Invalid/unsafe private config does not add any roots. */ }
}

type Root = { cwd: string; directory: string; source: WorkspaceSkill["source"]; grant: string };
type Scope = { cwd: string; checkout: string; stamp: string; roots: Root[] };
type Pin = { draft: boolean; binding: string };
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

  constructor(private readonly h: () => Harness, private readonly agent: string, private readonly trust: WorkspaceTrust) {}

  async #scope(query: WorkspaceSkillScope, signal: AbortSignal): Promise<Scope> {
    const cwd = "threadId" in query ? await threadWorkspace(this.h(), query.threadId, signal) : query.cwd;
    const [cfg, profile, work] = await Promise.all([
      settings(this.agent), realpath(this.agent), this.trust.binding(cwd, signal).catch(() => undefined),
    ]);
    // For untrusted paths, don't vary the visible result based on what
    // exists on disk. Only trusted checkouts get canonical path bindings.
    const checkout = work?.path ?? resolve(cwd);
    const roots: Root[] = [];
    const home = dirname(profile);
    if (cfg?.personal !== false) {
      roots.push({ cwd: profile, directory: "skills", source: "personal", grant: "" },
        { cwd: home, directory: ".agents/skills", source: "personal", grant: "" });
    }
    if (work) {
      const explicit = cfg?.projects.get(work.path);
      if (explicit) roots.push({ cwd: work.path, directory: explicit, source: "workspace", grant: work.stamp });
      for (const dir of await workspaceAncestors(work.path, signal)) {
        if (dir === home || dir === work.path && explicit === ".agents/skills") continue;
        roots.push({ cwd: dir, directory: ".agents/skills", source: "workspace", grant: work.stamp });
      }
    }
    signal.throwIfAborted();
    return { cwd, checkout, stamp: cfg?.stamp ?? "", roots };
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
    return names.sort();
  }

  #binding(query: WorkspaceSkillScope, scope: Scope, skill: Skill) {
    // Draft selection binds the backend checkout before it has a thread ID.
    // Established thread tokens include the exact ID and recorded cwd.
    const owner = "threadId" in query ? [query.threadId, scope.cwd, scope.checkout] : [scope.checkout];
    return hash([owner, scope.stamp, skill.fingerprint]);
  }

  async list(query: WorkspaceSkillScope, peer: AbortSignal) {
    if (this.#pending >= 4) throw new WireError("limit");
    this.#pending++;
    try {
      const signal = AbortSignal.any([peer, AbortSignal.timeout(3500)]);
      const scope = await this.#scope(query, signal);
      const candidates = new Map<string, Skill>();
      let remaining = maxSkillReads;
      for (const root of scope.roots) {
        if (!remaining || candidates.size === maxSkills) break;
        const names = (await this.#names(root, signal)).filter(name => !candidates.has(name)).slice(0, remaining);
        // Bound per-list concurrency and total file reads across all roots.
        for (let offset = 0; offset < names.length && candidates.size < maxSkills;) {
          const pending = names.slice(offset, offset + Math.min(4, maxSkills - candidates.size));
          const batch = await Promise.all(pending.map(name => this.#read(root, name, signal)));
          remaining -= pending.length;
          for (const skill of batch) if (skill) candidates.set(skill.name, skill);
          offset += pending.length;
        }
      }
      if (hash(await this.#scope(query, signal)) !== hash(scope)) throw skillUnavailable();
      const skills = Array.from(candidates.values()).sort((a, b) => a.name.localeCompare(b.name))
        .map(skill => ({ name: skill.name, description: skill.description, source: skill.source,
          token: this.#cursors.encode("workspaceSkill", { draft: "cwd" in query, binding: this.#binding(query, scope, skill) } satisfies Pin) }));
      return { skills };
    } finally { this.#pending--; }
  }

  async expand(thread: string, cwd: string | undefined, input: string, token: string | undefined, peer?: AbortSignal) {
    const match = skillCommand(input);
    if (!match) throw skillUnavailable();
    const signal = AbortSignal.any([...(peer ? [peer] : []), AbortSignal.timeout(3500)]);
    const query = { threadId: thread };
    const scope = await this.#scope(query, signal);
    if (!cwd || resolve(cwd) !== resolve(scope.cwd)) throw skillUnavailable();
    const name = match[1]!;
    let skill: Skill | undefined;
    for (const root of scope.roots) {
      skill = await this.#read(root, name, signal);
      if (skill) break;
    }
    if (!skill || hash(await this.#scope(query, signal)) !== hash(scope)) throw skillUnavailable();
    if (token) {
      try {
        const pin = this.#cursors.decode<Pin>("workspaceSkill", token);
        if (pin.binding !== this.#binding(pin.draft ? { cwd: scope.cwd } : query, scope, skill)) throw skillUnavailable();
      } catch { throw skillUnavailable(); }
    }
    const expected = this.#cursors.encode("workspaceSkill", { draft: false, binding: this.#binding(query, scope, skill) } satisfies Pin);
    const args = match[2]?.trim();
    return {
      text: `<skill name="${name}" location="${xml(skill.path)}">\nReferences are relative to ${JSON.stringify(skill.base)}.\n\n${skill.body}\n</skill>` + (args ? `\n\n${args}` : ""),
      confirm: async (signal?: AbortSignal) => { await this.expand(thread, cwd, input, expected, signal); },
    };
  }
}
