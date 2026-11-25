import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import fs, { constants, type BigIntStats } from "node:fs";
import { lstat, open, realpath } from "node:fs/promises";
import { isAbsolute, join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { setTimeout as delay } from "node:timers/promises";
import type { AuthOperationOptions, Credential, CredentialStore, OAuthCredential } from "@earendil-works/pi-ai";
import { authProvider, authProviders, defaultAuthProvider } from "./auth-providers.js";
import { configValue } from "./config-value.js";
import { readProfileJSON } from "./profile-json.js";
import { prepareAuthFile, privateFile } from "./profile.js";
import { replaceStoreJSON } from "./store-files.js";

const maxCredentialBytes = 64 * 1024; // private pipe and platform helper bound
const selectionName = "credential-store";
const advice = {
  selection: "Set ~/.hush/credential-store to file or keyring (private, mode 600), then restart the Hush backend.",
  changed: "The Hush credential store selection changed. Stop and restart the backend before using the new store.",
  locked: "Hush keyring is locked or unavailable. Unlock the login Keychain or the session's Secret Service on the backend host, then retry 'hush auth status'.",
  helper: "Hush keyring helper is unavailable. Start the backend with the installed hush binary (or set HUSH_KEYRING_HELPER to its trusted absolute path).",
  busy: "Hush keyring is busy with another sign-in, refresh, or logout. Wait for it to finish, then retry.",
  record: "The Hush keyring credential is invalid. Remove it with 'hush auth logout PROVIDER' and sign in again.",
  fileLock: "Could not lock the Hush credential file. Check the private profile and retry.",
  fileBusy: "The Hush credential file is busy. Wait for the other sign-in, refresh, or logout to finish, then retry.",
  fileSize: "The Hush credential file would exceed 1 MiB. Check ~/.hush/auth.json.",
  apiKey: "The API key in ~/.hush/auth.json must resolve to a single-line value without shell commands.",
} as const;

class CredentialError extends Error {
  constructor(readonly kind: keyof typeof advice) { super(advice[kind]); }
}

export function credentialAdvice(error: unknown) {
  for (let i = 0; i < 8 && error instanceof Error; i++, error = error.cause) {
    if (error instanceof CredentialError) return advice[error.kind];
  }
}

export function credentialConfigurationFailed(error: unknown) {
  return error instanceof CredentialError && error.kind === "selection";
}

async function selection(dir: string): Promise<"file" | "keyring"> {
  try {
    const fd = await open(join(dir, selectionName), constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    try {
      const st = await fd.stat();
      if (!st.isFile() || st.nlink !== 1 || st.uid !== process.getuid?.() || (st.mode & 0o077) !== 0 || st.size > 16) {
        throw new CredentialError("selection");
      }
      const bytes = Buffer.alloc(17);
      const { bytesRead } = await fd.read(bytes, 0, bytes.length, 0);
      const value = bytes.subarray(0, bytesRead).toString("utf8").trim();
      if (value === "file" || value === "keyring") return value;
      throw new CredentialError("selection");
    } finally { await fd.close(); }
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return "file";
    throw new CredentialError("selection");
  }
}

export async function acquireCredentialLock(dir: string, signal?: AbortSignal) {
  const path = join(dir, "keyring-lock.sqlite");
  let db: DatabaseSync;
  try {
    signal?.throwIfAborted();
    const file = await open(path, "wx", 0o600).catch((error: NodeJS.ErrnoException) => {
      if (error.code !== "EEXIST") throw error;
    });
    await file?.close();
    if (!await privateFile(path)) throw new Error("missing lock");
    for (const suffix of ["-wal", "-shm", "-journal"]) await privateFile(path + suffix);
    db = new DatabaseSync(path);
  } catch (error) {
    signal?.throwIfAborted();
    throw new CredentialError("locked");
  }
  try {
    db.exec("PRAGMA busy_timeout = 0");
    const deadline = performance.now() + 20_000;
    for (;;) {
      signal?.throwIfAborted();
      try {
        // A kernel-owned lock, also released if this process dies. Nothing
        // (especially no credential data) is written to this database.
        db.exec("BEGIN EXCLUSIVE");
        return () => db.close();
      } catch (error) {
        const errcode = (error as { errcode?: number }).errcode ?? 0;
        if ((errcode & 0xff) !== 5 && (errcode & 0xff) !== 6) throw new CredentialError("locked");
        if (performance.now() >= deadline) throw new CredentialError("busy");
        await delay(50, undefined, { signal });
      }
    }
  } catch (error) {
    db.close();
    throw error;
  }
}

function canonicalRecord(id: string, value: unknown): OAuthCredential {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new CredentialError("record");
  const fields = value as Record<string, unknown>;
  const { type, access, refresh, expires, accountId } = fields;
  if (type !== "oauth" || typeof access !== "string" || !access || typeof refresh !== "string" ||
      (!refresh && id !== "openrouter") || typeof expires !== "number" || !Number.isFinite(expires) || expires <= 0 ||
      (id === defaultAuthProvider && (typeof accountId !== "string" || !accountId))) {
    throw new CredentialError("record");
  }
  // Pi uses extra fields during refresh and model selection (e.g. ChatGPT's
  // issued client ID and Copilot's available model IDs). Keep them intact.
  return { ...fields, type, access, refresh, expires };
}

class ProfileKeyring implements CredentialStore {
  readonly #helper = process.env.HUSH_KEYRING_HELPER;
  // The keychain belongs to the OS account, not a possibly overridden
  // application HOME. Linux additionally needs the logged-in session bus.
  readonly #env = { PATH: "/usr/bin:/bin",
    DBUS_SESSION_BUS_ADDRESS: process.platform === "linux" ? process.env.DBUS_SESSION_BUS_ADDRESS : undefined };

  constructor(private readonly dir: string, private readonly scope: string, private readonly checkSelection: () => Promise<void>) {}

  async #call(operation: "get" | "set" | "delete", id: string, record?: OAuthCredential, signal?: AbortSignal) {
    signal?.throwIfAborted();
    if (!this.#helper || !isAbsolute(this.#helper)) throw new CredentialError("helper");
    if (record && Buffer.byteLength(JSON.stringify(record)) > maxCredentialBytes) throw new CredentialError("record");
    const input = JSON.stringify({ operation, scope: this.scope,
      provider: id === defaultAuthProvider ? undefined : id, record });
    return new Promise<string>((resolve, reject) => {
      const child = spawn(this.#helper!, ["--hush-internal-keyring"], {
        env: this.#env, stdio: ["pipe", "pipe", "ignore"], signal, timeout: 4_000, killSignal: "SIGKILL",
      });
      const chunks: Buffer[] = [];
      let size = 0, failed = false, launchFailed = false;
      child.on("error", () => { failed = true; launchFailed = child.pid === undefined; });
      child.stdin.on("error", () => { failed = true; });
      child.stdout.on("data", (chunk: Buffer) => {
        size += chunk.length;
        if (size > maxCredentialBytes) { failed = true; child.kill("SIGKILL"); }
        else chunks.push(chunk);
      });
      // Wait for close, not the abort notification, before releasing a mutation lock.
      child.on("close", code => {
        try {
          signal?.throwIfAborted();
          if (launchFailed) throw new CredentialError("helper");
          if (failed || code !== 0) throw new CredentialError(code === 2 ? "record" : "locked");
          if (operation !== "get" && size !== 0) throw new CredentialError("record");
          resolve(Buffer.concat(chunks, size).toString("utf8"));
        } catch (error) { reject(error); }
        finally { for (const chunk of chunks) chunk.fill(0); }
      });
      child.stdin.end(input);
    });
  }

  async read(id: string, options?: AuthOperationOptions) {
    await this.checkSelection();
    if (!authProvider(id)) return;
    const data = await this.#call("get", id, undefined, options?.signal);
    if (!data) return;
    try { return canonicalRecord(id, JSON.parse(data)); }
    catch { throw new CredentialError("record"); }
  }

  async list(options?: AuthOperationOptions) {
    const entries = await Promise.all(authProviders.map(async ({ id }) => {
      const saved = await this.read(id, options);
      return saved ? { providerId: id, type: saved.type } : undefined;
    }));
    return entries.filter(entry => entry !== undefined);
  }

  modify(id: string, edit: (current: Credential | undefined) => Promise<Credential | undefined>, options?: AuthOperationOptions) {
    return this.exclusive(store => store.modify(id, edit, options), options?.signal);
  }

  delete(id: string, options?: AuthOperationOptions) {
    return this.exclusive(store => store.delete(id, options), options?.signal);
  }

  // A locked view prevents reacquiring the same lock when ModelRuntime writes
  // at the end of interactive login. Logout cannot interleave with that login.
  async exclusive<T>(job: (store: CredentialStore) => Promise<T>, signal?: AbortSignal): Promise<T> {
    const unlock = await acquireCredentialLock(this.dir, signal);
    let active = true;
    try {
      await this.checkSelection();
      signal?.throwIfAborted();
      return await job({
        read: this.read.bind(this), list: this.list.bind(this),
        modify: async (id, edit, options) => {
          if (!active || !authProvider(id)) throw new CredentialError("record");
          const current = await this.read(id, options);
          options?.signal?.throwIfAborted();
          const next = await edit(current);
          options?.signal?.throwIfAborted();
          if (next === undefined) return current;
          const record = canonicalRecord(id, next);
          await this.checkSelection();
          await this.#call("set", id, record);
          return record;
        },
        delete: async (id, options) => {
          if (!active || !authProvider(id)) throw new CredentialError("record");
          await this.checkSelection();
          options?.signal?.throwIfAborted();
          await this.#call("delete", id);
        },
      });
    } finally { active = false; unlock(); }
  }
}

