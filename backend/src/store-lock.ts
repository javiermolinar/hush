import { open } from "node:fs/promises";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { BackendError } from "./config.js";
import { privateFile } from "./profile.js";

// Never replace this inode while another launcher can be using it. In the
// runtime directory it is also the interlock used by the format-1 executable.
export async function dataLock(dir: string) {
  const path = join(dir, "owner.sqlite");
  const file = await open(path, "wx", 0o600).catch((err: NodeJS.ErrnoException) => {
    if (err.code !== "EEXIST") throw err;
  });
  await file?.close();
  if (!await privateFile(path)) throw new BackendError("data_locked");
  for (const suffix of ["-wal", "-shm", "-journal"]) await privateFile(path + suffix);
  // On an empty lock database, simultaneous journal_mode reads can all
  // briefly hold a shared lock, then all lose their exclusive upgrade. Give
  // that transient contention a bounded retry; a live owner still wins.
  const attempts = 3;
  for (let attempt = 0; attempt < attempts; attempt++) {
    const db = new DatabaseSync(path);
    try {
      // Check the mode before taking the lock; WAL cannot exclude readers.
      const mode = db.prepare("PRAGMA journal_mode").get()?.journal_mode;
      if (mode !== "delete") throw new BackendError("data_locked");
      db.exec("PRAGMA busy_timeout = 0; BEGIN EXCLUSIVE");
      return () => db.close();
    } catch (error) {
      db.close();
      const code = (error as { errcode?: number }).errcode ?? 0;
      if (attempt + 1 === attempts || (code & 0xff) !== 5 && (code & 0xff) !== 6) break;
      await new Promise(resolve => setTimeout(resolve, 5 + (process.pid + 7 * attempt) % 23));
    }
  }
  throw new BackendError("data_locked");
}
