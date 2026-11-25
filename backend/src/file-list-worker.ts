import { spawn, type ChildProcessByStdio } from "node:child_process";
import type { Readable, Writable } from "node:stream";
import { object, WireError } from "../../server/src/wire.js";

// A search can return 32 paths of 2048 bytes, including JSON escaping.
const recordLimit = 512 * 1024;
const cancelGraceMs = 500;
type Child = ChildProcessByStdio<Writable, Readable, null>;
type Job = { id: number; chunk: Buffer[]; size: number; cancel: () => void;
  done: (error?: Error, value?: unknown) => void };
type Pending = { value: object; signal: AbortSignal; abort: () => void;
  resolve: (result: unknown) => void; reject: (error: unknown) => void };

// One private descriptor walker per backend. Only the Node host holds
// grants/cursors; every request reopens the root, even when reusing names.
export class FileListWorker {
  #child?: Child;
  #exited?: Promise<void>;
  #retiring = false;
  #closed = false;
  #job?: Job;
  #lastID = 0;
  #queue: Pending[] = [];
  #active?: Pending;
  #draining = false;

  constructor(private readonly executable: string) {}

  warm() {
    if (this.#child || this.#closed) return;
    const child = spawn(this.executable, ["--hush-internal-file-list"], { env: {}, stdio: ["pipe", "pipe", "ignore"] });
    this.#child = child;
    this.#exited = new Promise(resolve => child.once("close", () => {
      this.#job?.done(new WireError("not_found"));
      this.#child = undefined;
      this.#retiring = false;
      resolve();
    }));
    child.on("error", () => this.#retire());
    child.stdin.on("error", () => this.#retire());
    child.stdout.on("error", () => this.#retire());
    child.stdout.on("data", (chunk: Buffer) => {
      if (this.#closed || this.#retiring) return;
      const job = this.#job;
      if (!job || job.size + chunk.length > recordLimit) { this.#retire(); return; }
      job.chunk.push(chunk);
      job.size += chunk.length;
      const newline = chunk.indexOf(10);
      if (newline < 0) return;
      if (newline !== chunk.length - 1) { this.#retire(); return; }
      try {
        const reply: unknown = JSON.parse(Buffer.concat(job.chunk, job.size).toString("utf8"));
        if (!object(reply) || reply.id !== job.id) { this.#retire(); return; }
        const { id: _, ...value } = reply;
        job.done(undefined, value);
      } catch { this.#retire(); }
    });
  }

  #retire() {
    if (this.#retiring) return;
    this.#retiring = true;
    this.#job?.done(new WireError("not_found"));
    this.#child?.kill("SIGKILL"); // wait for close before reusing the stream
  }

  request(value: object, signal: AbortSignal) {
    return new Promise<unknown>((resolve, reject) => {
      signal.throwIfAborted();
      if (this.#closed) throw new WireError("not_found");
      const abort = () => {
        const i = this.#queue.indexOf(pending);
        if (i >= 0) this.#queue.splice(i, 1);
        // The caller stops now; the next job waits for the correlated reply
        // (a cancellation or an already-completed result) on this stream.
        if (this.#active === pending) this.#job?.cancel();
        reject(signal.reason);
      };
      const pending: Pending = { value, signal, abort, resolve, reject };
      signal.addEventListener("abort", abort, { once: true });
      this.#queue.push(pending);
      void this.#drain();
    });
  }

  async #drain() {
    if (this.#draining) return;
    this.#draining = true;
    try {
      for (let pending; (pending = this.#queue.shift());) {
        try {
          if (this.#retiring) await this.#exited;
          pending.signal.throwIfAborted();
          this.#active = pending;
          pending.resolve(await this.#exchange(pending.value));
        } catch (error) { pending.reject(error); }
        finally {
          this.#active = undefined;
          pending.signal.removeEventListener("abort", pending.abort);
        }
      }
    } finally { this.#draining = false; }
  }

  #exchange(value: object) {
    this.warm();
    const child = this.#child;
    if (!child || this.#retiring || this.#closed) throw new WireError("not_found");
    const id = ++this.#lastID;
    const line = JSON.stringify({ ...value, id }) + "\n";
    return new Promise<unknown>((resolve, reject) => {
      let timeout = setTimeout(() => this.#retire(), 3500);
      this.#job = { id, chunk: [], size: 0, cancel: () => {
        clearTimeout(timeout);
        timeout = setTimeout(() => this.#retire(), cancelGraceMs);
        child.stdin.write(JSON.stringify({ id, cancel: true }) + "\n");
      }, done: (error, value) => {
        clearTimeout(timeout);
        this.#job = undefined;
        if (error) reject(error); else resolve(value);
      } };
      child.stdin.write(line);
    });
  }

  async close() {
    if (this.#closed) return this.#exited;
    this.#closed = true;
    this.#job?.done(new WireError("not_found"));
    if (!this.#child || this.#retiring) return this.#exited;
    const timeout = setTimeout(() => this.#retire(), cancelGraceMs);
    this.#child.stdin.end();
    try { await this.#exited; } finally { clearTimeout(timeout); }
  }
}
