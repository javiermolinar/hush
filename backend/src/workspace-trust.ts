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

// Backend-host authority only. The caller must pass the Hush-owned agentDir;
// never construct this from client data or a Pi profile. No repo-local source
// is consulted for consent, and nothing in this store grants file access.
export class WorkspaceTrust {
  private readonly agentDir: string;

  constructor(agentDir: string) {
    if (!validPath(agentDir) || !isAbsolute(agentDir)) throw failure("storage");
    this.agentDir = resolve(agentDir);
  }

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

  #access({ kind, path, device, inode, birth }: WorkspaceTrustIdentity, signal: AbortSignal, change?: boolean): WorkspaceTrustStatus {
    signal.throwIfAborted();
    const status = (trusted: boolean) => ({ kind, path, trusted });
    try {
      const create = change === true;
      const dir = join(this.agentDir, "workspace-trust-v1");
      if (!privateDir(this.agentDir, create) || !privateDir(dir, create)) {
        if (create) throw failure("storage");
        return status(false);
      }
      // Each scope has a separate, content-checked record. Atomic rename/unlink
      // avoid a shared read-modify-write and lost updates across local processes.
      // Git grants key on the common directory; gitDir/cwd only fence a checkout.
      const data = Buffer.from(JSON.stringify([kind, path, String(device), String(inode), String(birth)]) + "\n");
      const grant = join(dir, createHash("sha256").update(data).digest("hex"));
      if (change === undefined) return status(hasGrant(grant, data));
      if (change) {
        if (!hasGrant(grant, data)) publish(dir, grant, data);
      } else {
        // An explicit revocation can remove an unsafe or corrupt record;
        // unlink never follows a symlink. Neither touches the workspace.
        try { unlinkSync(grant); } catch (error) { if (!enoent(error)) throw error; }
      }
      return status(change);
    } catch { throw failure("storage"); }
  }
}

// Foreground local operator command. No daemon, socket, Pi lookup, or model.
export async function runWorkspaceTrustCommand(argv: string[]): Promise<number> {
  try {
    const { values, positionals } = parseArgs({ args: argv, allowPositionals: true,
      options: { cwd: { type: "string" }, json: { type: "boolean" } } });
    const [action] = positionals;
    if (positionals.length !== 1 || !["status", "grant", "revoke"].includes(action ?? "") ||
        values.cwd === "" || process.env.HUSH_SOCKET || process.env.HUSH_PROFILE && process.env.HUSH_PROFILE !== "hush") {
      throw new Error();
    }
    const workspace = resolve(values.cwd ?? ".");
    const store = new WorkspaceTrust(hushDirectory());
    const signal = AbortSignal.timeout(30_000);
    const state = action === "status" ? await store.inspect(workspace, signal) :
      await store.change(workspace, action === "grant", signal);
    process.stdout.write(values.json ? JSON.stringify(state) + "\n" :
      `${state.trusted ? "trusted" : "not trusted"} ${state.kind} ${JSON.stringify(state.path)}\n`);
    return 0;
  } catch (error) {
    process.stderr.write("hush: " + (error instanceof WireError ? error.message :
      "Use trust status, grant, or revoke on the Hush backend host.") + "\n");
    return 1;
  }
}
