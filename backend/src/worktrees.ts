import { randomUUID } from "node:crypto";
import { lstat, mkdir, realpath, stat } from "node:fs/promises";
import { isAbsolute, join, relative, sep } from "node:path";
import { WireError } from "../../server/src/wire.js";
import { workspaceGit, type WorkspaceTrust } from "./workspace-trust.js";

export type ManagedCheckout = { cwd: string; branch: string; repository: string };

const within = (parent: string, path: string) => {
  const diff = relative(parent, path);
  return diff === "" || diff !== ".." && !diff.startsWith(".." + sep) && !isAbsolute(diff);
};

export async function existingDirectory(path: string): Promise<string> {
  const canonical = await realpath(path).catch(() => undefined);
  if (!canonical || !(await stat(canonical).catch(() => undefined))?.isDirectory()) throw new WireError("not_found");
  if (!canonical.isWellFormed() || Array.from(canonical).length > 4096) throw new WireError("invalid_request");
  return canonical;
}

// An explicit opt-in starts from committed HEAD, never from uncommitted files.
// Worktrees live beside the private backend DB, not inside the main checkout.
// Archive is metadata only. In particular, never delete a path on an unknown
// git/DB outcome: even a failed allocation can have already populated files.
export class ThreadWorktrees {
  #active = 0;

  // Supply Hush trust only for the standalone profile. It replaces, rather
  // than augments, the legacy Pi predicate in that profile.
  constructor(private readonly dataDir: string, private readonly trusted: (root: string) => boolean,
    private readonly workspaceTrust?: WorkspaceTrust) {}

  async create(workspace: string, signal: AbortSignal): Promise<ManagedCheckout> {
    if (this.#active >= 4) throw new WireError("limit");
    this.#active++;
    try {
      signal.throwIfAborted();
      if (/[\r\n]/.test(workspace)) throw new WireError("invalid_request");
      const git = (cwd: string, ...args: string[]) => workspaceGit(cwd, signal, ...args);
      const line = (s: string) => {
        const value = s.replace(/\r?\n$/, "");
        if (!value || /[\0\r\n]/.test(value)) throw new WireError("invalid_request");
        return value;
      };
      // Bind discovery and all checkout commands to the same authorized
      // source. The workspace can be a directory within the checkout.
      const identity = await this.workspaceTrust?.withGit(workspace, signal, value => value);
      const root = identity?.root.path ?? await existingDirectory(line(await git(workspace, "rev-parse", "--show-toplevel")));
      const source = identity?.cwd.path ?? root;
      const common = identity?.path ?? await existingDirectory(line(await git(root, "rev-parse", "--path-format=absolute", "--git-common-dir")));
      const first = (await git(source, "worktree", "list", "--porcelain", "-z")).split("\0", 1)[0];
      if (!first?.startsWith("worktree ")) throw new WireError("invalid_request");
      const primary = await existingDirectory(first.slice("worktree ".length));
      // A trusted checkout can have repo-local filter drivers. Do not check
      // out an untrusted repository or silently change its trust setting.
      if (!this.workspaceTrust && !this.trusted(root)) throw new WireError("invalid_request");
      const base = join(await existingDirectory(this.dataDir), "worktrees");
      if ([primary, root, common].some(parent => within(parent, base))) throw new WireError("invalid_request");
      await mkdir(base, { mode: 0o700 }).catch((e: NodeJS.ErrnoException) => { if (e.code !== "EEXIST") throw e; });
      const info = await lstat(base);
      if (!info.isDirectory() || (info.mode & 0o777) !== 0o700 || info.uid !== process.getuid?.()) throw new WireError("invalid_request");
      const head = line(await git(source, "rev-parse", "--verify", "HEAD^{commit}"));
      if (!/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(head)) throw new WireError("invalid_request");
      const nonce = randomUUID();
      const cwd = join(base, nonce), branch = "hush/" + nonce;
      if (Array.from(cwd).length > 4096) throw new WireError("invalid_request");
      const checkout = () => git(source, "worktree", "add", "-b", branch, "--", cwd, head);
      if (this.workspaceTrust) {
        if (!identity) throw new WireError("invalid_request");
        await this.workspaceTrust.withGit(workspace, signal, checkout, identity);
      } else await checkout();
      return { cwd, branch, repository: primary };
    } catch (error) {
      signal.throwIfAborted();
      // No stderr or local git configuration on the wire.
      if (error instanceof WireError) throw error;
      throw new WireError("invalid_request");
    } finally {
      this.#active--;
    }
  }
}