const maxAuthFileBytes = 1024 * 1024;
function storedCredential(file: Record<string, unknown>, id: string): Credential | undefined {
  if (!Object.hasOwn(file, id)) return;
  const record = file[id] as Partial<Credential> | null;
  if (!record || typeof record !== "object") return;
  if (record.type === "api_key" && (record.key === undefined || typeof record.key === "string") &&
      (record.env === undefined || !!record.env && typeof record.env === "object" && !Array.isArray(record.env) &&
        Object.values(record.env).every(value => typeof value === "string"))) return record as Credential;
  if (record.type === "oauth" && typeof record.access === "string" && typeof record.refresh === "string" &&
      typeof record.expires === "number" && Number.isFinite(record.expires)) return record as Credential;
}

// Model auth can return on cancellation while a previously started refresh
// persists the rotated token. Drain writes before dropping the daemon lock.
export class CredentialWrites {
  readonly #pending = new Set<Promise<unknown>>();

  track<T>(work: Promise<T>): Promise<T> {
    this.#pending.add(work);
    const done = () => { this.#pending.delete(work); };
    void work.then(done, done);
    return work;
  }

  async settle() {
    while (this.#pending.size) await Promise.allSettled(this.#pending);
  }
}

function trackedStore(store: CredentialStore, writes: CredentialWrites): CredentialStore {
  return {
    read: store.read.bind(store), list: store.list.bind(store),
    modify: (...args) => writes.track(store.modify(...args)),
    delete: (...args) => writes.track(store.delete(...args)),
  };
}

// Older Hush daemons use proper-lockfile on the resolved auth.json path.
// Hold that lock inside the SQLite mutex (used by newer Hush builds) across
// the credential read, provider exchange and atomic publication.
async function acquireLegacyFileLock(path: string, signal?: AbortSignal) {
  const { default: lockfile } = await import("proper-lockfile");
  path = await realpath(path);
  const deadline = performance.now() + 31_000;
  const lockPath = path + ".lock";
  let owner: BigIntStats | undefined;
  const owns = (info: BigIntStats | undefined) => info && owner && info.dev === owner.dev && info.ino === owner.ino;
  // If a stopped process wakes after the legacy lease expired, its cleanup
  // must not remove the replacement writer's directory.
  const remove = (file: string) => {
    if (!owner || owns(fs.lstatSync(file, { bigint: true, throwIfNoEntry: false }))) fs.rmdirSync(file);
  };
  const lockFs = { ...fs, rmdirSync: remove,
    rmdir(file: string, callback: (error: NodeJS.ErrnoException | null) => void) {
      if (!owner) return fs.rmdir(file, callback); // acquiring an absent or stale lock
      try { remove(file); } catch (error) { return callback(error as NodeJS.ErrnoException); }
      callback(null);
    },
  };
  let compromised = false;
  let release: () => Promise<void>;
  for (;;) {
    signal?.throwIfAborted();
    try {
      release = await lockfile.lock(path, {
        fs: lockFs, stale: 30_000, update: 1_000, onCompromised: () => { compromised = true; },
      });
      break;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ELOCKED") throw error;
      if (performance.now() >= deadline) throw new CredentialError("fileBusy");
      await delay(50, undefined, { signal });
    }
  }
  const unlock = () => release().catch(() => { /* a compromised lock may already be gone */ });
  try {
    owner = await lstat(lockPath, { bigint: true });
    if (!owner.isDirectory()) throw new CredentialError("fileLock");
    const verify = async () => {
      const current = await lstat(lockPath, { bigint: true }).catch(() => undefined);
      // Some older synchronous writers consider locks stale after 10 seconds.
      if (compromised || !current || !owns(current) || Date.now() - Number(current.mtimeMs) >= 10_000)
        throw new CredentialError("fileLock");
      signal?.throwIfAborted();
    };
    await verify();
    return { verify, unlock };
  } catch (error) { await unlock(); throw error; }
}

class ProfileFile implements CredentialStore {
  constructor(private readonly dir: string, private readonly path: string, private readonly check: () => Promise<void>) {}

  async #read(options?: AuthOperationOptions) {
    await this.check();
    options?.signal?.throwIfAborted();
    return await readProfileJSON(this.path, maxAuthFileBytes, true) ?? {};
  }

  async read(id: string, options?: AuthOperationOptions) {
    const record = storedCredential(await this.#read(options), id);
    if (record?.type !== "api_key" || record.key === undefined) return record;
    if (record.key.startsWith("!")) throw new CredentialError("apiKey");
    const key = configValue(record.key, record.env);
    if (key && /[\r\n\0]/.test(key)) throw new CredentialError("apiKey");
    return { ...record, key };
  }
  async list(options?: AuthOperationOptions) {
    const file = await this.#read(options);
    return Object.keys(file).flatMap(providerId => {
      const record = storedCredential(file, providerId);
      return record ? [{ providerId, type: record.type }] : [];
    });
  }

  async #locked<T>(options: AuthOperationOptions | undefined,
      job: (file: Record<string, unknown>, publish: (file: Record<string, unknown>) => Promise<void>) => Promise<T>) {
    await this.check();
    const signal = options?.signal;
    const unlock = await acquireCredentialLock(this.dir, signal).catch(error => {
      signal?.throwIfAborted();
      throw new CredentialError(error instanceof CredentialError && error.kind === "busy" ? "fileBusy" : "fileLock");
    });
    try {
      await this.check();
      await prepareAuthFile(this.path, true);
      const legacy = await acquireLegacyFileLock(this.path, signal).catch(error => {
        signal?.throwIfAborted();
        throw error instanceof CredentialError ? error : new CredentialError("fileLock");
      });
      try {
        return await job(await this.#read(options), async file => {
          if (Buffer.byteLength(JSON.stringify(file)) + 1 > maxAuthFileBytes) throw new CredentialError("fileSize");
          await replaceStoreJSON(this.path, file, async () => {
            await this.check();
            await legacy.verify();
          });
        });
      } finally { await legacy.unlock(); }
    } finally { unlock(); }
  }

