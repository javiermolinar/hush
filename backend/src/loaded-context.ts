import type { Harness } from "@earendil-works/pi-durable";
import { loadedContextAgentsVersion, type LoadedContextHost, type LoadedContextVersion } from "../../server/src/loaded-context.js";
import { WireError } from "../../server/src/wire.js";
import type { ProjectResources } from "./resources.js";
import { threadWorkspace } from "./thread-workspace.js";
import type { WorkspaceTrust } from "./workspace-trust.js";

export class LoadedContext implements LoadedContextHost {
  #pending = 0;

  constructor(private readonly h: Harness, private readonly resources: ProjectResources, private readonly work: WorkspaceTrust) {}

  async get(threadId: string, version: LoadedContextVersion, peer: AbortSignal) {
    if (this.#pending >= 8) throw new WireError("limit");
    this.#pending++;
    try {
      const signal = AbortSignal.any([peer, AbortSignal.timeout(3500)]);
      const cwd = await threadWorkspace(this.h, threadId, signal);
      const binding = await this.work.binding(cwd, signal).catch(() => undefined);
      const sources = await this.resources.instructionSources(binding?.path ?? cwd, !!binding, version === loadedContextAgentsVersion, signal);
      if (await threadWorkspace(this.h, threadId, signal) !== cwd) throw new WireError("not_found");
      if (binding) {
        const current = await this.work.binding(cwd, signal).catch(() => undefined);
        if (!current || current.stamp !== binding.stamp || current.path !== binding.path) throw new WireError("not_found");
      }
      signal.throwIfAborted();
      return { sources };
    } finally { this.#pending--; }
  }
}
