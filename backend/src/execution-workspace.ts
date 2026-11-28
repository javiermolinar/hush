import { lstat, realpath } from "node:fs/promises";
import { isAbsolute, join, posix, relative, sep } from "node:path";
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

// `checkouts` holds only Hush-managed checkouts. It may sit inside a
// protected directory, such as the backend's data directory, without
// exposing the rest of it.
export function allowedExecutionPath(root: string, path: unknown, protectedDirs: readonly string[], checkouts?: string): path is string {
  const fold = (value: string) => value.normalize("NFC").toLowerCase();
  if (!executionPath(path) || /(?:^|\/)\.git(?:\/|$)/i.test(root)) return false;
  const target = fold(join(root, path));
  return checkouts !== undefined && withinDirectory(fold(checkouts), target) ||
    !protectedDirs.some(dir => withinDirectory(fold(dir), target));
}

// A subagent's working directory, relative to its checkout: "" for the
// checkout itself, undefined if the path leaves it or names Git metadata.
export function delegateDirectory(w: ExecutionWorkspace, path: string) {
  const base = isAbsolute(path) ? [w.cwd, w.root].find(dir => withinDirectory(dir, path)) : w.cwd;
  if (base === undefined) return;
  const rel = posix.normalize(isAbsolute(path) ? relative(base, path) : path).replace(/\/+$/, "");
  if (rel === "." || rel === "") return "";
  return executionPath(rel) ? rel : undefined;
}

// The checkout's binding authorizes the subdirectory. Every component must
// still be a real directory, so a link can't move it out of the checkout.
export async function requireSubdirectory(w: ExecutionWorkspace, pwd: string | undefined) {
  if (!pwd) return { root: w.root, cwd: w.cwd };
  const root = join(w.root, pwd);
  const real = await realpath(root).catch(() => undefined);
  if (real !== root || !(await lstat(root)).isDirectory()) throw new Error(workspaceChanged);
  return { root, cwd: join(w.cwd, pwd) };
}

// A restricted read relative to a subagent's working directory. The result
// is checked as a checkout-relative path, so it can't climb out.
export function delegatePath(pwd: string, path: unknown) {
  return typeof path === "string" && !path.startsWith("/") ? posix.normalize(posix.join(pwd, path)) : path;
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
