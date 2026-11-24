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
  const db = new DatabaseSync(path);
  try {
    // Check the mode rather than assuming an arbitrary existing lock file
    // hasn't been opened in WAL mode. BEGIN EXCLUSIVE then excludes other owners.
    const mode = db.prepare("PRAGMA journal_mode").get()?.journal_mode;
    if (mode !== "delete") throw new Error("unexpected lock database mode");
    db.exec("PRAGMA busy_timeout = 0; BEGIN EXCLUSIVE");
    return () => db.close();
  } catch {
    db.close();
    throw new BackendError("data_locked");
  }
}
