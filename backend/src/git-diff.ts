import { randomUUID } from "node:crypto";
import { lstat } from "node:fs/promises";
import { join } from "node:path";
import { BACKGROUND_CONTEXT, withAbortSignal, type Context } from "@earendil-works/chord/context";
import { AgentDoc, type ConversationId, type Harness } from "@earendil-works/pi-durable";
import { gitDiffFiles, gitDiffLines, type GitDiffFile, type GitDiffHost, type GitDiffLine, type GitDiffRange,
  type GitDiffRequest, type GitDiffSummary, type GitDiffUpdate, type GitDiffWatch } from "../../server/src/git-diff.js";
import { WireError } from "../../server/src/wire.js";
import type { CheckpointService } from "./checkpoints.js";
import { checkFilters } from "./rewind-git.js";
import { workspaceGit, type DirectoryIdentity, type WorkspaceTrust, type GitWorkspaceIdentity } from "./workspace-trust.js";
import { inspectWorkspaceText } from "./workspace-file.js";
import { ChangePulse, observe, WorkspaceRevisions } from "./workspace-changes.js";

const maxObservations = 128;
const maxWatchThreads = 64;
const checkEvery = 500;
const fallback = 20_000;
const lifetime = 2 * 60_000;
const oid = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const maxLine = 2048;
const diffFlags = ["--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules=dirty", "--submodule=short"];
const git = (cwd: string, signal: AbortSignal, ...args: string[]) =>
  workspaceGit(cwd, signal, "--no-replace-objects", "--literal-pathspecs", ...args);
const regular = (mode: string) => mode === "000000" || mode === "100644" || mode === "100755";
const raw = /^:(\d{6}) (\d{6}) [0-9a-f]+ [0-9a-f]+ ([ADMTU])$/;
const validPath = (path: string) => Buffer.byteLength(path) <= 1024 &&
  path.split("/").every(part => !!part && part !== "." && part !== ".." && part.toLowerCase() !== ".git");

function unavailable(message = "Git review is unavailable for this worktree."): never {
  const error = new WireError("invalid_request");
  error.message = message;
  throw error;
}

function nulList(output: string) {
  if (!output) return [];
  if (!output.endsWith("\0")) unavailable();
  return output.slice(0, -1).split("\0");
}

function parseRows(output: string, staged: Set<string>) {
  const list = nulList(output), rows = new Map<string, GitDiffFile>();
  let i = 0;
  for (; i < list.length && list[i]!.startsWith(":"); i += 2) {
    const match = raw.exec(list[i]!), path = list[i + 1];
    if (!match || path === undefined || !validPath(path) || rows.has(path)) unavailable();
    if (rows.size >= gitDiffFiles) throw new WireError("limit");
    rows.set(path, { path, status: match[3]! as GitDiffFile["status"], staged: staged.has(path),
      text: match[3] !== "U" && regular(match[1]!) && regular(match[2]!) });
  }
  for (; i < list.length; i++) {
    const match = /^([0-9]+|-)\t([0-9]+|-)\t([\s\S]+)$/.exec(list[i]!);
    const file = match && rows.get(match[3]!);
    if (!file || !match) unavailable();
    if (match[1] === "-" || match[2] === "-") file.text = false;
    if (file.text) {
      file.added = Number(match[1]);
      file.removed = Number(match[2]);
      if (!Number.isSafeInteger(file.added) || !Number.isSafeInteger(file.removed)) unavailable();
    }
  }
  return rows;
}

function textLines(text: string) {
  const lines = text.split("\n");
  if (lines.at(-1) === "") lines.pop();
  return lines;
}

function boundedPatch(lines: Iterable<GitDiffLine>) {
  let truncated = false, bytes = 0;
  const out: GitDiffLine[] = [];
  for (const line of lines) {
    if (line.text.length > maxLine) {
      line.text = line.text.slice(0, maxLine - 1).toWellFormed() + "…";
      truncated = true;
    }
    bytes += Buffer.byteLength(line.text) + 80; // allow for the structural fields too
    if (out.length >= gitDiffLines || bytes > 128 * 1024) { truncated = true; break; }
    out.push(line);
  }
  if (!out.length) out.push({ kind: "note", text: "No textual changes (metadata only or worktree changed)." });
  return { lines: out, truncated };
}

