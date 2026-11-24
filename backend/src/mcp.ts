import { createHash } from "node:crypto";
import { join } from "node:path";
import type { ModelRegistry } from "@earendil-works/pi-coding-agent";
import { createMcpToolName, limitMcpContent, normalizeMcpInputSchema, toLlmContent } from "@earendil-works/pi-mcp";
import { defineExtension, defineTool, type Extension, type Registry, type ToolRegistration } from "@earendil-works/pi-durable";
import {
  createDefaultTransport, FileAuthStorageBackend, getMcpToolExposure, loadMcpConfig, McpOAuthCredentialStore, McpServerConnection,
} from "./pi-source.js";

type State = {
  signature: string;
  extension: Extension;
  connections: McpServerConnection[];
  ready: Promise<void>;
  enabled: boolean;
};

const errorResult = (text: string) => ({ isError: true, output: [{ type: "text" as const, text }] });

export class McpScopes {
  readonly #states = new Map<string, State>();
  readonly #credentials: McpOAuthCredentialStore;

  constructor(private readonly registry: Registry, private readonly agentDir: string,
    private readonly models: ModelRegistry, private readonly trusted: (cwd: string) => boolean,
    private readonly warn: (code: "mcp_unavailable") => void,
    private readonly tapTool?: (tool: ToolRegistration) => ToolRegistration) {
    this.#credentials = new McpOAuthCredentialStore(new FileAuthStorageBackend(join(agentDir, "mcp-auth.json")), agentDir);
  }

  async load(cwd: string) {
    const trust = this.trusted(cwd);
    const config = loadMcpConfig({ agentDir: this.agentDir, cwd, projectTrusted: trust });
    if (config.errors.length) this.warn("mcp_unavailable");
    const entries = config.servers.filter(({ config }) => config.enabled !== false);
    if (entries.length === 0) {
      await this.remove(cwd);
      return undefined;
    }
    // Use a digest, not serialized credentials, as the cache identity.
    const signature = createHash("sha256").update(JSON.stringify(entries)).digest("hex");
    let state = this.#states.get(cwd);
    if (state?.signature !== signature) {
      if (state) await this.remove(cwd);
      const extension = defineExtension({ name: "hush-mcp-" + createHash("sha256").update(cwd).digest("hex").slice(0, 24) });
      const latest: State = { signature, extension, connections: [], enabled: true, ready: Promise.resolve() };
      latest.connections = entries.map(entry => new McpServerConnection({
        entry, cwd, credentials: this.#credentials, createTransport: createDefaultTransport,
        providerToken: (provider) => this.models.getApiKeyForProvider(provider),
        onTools: () => this.#publish(cwd, latest),
      }));
      this.#states.set(cwd, latest);
      latest.ready = Promise.all(latest.connections.map(async connection => {
        try { await connection.getClient(); }
        catch { this.warn("mcp_unavailable"); }
      })).then(() => { this.#publish(cwd, latest); });
      state = latest;
    }
    await state.ready;
    return state.extension;
  }

  #publish(cwd: string, state: State) {
    if (!state.enabled || this.#states.get(cwd) !== state) return;
    const names = new Set<string>();
    const tools: ToolRegistration[] = [];
    for (const connection of state.connections) {
      for (const definition of connection.tools) {
        if (getMcpToolExposure(connection.entry.config, definition.name) === "hidden") continue;
        const name = createMcpToolName(connection.name, definition.name, n => names.has(n));
        names.add(name);
        tools.push(defineTool({
          name, description: definition.description ?? "Call a configured MCP tool.",
          parameters: normalizeMcpInputSchema(definition.inputSchema),
          replay: "unsafe",
          execute: async (args, _api, context) => {
            if (!state.enabled || this.#states.get(cwd) !== state ||
              (connection.entry.scope === "project" || connection.entry.override) && !this.trusted(cwd)) {
              return errorResult("MCP configuration is no longer trusted.");
            }
            try {
              const result = await connection.callTool(definition.name, args as Record<string, unknown>, { signal: context.abortSignal });
              const { content } = await limitMcpContent(toLlmContent(result));
              return { isError: result.isError === true, output: content };
            } catch {
              return errorResult("MCP call failed.");
            }
          },
        }));
      }
    }
    state.extension = defineExtension({ name: state.extension.name, tools: this.tapTool ? tools.map(this.tapTool) : tools });
    this.registry.install(state.extension);
  }

  async remove(cwd: string) {
    const state = this.#states.get(cwd);
    if (!state) return;
    this.#states.delete(cwd);
    state.enabled = false;
    this.registry.uninstall(state.extension);
    await Promise.allSettled(state.connections.map(c => c.close()));
  }

  async close() { await Promise.allSettled([...this.#states.keys()].map(cwd => this.remove(cwd))); }
}
