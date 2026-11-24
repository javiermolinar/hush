import { lstat, unlink } from "node:fs/promises";
import { createConnection } from "node:net";
import { BackendError } from "./config.js";

export async function checkSocket(path: string, mode: "refuse" | "inspect" | "clean") {
  const info = await lstat(path).catch((error: NodeJS.ErrnoException) => {
    if (error.code !== "ENOENT") throw error;
  });
  if (!info) return;
  if (mode === "refuse" || !info.isSocket() || info.uid !== process.getuid?.()) throw new BackendError("socket_unavailable");
  const stale = await new Promise<boolean>(resolve => {
    const peer = createConnection(path);
    peer.setTimeout(250);
    const done = (ok: boolean) => { peer.destroy(); resolve(ok); };
    peer.once("connect", () => done(false));
    peer.once("timeout", () => done(false));
    peer.once("error", (err: NodeJS.ErrnoException) => done(err.code === "ECONNREFUSED" || err.code === "ENOENT"));
  });
  if (!stale) throw new BackendError("socket_unavailable");
  if (mode === "clean") await unlink(path);
}
