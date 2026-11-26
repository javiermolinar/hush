import { isAbsolute, join, relative, sep } from "node:path";
import { readScopedText } from "./workspace-file.js";
import type { WorkspaceTrust } from "./workspace-trust.js";

// JSON pin, not a bearer credential. Never resolve it against another cwd.
export type ExecutionWorkspace = { cwd: string; root: string; device: string; inode: string; birth: string; grant: string };

export async function bindExecutionWorkspace(trust: WorkspaceTrust, cwd: string, signal?: AbortSignal): Promise<ExecutionWorkspace> {
  const w = await trust.binding(cwd, signal);
  return { cwd, root: w.path, device: String(w.device), inode: String(w.inode), birth: String(w.birth), grant: w.stamp };
}

export function sameExecutionWorkspace(a: ExecutionWorkspace, b: ExecutionWorkspace) {
  return a.cwd === b.cwd && a.root === b.root && a.device === b.device && a.inode === b.inode && a.birth === b.birth && a.grant === b.grant;
}

export const workspaceChanged = "Workspace trust or directory changed. Start a new authorized turn.";

export async function requireExecutionWorkspace(trust: WorkspaceTrust, w: ExecutionWorkspace, signal?: AbortSignal) {
  if (!sameExecutionWorkspace(w, await bindExecutionWorkspace(trust, w.cwd, signal))) throw new Error(workspaceChanged);
}

export function withinDirectory(root: string, path: string) {
  const rel = relative(root, path);
  return !isAbsolute(rel) && rel !== ".." && !rel.startsWith(`..${sep}`);
}

export function executionPath(path: unknown): path is string {
  return typeof path === "string" && path.isWellFormed() && Buffer.byteLength(path) <= 1024 &&
    !/[\\\p{Cc}\p{Zl}\p{Zp}\u202a-\u202e\u2066-\u2069]/u.test(path) &&
    path.split("/").every(part => !!part && part !== "." && part !== ".." && part.toLowerCase() !== ".git");
}

export function allowedExecutionPath(root: string, path: unknown, protectedDirs: readonly string[]): path is string {
  const fold = (value: string) => value.normalize("NFC").toLowerCase();
  return executionPath(path) && !/(?:^|\/)\.git(?:\/|$)/i.test(root) &&
    !protectedDirs.some(dir => withinDirectory(fold(dir), fold(join(root, path))));
}

export const executionFileLimit = 32 * 1024;

export async function readExecutionText(trust: WorkspaceTrust, w: ExecutionWorkspace, path: string, signal?: AbortSignal) {
  await requireExecutionWorkspace(trust, w, signal);
  const value = await readScopedText(w.root, path, executionFileLimit, signal, { exact: true });
  // A grant or directory replaced while the read was outstanding does not
  // authorize releasing the bytes, even if the new directory is trusted too.
  await requireExecutionWorkspace(trust, w, signal);
  return value;
}
