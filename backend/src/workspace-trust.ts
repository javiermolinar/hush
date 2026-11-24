import { execFile } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { closeSync, constants, fchmodSync, fstatSync, fsyncSync, lstatSync, mkdirSync, openSync, readSync, renameSync, unlinkSync,
  writeFileSync } from "node:fs";
import { lstat, realpath } from "node:fs/promises";
import { delimiter, dirname, isAbsolute, join, resolve } from "node:path";
import { parseArgs, promisify } from "node:util";
import { WireError } from "../../server/src/wire.js";
import { hushDirectory } from "./config.js";

export type WorkspaceTrustStatus = { kind: "git" | "directory"; path: string; trusted: boolean };
type DirectoryIdentity = { path: string; device: bigint; inode: bigint; birth: bigint };
export type WorkspaceTrustIdentity = DirectoryIdentity & ({ kind: "directory" } |
  { kind: "git"; cwd: DirectoryIdentity; gitDir: DirectoryIdentity });

const messages = {
  workspace: "Invalid workspace on the backend host.",
  gitUnavailable: "Git is unavailable on the backend host.",
  gitUnsupported: "Git on the backend host does not support workspace discovery.",
  notGit: "Managed checkout requires a Git working tree.",
  grant: "Workspace is not trusted by Hush. Run 'hush trust grant --cwd PATH' on the backend host.",
  work: "Workspace is not trusted for tool execution. Run 'hush trust grant --scope work --cwd PATH' on the backend host.",
  changed: "Workspace changed. Review the directory again before trusting it.",
  storage: "Hush workspace trust storage is unavailable or unsafe.",
} as const;

function failure(reason: keyof typeof messages) {
  const error = new WireError("invalid_request");
  error.message = messages[reason];
  return error;
}

class NotGitWorkspace extends WireError {
  constructor() { super("invalid_request"); this.message = messages.notGit; }
}

const run = promisify(execFile);
const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
const validPath = (path: string) => !!path && !/[\0\r\n]/.test(path) && path.isWellFormed() && Array.from(path).length <= 4096;

