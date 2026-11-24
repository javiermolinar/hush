import { createHash, randomUUID } from "node:crypto";
import { constants } from "node:fs";
import { lstat, open, rename, rm } from "node:fs/promises";
import { dirname } from "node:path";
import { parseRecord } from "../../server/src/wire.js";

export async function statStorePath(path: string) {
  return lstat(path, { bigint: true }).catch((err: NodeJS.ErrnoException) => {
    if (err.code !== "ENOENT") throw err;
    return undefined;
  });
}

const readFlags = constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK;
export async function readStoreJSON(path: string) {
  const info = await statStorePath(path);
  if (!info) return undefined;
  const f = await open(path, readFlags);
  try {
    const st = await f.stat({ bigint: true });
    if (!st.isFile() || st.nlink !== 1n || st.dev !== info.dev || st.ino !== info.ino ||
        st.uid !== BigInt(process.getuid!()) || (st.mode & 0o077n) !== 0n || st.size > 4096n) throw new Error("invalid metadata");
    return parseRecord(await f.readFile());
  } finally { await f.close(); }
}

export async function sha256File(path: string) {
  const f = await open(path, readFlags);
  try {
    if (!(await f.stat()).isFile()) throw new Error("not a regular file");
    const hash = createHash("sha256");
    for await (const chunk of f.createReadStream({ autoClose: false })) hash.update(chunk);
    return hash.digest("hex");
  } finally { await f.close(); }
}

export async function syncPath(path: string, dir = false) {
  const f = await open(path, readFlags | (dir ? constants.O_DIRECTORY : 0));
  try { await f.sync(); }
  finally { await f.close(); }
}

// The old executable must see either the old manifest or a complete refusal
// marker. Do not unlink the destination, even after an interrupted write.
export async function replaceStoreJSON(path: string, value: object) {
  const temp = path + ".new-" + randomUUID();
  const f = await open(temp, "wx", 0o600);
  try {
    try {
      await f.writeFile(JSON.stringify(value) + "\n");
      await f.sync();
    } finally { await f.close(); }
    await rename(temp, path);
    await syncPath(dirname(path), true);
  } finally { await rm(temp, { force: true }); }
}