function* unifiedPatch(patch: string): IterableIterator<GitDiffLine> {
  let inHunk = false, oldLine = 0, newLine = 0;
  for (const line of textLines(patch)) {
    if (line.startsWith("@@ ")) {
      const match = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
      if (!match) unavailable();
      oldLine = Number(match[1]); newLine = Number(match[2]); inHunk = true;
      yield { kind: "hunk", text: line };
    } else if (line.startsWith("diff --git ")) inHunk = false;
    else if (inHunk) {
      const text = line.slice(1);
      if (line[0] === " ") yield { kind: "context", oldLine: oldLine++, newLine: newLine++, text };
      else if (line[0] === "+") yield { kind: "add", newLine: newLine++, text };
      else if (line[0] === "-") yield { kind: "remove", oldLine: oldLine++, text };
      else if (line[0] === "\\") yield { kind: "note", text: line };
    }
  }
}

function* addedPatch(content: string): IterableIterator<GitDiffLine> {
  const lines = textLines(content);
  if (!lines.length) return;
  yield { kind: "hunk", text: `@@ -0,0 +1,${lines.length} @@` };
  for (const [i, text] of lines.entries()) yield { kind: "add", newLine: i + 1, text };
  if (!content.endsWith("\n")) yield { kind: "note", text: "\\ No newline at end of file" };
}

async function untrackedText(root: string, path: string, signal: AbortSignal) {
  try { return await inspectWorkspaceText(root, path, signal); }
  catch { signal.throwIfAborted(); return undefined; }
}

type Summary = Omit<GitDiffSummary, "revision">;
type Observation = { threadId: string; cwd: string; identity: GitWorkspaceIdentity; range: GitDiffRange;
  commit: string; files: GitDiffFile[]; expires: number };
type WorktreeSnapshot = Pick<Summary, "commit" | "files" | "branch"> & { stamp: string; expires: number };
type Worktree = { identity: GitWorkspaceIdentity; version: number; value?: WorktreeSnapshot;
  flight?: Promise<WorktreeSnapshot> };
type ThreadSnapshot = { revision: string } & ({ summary: Summary; cwd: string; identity: GitWorkspaceIdentity } |
  { error: { code: string; message: string } });
type WatchedThread = { peers: number; changes: ChangePulse; stop: AbortController; tree?: Worktree;
  read?: Promise<ThreadSnapshot> };

const directoryKey = (dir: DirectoryIdentity) => [dir.path, String(dir.device), String(dir.inode), String(dir.birth)];
const treeKey = (id: GitWorkspaceIdentity) => JSON.stringify([directoryKey(id.root), directoryKey(id.gitDir), directoryKey(id)]);

async function treeStamp({ root, gitDir, path: common }: GitWorkspaceIdentity, branch?: string) {
  // HEAD and index belong to the linked checkout; the current ref and
  // packed-refs may live in the shared repository. Only stat metadata here.
  const paths = [join(root.path, ".git"), join(gitDir.path, "HEAD"), join(gitDir.path, "index"),
    join(gitDir.path, "logs/HEAD"), join(gitDir.path, "config.worktree"), join(common, "packed-refs"),
    join(common, "config"), join(common, "reftable/tables.list")];
  if (branch && validPath(branch)) paths.push(join(common, "refs/heads", branch));
  return (await Promise.all(paths.map(async path => {
    const s = await lstat(path, { bigint: true }).catch(() => undefined);
    return s ? `${s.dev}:${s.ino}:${s.size}:${s.mtimeNs}:${s.ctimeNs}` : "-";
  }))).join("/");
}

export class GitDiffService implements GitDiffHost {
  readonly #seen = new Map<string, Observation>();
  readonly #trees = new Map<string, Worktree>();
  readonly #watching = new Map<string, WatchedThread>();
  readonly #revisions = new WorkspaceRevisions();
  readonly #stop = new AbortController();
  // Bound subprocess fanout on a change affecting many checkouts at once.
  readonly #lanes = Array.from({ length: 4 }, () => Promise.resolve());
  #nextLane = 0;
  #queued = 0;
  #active = 0;
  #timer?: NodeJS.Timeout;
  #probing = false;
  #nextFallback = 0;

  constructor(private readonly h: Harness, private readonly trust: WorkspaceTrust, private readonly checkpoints: CheckpointService) {}

