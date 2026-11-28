import { execFile } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { closeSync, constants, fchmodSync, fstatSync, fsyncSync, lstatSync, mkdirSync, openSync, readSync, renameSync, unlinkSync,
  watch, writeFileSync, type FSWatcher } from "node:fs";
import { lstat, realpath } from "node:fs/promises";
import { basename, delimiter, dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { parseArgs, promisify } from "node:util";
import { WireError } from "../../server/src/wire.js";
import { hushDirectory } from "./config.js";

export type DirectoryIdentity = { path: string; device: bigint; inode: bigint; birth: bigint };
export type GitWorkspaceIdentity = DirectoryIdentity & { cwd: DirectoryIdentity; root: DirectoryIdentity; gitDir: DirectoryIdentity };

const messages = {
  workspace: "Invalid workspace on the backend host.",
  gitUnavailable: "Git is unavailable on the backend host.",
  gitUnsupported: "Git on the backend host does not support workspace discovery.",
  notGit: "Managed checkout requires a Git working tree.",
  grant: "Workspace is not trusted by Hush. Run 'hush trust grant --cwd PATH' on the backend host.",
  work: "Workspace is not trusted for tool execution. Run 'hush trust grant --cwd PATH' on the backend host.",
  changed: "Workspace changed. Review the directory again before trusting it.",
  storage: "Hush workspace trust storage is unavailable or unsafe.",
} as const;

function failure(reason: keyof typeof messages) {
  const error = new WireError("invalid_request");
  error.message = messages[reason];
  return error;
}

const run = promisify(execFile);
const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
const validPath = (path: string) => !!path && !/[\0\r\n]/.test(path) && path.isWellFormed() && Array.from(path).length <= 4096;

export function workspaceGit(cwd: string, signal: AbortSignal, ...args: string[]) {
  return runWorkspaceGit(cwd, signal, args, 512 * 1024);
}

// --no-index also applies ignore rules to an explicitly named tracked file.
// Only the presence of a match is used; ignore patterns never enter a review.
export async function workspaceHasIgnoredPaths(cwd: string, paths: readonly string[], signal: AbortSignal) {
  return paths.length > 0 && !!await runWorkspaceGit(cwd, signal, ["check-ignore", "--no-index", "-z", "--stdin"], 64 * 1024, paths.join("\0") + "\0");
}

// Rewind compares every tracked path and mode. Give only this listing a
// larger output budget; ordinary metadata and discovery keep the small cap.
export function workspaceGitIndex(cwd: string, signal: AbortSignal, checkoutConfig: readonly string[]) {
  return runWorkspaceGit(cwd, signal, ["--no-replace-objects", ...checkoutConfig, "ls-files", "-v", "--stage", "-z"], 32 * 1024 * 1024);
}

// Use identical Git discovery/config inputs for the trust probe and checkout.
// Don't inherit environment-based repository, config, or discovery overrides.
function spawnWorkspaceGit(cwd: string, signal: AbortSignal, args: readonly string[], maxBuffer: number) {
  signal.throwIfAborted();
  // A relative or empty search component would let the requested cwd select
  // which executable is used for the pre-consent probes.
  const path = (process.env.PATH ?? "/usr/bin:/bin").split(delimiter).filter(isAbsolute).join(delimiter);
  if (!path) throw failure("gitUnavailable");
  const opts = ["--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "submodule.recurse=false",
    "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "protocol.allow=never"];
  return run("git", [...opts, ...args], {
    cwd, signal, timeout: 25_000, maxBuffer, encoding: "buffer",
    env: { PATH: path, HOME: process.env.HOME ?? "", LC_ALL: "C",
      GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null", GIT_TERMINAL_PROMPT: "0", GIT_NO_LAZY_FETCH: "1" },
  });
}

// A delegate's read-only command. The caller validates every argument. Git's
// own diagnostic is returned, since a failed revision or path is the model's
// to fix; output past the budget is cut rather than failing the command.
export async function workspaceGitOutput(cwd: string, signal: AbortSignal, args: readonly string[], maxBuffer: number) {
  const child = spawnWorkspaceGit(cwd, signal, args, maxBuffer);
  try {
    const { stdout } = await child;
    return { ok: true, text: stdout, truncated: false } as const;
  } catch (error) {
    signal.throwIfAborted();
    const { code, stdout, stderr, killed } = error as { code?: number | string | null; stdout?: Buffer; stderr?: Buffer; killed?: boolean };
    if (code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER") return { ok: true, text: stdout ?? Buffer.alloc(0), truncated: true } as const;
    if (killed) return { ok: false, text: Buffer.from("Git did not finish within 25 seconds."), truncated: false } as const;
    if (typeof code === "number") return { ok: false, text: stderr ?? Buffer.alloc(0), truncated: false } as const;
    if (code === "ENOENT" || code === "EACCES" || code === "ENOEXEC") throw failure("gitUnavailable");
    throw failure("workspace");
  }
}

async function runWorkspaceGit(cwd: string, signal: AbortSignal, args: string[], maxBuffer: number, ignoreProbe?: string) {
  const child = spawnWorkspaceGit(cwd, signal, args, maxBuffer);
  try {
    if (ignoreProbe !== undefined) {
      child.child.stdin?.on("error", () => {});
      child.child.stdin?.end(ignoreProbe);
    }
    const { stdout } = await child;
    // The write command's output isn't needed, even on success.
    return args[0] === "worktree" && args[1] === "add" ? "" : decoder.decode(stdout);
  } catch (error) {
    signal.throwIfAborted();
    const { code, stderr } = error as { code?: number | string; stderr?: Buffer };
    if (ignoreProbe !== undefined && code === 1 && !stderr?.length) return "";
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
        throw failure("notGit");
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

export { directory as workspaceDirectoryIdentity };

const sameDirectory = (a: DirectoryIdentity, b: DirectoryIdentity) =>
  a.path === b.path && a.device === b.device && a.inode === b.inode && a.birth === b.birth;
const sameWorkspace = (a: GitWorkspaceIdentity, b: GitWorkspaceIdentity) =>
  sameDirectory(a, b) && sameDirectory(a.cwd, b.cwd) && sameDirectory(a.root, b.root) && sameDirectory(a.gitDir, b.gitDir);

const enoent = (error: unknown) => (error as NodeJS.ErrnoException).code === "ENOENT";

async function gitCheckout(cwd: DirectoryIdentity, signal: AbortSignal) {
  const result = await workspaceGit(cwd.path, signal, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir");
  const paths = result.slice(0, -1).split("\n");
  if (paths[0] === "--path-format=absolute") throw failure("gitUnsupported");
  if (!result.endsWith("\n") || paths.length !== 3 || !paths.every(path => validPath(path) && isAbsolute(path))) throw failure("workspace");
  const [top, admin, shared] = paths as [string, string, string];
  const root = await directory(top);
  const fromRoot = relative(root.path, cwd.path);
  if (top !== root.path || fromRoot === ".." || fromRoot.startsWith(".." + sep) || isAbsolute(fromRoot)) throw failure("workspace");
  const gitDir = await directory(admin);
  const common = admin === shared ? gitDir : await directory(shared);
  // A linked worktree's private Git directory must belong to the common directory.
  if (!sameDirectory(gitDir, common) && dirname(gitDir.path) !== join(common.path, "worktrees")) throw failure("workspace");
  return { root, gitDir, common };
}

async function resolveGitWorkspace(cwd: DirectoryIdentity, signal: AbortSignal): Promise<GitWorkspaceIdentity> {
  const { root, gitDir, common } = await gitCheckout(cwd, signal);
  if (root.path !== cwd.path) {
    // A nested repository can point core.worktree at an ancestor. Git invoked
    // at the reported root must select the same checkout as the workspace.
    const atRoot = await gitCheckout(root, signal);
    if (!sameDirectory(root, atRoot.root) || !sameDirectory(gitDir, atRoot.gitDir) || !sameDirectory(common, atRoot.common)) {
      throw failure("workspace");
    }
  }
  return { ...common, cwd, root, gitDir };
}

// Read-only probe for a checkout Hush just created or already authorized.
export async function discoverGitWorkspace(path: string, signal: AbortSignal) {
  return resolveGitWorkspace(await directory(path), signal);
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

function grantStamp(path: string, expected: Buffer) {
  // Keep the final grant decision in the action's turn. This reads one
  // fixed-format record bounded by the workspace path limit, not a directory.
  // Verify both the name and opened file; don't follow links or open a FIFO
  // in blocking mode.
  const uid = process.getuid?.();
  const valid = (info: NonNullable<ReturnType<typeof present>>) => uid !== undefined && info.isFile() &&
    info.nlink === 1n && info.uid === BigInt(uid) && (info.mode & 0o7077n) === 0n && info.size === BigInt(expected.length);
  const named = present(path);
  if (!named) return;
  if (!valid(named)) throw failure("storage");
  let fd: number;
  try { fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK); }
  catch (error) { if (enoent(error)) return; throw error; }
  try {
    const actual = fstatSync(fd, { bigint: true });
    if (actual.nlink === 0n) return; // Removed/replaced by another process.
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
    if (!current) return;
    if (!valid(current)) throw failure("storage");
    if (current.dev === actual.dev && current.ino === actual.ino && current.ctimeNs === actual.ctimeNs) {
      return `${actual.dev}:${actual.ino}:${actual.birthtimeNs}:${actual.ctimeNs}`;
    }
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

function storedGrant(agentDir: string, area: string, data: Buffer, signal?: AbortSignal) {
  signal?.throwIfAborted();
  const dir = join(agentDir, area);
  if (privateDir(agentDir, false) && privateDir(dir, false)) return grantStamp(join(dir, key(data)), data);
}

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
    if (change === undefined) return !!grantStamp(path, data);
    if (change) {
      if (!grantStamp(path, data)) publish(dir, path, data);
    } else {
      try { unlinkSync(path); } catch (error) { if (!enoent(error)) throw error; }
    }
    return change;
  } catch { throw failure("storage"); }
}

// One exact-directory grant authorizes tools, workspace browsing, and Git in
// that workspace. Keep the existing work records; never promote old Git-only
// or browse-only grants, and never inherit trust to another checkout or cwd.
const workArea = "workspace-work-v1", homeArea = "workspace-browse-v1";

// Backend-host authority only: never take the agent directory from a peer.
export class WorkspaceTrust {
  private readonly agentDir: string;

  constructor(agentDir: string) { this.agentDir = trustDirectory(agentDir); }

  // The CLI publishes/removes grant files in another process. Watch names,
  // including the area directories themselves (first grant or replacement).
  // A notification is only a hint to re-read the authoritative binding.
  watchGrants(change: () => void): () => void {
    const watchers: FSWatcher[] = [];
    let pending: NodeJS.Immediate | undefined, closed = false;
    const disarm = () => { for (const watcher of watchers.splice(0)) watcher.close(); };
    const pulse = () => {
      if (closed || pending) return;
      pending = setImmediate(() => {
        pending = undefined;
        arm();
        change();
      });
    };
    const add = (path: string, matches: (name: string) => boolean) => {
      try {
        const watcher = watch(path, { persistent: false }, (_event, name) => {
          if (name === null || matches(name)) pulse();
        });
        // Filesystems without watch support still get server-side refreshes.
        watcher.on("error", () => watcher.close());
        watchers.push(watcher);
      } catch { /* An area need not exist yet. */ }
    };
    const arm = () => {
      disarm();
      add(dirname(this.agentDir), name => name === basename(this.agentDir));
      try {
        if (!privateDir(this.agentDir, false)) return;
        add(this.agentDir, name => name === workArea || name === homeArea);
        for (const area of [workArea, homeArea]) {
          const path = join(this.agentDir, area);
          if (privateDir(path, false)) add(path, name => /^[0-9a-f]{64}$/.test(name));
        }
      } catch { /* Unsafe grant storage never grants access. */ }
    };
    arm();
    return () => { closed = true; clearImmediate(pending); disarm(); };
  }

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

  // Grant exactly this directory. A path now naming another directory fails.
  grantIdentity(expected: DirectoryIdentity, signal: AbortSignal) {
    return this.change(expected.path, "grant", signal, key(record("work", expected)));
  }

  async require(workspace: string, signal?: AbortSignal) {
    return (await this.binding(workspace, signal)).path;
  }

  async binding(workspace: string, signal?: AbortSignal) {
    signal?.throwIfAborted();
    const identity = await directory(workspace);
    const stamp = this.#stamp(identity, signal);
    if (!stamp) throw failure("work");
    return { ...identity, stamp };
  }

  // Recheck the directory, repository and grant at the operation boundary.
  // Start the action in the same turn as the final authoritative grant read.
  async withGit<T>(workspace: string, signal: AbortSignal, action: (identity: GitWorkspaceIdentity) => T | Promise<T>,
      expected?: GitWorkspaceIdentity): Promise<T> {
    signal.throwIfAborted();
    const cwd = await directory(workspace);
    if (!this.#stamp(cwd, signal)) throw failure("grant");
    const identity = await resolveGitWorkspace(cwd, signal);
    if ((expected && !sameWorkspace(identity, expected)) || !sameDirectory(cwd, await directory(workspace))) throw failure("workspace");
    if (!this.#stamp(cwd, signal)) throw failure("grant");
    return action(identity);
  }

  // Home browsing is separate and never admits tools or Git. Its root is
  // fixed by the backend profile, not by the requester or the workspace.
  async homeBinding(signal: AbortSignal) {
    signal.throwIfAborted();
    const identity = await directory(dirname(this.agentDir));
    const stamp = storedGrant(this.agentDir, homeArea, record("home", identity), signal);
    return stamp === undefined ? undefined : { ...identity, stamp };
  }

  async changeHome(signal: AbortSignal, change?: boolean) {
    const identity = await directory(dirname(this.agentDir));
    return { kind: "directory" as const, path: identity.path,
      trusted: access(this.agentDir, homeArea, record("home", identity), signal, change) };
  }

  #stamp(identity: DirectoryIdentity, signal?: AbortSignal) {
    try { return storedGrant(this.agentDir, workArea, record("work", identity), signal); }
    catch { signal?.throwIfAborted(); throw failure("storage"); }
  }

  #status(identity: DirectoryIdentity, signal?: AbortSignal) {
    return { kind: "directory" as const, path: identity.path, trusted: this.#access(identity, "work", signal),
      dismissed: this.#access(identity, "dismissed", signal) };
  }

  #access(identity: DirectoryIdentity, purpose: "work" | "dismissed", signal?: AbortSignal, change?: boolean) {
    return access(this.agentDir, workArea, record(purpose, identity), signal, change);
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
    const signal = AbortSignal.timeout(30_000), store = new WorkspaceTrust(hushDirectory());
    // Private entry points for the managed local TUI, never socket methods.
    if (action === "review-work" || action === "grant-reviewed-work" || action === "dismiss-reviewed-work") {
      const cwd = values.cwd;
      if (!cwd || !isAbsolute(cwd) || values.scope !== undefined) throw new Error();
      if (action === "review-work" && values.expect === undefined) {
        process.stdout.write(JSON.stringify(await store.review(cwd, signal)) + "\n");
      } else if (action !== "review-work" && values.expect && /^[0-9a-f]{64}$/.test(values.expect)) {
        await store.change(cwd, action === "grant-reviewed-work" ? "grant" : "dismiss", signal, values.expect);
      } else throw new Error();
      return 0;
    }
    const scope = values.scope;
    if (action !== "status" && action !== "grant" && action !== "revoke" || values.expect !== undefined ||
        scope !== undefined && scope !== "work" && scope !== "home" || scope === "home" && values.cwd !== undefined) throw new Error();
    const workspace = resolve(values.cwd ?? ".");
    // --scope work is a compatibility alias. The reviewed identity stays
    // internal to the local confirmation flow.
    const state = scope === "home" ? await store.changeHome(signal, action === "status" ? undefined : action === "grant") :
      action === "status" ? await store.review(workspace, signal) : await store.change(workspace, action, signal);
    const { kind, path, trusted } = state;
    process.stdout.write(values.json ? JSON.stringify({ kind, path, trusted, ...("dismissed" in state && { dismissed: state.dismissed }) }) + "\n" :
      `${trusted ? "trusted" : "not trusted"} ${kind} ${JSON.stringify(path)}\n`);
    return 0;
  } catch (error) {
    process.stderr.write("hush: " + (error instanceof WireError ? error.message :
      "Use trust status, grant, or revoke with --cwd PATH for a workspace or --scope home for home browsing, on the Hush backend host.") + "\n");
    return 1;
  }
}
