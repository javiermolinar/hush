import type { ModelThinkingLevel } from "@earendil-works/pi-ai";
import type { RpcCommand } from "@earendil-works/pi-coding-agent";
import type { PiCommand, PiResponse } from "../../../server/src/index.js";
import { commands, rejectUnavailable, type CommandKind, type HostCapability, type Success } from "../../../protocol/v1/pi-1.6/surface.mts";

export class Rejected extends Error {}
export const thinkingLevels: readonly ModelThinkingLevel[] = ["off", "minimal", "low", "medium", "high", "xhigh", "max"];

export function rejected(command: PiCommand, error: string): PiResponse {
  return { type: "response", command: command.type, id: command.id, success: false, error };
}

type Check = (v: unknown) => boolean;
const text: Check = v => typeof v === "string";
const field: Check = v => typeof v === "string" && v.length > 0 && v.length <= 4096;
const flag: Check = v => typeof v === "boolean";
const oneOf = (...values: unknown[]): Check => v => values.includes(v);
const opt = (check: Check): Check => v => v === undefined || check(v);
// Reject any nonempty image list at the capability gate; don't read, coerce or stage it.
const images: Check = v => v === undefined || Array.isArray(v);
const shape = (required: Record<string, Check> = {}) => (v: PiCommand) =>
  Object.keys(v).every(k => k === "type" || k === "id" || Object.hasOwn(required, k)) &&
  Object.entries(required).every(([key, check]) => check(v[key]));

const shapes = {
  prompt: shape({ message: text, images, streamingBehavior: opt(oneOf("steer", "followUp")) }),
  steer: shape({ message: text, images }), follow_up: shape({ message: text, images }),
  abort: shape(), clear_queue: shape(), new_session: shape({ parentSession: opt(field) }), get_state: shape(),
  set_model: shape({ provider: field, modelId: field }), cycle_model: shape(), get_available_models: shape(),
  set_thinking_level: shape({ level: oneOf(...thinkingLevels) }), cycle_thinking_level: shape(), get_available_thinking_levels: shape(),
  set_steering_mode: shape({ mode: oneOf("all", "one-at-a-time") }), set_follow_up_mode: shape({ mode: oneOf("all", "one-at-a-time") }),
  compact: shape({ customInstructions: opt(text) }), set_auto_compaction: shape({ enabled: flag }), set_auto_retry: shape({ enabled: flag }), abort_retry: shape(),
  bash: shape({ command: text, excludeFromContext: opt(flag) }), abort_bash: shape(), get_session_stats: shape(),
  export_html: shape({ outputPath: opt(field) }), switch_session: shape({ sessionPath: field }),
  fork: shape({ entryId: field }), clone: shape(), get_fork_messages: shape(), get_entries: shape({ since: opt(field) }), get_tree: shape(),
  get_last_assistant_text: shape(), set_session_name: shape({ name: text }), get_messages: shape(), get_commands: shape(),
} satisfies Record<CommandKind, (v: PiCommand) => boolean>;

export function check(command: PiCommand, services: ReadonlySet<HostCapability>) {
  if (!Object.hasOwn(commands, command.type)) throw new Rejected(command.type === "extension_ui_response"
    ? "Unsupported in hush v1: extension_ui_response" : `Unknown command: ${command.type}`);
  const typed = command as RpcCommand;
  const unavailable = rejectUnavailable(typed, services);
  if (unavailable) throw new Rejected(unavailable.error);
  if (!shapes[typed.type](command)) throw new Rejected(`Invalid fields for ${typed.type}`);
  if ("images" in typed && typed.images?.length) throw new Rejected("Unsupported capability: images");
  return typed;
}

export function success<K extends CommandKind>(command: K, id: string | undefined,
  ...body: Success<K> extends { data: infer D } ? [data: D] : []): Success<K> {
  return { type: "response", command, id, success: true, ...(body.length ? { data: body[0] } : {}) } as Success<K>;
}
