import { constants } from "node:fs";
import { lstat, open, readlink, type FileHandle } from "node:fs/promises";
import { isAbsolute, join } from "node:path";

const maxFileBytes = 32 * 1024;
const readFlags = constants.O_RDONLY | constants.O_NONBLOCK;
// These flags are not exposed by Node. They are stable on the release targets.
const linuxOPath = 0o10000000; // Linux open(2): inspect an inode without opening it for I/O.
const darwinNoFollowAny = 0x20000000; // Darwin sys/fcntl.h: reject symlinks in *all* components.

export const workspaceFileError = "Expected a readable, single-link regular UTF-8 file of at most 32 KiB inside the thread's working directory (no symlinks).";

async function openInspection(path: string, signal?: AbortSignal): Promise<FileHandle> {
  if (process.platform === "linux") {
    // Checking realpath and then opening by pathname would race a symlink swap
    // in a parent directory. Verify the kernel path of the actual inode, then
    // open that same inode for reading. Fail closed without /proc/self/fd.
    const ref = await open(path, linuxOPath | constants.O_NOFOLLOW);
    try {
      const info = await ref.stat();
      const fdPath = `/proc/self/fd/${ref.fd}`;
      if (!info.isFile() || info.nlink !== 1 || info.size > maxFileBytes || await readlink(fdPath) !== path)
        throw new Error(workspaceFileError);
      signal?.throwIfAborted();
      return await open(fdPath, readFlags);
    } finally { await ref.close(); }
  }
  if (process.platform === "darwin") {
    if (!(await lstat(path)).isFile()) throw new Error(workspaceFileError);
    signal?.throwIfAborted();
    return open(path, readFlags | darwinNoFollowAny);
  }
  throw new Error(workspaceFileError);
}

export async function inspectWorkspaceText(cwd: string, path: string, signal?: AbortSignal) {
  // Require a physical workspace path too: resolving cwd with realpath here
  // would let a replaced workspace root redirect reads out of that workspace.
  if (!isAbsolute(cwd) || !path || Buffer.byteLength(path) > 1024 || isAbsolute(path) || path.includes("\0") ||
      path.includes("\\") || path.split("/").includes("..")) throw new Error(workspaceFileError);
  signal?.throwIfAborted();
  const file = await openInspection(join(cwd, path), signal);
  try {
    const info = await file.stat();
    if (!info.isFile() || info.nlink !== 1 || info.size > maxFileBytes) throw new Error(workspaceFileError);
    // Bound the read even if the file grows after stat(). Never read a FIFO.
    const data = Buffer.alloc(maxFileBytes + 1);
    let length = 0;
    while (length < data.length) {
      signal?.throwIfAborted();
      const { bytesRead } = await file.read(data, length, data.length - length, length);
      if (!bytesRead) break;
      length += bytesRead;
    }
    signal?.throwIfAborted();
    const bytes = data.subarray(0, length);
    if (length > maxFileBytes || bytes.includes(0)) throw new Error(workspaceFileError);
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } finally { await file.close(); }
}
