import { createHash } from "node:crypto";
import { closeSync, constants, fstatSync, lstatSync, mkdirSync, openSync, readSync, readdirSync,
  realpathSync } from "node:fs";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { WireError } from "../../server/src/wire.js";
import { workspaceGit, type WorkspaceTrust, type WorkspaceTrustIdentity } from "./workspace-trust.js";

export type GitSnapshot = Readonly<{ repository: string; fingerprint: string; commit: string; ref: string }>;
// branch is the short local branch name, not refs/heads/… .
export type RewindStage = Readonly<{ cwd: string; branch: string; repository: string }>;

type GitIdentity = Extract<WorkspaceTrustIdentity, { kind: "git" }>;
type Worktree = { cwd: string; fields: Map<string, string> };
type BaseState = { dataDir: string; privateDir: string | undefined };
type Inspection = { base: BaseState; matches: Worktree[] };

const messages = {
  input: "Invalid Hush rewind checkpoint or operation.",
  snapshot: "The checkpoint snapshot is unavailable or no longer matches this repository.",
  base: "Hush has no safe private location for the rewind worktree.",
  conflict: "Rewind staging has conflicting or incomplete files, refs, or worktree registration.",
  changed: "The staged rewind worktree could not be verified as clean at the checkpoint.",
  filters: "Hush cannot restore with external Git filters or conditional Git includes configured.",
  unavailable: "Git checkpoint or rewind staging is unavailable on the backend host.",
} as const;

function failure(reason: keyof typeof messages): never {
  const error = new WireError("invalid_request");
  error.message = messages[reason];
  throw error;
}

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const oid = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const checkpointPrefix = "refs/hush/checkpoints/";
const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
const safePath = (path: unknown): path is string => typeof path === "string" &&
  path.isWellFormed() && !/[\0\r\n]/.test(path) && isAbsolute(path) && resolve(path) === path;
const within = (root: string, path: string) => {
  const suffix = relative(root, path);
  if (suffix !== ".." && !suffix.startsWith(".." + sep) && !isAbsolute(suffix)) return true;
  // Case-folding/normalizing filesystems may spell the same directory two ways.
  const info = named(root);
  if (!info?.isDirectory()) return false;
  for (let parent = path;; parent = dirname(parent)) {
    const ancestor = named(parent);
    if (ancestor?.isDirectory() && ancestor.dev === info.dev && ancestor.ino === info.ino) return true;
    if (dirname(parent) === parent) return false;
  }
};
const overlaps = (a: string, b: string) => within(a, b) || within(b, a);
const canonical = (path: string) => decoder.decode(realpathSync(path, { encoding: "buffer" }));

function named(path: string) {
  try { return lstatSync(path, { bigint: true }); }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
}

// A prunable registration may name a missing path. Resolve its existing
// ancestors too, rather than treating the path recorded by Git as canonical.
function canonicalLocation(path: string): string {
  if (named(path)) return canonical(path);
  const parent = dirname(path);
  if (parent === path) failure("base");
  return join(canonicalLocation(parent), relative(parent, path));
}

function safeDataAncestors(path: string) {
  const uid = process.getuid?.();
  for (let parent = dirname(path);; parent = dirname(parent)) {
    const info = named(parent);
    // An owner-controlled sticky directory (e.g. /tmp) protects the name
    // underneath it; an ordinary writable ancestor does not.
    if (!info?.isDirectory() || uid === undefined || (info.uid !== 0n && info.uid !== BigInt(uid)) ||
        ((info.mode & 0o022n) !== 0n && (info.mode & 0o1000n) === 0n)) failure("base");
    if (dirname(parent) === parent) break;
  }
}

