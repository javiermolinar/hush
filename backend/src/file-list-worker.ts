import { spawn, type ChildProcessByStdio } from "node:child_process";
import type { Readable, Writable } from "node:stream";
import { WireError } from "../../server/src/wire.js";

// A search can return 32 paths of 2048 bytes, including JSON escaping.
const recordLimit = 512 * 1024;
type Child = ChildProcessByStdio<Writable, Readable, null>;
type Job = { chunk: Buffer[]; size: number; done: (error?: Error, value?: unknown) => void };

// One private, stateless descriptor walker per active backend. Avoid paying
// for a process launch on each editor token or page. Only the Node host holds
// grants/cursors; every request, including continuation, reopens the directory.
export class FileListWorker {
  #child?: Child;
  #exited?: Promise<void>;
  #retiring = false;
  #closed = false;
  #job?: Job;
  #tail = Promise.resolve();

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

  request(value: object, signal: AbortSignal) {
    const operation = this.#tail.then(async () => {
      signal.throwIfAborted(); // an abandoned queued request never scans disk
      if (this.#retiring) await this.#exited;
      signal.throwIfAborted();
      return this.#exchange(value, signal);
    });
    this.#tail = operation.then(() => {}, () => {});
    return operation;
  }

  #exchange(value: object, signal: AbortSignal) {
    this.warm();
    const child = this.#child;
    if (!child || this.#retiring || this.#closed) throw new WireError("not_found");
    return new Promise<unknown>((resolve, reject) => {
      const stop = () => this.#retire();
      const timeout = setTimeout(stop, 3500);
      this.#job = { chunk: [], size: 0, done: (error, value) => {
        clearTimeout(timeout);
        signal.removeEventListener("abort", stop);
        this.#job = undefined;
        if (error) reject(error); else resolve(value);
      } };
      signal.addEventListener("abort", stop, { once: true });
      if (signal.aborted) stop();
      else child.stdin.write(JSON.stringify(value) + "\n");
    });
  }

  async close() {
    this.#closed = true;
    this.#retire();
    await this.#exited;
  }
}
