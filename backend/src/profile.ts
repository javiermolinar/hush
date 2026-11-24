import { randomUUID } from "node:crypto";
import { constants, type BigIntStats } from "node:fs";
import { link, lstat, mkdir, open, readdir, rename, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { validStoreIdentity, type StoreIdentity } from "../../server/src/store-identity.js";
import { parseRecord } from "../../server/src/wire.js";
import { BackendError } from "./config.js";

export const hushDefaultModel = { provider: "openai-codex", modelId: "gpt-6-sol" };

export async function privateDirectory(path: string, create: boolean, optional = false) {
  if (create) await mkdir(path, { mode: 0o700, recursive: true });
  const info = await lstat(path).catch((err: NodeJS.ErrnoException) => {
    if (!optional || err.code !== "ENOENT") throw err;
  });
  if (info && (!info.isDirectory() || (info.mode & 0o777) !== 0o700 || info.uid !== process.getuid?.())) {
    throw new BackendError("socket_directory");
  }
}

export async function privateFile(path: string) {
  const info = await lstat(path).catch((err: NodeJS.ErrnoException) => {
    if (err.code !== "ENOENT") throw err;
  });
  if (info && (!info.isFile() || info.nlink !== 1 || info.uid !== process.getuid?.() || (info.mode & 0o077) !== 0)) {
    throw new BackendError("configuration");
  }
  return !!info;
}

// The library handles credential mutations under its lock. Reserve the file
// exclusively first: two first-time commands must not race to initialize it.
export async function prepareAuthFile(path: string, create: boolean) {
  if (await privateFile(path)) return true;
  if (!create) return false;
  // An empty file is an empty store to the library. Do not write even an
  // initial JSON object outside its lock after winning exclusive creation.
  const file = await open(path, "wx", 0o600).catch((err: NodeJS.ErrnoException) => {
    if (err.code !== "EEXIST") throw err;
  });
  await file?.close();
  if (!await privateFile(path)) throw new BackendError("configuration");
  return true;
}

const historyName = "history.sqlite";
const bindingName = "history.identity"; // hard link, never opened by SQLite
const markerName = "store-profile";
// SQLite application_id, independent of Durable's schema/user_version.
const applicationID = 0x48555348; // HUSH
const sqliteMagic = Buffer.from("SQLite format 3\0", "ascii");
const stat = (path: string) => lstat(path, { bigint: true }).catch((err: NodeJS.ErrnoException) => {
  if (err.code !== "ENOENT") throw err;
  return undefined;
});
const incompatible = () => new BackendError("store_profile");
const owned = (info: BigIntStats | undefined, links: bigint): info is BigIntStats =>
  !!info && info.isFile() && info.nlink === links && info.uid === BigInt(process.getuid!()) && (info.mode & 0o077n) === 0n;
const sameFile = (a: BigIntStats, b: BigIntStats) => a.dev === b.dev && a.ino === b.ino;

// Inspect under the writer lock, before touching a stale socket or letting SQLite
// open history (even read-only SQLite may touch WAL sidecars). Only a fresh
// directory is eligible for initialization; incomplete bootstrap fails closed.
export async function checkStoreProfile(dir: string, hush: boolean): Promise<StoreIdentity | undefined> {
  const marker = join(dir, markerName), history = join(dir, historyName);
  const manifest = await stat(marker);
  if (!hush) {
    if (manifest || await stat(join(dir, bindingName))) throw incompatible();
    return;
  }
  if (!manifest) {
    if ((await readdir(dir)).some(name => !["owner.sqlite", "owner.sqlite-journal", "hush.sock", "hush.log"].includes(name))) throw incompatible();
    return;
  }
  try {
    if (!owned(manifest, 1n) || manifest.size > 512n) throw incompatible();
    const mf = await open(marker, constants.O_RDONLY | constants.O_NOFOLLOW);
    let value: unknown;
    try { value = parseRecord(await mf.readFile()); }
    finally { await mf.close(); }
    if (!value || typeof value !== "object" || Array.isArray(value) || Object.keys(value).length !== 6) throw incompatible();
    const { profile, format, storeId, generation, device, inode } = value as Record<string, unknown>;
    const id = { storeId, generation };
    if (profile !== "hush" || format !== 1 || !validStoreIdentity(id)) throw incompatible();
    const file = await stat(history), binding = await stat(join(dir, bindingName));
    if (!owned(file, 2n) || !owned(binding, 2n) || !sameFile(file, binding) ||
        device !== String(file.dev) || inode !== String(file.ino)) throw incompatible();
    for (const suffix of ["-wal", "-shm", "-journal"]) {
      const sidecar = await stat(history + suffix);
      if (sidecar && !owned(sidecar, 1n)) throw incompatible();
    }
    const fd = await open(history, constants.O_RDONLY | constants.O_NOFOLLOW);
    try {
      const header = Buffer.alloc(100);
      const actual = await fd.stat({ bigint: true });
      const { bytesRead } = await fd.read(header, 0, header.length, 0);
      if (!sameFile(file, actual) || bytesRead !== header.length || !header.subarray(0, 16).equals(sqliteMagic) ||
          ![1, 2].includes(header[18]!) || ![1, 2].includes(header[19]!) || header.readUInt32BE(68) !== applicationID) {
        throw incompatible();
      }
    } finally { await fd.close(); }
    return id;
  } catch { throw incompatible(); }
}

// Called only after the fresh-store check and socket liveness check, with the
// writer lock still held. A crash can leave an incomplete store, never a store
// that a later boot silently adopts. Do not open the hard-link alias in SQLite.
export async function initializeStore(dir: string): Promise<StoreIdentity> {
  const marker = join(dir, markerName), history = join(dir, historyName);
  await writeFile(marker, "initializing\n", { mode: 0o600, flag: "wx" });
  await (await open(history, "wx", 0o600)).close();
  const db = new DatabaseSync(history);
  try { db.exec(`PRAGMA application_id = ${applicationID}`); }
  finally { db.close(); }
  await link(history, join(dir, bindingName));
  const file = await lstat(history, { bigint: true });
  const id = { storeId: randomUUID(), generation: randomUUID() };
  const pending = marker + ".new";
  await writeFile(pending, JSON.stringify({ profile: "hush", format: 1, ...id, device: String(file.dev), inode: String(file.ino) }) + "\n",
    { mode: 0o600, flag: "wx", flush: true });
  await rename(pending, marker);
  return id;
}
