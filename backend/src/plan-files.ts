import { createHash } from "node:crypto";
import { execFile } from "node:child_process";
import { realpath } from "node:fs/promises";
import { isAbsolute, join, relative, sep } from "node:path";
import { planFilePath, planScopeFileLimit, type PlanFileEdit } from "../../server/src/plan.js";
import { object } from "../../server/src/wire.js";
import type { WorkspaceTrust } from "./workspace-trust.js";

export type PlanWorkspace = { cwd: string; root: string; device: string; inode: string; birth: string; grant: string };
export type FileVersion = { hash: string; inode: string };
export type Replacement = { oldText: string; newText: string };

export type PlanFileTarget = { path: string; before: FileVersion; writable: boolean };
export type FileProposal = { path: string; text: string };
export const planFileBytes = 32 * 1024;
export const validFileText = (text: string) => text.isWellFormed() && !text.includes("\0") && Buffer.byteLength(text) <= planFileBytes;

const helperVersion = 2;
const digest = (text: string) => createHash("sha256").update(text, "utf8").digest("hex");
const sameVersion = (a: FileVersion, b: FileVersion) => a.hash === b.hash && a.inode === b.inode;
const within = (from: string, to: string) => {
  const path = relative(from, to);
  return !isAbsolute(path) && path !== ".." && !path.startsWith(`..${sep}`);
};

const failures = {
  scope: "Expected an existing, single-link plain UTF-8 file of at most 32 KiB in the declared scope.",
  busy: "A scoped file operation is still settling.",
  changed: "The file changed since Run was authorized. No replacement was applied.",
  replacement: "The worker did not provide a unique, bounded replacement.",
  check: "The replacement must make the declared literal check pass. No replacement was applied.",
  publish: "Could not publish the replacement.",
  exchange: "This filesystem does not support guarded file exchange. No files were published.",
  cleanup: "No files were published, but private staging cleanup could not be confirmed.",
  verify: "Could not verify the installed file.",
  partial: "The file helper could not verify the publication.",
  protocol: "The plan file helper is incompatible. Rebuild the Hush CLI and backend together.",
  unknown: "The file helper did not return a valid reply.",
} as const;

export class PlanFileError extends Error {
  constructor(message: string, readonly publicationUncertain = false, readonly recoveryFiles = false) { super(message); }
}

function invoke(helper: string, request: object, signal?: AbortSignal): Promise<unknown> {
  return new Promise<string>((resolve, reject) => {
    const child = execFile(helper, ["--hush-internal-plan-file"], {
      encoding: "utf8", env: {}, timeout: 5_000, maxBuffer: 2 * 1024 * 1024, signal,
    }, (error, stdout) => error ? reject(error) : resolve(stdout));
    child.stdin?.on("error", () => {});
    child.stdin?.end(JSON.stringify({ version: helperVersion, ...request }));
  }).then(JSON.parse);
}

// Launch only the private native helper. It does not interpret commands or
// consume ambient provider credentials. File paths and identity come from the
// admitted plan and workspace grant, not from worker tool arguments.
export class PlanFiles {
  private constructor(private readonly helper: string, private readonly trust: WorkspaceTrust,
    private readonly protectedDirs: readonly string[], private readonly changed: (cwd: string) => void) {}

  static async open(helper: string, trust: WorkspaceTrust, protectedDirs: string[], changed: (cwd: string) => void) {
    // Probe without opening a workspace or sending any write authority. Old
    // helpers reject the request, so they cannot be mistaken for a lost write.
    const probe = await invoke(helper, {}).catch(() => undefined);
    if (!object(probe) || probe.version !== helperVersion || probe.error !== undefined) return;
    return new PlanFiles(helper, trust, await Promise.all(protectedDirs.map(dir => realpath(dir))), changed);
  }

  async bind(cwd: string, signal?: AbortSignal): Promise<PlanWorkspace> {
    const w = await this.trust.binding(cwd, signal);
    return { cwd, root: w.path, device: String(w.device), inode: String(w.inode), birth: String(w.birth), grant: w.stamp };
  }

