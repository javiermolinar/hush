import { spawn, type ChildProcessByStdio } from "node:child_process";
import type { Readable, Writable } from "node:stream";
import { WireError } from "../../server/src/wire.js";

// A search can return 32 paths of 2048 bytes, including JSON escaping.
const recordLimit = 512 * 1024;
type Child = ChildProcessByStdio<Writable, Readable, null>;
type Job = { chunk: Buffer[]; size: number; done: (error?: Error, value?: unknown) => void };
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
      const job = this.#job;
      if (!job || job.size + chunk.length > recordLimit) { this.#retire(); return; }
      job.chunk.push(chunk);
      job.size += chunk.length;
      if (chunk.indexOf(10) < 0) return;
      if (chunk.indexOf(10) !== chunk.length - 1) { this.#retire(); return; }
      try { job.done(undefined, JSON.parse(Buffer.concat(job.chunk, job.size).toString("utf8"))); }
      catch { this.#retire(); }
    });
  }

  #retire() {
    if (this.#retiring) return;
    this.#retiring = true;
    this.#job?.done(new WireError("not_found"));
    this.#child?.kill("SIGKILL"); // wait for close before reusing the stream
  }

  request(value: object, signal: AbortSignal, finishSearch = false) {
    return new Promise<unknown>((resolve, reject) => {
      signal.throwIfAborted();
      if (this.#closed) throw new WireError("not_found");
      const abort = () => {
        const i = this.#queue.indexOf(pending);
        if (i >= 0) this.#queue.splice(i, 1);
        // Let a bounded search finish for the next query instead of killing
        // the helper and restarting the same cold index on every keystroke.
        if (this.#active === pending && this.#job && !finishSearch) this.#retire();
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
    return new Promise<unknown>((resolve, reject) => {
      const timeout = setTimeout(() => this.#retire(), 3500);
      this.#job = { chunk: [], size: 0, done: (error, value) => {
        clearTimeout(timeout);
        this.#job = undefined;
        if (error) reject(error); else resolve(value);
      } };
      child.stdin.write(JSON.stringify(value) + "\n");
    });
  }

  async close() {
    this.#closed = true;
    this.#retire();
    await this.#exited;
  }
}
