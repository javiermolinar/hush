import { createHash } from "node:crypto";
import { lstat } from "node:fs/promises";
import { join, relative } from "node:path";
import { withAbortSignal, type Context } from "@earendil-works/chord/context";
import { AgentDoc, defineDocFamily, defineExtension, defineTool, type ConversationId, type Harness, type Tx } from "@earendil-works/pi-durable";
import { allowedExecutionPath, bindExecutionWorkspace, executionFileLimit, executionPath, readExecutionText, requireExecutionWorkspace,
  type ExecutionWorkspace } from "./execution-workspace.js";
import { checkPlanWorktree } from "./plan.js";
import { reviewReadTool } from "./tool-policy.js";
import { workspaceGit, workspaceHasIgnoredPaths, type DirectoryIdentity, type GitWorkspaceIdentity, type WorkspaceTrust } from "./workspace-trust.js";

const pathLimit = 16, snapshotLimit = 256 * 1024, captureMs = 8_000;
const objectId = /^[a-f0-9]{40}(?:[a-f0-9]{24})?$/;
const digest = (text: string) => createHash("sha256").update(text, "utf8").digest("hex");
const json = (value: unknown) => JSON.stringify(value);

export type CodeReviewScope = { tracked: readonly string[]; untracked: readonly string[] };
type Scope = { tracked: string[]; untracked: string[] };
type Mode = "100644" | "100755";
type GitFile = { mode: Mode; oid: string };
type Version = { mode: Mode; hash: string; text: string };
type SnapshotFile = { path: string; base: Version | null; staged: Version | null; working: Version | null; stamp?: string };
type Captured = { owner: ConversationId; workspace: ExecutionWorkspace; checkout: string[]; subdirectory: string; base: string;
  scope: Scope; files: SnapshotFile[] };
const Snapshots = defineDocFamily<{ target?: Captured }, null>({
  kind: "hush.code-review-snapshot", family: true, version: 1, scope: "session",
  initial: () => ({}), checkpointWhen: () => true,
});

class ReviewSnapshotError extends Error {}
const unavailable = () => new ReviewSnapshotError("The exact code review target is unavailable or outside the admitted scope.");
const changed = () => new ReviewSnapshotError("The code review target changed during capture. Select the target again.");
const workspaceIdentity = ({ path, device, inode, birth }: DirectoryIdentity) => `${path}\0${device}:${inode}:${birth}`;
const checkoutIdentity = (git: GitWorkspaceIdentity) => [workspaceIdentity(git.root), workspaceIdentity(git.gitDir), workspaceIdentity(git)];
function exactScope(scope: CodeReviewScope): Scope {
  const all = [...scope.tracked, ...scope.untracked];
  if (!all.length || all.length > pathLimit || all.some(p => !executionPath(p)) || new Set(all).size !== all.length) throw unavailable();
  return { tracked: scope.tracked.toSorted(), untracked: scope.untracked.toSorted() };
}

function records(stdout: string) {
  if (!stdout) return [];
  if (!stdout.endsWith("\0")) throw unavailable();
  return stdout.slice(0, -1).split("\0");
}

function gitFiles(stdout: string, paths: ReadonlySet<string>, index: boolean) {
  const pattern = index ? /^H (100644|100755) ([a-f0-9]+) 0\t(.+)$/ : /^(100644|100755) blob ([a-f0-9]+)\t(.+)$/;
  const files = new Map<string, GitFile>();
  for (const record of records(stdout)) {
    const match = pattern.exec(record);
    if (!match || !objectId.test(match[2]!) || !paths.has(match[3]!) || files.has(match[3]!)) throw unavailable();
    files.set(match[3]!, { mode: match[1]! as Mode, oid: match[2]! });
  }
  return files;
}

