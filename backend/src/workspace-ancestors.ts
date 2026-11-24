import { lstat } from "node:fs/promises";
import { dirname, join } from "node:path";

// Walk a physical checkout from nearest to farthest, including linked Git
// worktrees. Without a known repository boundary, only the cwd applies.
export async function workspaceAncestors(cwd: string, signal?: AbortSignal): Promise<string[]> {
  const parents = [cwd];
  for (let dir = cwd;;) {
    signal?.throwIfAborted();
    const git = await lstat(join(dir, ".git")).catch(() => undefined);
    if (git?.isDirectory() || git?.isFile()) return parents;
    const parent = dirname(dir);
    if (parent === dir || parents.length >= 64) return [cwd];
    parents.push(parent);
    dir = parent;
  }
}