function ownedDirectory(path: string, privateMode: boolean, reason: "base" | "changed") {
  const info = named(path);
  const uid = process.getuid?.();
  if (!info?.isDirectory() || uid === undefined || info.uid !== BigInt(uid) ||
      (privateMode ? (info.mode & 0o7777n) !== 0o700n : (info.mode & 0o022n) !== 0n) ||
      canonical(path) !== path) failure(reason);
  return `${info.dev}:${info.ino}:${info.birthtimeNs}`;
}

// Check every component before Git may write under these metadata paths.
// In a reftable repository the loose-ref path may legitimately be absent.
function metadata(root: string, suffix: string) {
  let path = root;
  const parts = suffix.split("/");
  for (const [index, part] of parts.entries()) {
    path = join(path, part);
    const info = named(path);
    if (!info) return undefined;
    if (info.isSymbolicLink() || (index < parts.length - 1 && !info.isDirectory())) failure("conflict");
    if (index === parts.length - 1) return info;
  }
  return undefined;
}

function marker(path: string, expected: string) {
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  try {
    const info = fstatSync(fd, { bigint: true });
    const bytes = Buffer.from(expected);
    if (!info.isFile() || info.nlink !== 1n || info.size !== BigInt(bytes.length)) failure("changed");
    const actual = Buffer.alloc(bytes.length + 1);
    let size = 0;
    while (size < actual.length) {
      const count = readSync(fd, actual, size, actual.length - size, null);
      if (!count) break;
      size += count;
    }
    if (!actual.subarray(0, size).equals(bytes)) failure("changed");
    return `${info.dev}:${info.ino}:${info.birthtimeNs}`;
  } finally { closeSync(fd); }
}

const fingerprint = ({ path, device, inode, birth }: GitIdentity) =>
  createHash("sha256").update(JSON.stringify([path, String(device), String(inode), String(birth)])).digest("hex");

function git(cwd: string, signal: AbortSignal, ...args: string[]) {
  return workspaceGit(cwd, signal, "--no-replace-objects", ...args);
}

const checkoutConfig = ["-c", "core.sparseCheckout=false", "-c", "core.sparseCheckoutCone=false",
  "-c", "core.ignoreStat=false", "-c", "core.fileMode=true", "-c", "core.trustctime=true", "-c", "core.checkStat=default"];

async function checkFilters(cwd: string, signal: AbortSignal) {
  // Checkout and status can execute external filters. A conditional include
  // can also introduce one only after Git switches to the fork's path/branch.
  const config = await git(cwd, signal, "config", "--includes", "--null", "--list");
  for (const entry of config.split("\0")) {
    const key = entry.split("\n", 1)[0]!.toLowerCase();
    if (key.startsWith("includeif.") || /^filter\..+\.(clean|smudge|process)$/.test(key)) failure("filters");
  }
}

async function worktrees(cwd: string, signal: AbortSignal): Promise<Worktree[]> {
  const output = await git(cwd, signal, "worktree", "list", "--porcelain", "-z");
  if (!output.endsWith("\0\0")) failure("unavailable");
  return output.slice(0, -2).split("\0\0").map(block => {
    const [root, ...rest] = block.split("\0");
    const path = root?.slice("worktree ".length);
    if (!root?.startsWith("worktree ") || !safePath(path)) failure("unavailable");
    const fields = new Map<string, string>();
    for (const field of rest) {
      const space = field.indexOf(" ");
      const key = space < 0 ? field : field.slice(0, space);
      if (!key || fields.has(key)) failure("unavailable");
      fields.set(key, space < 0 ? "" : field.slice(space + 1));
    }
    return { cwd: path, fields };
  });
}

function snapshotInput(snapshot: GitSnapshot): GitSnapshot {
  if (!snapshot || typeof snapshot !== "object") failure("input");
  const { repository, fingerprint, commit, ref } = snapshot;
  if (!safePath(repository) || typeof fingerprint !== "string" || fingerprint.length !== 64 || !oid.test(fingerprint) ||
      typeof commit !== "string" || !oid.test(commit) || typeof ref !== "string" ||
      !ref.startsWith(checkpointPrefix) || !uuid.test(ref.slice(checkpointPrefix.length))) failure("input");
  return Object.freeze({ repository, fingerprint, commit, ref });
}

