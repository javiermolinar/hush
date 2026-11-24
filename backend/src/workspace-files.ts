import { createHash } from "node:crypto";
import { constants } from "node:fs";
import { access, realpath } from "node:fs/promises";
import { isAbsolute } from "node:path";
import type { Harness } from "@earendil-works/pi-durable";
import { Cursors } from "../../server/src/cursor.js";
import { browseName, type WorkspaceFileQuery, type WorkspaceFilesHost } from "../../server/src/workspace-files.js";
import { object, WireError } from "../../server/src/wire.js";
import type { WorkspaceTrust } from "./workspace-trust.js";
import { threadWorkspace } from "./thread-workspace.js";
import { FileListWorker } from "./file-list-worker.js";
const digest = (value: unknown) => createHash("sha256").update(JSON.stringify(value)).digest("hex");
type Cursor = { scope: string; listing: string; offset: number; expires: number };

export async function workspaceFileHelper() {
  const helper = process.env.HUSH_FILE_LIST_HELPER;
  if (!helper || !isAbsolute(helper) || !["darwin", "linux"].includes(process.platform)) return undefined;
  try { const pin = await realpath(helper); await access(pin, constants.X_OK); return pin; }
  catch { return undefined; }
}

export class WorkspaceFiles implements WorkspaceFilesHost {
  readonly #cursors = new Cursors();
  readonly #worker: FileListWorker;
  #pending = 0;

  constructor(private readonly harness: Harness, private readonly trust: WorkspaceTrust, helper: string) {
    this.#worker = new FileListWorker(helper);
  }

  close() { return this.#worker.close(); }

  async #root(kind: "workspace" | "home", cwd: string, signal: AbortSignal) {
    try { return await (kind === "workspace" ? this.trust.binding(cwd, signal) : this.trust.homeBinding(signal)); }
    catch { signal.throwIfAborted(); return undefined; }
  }

  async call(query: WorkspaceFileQuery, peer: AbortSignal) {
    const signal = AbortSignal.any([peer, AbortSignal.timeout(3500)]);
    const cwd = await threadWorkspace(this.harness, query.threadId, signal);
    if (query.kind === "scope") {
      const [workspace, home] = await Promise.all([this.#root("workspace", cwd, signal), this.#root("home", cwd, signal)]);
      if (workspace || home) this.#worker.warm();
      return { workspace: !!workspace, home: !!home };
    }
    const root = await this.#root(query.root, cwd, signal);
    if (!root) throw new WireError(query.cursor ? "invalid_cursor" : "not_found");
    const scope = digest([query.threadId, cwd, query.root, query.directory, query.limit, root.path,
      String(root.device), String(root.inode), String(root.birth), root.stamp]);
    const cursor = query.cursor ? this.#cursors.decode<Cursor>("workspaceFiles", query.cursor) : undefined;
    if (cursor && (cursor.scope !== scope || cursor.expires < Date.now())) throw new WireError("invalid_cursor");
    if (this.#pending >= 8) throw new WireError("limit");
    this.#pending++;
    let value: unknown;
    try {
      const result = await this.#worker.request({ root: root.path, device: String(root.device), inode: String(root.inode),
        directory: query.directory, offset: cursor?.offset ?? 0, limit: query.limit }, signal);
      if (!object(result) || result.error !== undefined) {
        if (object(result) && result.error === "limit") throw new WireError("limit");
        throw new WireError("not_found");
      }
      value = result.page;
    } catch (error) {
      signal.throwIfAborted();
      if (error instanceof WireError && error.code === "limit") throw error;
      throw new WireError(query.cursor ? "invalid_cursor" : "not_found");
    } finally { this.#pending--; }
    if (!object(value) || typeof value.stamp !== "string" || !/^[a-f0-9]{64}$/.test(value.stamp) ||
        typeof value.more !== "boolean" || !Array.isArray(value.entries) || value.entries.length > query.limit ||
        value.more && value.entries.length !== query.limit ||
        !value.entries.every(x => object(x) && browseName(x.name) && typeof x.directory === "boolean")) throw new WireError("internal");
    // A revoked or changed grant, checkout, or thread invalidates an in-flight
    // response too. No peer-provided path is used to choose the grant.
    const currentCwd = await threadWorkspace(this.harness, query.threadId, signal);
    const current = await this.#root(query.root, currentCwd, signal);
    if (!current || cwd !== currentCwd || current.path !== root.path || current.device !== root.device ||
        current.inode !== root.inode || current.birth !== root.birth || current.stamp !== root.stamp ||
        cursor && cursor.listing !== value.stamp) throw new WireError(query.cursor ? "invalid_cursor" : "not_found");
    const entries = value.entries.map(({ name, directory }) => ({ name: name as string, directory: directory as boolean }));
    return { entries, ...(value.more && { nextCursor: this.#cursors.encode("workspaceFiles", { scope, listing: value.stamp,
      offset: (cursor?.offset ?? 0) + entries.length, expires: cursor?.expires ?? Date.now() + 60_000 } satisfies Cursor) }) };
  }
}
