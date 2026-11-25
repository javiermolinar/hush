import { createHmac, randomBytes } from "node:crypto";

// A pulse carries no authority. Readers take a new, authorized snapshot
// after it and check the generation to avoid losing changes during that read.
export class ChangePulse {
  #generation = 0;
  readonly #waiters = new Set<() => void>();

  get generation() { return this.#generation; }

  emit() {
    this.#generation++;
    for (const ready of this.#waiters) ready();
  }

  wait(after: number, signal: AbortSignal) {
    signal.throwIfAborted();
    if (after !== this.#generation) return Promise.resolve();
    return new Promise<void>((resolve, reject) => {
      const stop = () => { cleanup(); reject(signal.reason); };
      const ready = () => { cleanup(); resolve(); };
      const cleanup = () => {
        this.#waiters.delete(ready);
        signal.removeEventListener("abort", stop);
      };
      this.#waiters.add(ready);
      signal.addEventListener("abort", stop, { once: true });
    });
  }
}

// Wait on shared work without allowing one disconnect to cancel its peers.
export function observe<T>(work: Promise<T>, signal: AbortSignal): Promise<T> {
  return new Promise((resolve, reject) => {
    const stop = () => reject(signal.reason);
    if (signal.aborted) stop();
    else signal.addEventListener("abort", stop, { once: true });
    void work.then(resolve, reject).finally(() => signal.removeEventListener("abort", stop));
  });
}

// Equal observations in one boot have equal opaque revisions. Tokens do not
// carry a grant stamp, filesystem identity or authorization across requests.
export class WorkspaceRevisions {
  readonly #key = randomBytes(32);
  of(value: unknown) {
    return createHmac("sha256", this.#key).update(JSON.stringify(value)).digest("hex");
  }
}