async function exactRef(cwd: string, signal: AbortSignal, ref: string, commit: string, reason: "snapshot" | "changed") {
  const output = await git(cwd, signal, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(objecttype)%00%(symref)", "--", ref);
  if (output !== `${ref}\0${commit}\0commit\0\n`) failure(reason);
}

function sourceRegistration(registrations: Worktree[], stage: RewindStage) {
  return registrations.filter(tree => tree.cwd === stage.cwd || tree.fields.get("branch") === `refs/heads/${stage.branch}`);
}

// Status does not report empty untracked directories. Also reject unexpected
// filesystem entries and sparse/assume-unchanged index entries on recovery.
function checkoutEntries(cwd: string, index: string, signal: AbortSignal) {
  const files = new Map<string, string>();
  const directories = new Set<string>();
  if (index && !index.endsWith("\0")) failure("changed");
  for (const entry of index ? index.slice(0, -1).split("\0") : []) {
    const tab = entry.indexOf("\t");
    const match = /^H (100644|100755|120000|160000) ([0-9a-f]+) 0$/.exec(entry.slice(0, tab));
    const path = entry.slice(tab + 1);
    if (tab < 0 || !match || !oid.test(match[2]!) || files.has(path) ||
        path.split("/").some(part => !part || part === "." || part === ".." || part.toLowerCase() === ".git")) failure("changed");
    files.set(path, match[1]!);
    for (let dir = dirname(path); dir !== "."; dir = dirname(dir)) directories.add(dir);
  }
  const pending = [""];
  while (pending.length) {
    signal.throwIfAborted();
    const dir = pending.pop()!;
    for (const entry of readdirSync(join(cwd, dir), { withFileTypes: true, encoding: "buffer" })) {
      const name = decoder.decode(entry.name);
      if (!dir && name === ".git") continue; // Verified separately.
      const path = dir ? `${dir}/${name}` : name;
      const mode = files.get(path);
      if (mode === "160000") {
        // No submodule checkout is part of this operation.
        if (!entry.isDirectory() || readdirSync(join(cwd, path)).length) failure("changed");
      } else if (mode ? (mode === "120000" ? !entry.isSymbolicLink() : !entry.isFile()) :
          !directories.has(path) || !entry.isDirectory()) failure("changed");
      else if (!mode) pending.push(path);
    }
  }
}

export class RewindGit {
  readonly #dataDir: string;
  readonly #base: string;
  readonly #trust: WorkspaceTrust;

  // dataDir is the backend-owned, already existing Hush data directory. It
  // must be supplied by the backend, not taken from an operation payload.
  constructor(dataDir: string, trust: WorkspaceTrust) {
    if (!safePath(dataDir)) failure("base");
    this.#dataDir = dataDir;
    this.#base = join(dataDir, "rewind-worktrees-v1");
    this.#trust = trust;
  }

  /** Explicitly capture committed HEAD only; never read/copy a user's index. */
  async capture(workspace: string, checkpointId: string, signal: AbortSignal): Promise<GitSnapshot> {
    return this.#safe(signal, async () => {
      if (typeof checkpointId !== "string" || !uuid.test(checkpointId)) failure("input");
      const identity = await this.#identity(workspace, signal);
      const cwd = identity.cwd.path;
      const head = await git(cwd, signal, "rev-parse", "--verify", "HEAD");
      const commit = head.slice(0, -1);
      if (!head.endsWith("\n") || !oid.test(commit) || await git(cwd, signal, "cat-file", "-t", commit) !== "commit\n") failure("snapshot");
      const ref = checkpointPrefix + checkpointId;
      await this.#trust.withTrust(workspace, signal, () => {
        if (metadata(identity.path, ref) || metadata(identity.path, `${ref}.lock`) ||
            metadata(identity.path, `logs/${ref}`)) failure("conflict");
        return git(cwd, signal, "-c", "core.logAllRefUpdates=false", "update-ref", "--no-deref", ref, commit, "0".repeat(commit.length));
      }, { git: true, expected: identity });
      await exactRef(cwd, signal, ref, commit, "snapshot");
      return Object.freeze({ repository: identity.path, fingerprint: fingerprint(identity), commit, ref });
    });
  }

  /** Read-only preview. An operation with any leftover state is not fresh. */
  async prepare(workspace: string, snapshot: GitSnapshot, operationId: string, signal: AbortSignal): Promise<RewindStage> {
    return this.#safe(signal, async () => {
      const input = snapshotInput(snapshot);
      const plan = this.#plan(input, operationId);
      const identity = await this.#identity(workspace, signal, input);
      const state = await this.#inspect(identity, input, plan, signal);
      if (state.matches.length || await this.#branchExists(identity, plan, signal)) failure("conflict");
      return this.#trust.withTrust(workspace, signal, () => {
        this.#unchangedBase(state.base);
        this.#vacant(identity, plan, operationId);
        return plan;
      }, { git: true, expected: identity });
    });
  }

  /** Add once, or verify a fully staged checkout after interrupted execution. */
  async stage(workspace: string, snapshot: GitSnapshot, operationId: string, expectedStage: RewindStage,
      signal: AbortSignal): Promise<RewindStage> {
    return this.#safe(signal, async () => {
      const input = snapshotInput(snapshot);
      const plan = this.#plan(input, operationId, expectedStage, true);
      const identity = await this.#identity(workspace, signal, input);
      const state = await this.#inspect(identity, input, plan, signal);
      if (state.matches.length || named(plan.cwd) || metadata(identity.path, `worktrees/${operationId}`)) {
        return this.#verified(workspace, identity, input, plan, operationId, state, signal);
      }
      if (await this.#branchExists(identity, plan, signal)) failure("conflict");
      await this.#snapshot(identity, input, signal);
      let privateDir = state.base.privateDir;
      await this.#trust.withTrust(workspace, signal, () => {
        // No await between the authoritative grant, the final local fences,
        // and starting Git. -b without --force asks Git to create the branch.
        this.#unchangedBase(state.base);
        this.#vacant(identity, plan, operationId);
        if (!privateDir) {
          mkdirSync(this.#base, { mode: 0o700 });
          privateDir = ownedDirectory(this.#base, true, "base");
        }
        return git(identity.cwd.path, signal, ...checkoutConfig, "-c", "worktree.useRelativePaths=false",
          "-c", "core.logAllRefUpdates=false", "worktree", "add", "--quiet", "--no-track",
          "-b", plan.branch, "--", plan.cwd, input.commit);
      }, { git: true, expected: identity });
      const staged = await this.#trust.withTrust(workspace, signal, () => this.#inspect(identity, input, plan, signal),
        { git: true, expected: identity });
      if (staged.base.dataDir !== state.base.dataDir || staged.base.privateDir !== privateDir) failure("base");
      return this.#verified(workspace, identity, input, plan, operationId, staged, signal);
    });
  }

  /** Run again immediately before publishing the fork as ready. Never writes. */
  async verify(workspace: string, snapshot: GitSnapshot, operationId: string, expectedStage: RewindStage,
      signal: AbortSignal): Promise<RewindStage> {
    return this.#safe(signal, async () => {
      const input = snapshotInput(snapshot);
      const plan = this.#plan(input, operationId, expectedStage, true);
      const identity = await this.#identity(workspace, signal, input);
      const state = await this.#inspect(identity, input, plan, signal);
      return this.#verified(workspace, identity, input, plan, operationId, state, signal);
    });
  }

  async #safe<T>(signal: AbortSignal, action: () => Promise<T>): Promise<T> {
    try { signal.throwIfAborted(); return await action(); }
    catch (error) {
      signal.throwIfAborted();
      // WorkspaceTrust and workspaceGit use fixed WireError diagnostics too.
      if (error instanceof WireError) throw error;
      failure("unavailable");
    }
  }

  #identity(workspace: string, signal: AbortSignal, snapshot?: GitSnapshot) {
    return this.#trust.withTrust(workspace, signal, identity => {
      if (identity.kind !== "git") failure("snapshot");
      if (snapshot && (snapshot.repository !== identity.path || snapshot.fingerprint !== fingerprint(identity))) failure("snapshot");
      return identity;
    }, { git: true });
  }

  #snapshot(identity: GitIdentity, snapshot: GitSnapshot, signal: AbortSignal) {
    metadata(identity.path, snapshot.ref);
    return exactRef(identity.cwd.path, signal, snapshot.ref, snapshot.commit, "snapshot");
  }

  #plan(snapshot: GitSnapshot, operationId: string, expected?: RewindStage, requireExpected = false): RewindStage {
    if (typeof operationId !== "string" || !uuid.test(operationId)) failure("input");
    const plan = Object.freeze({ cwd: join(this.#base, operationId), branch: `hush/rewind/${operationId}`, repository: snapshot.repository });
    if (requireExpected && (!expected || expected.cwd !== plan.cwd || expected.branch !== plan.branch ||
        expected.repository !== plan.repository)) failure("input");
    return plan;
  }

  #baseState(): BaseState {
    safeDataAncestors(this.#dataDir);
    const dataDir = ownedDirectory(this.#dataDir, false, "base");
    const privateDir = named(this.#base) ? ownedDirectory(this.#base, true, "base") : undefined;
    // Don't put a private checkout inside another repository, even one not
    // participating in this operation. A dangling or invalid .git is a marker.
    for (let path = this.#base;; path = dirname(path)) {
      if (named(join(path, ".git"))) failure("base");
      if (dirname(path) === path) break;
    }
    return { dataDir, privateDir };
  }

  #unchangedBase(state: BaseState) {
    const current = this.#baseState();
    if (current.dataDir !== state.dataDir || current.privateDir !== state.privateDir) failure("base");
  }

  async #inspect(identity: GitIdentity, snapshot: GitSnapshot, plan: RewindStage, signal: AbortSignal): Promise<Inspection> {
    const base = this.#baseState();
    if (overlaps(identity.path, this.#base) || overlaps(identity.gitDir.path, this.#base)) failure("base");
    const cwd = identity.cwd.path;
    const topOutput = await git(cwd, signal, "rev-parse", "--show-toplevel");
    const top = topOutput.slice(0, -1);
    if (!topOutput.endsWith("\n") || !safePath(top)) failure("base");
    const sourceRoot = canonical(top);
    if (!within(sourceRoot, cwd) || within(sourceRoot, this.#base) || overlaps(sourceRoot, plan.cwd)) failure("base");
    await this.#snapshot(identity, snapshot, signal);
    const admin = metadata(identity.path, "worktrees");
    if (admin && !admin.isDirectory()) failure("conflict");
    const registrations = await worktrees(cwd, signal);
    for (const tree of registrations) {
      for (const root of [tree.cwd, canonicalLocation(tree.cwd)]) {
        if (within(root, this.#base) || (overlaps(root, plan.cwd) && tree.cwd !== plan.cwd)) failure("base");
      }
    }
    await checkFilters(cwd, signal);
    return { base, matches: sourceRegistration(registrations, plan) };
  }

  async #branchExists(identity: GitIdentity, stage: RewindStage, signal: AbortSignal) {
    const ref = `refs/heads/${stage.branch}`;
    const refs = await git(identity.cwd.path, signal, "for-each-ref", "--format=%(refname)", "--", "refs/heads/hush");
    return refs.split("\n").some(name => name && (name === ref || ref.startsWith(name + "/") || name.startsWith(ref + "/")));
  }

  #vacant(identity: GitIdentity, plan: RewindStage, id: string) {
    const admin = metadata(identity.path, "worktrees");
    const ref = `refs/heads/${plan.branch}`;
    if (named(plan.cwd) || (admin && !admin.isDirectory()) || metadata(identity.path, `worktrees/${id}`) ||
        metadata(identity.path, ref) || metadata(identity.path, `${ref}.lock`) || metadata(identity.path, `logs/${ref}`)) failure("conflict");
  }

  #checkoutStamp(identity: GitIdentity, plan: RewindStage, id: string) {
    const gitDir = join(identity.path, "worktrees", id);
    const admin = metadata(identity.path, `worktrees/${id}`);
    const index = metadata(identity.path, `worktrees/${id}/index`);
    if (!admin?.isDirectory() || canonical(gitDir) !== gitDir || !index?.isFile() || index.nlink !== 1n ||
        ["locked", "index.lock", "HEAD.lock"].some(name => named(join(gitDir, name)))) failure("changed");
    const checkout = ownedDirectory(plan.cwd, false, "changed");
    const dotGit = join(plan.cwd, ".git");
    const markerId = marker(dotGit, `gitdir: ${gitDir}\n`);
    const backlinkId = marker(join(gitDir, "gitdir"), `${dotGit}\n`);
    return { gitDir, stamp: `${checkout}/${admin.dev}:${admin.ino}:${admin.birthtimeNs}/${markerId}/${backlinkId}/${index.dev}:${index.ino}:${index.mtimeNs}:${index.ctimeNs}` };
  }

  async #verified(workspace: string, identity: GitIdentity, snapshot: GitSnapshot, plan: RewindStage, id: string,
      state: Inspection, signal: AbortSignal) {
    const [registered] = state.matches;
    const branch = `refs/heads/${plan.branch}`;
    if (!state.base.privateDir || state.matches.length !== 1 || !registered || registered.cwd !== plan.cwd ||
        registered.fields.get("branch") !== branch || registered.fields.get("HEAD") !== snapshot.commit ||
        registered.fields.has("bare") || registered.fields.has("detached") || registered.fields.has("locked") ||
        registered.fields.has("prunable")) failure("conflict");
    this.#unchangedBase(state.base);
    const { gitDir, stamp } = this.#checkoutStamp(identity, plan, id);
    const paths = await git(plan.cwd, signal, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir");
    if (paths !== `${plan.cwd}\n${gitDir}\n${identity.path}\n`) failure("changed");
    if (await git(plan.cwd, signal, "symbolic-ref", "--no-recurse", "HEAD") !== `${branch}\n`) failure("changed");
    await exactRef(plan.cwd, signal, branch, snapshot.commit, "changed");
    await checkFilters(plan.cwd, signal);
    const index = await git(plan.cwd, signal, ...checkoutConfig, "ls-files", "-v", "--stage", "-z");
    checkoutEntries(plan.cwd, index, signal);
    const status = await git(plan.cwd, signal, ...checkoutConfig, "status", "--porcelain=v1", "-z", "--untracked-files=all",
      "--ignored", "--ignore-submodules=none", "--no-renames");
    if (status) failure("changed");
    if (await git(plan.cwd, signal, "symbolic-ref", "--no-recurse", "HEAD") !== `${branch}\n`) failure("changed");
    await exactRef(plan.cwd, signal, branch, snapshot.commit, "changed");
    await this.#snapshot(identity, snapshot, signal);
    return this.#trust.withTrust(workspace, signal, () => {
      this.#unchangedBase(state.base);
      if (this.#checkoutStamp(identity, plan, id).stamp !== stamp) failure("changed");
      return plan;
    }, { git: true, expected: identity });
  }
}
