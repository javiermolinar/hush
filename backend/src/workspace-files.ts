import { createHash } from "node:crypto";
import { constants } from "node:fs";
import { access, realpath } from "node:fs/promises";
import { isAbsolute } from "node:path";
import type { Harness } from "@earendil-works/pi-durable";
import { Cursors } from "../../server/src/cursor.js";
import { browseName, githubDirectory, searchPath, type WorkspaceFileQuery, type WorkspaceFileSearch, type WorkspaceFileSearchResult, type WorkspaceFilesHost,
  type WorkspaceFileTarget, type WorkspaceScopeWatch } from "../../server/src/workspace-files.js";
import { object, WireError } from "../../server/src/wire.js";
import type { WorkspaceTrust } from "./workspace-trust.js";
import { prospectiveWorkspace, threadWorkspace } from "./thread-workspace.js";
import { FileListWorker } from "./file-list-worker.js";
import { ChangePulse, WorkspaceRevisions } from "./workspace-changes.js";

const digest = (value: unknown) => createHash("sha256").update(JSON.stringify(value)).digest("hex");
type Cursor = { scope: string; listing: string; offset: number; expires: number };
type Root = "workspace" | "home";
type Binding = Awaited<ReturnType<WorkspaceTrust["binding"]>>;
const bindingStamp = (b: Binding | undefined) =>
  b === undefined ? null : [b.path, String(b.device), String(b.inode), String(b.birth), b.stamp];

export async function workspaceFileHelper() {
  const helper = process.env.HUSH_FILE_LIST_HELPER;
  if (!helper || !isAbsolute(helper) || !["darwin", "linux"].includes(process.platform)) return undefined;
  try { const pin = await realpath(helper); await access(pin, constants.X_OK); return pin; }
  catch { return undefined; }
}

export class WorkspaceFiles implements WorkspaceFilesHost {
  readonly projectPaths = true;
  readonly #cursors = new Cursors();
  readonly #worker: FileListWorker;
  #pending = 0;
  #searchGeneration = 0;
  readonly #changes = new ChangePulse();
  readonly #revisions = new WorkspaceRevisions();
  #watchers = 0;
  #fallback?: NodeJS.Timeout;

  constructor(private readonly harness: Harness, private readonly trust: WorkspaceTrust, helper: string) {
    this.#worker = new FileListWorker(helper);
  }

  close() {
    clearInterval(this.#fallback);
    return this.#worker.close();
  }

  invalidateSearch() { this.#searchGeneration++; }

  grantsChanged() {
    this.invalidateSearch();
    this.#changes.emit();
  }

  async waitScope({ target, after }: WorkspaceScopeWatch, peer: AbortSignal) {
    if (this.#watchers++ === 0) {
      // Revalidate checkout bindings even if the filesystem dropped a grant
      // notification or the thread's directory moved without a grant write.
      this.#fallback = setInterval(() => this.#changes.emit(), 20_000).unref();
    }
    try {
      for (;;) {
        const generation = this.#changes.generation;
        const signal = AbortSignal.any([peer, AbortSignal.timeout(3500)]);
        const cwd = await this.#workspace(target, signal).catch(error => {
          if (!(error instanceof WireError) || error.code !== "not_found") throw error;
          return undefined;
        });
        const [workspace, home] = cwd ? await this.#scope(cwd, signal) : [];
        signal.throwIfAborted();
        if (generation !== this.#changes.generation) continue;
        const revision = this.#revisions.of([target, cwd, bindingStamp(workspace), bindingStamp(home)]);
        if (revision !== after) {
          if (workspace || home) this.#worker.warm();
          return { revision, workspace: !!workspace, home: !!home };
        }
        await this.#changes.wait(generation, peer);
      }
    } finally {
      if (--this.#watchers === 0) clearInterval(this.#fallback);
    }
  }

  // A thread's recorded checkout, or the directory a prospective thread would
  // record. Either way the same grant applies and no conversation is created.
  #workspace(target: WorkspaceFileTarget, signal: AbortSignal) {
    return "threadId" in target ? threadWorkspace(this.harness, target.threadId, signal) : prospectiveWorkspace(target.cwd, signal);
  }

  async #root(kind: Root, cwd: string, signal: AbortSignal) {
    try { return await (kind === "workspace" ? this.trust.binding(cwd, signal) : this.trust.homeBinding(signal)); }
    catch { signal.throwIfAborted(); return undefined; }
  }