function confirmStaging(stdout: string, paths: readonly string[], tree: ReadonlyMap<string, GitFile>, index: ReadonlyMap<string, GitFile>, oidLength: number) {
  // ls-files presents intent-to-add as an ordinary empty blob. The cached
  // diff hides those placeholders. Require both views of the index to agree.
  const missing = "0".repeat(oidLength);
  const expected = new Map<string, string>();
  for (const path of paths) {
    const before = tree.get(path), after = index.get(path);
    if (before?.mode === after?.mode && before?.oid === after?.oid) continue;
    const status = !before ? "A" : !after ? "D" : "M";
    expected.set(path, `:${before?.mode ?? "000000"} ${after?.mode ?? "000000"} ${before?.oid ?? missing} ${after?.oid ?? missing} ${status}`);
  }
  const raw = records(stdout);
  for (let i = 0; i < raw.length; i += 2) {
    const path = raw[i + 1];
    if (path === undefined || expected.get(path) !== raw[i]) throw unavailable();
    expected.delete(path);
  }
  if (expected.size) throw unavailable();
}

async function existsAsFile(root: string, path: string, signal: AbortSignal) {
  // A missing file is a deletion, not an excuse to follow a replaced parent
  // or to accept a directory, special file, symlink or hardlink at the name.
  const segments = path.split("/");
  let name = root;
  for (const [index, segment] of segments.entries()) {
    signal.throwIfAborted();
    name = join(name, segment);
    try {
      const info = await lstat(name);
      if (index === segments.length - 1 ? !info.isFile() || info.nlink !== 1 : !info.isDirectory()) throw unavailable();
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
      throw error;
    }
  }
  return true;
}

function manifest(id: string, { base, subdirectory, scope, files }: Captured) {
  const describe = (value: Version | null) => value && ({ mode: value.mode, hash: value.hash });
  const same = (a: Version | null, b: Version | null) => a?.hash === b?.hash && a?.mode === b?.mode;
  return { kind: "code" as const, id, base, subdirectory, scope, files: files.map(file => ({ path: file.path,
    base: describe(file.base), staged: describe(file.staged), working: describe(file.working),
    stagedChange: !same(file.base, file.staged), unstagedChange: scope.tracked.includes(file.path) && !same(file.staged, file.working),
  })) };
}
const failure = { output: [{ type: "text" as const, text: "That input is not available in this code review snapshot." }], isError: true };

// Host-only capture and freshness checks. Neither starts a worker nor refreshes
// the frozen input of one already admitted. No Git commands run in the tool.
export class ReviewSnapshots {
  constructor(private readonly h: () => Harness, private readonly trust: WorkspaceTrust,
    private readonly directory: (cwd?: string) => string, private readonly protectedDirs: readonly string[]) {}

