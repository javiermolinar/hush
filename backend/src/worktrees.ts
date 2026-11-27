import { createHash, randomUUID } from "node:crypto";
import { lstat, mkdir, realpath, stat } from "node:fs/promises";
import { isAbsolute, join, relative, sep } from "node:path";
import { validPlanCheckoutPath } from "../../server/src/plan.js";
import { WireError } from "../../server/src/wire.js";
import { discoverGitWorkspace, workspaceGit, type DirectoryIdentity, type WorkspaceTrust } from "./workspace-trust.js";

export type ManagedCheckout = { cwd: string; branch: string; repository: string };
export type PlannedCheckout = ManagedCheckout & { root: string; head: string };
export type PlanWorktreeIdentity = { directory: string; git: string };
type PlanAllocation = {
  name(signal: AbortSignal): Promise<{ preferred?: string; fallback: string }>;
  reserve(checkout: PlannedCheckout): Promise<void>;
};

const directoryKey = ({ path, device, inode, birth }: DirectoryIdentity) => `${path}\0${device}:${inode}:${birth}`;
const slug = (label: string | undefined) => label?.normalize("NFKD").toLowerCase().replace(/[^a-z0-9]+/g, "-")
  .replace(/^-|-$/g, "").slice(0, 40).replace(/-$/, "");

export async function planWorktreeIdentity({ cwd, root, branch, head }: PlannedCheckout, signal: AbortSignal,
    source?: DirectoryIdentity): Promise<PlanWorktreeIdentity | undefined> {
  try {
    const git = await discoverGitWorkspace(cwd, signal);
    if (git.cwd.path !== cwd || git.root.path !== root || git.gitDir.path === git.path ||
        source && directoryKey(git) !== directoryKey(source)) return;
    if (await workspaceGit(cwd, signal, "symbolic-ref", "HEAD") !== `refs/heads/${branch}\n` ||
        await workspaceGit(cwd, signal, "rev-parse", "--verify", "HEAD^{commit}") !== `${head}\n`) return;
    return { directory: `${git.cwd.device}:${git.cwd.inode}:${git.cwd.birth}`, git: createHash("sha256")
      .update(JSON.stringify([git.cwd, git.root, git.gitDir, git].map(directoryKey))).digest("hex") };
  } catch { signal.throwIfAborted(); }
}

export async function checkoutIdentity(cwd: string) {
  const info = await lstat(cwd, { bigint: true }).catch(() => undefined);
  if (!info?.isDirectory() || await realpath(cwd).catch(() => undefined) !== cwd) return;
  return `${info.dev}:${info.ino}:${info.birthtimeNs}`;
}

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

// A branch selects only an already registered checkout. Both the source and
// target need their own valid directory grant; lookup never grants or creates.
export async function existingWorktree(trust: WorkspaceTrust, cwd: string, branch: string, signal: AbortSignal) {
  const ref = `refs/heads/${branch}`;
  return trust.withGit(cwd, signal, async source => {
    const listing = await workspaceGit(source.cwd.path, signal, "worktree", "list", "--porcelain", "-z");
    if (!listing.endsWith("\0\0")) throw new WireError("invalid_request");
    const matches = listing.slice(0, -2).split("\0\0").flatMap(row => {
      const [path, ...fields] = row.split("\0");
      return path?.startsWith("worktree ") && fields.includes(`branch ${ref}`) &&
        !fields.some(f => f === "bare" || f === "prunable" || f.startsWith("prunable ")) ? [path.slice(9)] : [];
    });
    if (matches.length !== 1) throw new WireError("not_found");
    return trust.withGit(matches[0]!, signal, async target => {
      if (target.root.path !== target.cwd.path || directoryKey(source) !== directoryKey(target) ||
          await workspaceGit(target.cwd.path, signal, "symbolic-ref", "HEAD") !== `${ref}\n`) throw new WireError("not_found");
      return target.cwd.path;
    });
  });
}

// An explicit opt-in starts from committed HEAD, never from uncommitted files.
// Worktrees live beside the private backend DB, not inside the main checkout.
// Archive is metadata only. In particular, never delete a path on an unknown
// git/DB outcome: even a failed allocation can have already populated files.
export class ThreadWorktrees {
  #active = 0;

  constructor(private readonly dataDir: string, private readonly workspaceTrust: WorkspaceTrust) {}

  create(workspace: string, signal: AbortSignal): Promise<ManagedCheckout>;
  create(workspace: string, signal: AbortSignal, plan: PlanAllocation): Promise<PlannedCheckout & { identity: PlanWorktreeIdentity }>;
  async create(workspace: string, signal: AbortSignal, plan?: PlanAllocation): Promise<ManagedCheckout | (PlannedCheckout & { identity: PlanWorktreeIdentity })> {
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
      const identity = await this.workspaceTrust.withGit(workspace, signal, value => value);
      const { root: { path: root }, cwd: { path: source }, path: common } = identity;
      const first = (await git(source, "worktree", "list", "--porcelain", "-z")).split("\0", 1)[0];
      if (!first?.startsWith("worktree ")) throw new WireError("invalid_request");
      const primary = await existingDirectory(first.slice("worktree ".length));
      const base = join(await existingDirectory(this.dataDir), "worktrees");
      if ([primary, root, common].some(parent => within(parent, base))) throw new WireError("invalid_request");
      await mkdir(base, { mode: 0o700 }).catch((e: NodeJS.ErrnoException) => { if (e.code !== "EEXIST") throw e; });
      const info = await lstat(base);
      if (!info.isDirectory() || (info.mode & 0o777) !== 0o700 || info.uid !== process.getuid?.()) throw new WireError("invalid_request");
      const head = line(await git(source, "rev-parse", "--verify", "HEAD^{commit}"));
      if (!/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(head)) throw new WireError("invalid_request");
      const subdir = plan ? relative(root, source) : "";
      if (subdir) {
        const kind = await git(source, "cat-file", "-t", `${head}:${subdir}`).catch(() => { signal.throwIfAborted(); });
        if (kind !== "tree\n") {
          const error = new WireError("invalid_request");
          error.message = "The plan's workspace directory does not exist in committed HEAD.";
          throw error;
        }
      }
      const nonce = randomUUID(), name = plan && await plan.name(signal);
      const prefix = slug(name?.preferred) || slug(name?.fallback);
      const key = prefix ? `${prefix}-${nonce}` : nonce;
      const target = join(base, key), cwd = join(target, subdir), branch = "hush/" + key;
      if (Array.from(cwd).length > 4096 || plan && (!validPlanCheckoutPath(cwd) || !validPlanCheckoutPath(target))) {
        throw new WireError("invalid_request");
      }
      const allocation: PlannedCheckout = { cwd, root: target, branch, head, repository: primary };
      await plan?.reserve(allocation); // publish a discoverable thread before touching branches or files
      // Once reserved, finish independently of the peer, with a bounded Git operation.
      const write = plan ? AbortSignal.timeout(40_000) : signal;
      await this.workspaceTrust.withGit(workspace, write,
        () => workspaceGit(source, write, "worktree", "add", "-b", branch, "--", target, head), identity);
      if (!plan) return { cwd, branch, repository: primary };
      const verified = await planWorktreeIdentity(allocation, write, identity);
      if (!verified) throw new WireError("invalid_request");
      return { ...allocation, identity: verified };
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