  #scope(cwd: string, signal: AbortSignal) {
    return Promise.all([this.#root("workspace", cwd, signal), this.#root("home", cwd, signal)]);
  }

  async call(query: WorkspaceFileQuery, peer: AbortSignal) {
    const signal = AbortSignal.any([peer, AbortSignal.timeout(3500)]);
    const cwd = await this.#workspace(query.target, signal);
    if (query.kind === "scope") {
      const [workspace, home] = await this.#scope(cwd, signal);
      if (workspace || home) this.#worker.warm();
      return { workspace: !!workspace, home: !!home };
    }
    const root = await this.#root(query.root, cwd, signal);
    if (!root) throw new WireError(query.cursor ? "invalid_cursor" : "not_found");
    const scope = digest([query.target, cwd, query.root, query.directory, query.limit, root.path,
      String(root.device), String(root.inode), String(root.birth), root.stamp, !!query.includeGitHub]);
    const cursor = query.cursor ? this.#cursors.decode<Cursor>("workspaceFiles", query.cursor) : undefined;
    if (cursor && (cursor.scope !== scope || cursor.expires < Date.now())) throw new WireError("invalid_cursor");
    const value = (await this.#scan(query, cwd, root, { directory: query.directory, offset: cursor?.offset ?? 0, limit: query.limit,
      includeGitHub: query.includeGitHub }, signal, query.cursor ? "invalid_cursor" : "not_found")).page;
    if (!object(value) || typeof value.stamp !== "string" || !/^[a-f0-9]{64}$/.test(value.stamp) ||
        typeof value.more !== "boolean" || !Array.isArray(value.entries) || value.entries.length > query.limit ||
        value.more && value.entries.length !== query.limit ||
        !value.entries.every(x => object(x) && typeof x.directory === "boolean" &&
          browseName(x.name, !!query.includeGitHub && query.directory === "" && x.directory))) throw new WireError("internal");
    if (cursor && cursor.listing !== value.stamp) throw new WireError("invalid_cursor");
    const entries = value.entries.map(({ name, directory }) => ({ name: name as string, directory: directory as boolean }));
    return { entries, ...(value.more && { nextCursor: this.#cursors.encode("workspaceFiles", { scope, listing: value.stamp,
      offset: (cursor?.offset ?? 0) + entries.length, expires: cursor?.expires ?? Date.now() + 60_000 } satisfies Cursor) }) };
  }

  async search(query: WorkspaceFileSearch, peer: AbortSignal): Promise<WorkspaceFileSearchResult> {
    const signal = AbortSignal.any([peer, AbortSignal.timeout(3500)]);
    const cwd = await this.#workspace(query.target, signal);
    const root = await this.#root("workspace", cwd, signal);
    if (!root) throw new WireError("not_found");
    const value = (await this.#scan({ target: query.target, root: "workspace" }, cwd, root,
      { query: query.query, limit: query.limit, includeGitHub: query.includeGitHub,
        searchKey: digest([String(root.birth), root.stamp, this.#searchGeneration]) }, signal)).search;
    if (!object(value) || typeof value.partial !== "boolean" || !Array.isArray(value.matches) || value.matches.length > query.limit ||
        !value.matches.every(x => object(x) && typeof x.directory === "boolean" && searchPath(x.path, !!query.includeGitHub) &&
          (x.path !== githubDirectory || x.directory))) throw new WireError("internal");
    const matches = value.matches.map(({ path, directory }) => ({ path: path as string, directory: directory as boolean }));
    if (new Set(matches.map(x => x.path)).size !== matches.length) throw new WireError("internal");
    return { matches, partial: value.partial };
  }

  async #scan(query: { target: WorkspaceFileTarget; root: Root }, cwd: string, root: Binding, input: object,
      signal: AbortSignal, missing: "not_found" | "invalid_cursor" = "not_found") {
    if (this.#pending >= 8) throw new WireError("limit");
    this.#pending++;
    let value: Record<string, unknown>;
    try {
      const result = await this.#worker.request({ ...input, root: root.path, device: String(root.device), inode: String(root.inode) }, signal);
      if (!object(result) || result.error !== undefined) {
        if (object(result) && result.error === "limit") throw new WireError("limit");
        throw new WireError("not_found");
      }
      value = result;
    } catch (error) {
      signal.throwIfAborted();
      if (error instanceof WireError && error.code === "limit") throw error;
      throw new WireError(missing);
    } finally { this.#pending--; }
    // A revoked or changed grant, checkout, or thread invalidates any in-flight
    // response too. The grant is chosen only by the resolved target.
    const currentCwd = await this.#workspace(query.target, signal);
    const current = await this.#root(query.root, currentCwd, signal);
    signal.throwIfAborted();
    if (!current || cwd !== currentCwd || current.path !== root.path || current.device !== root.device ||
        current.inode !== root.inode || current.birth !== root.birth || current.stamp !== root.stamp) throw new WireError(missing);
    return value;
  }
}