  close() {
    clearInterval(this.#timer);
    this.#stop.abort();
    this.#trees.clear();
    this.#seen.clear();
  }

  invalidate() {
    // Dirty the bounded cache and wake waiters; each watch rechecks its own
    // authority before using the shared worktree scan.
    for (const tree of this.#trees.values()) this.#dirty(tree);
    for (const thread of this.#watching.values()) {
      if (!thread.tree) thread.changes.emit();
    }
  }

  #dirty(tree: Worktree) {
    tree.version++;
    tree.value = undefined;
    for (const thread of this.#watching.values()) if (thread.tree === tree) thread.changes.emit();
  }

  async #probe() {
    if (this.#probing || !this.#watching.size) return;
    if (Date.now() >= this.#nextFallback) {
      this.#nextFallback = Date.now() + fallback;
      // Tree edits that don't touch Git metadata (including external editors)
      // have no reliable portable notification; refresh once per worktree.
      this.invalidate();
      return;
    }
    this.#probing = true;
    try {
      const trees = new Set([...this.#watching.values()].flatMap(t => t.tree ? [t.tree] : []));
      await Promise.all([...trees].map(async tree => {
        const value = tree.value;
        if (!value) return;
        const stamp = await treeStamp(tree.identity, value.branch);
        if (!this.#stop.signal.aborted && tree.value === value && value.stamp !== stamp) this.#dirty(tree);
      }));
    } finally { this.#probing = false; }
  }

  async wait({ threadId, after }: GitDiffWatch, peer: AbortSignal): Promise<GitDiffUpdate> {
    const signal = AbortSignal.any([peer, this.#stop.signal]);
    signal.throwIfAborted();
    let thread = this.#watching.get(threadId);
    if (!thread) {
      if (this.#watching.size >= maxWatchThreads) throw new WireError("limit");
      thread = { peers: 0, changes: new ChangePulse(), stop: new AbortController() };
      this.#watching.set(threadId, thread);
    }
    if (!this.#timer) {
      this.#nextFallback = Date.now() + fallback;
      this.#timer = setInterval(() => { void this.#probe(); }, checkEvery).unref();
    }
    thread.peers++;
    try {
      for (;;) {
        const generation = thread.changes.generation;
        const view = await observe(this.#watchSnapshot(threadId, thread), signal);
        signal.throwIfAborted();
        if (generation !== thread.changes.generation) continue;
        if (view.revision !== after) {
          if ("error" in view) return view;
          const { revision, summary, cwd, identity } = view;
          return { revision, summary: this.#remember(threadId, cwd, identity, summary) };
        }
        await thread.changes.wait(generation, signal);
      }
    } finally {
      if (--thread.peers === 0) {
        thread.stop.abort();
        this.#watching.delete(threadId);
      }
      if (!this.#watching.size) {
        clearInterval(this.#timer);
        this.#timer = undefined;
      }
    }
  }

  #watchSnapshot(threadId: string, thread: WatchedThread) {
    if (thread.read) return thread.read;
    if (this.#queued >= maxWatchThreads) throw new WireError("limit");
    this.#queued++;
    const lane = this.#nextLane++ % this.#lanes.length;
    const read = this.#lanes[lane]!.then(() => this.#readWatch(threadId, thread)).finally(() => {
      thread.read = undefined;
      this.#queued--;
    });
    this.#lanes[lane] = read.then(() => {}, () => {});
    return thread.read = read;
  }

  async #readWatch(threadId: string, thread: WatchedThread): Promise<ThreadSnapshot> {
    const life = AbortSignal.any([thread.stop.signal, this.#stop.signal]);
    const signal = AbortSignal.any([life, AbortSignal.timeout(4_000)]);
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT), id = Number(threadId) as ConversationId;
    try {
      const cwd = await this.#cwd(id, ctx);
      // Each thread must still authorize its own exact directory. A shared
      // checkout, or a cached successful observation, is not a grant.
      const identity = await this.trust.withGit(cwd, signal, value => value);
      const key = treeKey(identity), tree = thread.tree = this.#worktree(key, identity);
      const [{ commit, files, branch }, checkpoint] = await Promise.all([
        this.#headStats(tree, signal), this.checkpoints.latestFileSnapshot(id, ctx),
      ]);
      await this.#verify(id, cwd, identity, ctx, signal);
      const summary: Summary = { range: "head", commit, checkpoint: checkpoint !== undefined, files,
        ...(branch && { branch }) };
      return { revision: this.#revisions.of([threadId, cwd, key, summary]), summary, cwd, identity };
    } catch (failure) {
      life.throwIfAborted();
      thread.tree = undefined;
      const e = signal.aborted ? new WireError("limit") : failure instanceof WireError ? failure : new WireError("internal");
      const error = { code: e.code, message: e.message };
      return { revision: this.#revisions.of([threadId, error]), error };
    }
  }

  #worktree(key: string, identity: GitWorkspaceIdentity) {
    const cached = this.#trees.get(key);
    if (cached) {
      this.#trees.delete(key);
      this.#trees.set(key, cached);
      return cached;
    }
    if (this.#trees.size >= maxObservations) {
      const attached = new Set([...this.#watching.values()].map(t => t.tree));
      for (const [old, tree] of this.#trees) {
        if (tree.flight || attached.has(tree)) continue;
        this.#trees.delete(old);
        break;
      }
      if (this.#trees.size >= maxObservations) throw new WireError("limit");
    }
    const tree: Worktree = { identity, version: 0 };
    this.#trees.set(key, tree);
    return tree;
  }

  async #headStats(tree: Worktree, signal: AbortSignal) {
    for (;;) {
      signal.throwIfAborted();
      if (tree.value && tree.value.expires > Date.now()) return tree.value;
      const version = tree.version;
      // Reuse the flight even if a write dirtied it, then refresh instead of
      // launching overlapping Git processes for the same worktree.
      const flight = tree.flight ??= this.#computeHead(tree.identity).then(result => {
        if (tree.version === version) tree.value = result;
        return result;
      }).finally(() => { tree.flight = undefined; });
      // The peer's wait is already abortable. Keep the worker lane until
      // the shared Git work settles so rapid disconnects can't fan out scans.
      await flight;
      signal.throwIfAborted();
      if (tree.value) return tree.value; // only the current generation publishes

    }
  }

  async #computeHead(identity: GitWorkspaceIdentity): Promise<WorktreeSnapshot> {
    const signal = AbortSignal.any([this.#stop.signal, AbortSignal.timeout(4_000)]);
    const root = identity.root.path;
    await checkFilters(root, signal);
    const branch = await this.#branch(root, signal);
    const stamp = await treeStamp(identity, branch);
    const commit = await this.#head(root, signal);
    const files = await this.#files(root, commit, commit, signal);
    return { commit, files, branch, stamp, expires: Date.now() + fallback };
  }

  async #cwd(id: ConversationId, ctx: Context) {
    const cwd = await this.h.commit(async tx => {
      const source = await tx.conversation(id);
      if (!source || source.owner) throw new WireError("not_found");
      return (await tx.doc(AgentDoc, id)).cwd;
    }, ctx);
    if (!cwd) unavailable("This thread has no worktree.");
    return cwd;
  }

  async #verify(id: ConversationId, cwd: string, identity: GitWorkspaceIdentity, ctx: Context, signal: AbortSignal) {
    if ((await this.h.snapshot(AgentDoc, id, ctx))?.cwd !== cwd) throw new WireError("invalid_cursor");
    await this.trust.withGit(cwd, signal, () => {}, identity);
  }

  async #head(root: string, signal: AbortSignal) {
    const out = await git(root, signal, "rev-parse", "--verify", "HEAD^{commit}");
    const head = out.slice(0, -1);
    if (!out.endsWith("\n") || !oid.test(head)) unavailable("Git review needs a committed HEAD.");
    return head;
  }

  async #base(id: ConversationId, cwd: string, root: string, range: GitDiffRange, ctx: Context, signal: AbortSignal) {
    const snapshot = await this.checkpoints.latestFileSnapshot(id, ctx);
    const head = await this.#head(root, signal);
    if (range === "head") return { head, commit: head, checkpoint: snapshot !== undefined };
    if (!snapshot) unavailable("No file checkpoint on this thread.");
    const commit = await this.checkpoints.comparisonCommit(cwd, snapshot, signal);
    return { head, commit, checkpoint: true };
  }

  async call(q: GitDiffRequest, peer: AbortSignal) {
    if (this.#active >= 4) throw new WireError("limit");
    this.#active++;
    const signal = AbortSignal.any([peer, AbortSignal.timeout(4_000)]);
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT), id = Number(q.threadId) as ConversationId;
    try {
      const input = q.op === "file" ? this.#seen.get(q.revision) : undefined;
      if (q.op === "file" && (input?.threadId !== q.threadId || input.expires < Date.now() || !input.files[q.file]))
        throw new WireError("invalid_cursor");
      const cwd = await this.#cwd(id, ctx);
      if (input && cwd !== input.cwd) throw new WireError("invalid_cursor");
      const identity = await this.trust.withGit(cwd, signal, value => value, input?.identity);
      const root = identity.root.path;
      await checkFilters(root, signal);
      const range = input?.range ?? (q.op === "summary" ? q.range : "head");
      const { commit, head, checkpoint } = await this.#base(id, cwd, root, range, ctx, signal);
      if (q.op === "file" && input) {
        if (input.commit !== commit) throw new WireError("invalid_cursor");
        const file = input.files[q.file]!;
        const patch = file.status === "?" ? await this.#untrackedPatch(root, file.path, signal)
          : await this.#trackedPatch(root, commit, file.path, signal);
        const result = { revision: q.revision, file: q.file, ...boundedPatch(patch) };
        await this.#verify(id, cwd, identity, ctx, signal);
        return result;
      }
      const [files, branch] = await Promise.all([this.#files(root, commit, head, signal), this.#branch(root, signal)]);
      await this.#verify(id, cwd, identity, ctx, signal);
      return this.#remember(q.threadId, cwd, identity, { range, commit, checkpoint, files, ...(branch && { branch }) });
    } catch (error) {
      peer.throwIfAborted();
      if (signal.aborted) throw new WireError("limit");
      if (error instanceof WireError) throw error;
      unavailable();
    } finally { this.#active--; }
  }

  #remember(threadId: string, cwd: string, identity: GitWorkspaceIdentity, summary: Summary): GitDiffSummary {
    const revision = randomUUID(), { range, commit, files } = summary;
    while (this.#seen.size >= maxObservations) this.#seen.delete(this.#seen.keys().next().value!);
    this.#seen.set(revision, { threadId, cwd, identity, range, commit, files, expires: Date.now() + lifetime });
    return { revision, ...summary };
  }

  async #branch(root: string, signal: AbortSignal) {
    const ref = await git(root, signal, "rev-parse", "--symbolic-full-name", "HEAD").catch(() => "");
    // Detached HEAD is not a local branch. Treat unavailable metadata as optional.
    const branch = /^refs\/heads\/([^\x00-\x20\x7f]+)\n$/.exec(ref)?.[1];
    return branch && Buffer.byteLength(branch) <= 1024 ? branch : undefined;
  }

  async #files(root: string, commit: string, head: string, signal: AbortSignal) {
    const [diff, index, others] = await Promise.all([
      git(root, signal, "diff", ...diffFlags, "--raw", "--numstat", "-z", commit, "--"),
      git(root, signal, "diff", ...diffFlags, "--cached", "--name-only", "-z", head, "--"),
      git(root, signal, "ls-files", "--others", "--exclude-standard", "-z", "--"),
    ]);
    const rows = parseRows(diff, new Set(nulList(index)));
    for (const path of nulList(others)) {
      if (rows.has(path)) continue;
      if (rows.size >= gitDiffFiles) throw new WireError("limit");
      if (!validPath(path)) unavailable();
      // Don't read file contents for a badge or list. The file endpoint
      // verifies that this is still an untracked, safe text path on demand.
      rows.set(path, { path, status: "?", staged: false, text: false });
    }
    return [...rows.values()].sort((a, b) => a.path < b.path ? -1 : a.path > b.path ? 1 : 0);
  }

  async #trackedPatch(root: string, commit: string, path: string, signal: AbortSignal): Promise<Iterable<GitDiffLine>> {
    const stats = await git(root, signal, "diff", ...diffFlags, "--raw", "--numstat", "-z", commit, "--", path);
    const rows = parseRows(stats, new Set()), row = rows.get(path);
    if (!row || rows.size !== 1) throw new WireError("invalid_cursor");
    if (!row.text) return [{ kind: "note", text: "Binary, link, submodule, or unresolved change (no text preview)." }];
    const patch = await git(root, signal, "diff", ...diffFlags, "--patch", "--unified=3",
      "--output-indicator-new=+", "--output-indicator-old=-", "--output-indicator-context= ", commit, "--", path);
    // A literal pathspec can still match descendants of a directory. If a
    // reviewed path becomes one, do not return patches for its children.
    if (patch.indexOf("\ndiff --git ") >= 0) throw new WireError("invalid_cursor");
    return unifiedPatch(patch);
  }

  async #untrackedPatch(root: string, path: string, signal: AbortSignal): Promise<Iterable<GitDiffLine>> {
    const others = await git(root, signal, "ls-files", "--others", "--exclude-standard", "-z", "--", path);
    if (!nulList(others).includes(path)) throw new WireError("invalid_cursor");
    const content = await untrackedText(root, path, signal);
    return content === undefined ? [{ kind: "note", text: "Untracked file has no preview (not small, regular UTF-8 text)." }] : addedPatch(content);
  }
}
