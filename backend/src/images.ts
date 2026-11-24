import { Worker } from "node:worker_threads";
import type { ModelRuntime } from "@earendil-works/pi-coding-agent";
import { imageLimits } from "@earendil-works/pi-durable/tools";
import { maxImageBytes, type ImagesHost } from "../../server/src/images.js";
import { WireError } from "../../server/src/wire.js";
import type { FitJob } from "./image-worker.js";

declare const HUSH_IMAGE_WORKER: string | undefined;
type Fitted = Awaited<ReturnType<ImagesHost["fit"]>>;

export class ImageFitter implements ImagesHost {
  #worker?: Worker;
  #closed = false;

  constructor(private readonly runtime: ModelRuntime) {}

  async fit(ref: Parameters<ImagesHost["fit"]>[0], images: FitJob["images"], signal: AbortSignal): Promise<Fitted> {
    const model = ref && this.runtime.getModel(ref.provider, ref.modelId);
    if (!model?.input.includes("image")) throw new WireError("invalid_request");
    const limit = model.inputLimits?.images;
    if (images.length > Math.min(limit?.maxPerMessage ?? Infinity, limit?.maxPerRequest ?? Infinity)) {
      throw new WireError("invalid_request");
    }
    if (this.#worker || this.#closed) throw new WireError("limit");
    signal.throwIfAborted();
    const requested = imageLimits(model);
    const limits = {
      maxBytes: Math.min(requested.maxBytes, Math.ceil(maxImageBytes / 3) * 4),
      maxWidth: Math.min(requested.maxWidth, 4096),
      maxHeight: Math.min(requested.maxHeight, 4096),
    };
    const file = typeof HUSH_IMAGE_WORKER === "undefined" ? new URL("./image-worker.js", import.meta.url) : new URL(HUSH_IMAGE_WORKER);
    const worker = this.#worker = new Worker(file, { workerData: { images, limits } satisfies FitJob });
    const result = Promise.withResolvers<Fitted | undefined>();
    const abort = () => result.reject(signal.reason);
    const timeout = setTimeout(() => result.reject(new WireError("limit")), 20_000);
    timeout.unref();
    signal.addEventListener("abort", abort, { once: true });
    worker.once("message", result.resolve);
    worker.once("error", () => result.reject(new WireError("internal")));
    worker.once("exit", () => result.reject(new WireError("internal")));
    try {
      const fitted = await result.promise;
      if (!fitted) throw new WireError("invalid_request");
      if (this.#closed) throw new WireError("internal");
      signal.throwIfAborted();
      return fitted;
    } finally {
      clearTimeout(timeout);
      signal.removeEventListener("abort", abort);
      await worker.terminate();
      this.#worker = undefined;
    }
  }

  async close() {
    this.#closed = true;
    await this.#worker?.terminate();
  }
}