// Use identical Git discovery/config inputs for the trust probe and checkout.
// Don't inherit environment-based repository, config, or discovery overrides.
export async function workspaceGit(cwd: string, signal: AbortSignal, ...args: string[]) {
  signal.throwIfAborted();
  // A relative or empty search component would let the requested cwd select
  // which executable is used for the pre-consent probes.
  const path = (process.env.PATH ?? "/usr/bin:/bin").split(delimiter).filter(isAbsolute).join(delimiter);
  if (!path) throw failure("gitUnavailable");
  const opts = ["--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "submodule.recurse=false",
    "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "protocol.allow=never"];
  try {
    const { stdout } = await run("git", [...opts, ...args], {
      cwd, signal, timeout: 25_000, maxBuffer: 512 * 1024, encoding: "buffer",
      env: { PATH: path, HOME: process.env.HOME ?? "", LC_ALL: "C",
        GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null", GIT_TERMINAL_PROMPT: "0", GIT_NO_LAZY_FETCH: "1" },
    });
    // The write command's output isn't needed, even on success.
    return args[0] === "worktree" && args[1] === "add" ? "" : decoder.decode(stdout);
  } catch (error) {
    signal.throwIfAborted();
    const { code, stderr } = error as { code?: number | string; stderr?: Buffer };
    if (code === "ENOENT" || code === "EACCES" || code === "ENOEXEC") {
      // A disappearing cwd can also cause spawn to fail (including ENOENT).
      await directory(cwd);
      signal.throwIfAborted();
      throw failure("gitUnavailable");
    }
    const diagnostic = stderr?.toString("utf8") ?? "";
    if (diagnostic.startsWith("xcode-select: ")) throw failure("gitUnavailable");
    if (code === 128) {
      if (diagnostic === "fatal: not a git repository (or any of the parent directories): .git\n" ||
          /^fatal: not a git repository \(or any parent up to mount point [^\r\n]*\)\nStopping at filesystem boundary \(GIT_DISCOVERY_ACROSS_FILESYSTEM not set\)\.\n$/.test(diagnostic)) {
        throw new NotGitWorkspace();
      }
      if (diagnostic === "fatal: this operation must be run in a work tree\n") throw failure("notGit");
    }
    throw failure("workspace");
  }
}

async function directory(path: string): Promise<DirectoryIdentity> {
  if (!validPath(path)) throw failure("workspace");
  try {
    const canonical = decoder.decode(await realpath(path, { encoding: "buffer" }));
    if (!validPath(canonical) || !isAbsolute(canonical)) throw failure("workspace");
    const info = await lstat(canonical, { bigint: true });
    if (!info.isDirectory()) throw failure("workspace");
    return { path: canonical, device: info.dev, inode: info.ino, birth: info.birthtimeNs };
  } catch { throw failure("workspace"); }
}

const sameDirectory = (a: DirectoryIdentity, b: DirectoryIdentity) =>
  a.path === b.path && a.device === b.device && a.inode === b.inode && a.birth === b.birth;
const sameWorkspace = (a: WorkspaceTrustIdentity, b: WorkspaceTrustIdentity) =>
  sameDirectory(a, b) && (a.kind === "directory" ? b.kind === "directory" :
    b.kind === "git" && sameDirectory(a.cwd, b.cwd) && sameDirectory(a.gitDir, b.gitDir));

const enoent = (error: unknown) => (error as NodeJS.ErrnoException).code === "ENOENT";

async function resolveWorkspace(workspace: string, signal: AbortSignal): Promise<WorkspaceTrustIdentity> {
  try {
    signal.throwIfAborted();
    const cwd = await directory(workspace);
    let identity: WorkspaceTrustIdentity;
    let result: string | undefined;
    try {
      result = await workspaceGit(cwd.path, signal, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir");
    } catch (error) {
      if (!(error instanceof NotGitWorkspace)) throw error;
    }
    if (result !== undefined) {
      const paths = result.replace(/\n$/, "").split("\n");
      if (paths[0] === "--path-format=absolute") throw failure("gitUnsupported");
      if (paths.length !== 2 || !paths.every(path => validPath(path) && isAbsolute(path))) throw failure("workspace");
      const gitDir = await directory(paths[0]!);
      const common = paths[0] === paths[1] ? gitDir : await directory(paths[1]!);
      // A linked worktree's private Git directory is in the common directory.
      // Don't let a foreign Git dir borrow trust just by writing a commondir file.
      if (!sameDirectory(gitDir, common) && dirname(gitDir.path) !== join(common.path, "worktrees")) {
        throw failure("workspace");
      }
      identity = { ...common, kind: "git", cwd, gitDir };
    } else {
      // Git may also report "not a git repository" for broken metadata. Don't
      // turn an unresolved repository into a directory grant. In particular,
      // a symlink or dangling gitfile is still a marker, not an absence.
      for (let path = cwd.path;; path = dirname(path)) {
        signal.throwIfAborted();
        const marker = await lstat(join(path, ".git")).catch(error => { if (!enoent(error)) throw error; });
        if (marker) throw failure("workspace");
        if (dirname(path) === path) break;
      }
      identity = { ...cwd, kind: "directory" };
    }
    if (!sameDirectory(cwd, await directory(workspace))) throw failure("workspace");
    signal.throwIfAborted();
    return identity;
  } catch (error) {
    signal.throwIfAborted();
    if (error instanceof WireError) throw error;
    throw failure("workspace");
  }
}

function privateDir(path: string, create: boolean) {
  if (create) {
    try { mkdirSync(path, { mode: 0o700 }); }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error; }
  }
  const info = present(path);
  if (!info) return false;
  const uid = process.getuid?.();
  if (uid === undefined || !info.isDirectory() || (info.mode & 0o7777n) !== 0o700n || info.uid !== BigInt(uid)) throw failure("storage");
  return true;
}

function present(path: string) {
  try { return lstatSync(path, { bigint: true }); }
  catch (error) { if (!enoent(error)) throw error; }
}

function hasGrant(path: string, expected: Buffer) {
  // Verify both the name and the opened file. In particular don't follow links
  // or block on a FIFO, and never read an unbounded or permissive grant file.
  const uid = process.getuid?.();
  const valid = (info: NonNullable<ReturnType<typeof present>>) => uid !== undefined && info.isFile() &&
    info.nlink === 1n && info.uid === BigInt(uid) && (info.mode & 0o7077n) === 0n && info.size === BigInt(expected.length);
  const named = present(path);
  if (!named) return false;
  if (!valid(named)) throw failure("storage");
  let fd: number;
  try { fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK); }
  catch (error) { if (enoent(error)) return false; throw error; }
  try {
    const actual = fstatSync(fd, { bigint: true });
    if (actual.nlink === 0n) return false; // Removed/replaced by another process.
    if (!valid(actual)) throw failure("storage");
    const data = Buffer.alloc(expected.length + 1);
    let size = 0;
    while (size < data.length) {
      const count = readSync(fd, data, size, data.length - size, null);
      if (!count) break;
      size += count;
    }
    if (!data.subarray(0, size).equals(expected)) throw failure("storage");
    // Ensure a cross-process revocation during the read isn't accepted from an
    // already open descriptor. Atomic replacement may conservatively deny.
    const current = present(path);
    if (!current) return false;
    if (!valid(current)) throw failure("storage");
    return current.dev === actual.dev && current.ino === actual.ino;
  } finally { closeSync(fd); }
}

function publish(dir: string, path: string, data: Buffer) {
  const pending = join(dir, ".pending-" + randomUUID());
  const fd = openSync(pending, "wx", 0o600);
  try {
    try { fchmodSync(fd, 0o600); writeFileSync(fd, data); fsyncSync(fd); }
    finally { closeSync(fd); }
    renameSync(pending, path);
  } catch (error) {
    try { unlinkSync(pending); } catch { /* Unpublished files never authorize anything. */ }
    throw error;
  }
}

function trustDirectory(agentDir: string) {
  if (!validPath(agentDir) || !isAbsolute(agentDir)) throw failure("storage");
  return resolve(agentDir);
}

const key = (data: Buffer) => createHash("sha256").update(data).digest("hex");
const record = (purpose: string, { path, device, inode, birth }: DirectoryIdentity) =>
  Buffer.from(JSON.stringify([purpose, path, String(device), String(inode), String(birth)]) + "\n");

function access(agentDir: string, area: string, data: Buffer, signal?: AbortSignal, change?: boolean) {
  signal?.throwIfAborted();
  try {
    const create = change === true;
    const dir = join(agentDir, area);
    if (!privateDir(agentDir, create) || !privateDir(dir, create)) {
      if (create) throw failure("storage");
      return false;
    }
    // Atomic publication and unlink never change the workspace or overwrite
    // decisions about other directories. An unsafe record never grants trust.
    const path = join(dir, key(data));
    if (change === undefined) return hasGrant(path, data);
    if (change) {
      if (!hasGrant(path, data)) publish(dir, path, data);
    } else {
      try { unlinkSync(path); } catch (error) { if (!enoent(error)) throw error; }
    }
    return change;
  } catch { throw failure("storage"); }
}

// Backend-host authority only: never take the agent directory from a peer.
// Git trust keeps its existing scope and never authorizes agent tools.
export class WorkspaceTrust {
  private readonly agentDir: string;

  constructor(agentDir: string) { this.agentDir = trustDirectory(agentDir); }

  async inspect(workspace: string, signal: AbortSignal): Promise<WorkspaceTrustStatus> {
    const identity = await resolveWorkspace(workspace, signal);
    return this.#access(identity, signal);
  }

  async change(workspace: string, trusted: boolean, signal: AbortSignal): Promise<WorkspaceTrustStatus> {
    signal.throwIfAborted();
    if (typeof trusted !== "boolean") throw new WireError("invalid_request");
    const identity = await resolveWorkspace(workspace, signal);
    return this.#access(identity, signal, trusted);
  }

  // For operations that need a fresh grant, start the action in the same turn
  // as the authoritative read. Pass the initial identity back at a later
  // irreversible boundary to also reject a changed workspace/repository.
  async withTrust<T>(workspace: string, signal: AbortSignal, action: (identity: WorkspaceTrustIdentity) => T | Promise<T>,
      options?: { git?: boolean; expected?: WorkspaceTrustIdentity }): Promise<T> {
    const identity = await resolveWorkspace(workspace, signal);
    if (options?.git && identity.kind !== "git") throw failure("notGit");
    if (options?.expected && !sameWorkspace(identity, options.expected)) throw failure("workspace");
    if (!this.#access(identity, signal).trusted) throw failure("grant");
    return action(identity);
  }

  #access(identity: WorkspaceTrustIdentity, signal: AbortSignal, change?: boolean): WorkspaceTrustStatus {
    const { kind, path } = identity;
    return { kind, path, trusted: access(this.agentDir, "workspace-trust-v1", record(kind, identity), signal, change) };
  }
}

// Work grants bind the exact canonical directory, with no parent or linked
// checkout inheritance and no Git discovery. A dismissal is not a grant.
export class WorkspaceWorkTrust {
  private readonly agentDir: string;

  constructor(agentDir: string) { this.agentDir = trustDirectory(agentDir); }

  async review(workspace: string, signal?: AbortSignal) {
    signal?.throwIfAborted();
    const identity = await directory(workspace);
    return { ...this.#status(identity, signal), expected: key(record("work", identity)) };
  }

  async change(workspace: string, action: "grant" | "revoke" | "dismiss", signal: AbortSignal, expected?: string) {
    signal.throwIfAborted();
    const identity = await directory(workspace);
    // This fingerprint binds what the operator saw, not a credential.
    if (expected !== undefined && expected !== key(record("work", identity))) throw failure("changed");
    if (action === "dismiss") this.#access(identity, "dismissed", signal, true);
    else this.#access(identity, "work", signal, action === "grant");
    return this.#status(identity, signal);
  }

  async require(workspace: string, signal?: AbortSignal) {
    signal?.throwIfAborted();
    const identity = await directory(workspace);
    if (!this.#access(identity, "work", signal)) throw failure("work");
    return identity.path;
  }

  #status(identity: DirectoryIdentity, signal?: AbortSignal) {
    return { kind: "directory" as const, path: identity.path, trusted: this.#access(identity, "work", signal),
      dismissed: this.#access(identity, "dismissed", signal) };
  }

  #access(identity: DirectoryIdentity, purpose: "work" | "dismissed", signal?: AbortSignal, change?: boolean) {
    return access(this.agentDir, "workspace-work-v1", record(purpose, identity), signal, change);
  }
}

// Foreground local operator command. No daemon, socket, Pi lookup, or model.
export async function runWorkspaceTrustCommand(argv: string[]): Promise<number> {
  try {
    const { values, positionals } = parseArgs({ args: argv, allowPositionals: true,
      options: { cwd: { type: "string" }, json: { type: "boolean" }, expect: { type: "string" }, scope: { type: "string" } } });
    const [action] = positionals;
    if (positionals.length !== 1 || values.cwd === "" || process.env.HUSH_SOCKET ||
        process.env.HUSH_PROFILE && process.env.HUSH_PROFILE !== "hush") throw new Error();
    const signal = AbortSignal.timeout(30_000), agentDir = hushDirectory();
    // Private entry points for the managed local TUI, never socket methods.
    if (action === "review-work" || action === "grant-reviewed-work" || action === "dismiss-reviewed-work") {
      const cwd = values.cwd, store = new WorkspaceWorkTrust(agentDir);
      if (!cwd || !isAbsolute(cwd) || values.scope !== undefined) throw new Error();
      if (action === "review-work" && values.expect === undefined) {
        process.stdout.write(JSON.stringify(await store.review(cwd, signal)) + "\n");
      } else if (action !== "review-work" && values.expect && /^[0-9a-f]{64}$/.test(values.expect)) {
        await store.change(cwd, action === "grant-reviewed-work" ? "grant" : "dismiss", signal, values.expect);
      } else throw new Error();
      return 0;
    }
    const scope = values.scope ?? "git";
    if (action !== "status" && action !== "grant" && action !== "revoke" || values.expect !== undefined ||
        scope !== "git" && scope !== "work") throw new Error();
    const workspace = resolve(values.cwd ?? ".");
    let state: WorkspaceTrustStatus & { dismissed?: boolean };
    if (scope === "work") {
      const store = new WorkspaceWorkTrust(agentDir);
      // The reviewed identity stays internal to the local confirmation flow.
      const { kind, path, trusted, dismissed } = action === "status" ? await store.review(workspace, signal) :
        await store.change(workspace, action, signal);
      state = { kind, path, trusted, dismissed };
    } else {
      const store = new WorkspaceTrust(agentDir);
      state = action === "status" ? await store.inspect(workspace, signal) : await store.change(workspace, action === "grant", signal);
    }
    process.stdout.write(values.json ? JSON.stringify(state) + "\n" :
      `${state.trusted ? "trusted" : "not trusted"} ${state.kind} ${JSON.stringify(state.path)}\n`);
    return 0;
  } catch (error) {
    process.stderr.write("hush: " + (error instanceof WireError ? error.message :
      "Use trust status, grant, or revoke [--scope git|work] on the Hush backend host.") + "\n");
    return 1;
  }
}
