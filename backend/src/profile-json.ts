import { constants } from "node:fs";
import { open } from "node:fs/promises";
import { object } from "../../server/src/wire.js";
import { BackendError } from "./config.js";

// Read one version from an open inode. Atomic replacement by another process
// is fine; links, non-private files and unbounded reads are not.
export async function readProfileText(path: string, limit: number): Promise<string | undefined> {
  const file = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK).catch((error: NodeJS.ErrnoException) => {
    if (error.code === "ENOENT") return undefined;
    throw new BackendError("configuration");
  });
  if (!file) return;
  try {
    const st = await file.stat();
    if (!st.isFile() || st.nlink !== 1 || st.uid !== process.getuid?.() || (st.mode & 0o077) !== 0 || st.size > limit) {
      throw new BackendError("configuration");
    }
    const bytes = Buffer.alloc(Math.min(st.size + 1, limit + 1));
    let n = 0;
    while (n < bytes.length) {
      const { bytesRead } = await file.read(bytes, n, bytes.length - n, n);
      if (!bytesRead) break;
      n += bytesRead;
    }
    if (n !== st.size) throw new BackendError("configuration");
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes.subarray(0, n));
  } catch { throw new BackendError("configuration"); }
  finally { await file.close(); }
}

export async function readProfileJSON(path: string, limit: number, empty = false): Promise<Record<string, unknown> | undefined> {
  const content = await readProfileText(path, limit);
  if (content === undefined) return;
  if (content === "" && empty) return {};
  try {
    const value: unknown = JSON.parse(content);
    if (object(value)) return value;
  } catch { /* do not forward content in parser errors */ }
  throw new BackendError("configuration");
}