  modify(id: string, edit: (current: Credential | undefined) => Promise<Credential | undefined>, options?: AuthOperationOptions) {
    // Hold the lock through the provider's refresh exchange. A second process
    // sees the rotated credential, or a logout, rather than renewing stale data.
    return this.#locked(options, async (file, publish) => {
      const current = storedCredential(file, id);
      const next = await edit(current);
      if (next !== undefined) await publish({ ...file, [id]: next });
      return next ?? current;
    });
  }

  async delete(id: string, options?: AuthOperationOptions) {
    await this.check();
    if (!await privateFile(this.path)) return;
    await this.#locked(options, async (file, publish) => {
      if (!Object.hasOwn(file, id)) return;
      delete file[id];
      await publish(file);
    });
  }
}

function readOnly(read: CredentialStore["read"]): CredentialStore {
  const immutable = async () => { throw new Error("Private credential store cannot write"); };
  return {
    async read(id, options) {
      options?.signal?.throwIfAborted();
      const saved = await read(id, options);
      if (saved?.type === "oauth" && saved.expires <= Date.now() + 5 * 60_000) return;
      return saved;
    },
    list: async () => [], modify: immutable, delete: immutable,
  };
}

export async function hushCredentialStore(dir: string, options: { createFile: boolean; readOnly?: boolean; writes?: CredentialWrites }):
    Promise<{ credentials: CredentialStore; keyring?: ProfileKeyring }> {
  const tracked = (store: CredentialStore) => options.writes ? trackedStore(store, options.writes) : store;
  const mode = await selection(dir);
  const check = async () => { if (await selection(dir) !== mode) throw new CredentialError("changed"); };
  if (mode === "keyring") {
    const scope = createHash("sha256").update(await realpath(dir)).digest("hex");
    const keyring = new ProfileKeyring(dir, scope, check);
    return options.readOnly ? { credentials: readOnly(keyring.read.bind(keyring)) } : { credentials: tracked(keyring), keyring };
  }
  const path = join(dir, "auth.json");
  const file = new ProfileFile(dir, path, check);
  if (options.readOnly) return { credentials: readOnly(file.read.bind(file)) };
  await prepareAuthFile(path, options.createFile);
  return { credentials: tracked(file) };
}
