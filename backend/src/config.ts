import { readFile } from "node:fs/promises";
import { isAbsolute, join, resolve } from "node:path";
import { parseArgs } from "node:util";
import { getAgentDir } from "@earendil-works/pi-coding-agent";

const supplied = {
  "data-dir": { type: "string" },
  "socket": { type: "string" },
  "cwd": { type: "string" },
  "agent-dir": { type: "string" },
  profile: { type: "string" },
  "model": { type: "string" },
  "thread-title-model": { type: "string" },
  "idle-ms": { type: "string" },
  "schedule-late-ms": { type: "string" },
  "no-session": { type: "boolean" },
  "parent-stdin": { type: "boolean" },
  "startup-fd": { type: "string" },
  "pi-events": { type: "boolean" },
  "faux": { type: "boolean" },
  "config": { type: "string" },
} as const;

export type BackendConfig = {
  // Older programmatic callers supply a Pi agentDir explicitly.
  profile?: "hush" | "pi";
  dataDir?: string;
  socket: string;
  cwd: string;
  agentDir: string;
  model?: { provider: string; modelId: string };
  threadTitleModel?: { provider: string; modelId: string };
  idleMs: number;
  scheduleLateMs?: number;
  ephemeral: boolean;
  parentStdin?: boolean;
  startupFd?: 3;
  piEvents?: boolean;
  faux: boolean;
};

export class BackendError extends Error {
  constructor(readonly code: "configuration" | "socket_directory" | "data_locked" | "socket_unavailable" |
     "store_profile" | "store_cutover" | "startup_failed" | "shutdown_failed") { super(code); }
}

export function hushDirectory() {
  const home = process.env.HOME;
  if (!home || !isAbsolute(home)) throw new BackendError("configuration");
  return join(home, ".hush");
}

export async function parseConfig(argv: string[]): Promise<BackendConfig> {
  const { values: cli } = parseArgs({ args: argv, options: supplied });
  const file: Record<string, unknown> = cli.config ? JSON.parse(await readFile(cli.config, "utf8")) : {};
  if (file === null || Array.isArray(file) || typeof file !== "object") throw new BackendError("configuration");
  for (const k of Object.keys(file)) {
    if (k === "config" || k === "parent-stdin" || k === "startup-fd" || !Object.hasOwn(supplied, k)) throw new BackendError("configuration");
    const type = supplied[k as keyof typeof supplied].type;
    if (typeof file[k] !== type && !((k === "idle-ms" || k === "schedule-late-ms") && typeof file[k] === "number"))
      throw new BackendError("configuration");
  }
  const val: Record<string, unknown> = { ...file, ...cli };
  const str = (key: string) => {
    const value = val[key];
    if (value === undefined) return undefined;
    if (typeof value !== "string" || !value || value.includes("\0")) throw new BackendError("configuration");
    return value;
  };
  const modelName = str("model");
  const m = modelName && /^([^/]+)\/(.+)$/.exec(modelName);
  const titleModelName = str("thread-title-model");
  const title = titleModelName && /^([^/]+)\/(.+)$/.exec(titleModelName);
  const faux = val.faux === true;
  if (modelName && !m || faux && m) throw new BackendError("configuration");
  const idleMs = val["idle-ms"] === undefined ? 600_000 : Number(val["idle-ms"]);
  if (!Number.isSafeInteger(idleMs) || idleMs < 0 || idleMs > 86_400_000) throw new BackendError("configuration");
  const scheduleLateMs = val["schedule-late-ms"] === undefined ? undefined : Number(val["schedule-late-ms"]);
  if (scheduleLateMs !== undefined && (!Number.isSafeInteger(scheduleLateMs) || scheduleLateMs < 0 || scheduleLateMs > 86_400_000))
    throw new BackendError("configuration");
  const ephemeral = val["no-session"] === true;
  if (titleModelName && (!title || ephemeral || faux)) throw new BackendError("configuration");
  const parentStdin = cli["parent-stdin"] === true;
  const data = str("data-dir");
  const sock = str("socket");
  const startup = cli["startup-fd"];
  if (startup !== undefined && (startup !== "3" || ephemeral || !data)) throw new BackendError("configuration");
  if (ephemeral ? !sock || data : sock || parentStdin) throw new BackendError("configuration");
  const explicitAgent = str("agent-dir");
  const piEvents = val["pi-events"] === true;
  const profile = str("profile") ?? (explicitAgent || piEvents ? "pi" : process.env.HUSH_PROFILE || "hush");
  if (profile !== "pi" && profile !== "hush" || profile === "hush" && (explicitAgent || piEvents)) {
    throw new BackendError("configuration");
  }
  const agentDir = profile === "hush" ? hushDirectory() : resolve(explicitAgent ?? getAgentDir());
  const dataDir = ephemeral ? undefined : data === undefined ? join(agentDir, profile === "pi" ? "hush" : "runtime") : resolve(data);
  return {
    profile, dataDir, socket: dataDir ? join(dataDir, "hush.sock") : resolve(sock!),
    cwd: resolve(str("cwd") ?? process.cwd()), agentDir,
    model: m ? { provider: m[1], modelId: m[2] } : undefined,
    threadTitleModel: title ? { provider: title[1], modelId: title[2] } : undefined,
    idleMs, scheduleLateMs, ephemeral, parentStdin, startupFd: startup === "3" ? 3 : undefined, piEvents, faux,
  };
}
