import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { constants } from "node:fs";
import { open, realpath } from "node:fs/promises";
import { isAbsolute, join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { setTimeout as delay } from "node:timers/promises";
import type { AuthOperationOptions, Credential, CredentialStore, OAuthCredential } from "@earendil-works/pi-ai";
import { readStoredCredential } from "@earendil-works/pi-coding-agent";
import { authProvider, authProviders, defaultAuthProvider } from "./auth-providers.js";
import { AuthStorage, InMemoryCredentialStore } from "./pi-source.js";
import { prepareAuthFile, privateFile } from "./profile.js";

const maxCredentialBytes = 64 * 1024; // private pipe and platform helper bound
const selectionName = "credential-store";
const advice = {
  selection: "Set ~/.hush/credential-store to file or keyring (private, mode 600), then restart the Hush backend.",
  changed: "The Hush credential store selection changed. Stop and restart the backend before using the new store.",
  locked: "Hush keyring is locked or unavailable. Unlock the login Keychain or the session's Secret Service on the backend host, then retry 'hush auth status'.",
  helper: "Hush keyring helper is unavailable. Start the backend with the installed hush binary (or set HUSH_KEYRING_HELPER to its trusted absolute path).",
  busy: "Hush keyring is busy with another sign-in, refresh, or logout. Wait for it to finish, then retry.",
  record: "The Hush keyring credential is invalid. Remove it with 'hush auth logout PROVIDER' and sign in again.",
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

function guardedFile(store: CredentialStore, check: () => Promise<void>): CredentialStore {
  return {
    async read(id, options) { await check(); return store.read(id, options); },
    async list(options) { await check(); return store.list(options); },
    async modify(id, edit, options) {
      await check();
      return store.modify(id, async current => {
        await check();
        const next = await edit(current);
        await check();
        return next;
      }, options);
    },
    async delete(id, options) { await check(); return store.delete(id, options); },
  };
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

export async function hushCredentialStore(dir: string, options: { createFile: boolean; readOnly?: boolean }):
    Promise<{ credentials: CredentialStore; keyring?: ProfileKeyring }> {
  const mode = await selection(dir);
  const check = async () => { if (await selection(dir) !== mode) throw new CredentialError("changed"); };
  if (mode === "keyring") {
    const scope = createHash("sha256").update(await realpath(dir)).digest("hex");
    const keyring = new ProfileKeyring(dir, scope, check);
    return options.readOnly ? { credentials: readOnly(keyring.read.bind(keyring)) } : { credentials: keyring, keyring };
  }
  const path = join(dir, "auth.json");
  if (options.readOnly) return { credentials: readOnly(async id => {
    await check();
    return await privateFile(path) ? readStoredCredential(id, path) : undefined;
  }) };
  const present = await prepareAuthFile(path, options.createFile);
  const file = present ? AuthStorage.create(path) : new InMemoryCredentialStore();
  return { credentials: guardedFile(file, check) };
}
