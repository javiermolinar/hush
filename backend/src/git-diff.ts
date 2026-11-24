import { randomUUID } from "node:crypto";
import { realpath } from "node:fs/promises";
import { isAbsolute, relative, sep } from "node:path";
import { BACKGROUND_CONTEXT, withAbortSignal, type Context } from "@earendil-works/chord/context";
import { AgentDoc, type ConversationId, type Harness } from "@earendil-works/pi-durable";
import { gitDiffFiles, gitDiffLines, type GitDiffFile, type GitDiffHost, type GitDiffLine, type GitDiffRange,
  type GitDiffRequest } from "../../server/src/git-diff.js";
import { WireError } from "../../server/src/wire.js";
import type { CheckpointService } from "./checkpoints.js";
import { checkFilters } from "./rewind-git.js";
import { workspaceGit, type WorkspaceTrust, type WorkspaceTrustIdentity } from "./workspace-trust.js";
import { inspectWorkspaceText } from "./workspace-file.js";

const maxObservations = 128;
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

async function checkoutRoot(cwd: string, signal: AbortSignal) {
  const output = await git(cwd, signal, "rev-parse", "--show-toplevel");
  const root = output.slice(0, -1);
  if (!output.endsWith("\n") || !isAbsolute(root) || /[\r\n\0]/.test(root) || await realpath(root) !== root) unavailable();
  const fromRoot = relative(root, cwd);
  if (fromRoot === ".." || fromRoot.startsWith(".." + sep) || isAbsolute(fromRoot)) unavailable();
  return root;
}

type GitIdentity = Extract<WorkspaceTrustIdentity, { kind: "git" }>;
const sameDirectory = (a: GitIdentity["gitDir"], b: GitIdentity["gitDir"]) =>
  a.path === b.path && a.device === b.device && a.inode === b.inode && a.birth === b.birth;

type Observation = { threadId: string; cwd: string; identity: GitIdentity; root: string; range: GitDiffRange;
  commit: string; files: GitDiffFile[]; expires: number };

export class GitDiffService implements GitDiffHost {
  readonly #seen = new Map<string, Observation>();
  #active = 0;

  constructor(private readonly h: Harness, private readonly trust: WorkspaceTrust, private readonly checkpoints: CheckpointService) {}

  async #base(id: ConversationId, cwd: string, root: string, range: GitDiffRange, ctx: Context, signal: AbortSignal) {
    const snapshot = await this.checkpoints.latestFileSnapshot(id, ctx);
    const out = await git(root, signal, "rev-parse", "--verify", "HEAD^{commit}");
    const head = out.slice(0, -1);
    if (!out.endsWith("\n") || !oid.test(head)) unavailable("Git review needs a committed HEAD.");
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
      const cwd = await this.h.commit(async tx => {
        const source = await tx.conversation(id);
        if (!source || source.owner) throw new WireError("not_found");
        return (await tx.doc(AgentDoc, id)).cwd;
      }, ctx);
      if (!cwd) unavailable("This thread has no worktree.");
      if (input && cwd !== input.cwd) throw new WireError("invalid_cursor");
      const identity = await this.trust.withTrust(cwd, signal, value => value, { git: true, ...(input && { expected: input.identity }) });
      if (identity.kind !== "git") unavailable();
      const root = await checkoutRoot(identity.cwd.path, signal);
      if (input && root !== input.root) throw new WireError("invalid_cursor");
      // A nested Git directory can configure an ancestor as core.worktree.
      // Running Git from that root must still select the granted checkout.
      const rootIdentity = root === identity.cwd.path ? identity : await this.trust.withTrust(root, signal, candidate => {
        if (candidate.kind !== "git" || !sameDirectory(identity, candidate) || !sameDirectory(identity.gitDir, candidate.gitDir))
          unavailable();
        return candidate;
      }, { git: true });
      await checkFilters(root, signal);
      const finish = async <T>(result: T) => {
        if ((await this.h.snapshot(AgentDoc, id, ctx))?.cwd !== cwd) throw new WireError("invalid_cursor");
        if (root !== identity.cwd.path) {
          await this.trust.withTrust(root, signal, () => {}, { git: true, expected: rootIdentity });
        }
        return this.trust.withTrust(cwd, signal, () => result, { git: true, expected: identity });
      };
      const range = input?.range ?? (q.op === "summary" ? q.range : "head");
      const { commit, head, checkpoint } = await this.#base(id, cwd, root, range, ctx, signal);
      if (q.op === "file" && input) {
        if (input.commit !== commit) throw new WireError("invalid_cursor");
        const file = input.files[q.file]!;
        const patch = file.status === "?" ? await this.#untrackedPatch(root, file.path, signal)
          : await this.#trackedPatch(root, commit, file.path, signal);
        return await finish({ revision: q.revision, file: q.file, ...boundedPatch(patch) });
      }
      const [diff, index, others, ref] = await Promise.all([
        git(root, signal, "diff", ...diffFlags, "--raw", "--numstat", "-z", commit, "--"),
        git(root, signal, "diff", ...diffFlags, "--cached", "--name-only", "-z", head, "--"),
        git(root, signal, "ls-files", "--others", "--exclude-standard", "-z", "--"),
        git(root, signal, "rev-parse", "--symbolic-full-name", "HEAD").catch(() => ""),
      ]);
      // Detached HEAD is not a local branch. Treat unavailable metadata as optional.
      const branch = /^refs\/heads\/([^\x00-\x20\x7f]+)\n$/.exec(ref)?.[1];
      const rows = parseRows(diff, new Set(nulList(index)));
      for (const path of nulList(others)) {
        if (rows.has(path)) continue;
        if (rows.size >= gitDiffFiles) throw new WireError("limit");
        if (!validPath(path)) unavailable();
        const content = await untrackedText(root, path, signal);
        rows.set(path, { path, status: "?", staged: false, text: content !== undefined,
          ...(content !== undefined && { added: textLines(content).length, removed: 0 }) });
      }
      const files = [...rows.values()].sort((a, b) => a.path < b.path ? -1 : a.path > b.path ? 1 : 0), revision = randomUUID();
      return await finish({ revision, range, commit, checkpoint, files,
        ...(branch && Buffer.byteLength(branch) <= 1024 && { branch }),
      }).then(result => {
        while (this.#seen.size >= maxObservations) this.#seen.delete(this.#seen.keys().next().value!);
        this.#seen.set(revision, { threadId: q.threadId, cwd, identity, root, range, commit, files, expires: Date.now() + lifetime });
        return result;
      });
    } catch (error) {
      peer.throwIfAborted();
      if (signal.aborted) throw new WireError("limit");
      if (error instanceof WireError) throw error;
      unavailable();
    } finally { this.#active--; }
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