  async #ownerCwd(tx: Tx, owner: ConversationId) {
    const conversation = await tx.conversation(owner);
    if (!conversation || conversation.owner) throw unavailable();
    return this.directory((await tx.doc(AgentDoc, owner)).cwd);
  }

  async #sample(owner: ConversationId, scope: Scope, workspace: ExecutionWorkspace, identity: GitWorkspaceIdentity, signal: AbortSignal): Promise<Captured> {
    await requireExecutionWorkspace(this.trust, workspace, signal);
    const root = identity.root.path;
    const subdirectory = relative(root, workspace.root);
    const protectedDirs = [...this.protectedDirs, identity.gitDir.path, identity.path];
    const paths = [...scope.tracked, ...scope.untracked];
    if (paths.some(path => !allowedExecutionPath(workspace.root, path, protectedDirs))) throw unavailable();
    const repoPath = (p: string) => subdirectory ? `${subdirectory}/${p}` : p;
    const names = paths.map(repoPath), wanted = new Set(names);
    const git = (...args: string[]) => workspaceGit(root, signal, "--no-replace-objects", "--literal-pathspecs", ...args);
    const head = await git("rev-parse", "--verify", "HEAD^{commit}");
    const base = head.slice(0, -1);
    if (!head.endsWith("\n") || !objectId.test(base)) throw unavailable();
    // Compare only the pinned tree and cached index. Do not diff worktree
    // files or run filters; worktree bytes come from the scoped reader.
    const [tree, staged, cached, other, ignored] = await Promise.all([
      git("ls-tree", "-z", "--full-tree", base, "--", ...names),
      git("ls-files", "-z", "--stage", "-v", "--", ...names),
      git("diff-index", "--cached", "--raw", "-z", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv",
        "--ignore-submodules=none", "--ita-invisible-in-index", base, "--", ...names),
      scope.untracked.length ? git("ls-files", "-z", "--others", "--exclude-standard", "--", ...scope.untracked.map(repoPath)) : "",
      workspaceHasIgnoredPaths(root, names, signal),
    ]);
    if (ignored) throw unavailable();
    const old = gitFiles(tree, wanted, false), index = gitFiles(staged, wanted, true);
    confirmStaging(cached, names, old, index, base.length);
    const untracked = records(other);
    if (untracked.length !== scope.untracked.length || new Set(untracked).size !== untracked.length ||
        scope.untracked.some(p => !untracked.includes(repoPath(p)) || index.has(repoPath(p)))) throw unavailable();
    if (scope.tracked.some(p => !old.has(repoPath(p)) && !index.has(repoPath(p)))) throw unavailable();

    const blobs = new Map<string, string>();
    let bytes = 0;
    const count = (text: string) => {
      const size = Buffer.byteLength(text);
      bytes += size;
      if (size > executionFileLimit || bytes > snapshotLimit || text.includes("\0")) throw unavailable();
    };
    async function version(file: GitFile | undefined): Promise<Version | null> {
      if (!file) return null;
      let text = blobs.get(file.oid);
      if (text === undefined) {
        text = await git("cat-file", "blob", file.oid);
        blobs.set(file.oid, text);
      }
      count(text);
      return { mode: file.mode, hash: digest(text), text };
    }
    const files: SnapshotFile[] = [];
    for (const path of paths) {
      const key = repoPath(path), inIndex = index.get(key), explicit = scope.untracked.includes(path);
      const present = await existsAsFile(workspace.root, path, signal);
      // A staged deletion can leave a different, now untracked file at the
      // same name. Require separate explicit untracked admission for it.
      if ((present && !inIndex && !explicit) || (!present && explicit)) throw unavailable();
      const live = present ? await readExecutionText(this.trust, workspace, path, signal) : undefined;
      if (live) count(live.text);
      files.push({ path, base: await version(old.get(key)), staged: await version(inIndex),
        working: live ? { mode: live.executable ? "100755" : "100644", hash: digest(live.text), text: live.text } : null,
        ...(live && { stamp: live.stamp }),
      });
    }
    return { owner, workspace, checkout: checkoutIdentity(identity), subdirectory, base, scope, files };
  }

  async #collect(owner: ConversationId, scope: Scope, ctx: Context) {
    const h = this.h();
    const signal = ctx.abortSignal ? AbortSignal.any([ctx.abortSignal, AbortSignal.timeout(captureMs)]) : AbortSignal.timeout(captureMs);
    const timed = withAbortSignal(signal, ctx);
    const cwd = await h.commit(tx => this.#ownerCwd(tx, owner), timed);
    const workspace = await bindExecutionWorkspace(this.trust, cwd, signal);
    await checkPlanWorktree(h, owner, workspace.root, timed);
    const identity = await this.trust.withGit(cwd, signal, git => git);
    if (workspace.root !== identity.cwd.path || workspace.device !== String(identity.cwd.device) ||
        workspace.inode !== String(identity.cwd.inode) || workspace.birth !== String(identity.cwd.birth)) throw changed();
    const first = await this.#sample(owner, scope, workspace, identity, signal);
    const again = await this.#sample(owner, scope, workspace, identity, signal);
    const data = json(first);
    if (Buffer.byteLength(data) > snapshotLimit) throw unavailable();
    if (data !== json(again)) throw changed();
    // Recheck discovery and grant after all file and Git reads. Capture is an
    // observed snapshot, not a worktree lock or an implementation checkout.
    await this.trust.withGit(cwd, signal, () => {}, identity);
    await requireExecutionWorkspace(this.trust, workspace, signal);
    await checkPlanWorktree(h, owner, workspace.root, timed);
    signal.throwIfAborted();
    return { target: first, id: digest(data) };
  }

  async capture(owner: ConversationId, requested: CodeReviewScope, ctx: Context) {
    try {
      const scope = exactScope(requested);
      const { target, id } = await this.#collect(owner, scope, ctx);
      await this.h().commit(async tx => {
        if (await this.#ownerCwd(tx, owner) !== target.workspace.cwd) throw changed();
        const doc = await tx.doc(Snapshots, id, null);
        if (!doc.target) doc.target = target;
        else if (digest(json(doc.target)) !== id) throw unavailable();
      }, ctx);
      return manifest(id, target);
    } catch (error) {
      ctx.abortSignal?.throwIfAborted();
      // Never project subprocess errors or host paths to a caller.
      throw error instanceof ReviewSnapshotError ? error : unavailable();
    }
  }

  async pin(owner: ConversationId, id: string, ctx: Context) {
    const target = await this.#load(owner, id, ctx);
    await requireExecutionWorkspace(this.trust, target.workspace, ctx.abortSignal);
    const checkout = target.checkout[0]!.split("\0", 2);
    return { workspace: target.workspace, claims: target.files.map(file => ({
      checkout: checkout[1]!, path: target.subdirectory ? `${target.subdirectory}/${file.path}` : file.path,
      ...(file.stamp && { inode: file.stamp.split(":", 2).join(":") }),
    })) };
  }

  async has(tx: Tx, owner: ConversationId, id: string) {
    const target = (await tx.doc(Snapshots, id, null)).target;
    return !!target && target.owner === owner && digest(json(target)) === id;
  }

  async #load(owner: ConversationId, id: string, ctx: Context) {
    if (!/^[a-f0-9]{64}$/.test(id)) throw unavailable();
    const target = (await this.h().snapshot(Snapshots, id, ctx))?.target;
    if (!target || target.owner !== owner || digest(json(target)) !== id) throw unavailable();
    return target;
  }

  async freshness(owner: ConversationId, id: string, requested: CodeReviewScope, ctx: Context) {
    try {
      const target = await this.#load(owner, id, ctx);
      const scope = exactScope(requested);
      if (json(scope) !== json(target.scope)) return { stale: true, reason: "scope_changed" } as const;
      const observed = await this.#collect(owner, scope, ctx);
      if (observed.target.workspace.cwd !== await this.h().commit(tx => this.#ownerCwd(tx, owner), ctx)) throw changed();
      return observed.id === id ? { stale: false } as const : { stale: true, reason: "source_changed" } as const;
    } catch {
      ctx.abortSignal?.throwIfAborted();
      return { stale: true, reason: "unavailable" } as const;
    }
  }

  extension(authorize: (tx: Tx, child: ConversationId) => Promise<{ owner: ConversationId; id: string } | undefined>) {
    const tool = defineTool({ name: reviewReadTool, description: "Read the pinned code review manifest or the exact base, staged and working versions of one admitted path. Omit path for the manifest.",
      parameters: { type: "object", additionalProperties: false, properties: { path: { type: "string", maxLength: 1024 } } } as const,
      callers: ["model"], replay: "safe", outputLimits: { maxBytes: snapshotLimit, maxLines: 8 },
      execute: async ({ path }, api, ctx) => {
        try {
          const readPermit = () => api.commit(async tx => {
            const access = await authorize(tx, api.conversationId);
            return access && { ...access, cwd: await this.#ownerCwd(tx, access.owner) };
          }, ctx);
          const permit = await readPermit();
          if (!permit) return failure;
          const { id, owner, cwd } = permit;
          const target = await this.#load(owner, id, ctx);
          if (cwd !== target.workspace.cwd) return failure;
          await requireExecutionWorkspace(this.trust, target.workspace, ctx.abortSignal);
          // Workspace validation awaits the filesystem; recheck the live assignment before returning bytes.
          const current = await readPermit();
          if (current?.owner !== owner || current.id !== id || current.cwd !== cwd) return failure;
          const file = path === undefined ? undefined : target.files.find(f => f.path === path);
          if (path !== undefined && !file) return failure;
          const content = file ? { path: file.path, base: file.base, staged: file.staged, working: file.working } : manifest(id, target);
          const lines = (version: Version | null) => {
            const text = version?.text ?? "";
            if (!text) return 0;
            let n = text.endsWith("\n") ? 0 : 1;
            for (let i = text.indexOf("\n"); i >= 0; i = text.indexOf("\n", i + 1)) n++;
            return n;
          };
          return { output: [{ type: "text" as const, text: json(content) }],
            ...(file && { details: { snapshotId: id, path: file.path, lines: { base: lines(file.base), staged: lines(file.staged), working: lines(file.working) } } }) };
        } catch {
          ctx.abortSignal?.throwIfAborted();
          return failure;
        }
      },
    });
    return defineExtension({ name: "hush-code-review-snapshot", tools: [tool] });
  }
}