  async #check(w: PlanWorkspace, paths: readonly string[], signal?: AbortSignal) {
    if (/(?:^|\/)\.git(?:\/|$)/i.test(w.root) || paths.some(path =>
      !planFilePath(path) || this.protectedDirs.some(dir => within(dir, join(w.root, path))))) {
      throw new PlanFileError("The declared file is outside plan execution scope.");
    }
    const current = await this.bind(w.cwd, signal).catch(() => {
      signal?.throwIfAborted();
      throw new PlanFileError("Workspace trust is no longer valid. No replacement was applied.");
    });
    if (current.root !== w.root || current.device !== w.device || current.inode !== w.inode || current.birth !== w.birth || current.grant !== w.grant) {
      throw new PlanFileError("Workspace trust changed since Run was authorized.");
    }
    signal?.throwIfAborted();
  }

  async #request(w: PlanWorkspace, paths: readonly string[], request: object, publishing: boolean, signal?: AbortSignal) {
    await this.#check(w, paths, signal);
    let reply: unknown;
    try {
      // Once publication starts, let the helper settle before releasing the
      // task even if the thread is stopped. A lost reply is not a clean abort.
      reply = await invoke(this.helper, { root: w.root, device: w.device, inode: w.inode, ...request }, publishing ? undefined : signal);
    } catch {
      throw new PlanFileError(failures.unknown, publishing);
    }
    if (!object(reply) || reply.version !== helperVersion) throw new PlanFileError(failures.unknown, publishing);
    if (typeof reply.error === "string" && Object.hasOwn(failures, reply.error)) {
      throw new PlanFileError(failures[reply.error as keyof typeof failures], publishing && (reply.error === "partial" || reply.error === "verify"),
        reply.error === "cleanup");
    }
    if (reply.error !== undefined) throw new PlanFileError(failures.unknown, publishing);
    return reply;
  }

  #file(reply: unknown, path: string, publishing: boolean) {
    const bad = () => new PlanFileError(failures.unknown, publishing);
    if (!object(reply) || reply.path !== path) throw bad();
    const file = reply.file;
    if (!object(file) || typeof file.hash !== "string" || !/^[a-f0-9]{64}$/.test(file.hash) ||
        typeof file.inode !== "string" || !/^\d+:\d+$/.test(file.inode)) throw bad();
    return { hash: file.hash, inode: file.inode };
  }

  #snapshotFile(reply: unknown, path: string) {
    const file = this.#file(reply, path, false);
    if (!object(reply) || typeof reply.text !== "string" || !validFileText(reply.text) || digest(reply.text) !== file.hash) {
      throw new PlanFileError(failures.unknown);
    }
    return { file, text: reply.text };
  }

  async #read(w: PlanWorkspace, path: string, signal?: AbortSignal) {
    return this.#snapshotFile(await this.#request(w, [path], { path }, false, signal), path);
  }

  async snapshot(w: PlanWorkspace, paths: readonly string[], signal?: AbortSignal) {
    if (!paths.length || paths.length > planScopeFileLimit || new Set(paths).size !== paths.length) throw new PlanFileError(failures.scope);
    const reply = await this.#request(w, paths, { batch: paths.map(path => ({ path })) }, false, signal);
    const batch = reply.batch;
    if (!Array.isArray(batch) || batch.length !== paths.length) throw new PlanFileError(failures.unknown);
    return paths.map((path, i) => ({ path, ...this.#snapshotFile(batch[i], path) }));
  }

  async candidate(w: PlanWorkspace, files: readonly PlanFileTarget[], proposals: readonly FileProposal[], signal: AbortSignal) {
    if (proposals.length !== files.filter(f => f.writable).length) throw new PlanFileError("The task did not propose every declared write.");
    const patch = new Map(proposals.map(p => [p.path, p.text]));
    if (patch.size !== proposals.length || proposals.some(p => !validFileText(p.text) || !files.some(f => f.writable && f.path === p.path))) {
      throw new PlanFileError(failures.replacement);
    }
    const snapshot = await this.snapshot(w, files.map(f => f.path), signal);
    const before = new Map<string, string>(), after = new Map<string, string>();
    for (const [i, file] of files.entries()) {
      const original = snapshot[i];
      if (!sameVersion(original.file, file.before)) throw new PlanFileError(failures.changed);
      const replacement = patch.get(file.path);
      if (replacement === original.text) throw new PlanFileError("A declared write did not change its file.");
      before.set(file.path, original.text);
      after.set(file.path, replacement ?? original.text);
    }
    return { before, after, patch };
  }

  async publish(w: PlanWorkspace, files: readonly PlanFileTarget[], patch: ReadonlyMap<string, string>, stage: string, signal: AbortSignal) {
    const paths = files.map(f => f.path);
    if (patch.size < 1 || patch.size !== files.filter(f => f.writable).length || files.some(f => patch.has(f.path) !== f.writable)) {
      throw new PlanFileError("The publication did not match the declared write set. No files were published.");
    }
    try {
      const reply = await this.#request(w, paths, { stage, batch: files.map(({ path, before }) => ({ path, before, text: patch.get(path) })) }, true, signal);
      if (!Array.isArray(reply.batch) || reply.batch.length !== paths.length) throw new PlanFileError(failures.unknown, true);
      const versions = reply.batch, seen = new Set<string>();
      for (const [i, f] of files.entries()) {
        const actual = this.#file(versions[i], f.path, true);
        const text = patch.get(f.path);
        if (seen.has(actual.inode) || (text === undefined ? !sameVersion(actual, f.before)
          : actual.hash !== digest(text) || actual.inode === f.before.inode)) throw new PlanFileError(failures.verify, true);
        seen.add(actual.inode);
      }
      try { await this.#check(w, paths, signal); }
      catch { throw new PlanFileError("Workspace trust or Run authority changed while publishing.", true); }
    } finally { this.changed(w.root); }
  }

  async preflight(w: PlanWorkspace, edit: PlanFileEdit, signal?: AbortSignal) {
    const current = await this.#read(w, edit.path, signal);
    if (!current.text.length) throw new PlanFileError("The file must contain text to replace.");
    if (current.text.includes(edit.contains)) throw new PlanFileError("The literal check already passes. Revise the task before running it.");
    return current.file;
  }

  async read(w: PlanWorkspace, path: string, before: FileVersion, signal?: AbortSignal) {
    const current = await this.#read(w, path, signal);
    if (!sameVersion(current.file, before)) throw new PlanFileError(failures.changed);
    return current.text;
  }

  async literalCandidate(w: PlanWorkspace, edit: PlanFileEdit, before: FileVersion, patch: Replacement, signal: AbortSignal) {
    const original = await this.read(w, edit.path, before, signal);
    const match = original.indexOf(patch.oldText);
    if (!patch.oldText || match < 0 || match !== original.lastIndexOf(patch.oldText)) throw new PlanFileError(failures.replacement);
    const next = original.slice(0, match) + patch.newText + original.slice(match + patch.oldText.length);
    if (!validFileText(next) || original.includes(edit.contains) || !next.includes(edit.contains)) throw new PlanFileError(failures.check);
    return new Map([[edit.path, next]]);
  }
}
