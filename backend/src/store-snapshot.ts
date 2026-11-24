import { constants } from "node:fs";
import { chmod, copyFile, mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { backup, DatabaseSync } from "node:sqlite";
import { createRegistry, type RegistrySnapshot } from "@earendil-works/pi-durable";
import { object } from "../../server/src/wire.js";
import { applicationID, historyName, privateFile, sidecars } from "./profile.js";
import { sha256File, statStorePath, syncPath } from "./store-files.js";

// This is a format/registry compatibility table for the pinned predecessor,
// not an importer for arbitrary SQLite or for today's format-1 development stores.
export const durableVersion = "1.6.0";
export const durableSchema = 2;
export const predecessorTasks = { "pi.compaction": 1, "pi.generation": 3, "pi.tool": 4 } as const;
const documents: Readonly<Record<string, number>> = {
  "pi.agent": 1, "pi.inbox": 1, "pi.live": 3, "pi.provider": 1, "pi.tool.nested-call": 1, "pi.usage": 1,
  "hush.thread-presentation": 1,
};
const extensions = new Set(["hush-standalone-prompt", "coding-tools"]);
const tools = new Set(["read", "bash", "edit", "write"]);
// Recent development builds used format 1 with these standalone features.
// This wider set is for in-place compatibility only, never for pinned cutover.
const standaloneDocuments: Readonly<Record<string, number>> = {
  ...documents,
  "hush.checkpoints": 1, "hush.known-effects": 1, "hush.rewind": 1, "hush.schedule-index": 1, "hush.schedule": 1,
  "pi.codemode.run": 1, "pi.codemode.journal": 1, "pi.codemode.store": 1,
};
const tables = ["conversations", "document_revisions", "documents", "durable_metadata", "durable_schema",
  "entries", "record_ids", "submissions", "tasks"];
const registry = createRegistry().snapshot(); // Definitions only. Never open a Harness during validation.
const positiveID = (v: unknown) => typeof v === "number" && Number.isSafeInteger(v) && v > 0;

export class StoreValidationError extends Error {}
function reject(why: string): never { throw new StoreValidationError(why); }

async function readDatabase(path: string, standaloneCopy: boolean, read: (db: DatabaseSync) => void) {
  if (!await privateFile(path)) reject("Missing snapshot database.");
  if (standaloneCopy) for (const suffix of sidecars) {
    if (await statStorePath(path + suffix)) reject("Unexpected snapshot sidecar.");
  }
  const db = new DatabaseSync(path, { readOnly: true });
  try {
    db.exec("PRAGMA trusted_schema=OFF; PRAGMA query_only=ON");
    if (db.prepare("PRAGMA application_id").get()?.application_id !== applicationID) reject("Not a standalone Hush database.");
    const schema = db.prepare("SELECT name, type FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY name").all();
    const actual = schema.filter(row => row.type === "table").map(row => row.name);
    if (schema.some(row => row.type !== "table" && row.type !== "index") ||
        JSON.stringify(actual) !== JSON.stringify(tables) ||
        db.prepare("SELECT version FROM durable_schema WHERE singleton=1").get()?.version !== durableSchema ||
        db.prepare("PRAGMA integrity_check(1)").get()?.integrity_check !== "ok") reject("Unsupported or damaged Durable SQLite schema.");

    read(db);
  } catch (error) {
    if (error instanceof StoreValidationError) throw error;
    reject("Cannot validate the Durable database shape.");
  } finally { db.close(); }
}

function checkDocuments(db: DatabaseSync, supported: Readonly<Record<string, number>>) {
  for (const row of db.prepare(`SELECT DISTINCT json_extract(d.kind,'$') AS kind, r.version
    FROM documents d LEFT JOIN document_revisions r ON r.document_id=d.id`).iterate()) {
    if (typeof row.kind !== "string" || !Object.hasOwn(supported, row.kind) ||
        supported[row.kind] !== row.version) reject("Unsupported document registry in the source store.");
  }
}

async function inspect(path: string, standaloneCopy: boolean) {
  await readDatabase(path, standaloneCopy, db => {
    checkDocuments(db, documents);
    // Even retained historical agent revisions must not select unsupported
    // executable extensions, and worktree artifacts are not part of this slice.
    for (const row of db.prepare(`SELECT json_extract(d.kind,'$') AS doc_kind, r.kind AS revision_kind, r.content
      FROM documents d JOIN document_revisions r ON r.document_id=d.id
      WHERE d.kind IN (json_quote('pi.agent'),json_quote('hush.thread-presentation'))`).iterate()) {
      // The two definitions above checkpoint every revision in Durable 1.6.0.
      if (row.revision_kind !== "base") reject("Unexpected agent document delta.");
      const value: unknown = JSON.parse(String(row.content));
      if (!object(value)) reject("Unexpected agent document encoding.");
      if (row.doc_kind === "hush.thread-presentation" && Object.hasOwn(value, "checkout")) reject("Managed worktrees are not supported sources for this cutover.");
      if (row.doc_kind === "pi.agent" && value.extensions !== undefined &&
          (!Array.isArray(value.extensions) || value.extensions.some(name => !extensions.has(name)))) reject("Unsupported executable extension in the source store.");
    }

    const entry = db.prepare("SELECT record FROM entries WHERE id=?");
    const lookup = db.prepare("SELECT 1 FROM tasks WHERE id=?");
    const tasks = db.prepare("SELECT json_extract(kind,'$') AS kind,status,background,record FROM tasks");
    for (const row of tasks.iterate()) {
      const task: unknown = JSON.parse(String(row.record));
      const kind = String(row.kind);
      const definition = registry.task(kind)?.definition;
      if (!Object.hasOwn(predecessorTasks, kind) || !definition ||
          definition.version !== predecessorTasks[kind as keyof typeof predecessorTasks] || !object(task) ||
          task.kind !== kind || task.version !== definition.version || !object(task.state) || task.state.status !== row.status ||
          task.background !== false || row.background !== 0) reject("Unsupported task definition in the source store.");
      const { status, checkpoint, signals, until, on, policy, wake } = task.state;
      if (status === "terminal" || status === "completing") continue;
      if (!object(checkpoint) || typeof checkpoint.phase !== "string" || !Object.hasOwn(definition.phases, checkpoint.phase)) {
        reject("Unsupported task phase in the source store.");
      }
      if (kind === "pi.tool") {
        if (status === "running") reject("A running tool was left by the source. Quiesce its writers before cutover.");
        const input = task.input;
        if (!object(input) || input.kind !== "model" || !positiveID(input.assistant) || typeof input.callId !== "string") {
          reject("Unsupported held tool call in the source store.");
        }
        const assistant: unknown = JSON.parse(String(entry.get(input.assistant as number)?.record));
        const message = object(assistant) && assistant.kind === "pi.assistant" && Array.isArray(assistant.model) && assistant.model[0];
        const call = object(message) && message.role === "assistant" && Array.isArray(message.content) &&
          message.content.find(v => object(v) && v.type === "toolCall" && v.id === input.callId);
        if (!object(call) || typeof call.name !== "string" || !tools.has(call.name)) reject("Unsupported held tool definition in the source store.");
      }
      if (status === "sleeping" && (!Number.isSafeInteger(until) || Number(until) < 0 ||
          signals !== undefined && (!Array.isArray(signals) || signals.length !== 0))) reject("Unsupported sleep/wake definition in the source store.");
      if (status === "waiting" && (!Array.isArray(on) || !on.length || !["allSettled", "failFast", "any"].includes(String(policy)) ||
          on.some(id => !positiveID(id) || !lookup.get(id)))) reject("Unsupported task wait in the source store.");
      if (wake !== undefined && (!object(wake) || wake.reason !== "time" &&
          (wake.reason !== "tasks" || !Array.isArray(wake.ended) || !wake.ended.length ||
            wake.ended.some(id => !positiveID(id) || !lookup.get(id))))) reject("Unsupported task wake in the source store.");
    }
  });
}

// The caller holds the runtime writer lock. Only the private scratch copy may
// get a SQLite recovery or shm write, including when opening read-only.
async function withReadCopy<T>(runtime: string, dir: string, read: (clone: string) => Promise<T>) {
  const scratch = await mkdtemp(join(dir, "read-"));
  const clone = join(scratch, historyName), source = join(runtime, historyName);
  try {
    if (await statStorePath(source + "-journal")) reject("A rollback journal remains. Recover the source with the predecessor first.");
    await copyFile(source, clone, constants.COPYFILE_EXCL);
    if (await statStorePath(source + "-wal")) await copyFile(source + "-wal", clone + "-wal", constants.COPYFILE_EXCL);
    return await read(clone);
  } finally { await rm(scratch, { recursive: true, force: true }); }
}

export async function verifyLegacyStore(runtime: string, scratchParent: string, installed: RegistrySnapshot) {
  await withReadCopy(runtime, scratchParent, clone => readDatabase(clone, false, db => {
    checkDocuments(db, standaloneDocuments);
    for (const row of db.prepare("SELECT json_extract(kind,'$') AS kind,status,background,record FROM tasks").iterate()) {
      const definition = typeof row.kind === "string" ? installed.task(row.kind)?.definition : undefined;
      const task: unknown = JSON.parse(String(row.record));
      if (!definition || !object(task) || task.kind !== row.kind || task.version !== definition.version ||
          typeof task.background !== "boolean" || Number(task.background) !== row.background ||
          !object(task.state) || task.state.status !== row.status) reject("Unsupported task definition in the source store.");
      const { status, checkpoint } = task.state;
      if (status !== "terminal" && status !== "completing" &&
          (!object(checkpoint) || typeof checkpoint.phase !== "string" || !Object.hasOwn(definition.phases, checkpoint.phase))) {
        reject("Unsupported task phase in the source store.");
      }
    }
  }));
}

export async function makeSnapshot(runtime: string, dir: string, snapshot: string) {
  return withReadCopy(runtime, dir, async clone => {
    await inspect(clone, false);
    const db = new DatabaseSync(clone, { readOnly: true });
    try { await backup(db, snapshot); }
    finally { db.close(); }
    await chmod(snapshot, 0o600);
    // The archived snapshot is one self-contained database, not a runnable
    // directory and not a main file that requires its predecessor's WAL.
    const copy = new DatabaseSync(snapshot);
    try {
      if (copy.prepare("PRAGMA journal_mode=DELETE").get()?.journal_mode !== "delete") reject("Cannot detach the snapshot from WAL.");
    } finally { copy.close(); }
    await inspect(snapshot, true);
    await syncPath(snapshot);
    await syncPath(dir, true);
    return sha256File(snapshot);
  });
}

export async function verifySnapshot(path: string, expected: string) {
  await inspect(path, true);
  if (await sha256File(path) !== expected) reject("The snapshot digest does not match the prepared cutover.");
}
