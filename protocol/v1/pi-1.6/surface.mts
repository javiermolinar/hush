// The pinned Pi wire declarations are the schema for this profile, with observed serializer exceptions below.
import type {
  JsonAgentSessionEvent, RpcCommand, RpcExtensionUIRequest, RpcExtensionUIResponse, RpcResponse,
} from "@earendil-works/pi-coding-agent";

export type CommandKind = RpcCommand["type"];
export type Command<K extends CommandKind> = Extract<RpcCommand, { type: K }>;
export type Failure = Extract<RpcResponse, { success: false }>;
type UpstreamSuccess<K extends CommandKind> = Extract<RpcResponse, { command: K; success: true }>;
// Pi 1.6 serializes data:{} for an empty last-text result, despite the declaration promising null.
export type Success<K extends CommandKind> = K extends "get_last_assistant_text"
  ? Omit<UpstreamSuccess<K>, "data"> & { data: { text?: string } }
  : UpstreamSuccess<K>;
export type Response<K extends CommandKind> = Success<K> | Failure;
export type Event<K extends JsonAgentSessionEvent["type"]> = Extract<JsonAgentSessionEvent, { type: K }>;
export type Header = { type: "session"; version: 3; id: string; timestamp: string; cwd: string; parentSession?: string };
export type ExtensionUI = RpcExtensionUIRequest | RpcExtensionUIResponse;
export type RpcOnlyEvent = { type: "extension_error"; extensionPath: string; event: string; error: string };

const core = (loss: "read" | "mutation" | "keyedSubmit", source: string, reference: string) =>
  ({ scope: "core", loss, source, reference }) as const;
const host = (capability: "models" | "titles" | "resources" | "piSessions", loss: "read" | "mutation", source: string, reference: string) =>
  ({ scope: "host", capability, loss, source, reference, absent: "profile:failures" }) as const;
const no = (reason: string) => ({ scope: "no", loss: "noSideEffect", reason, reference: "profile:failures" }) as const;

type CommandRow = ReturnType<typeof core | typeof host | typeof no>;
// `reference` identifies a fixture of Pi's output, not proof that the corresponding hush adapter exists.
export const commands = {
  prompt: core("keyedSubmit", "local queue admission proof: Conversation.submit(tx, draft) and first staged steer in one commit; optional resource/image services", "wire:busy-admission-abort"),
  steer: core("mutation", "hush per-conversation staging document, then transactional handoff at a turn boundary", "wire:local-queue-order"),
  follow_up: core("mutation", "hush per-conversation staging document, then transactional handoff at an answer boundary", "wire:local-queue-order"),
  abort: core("mutation", "Conversation.abort and ordinary idle fence; leaves unselected local inputs pending", "wire:abort-retains-local-inputs"),
  clear_queue: core("mutation", "one staging transaction returns text and removes both pending lists; no-op clear is explicit", "wire:idle-queue-models-resources"),
  new_session: core("mutation", "Harness.createConversation; explicit provenance for parentSession", "wire:branch-cursor-title"),
  get_state: core("read", "pi.live/agent, local pending inputs and active context; optional models, title and imported-file identity", "wire:idle-queue-models-resources"),
  set_model: host("models", "mutation", "ModelRegistry/ModelRuntime selection; Conversation.configure", "wire:idle-queue-models-resources"),
  cycle_model: host("models", "mutation", "profile catalog and model scope; Conversation.configure", "wire:idle-queue-models-resources"),
  get_available_models: host("models", "read", "public ModelRegistry/ModelRuntime catalog projection", "wire:idle-queue-models-resources"),
  set_thinking_level: host("models", "mutation", "model-supported clamp; Conversation.configure", "wire:idle-queue-models-resources"),
  cycle_thinking_level: host("models", "mutation", "model-supported levels; Conversation.configure", "wire:idle-queue-models-resources"),
  get_available_thinking_levels: host("models", "read", "model capabilities from the active model", "wire:idle-queue-models-resources"),
  set_steering_mode: no("no profile per-session setter"),
  set_follow_up_mode: no("no profile per-session setter"),
  compact: core("mutation", "compaction-result.mts: committed selection, waitForTask, exact summary placement and anchored Pi token estimates", "wire:manual-compaction-retry"),
  set_auto_compaction: core("mutation", "profile-owned threshold and overflow policy in threshold-fence.mts; no raw toggle of Durable autonomous scheduling", "wire:manual-compaction-retry"),
  set_auto_retry: no("no profile per-session setter"),
  abort_retry: no("no profile retry-only cancellation"),
  bash: no("no direct RPC shell"),
  abort_bash: no("no direct RPC shell"),
  get_session_stats: core("read", "pi.usage, full active messages, usage estimates with optional context-window policy", "wire:branch-cursor-title"),
  export_html: no("no direct HTML export"),
  switch_session: host("piSessions", "mutation", "read-only Pi file import into a conversation", "wire:branch-cursor-title"),
  fork: core("mutation", "active history and previous-entry fork; create empty when there is no predecessor", "wire:branch-cursor-title"),
  clone: core("mutation", "active leaf and Conversation.fork at leaf; reject absent leaf", "wire:branch-cursor-title"),
  get_fork_messages: core("read", "full active branch user entries and stable entry IDs", "wire:branch-cursor-title"),
  get_entries: host("piSessions", "read", "append-order Pi entry/parent projection and leaf, not a transcript page", "wire:abandoned-branch-and-compacted-context"),
  get_tree: host("piSessions", "read", "complete Pi entry/parent projection and leaf, including abandoned branches", "wire:abandoned-branch-and-compacted-context"),
  get_last_assistant_text: core("read", "search active context backward; skip textless non-stop messages; normal textless stop gives {}", "wire:normal-textless-reply-replaces-previous-last-text"),
  set_session_name: host("titles", "mutation", "persisted title metadata, not a derived preview", "wire:branch-cursor-title"),
  get_messages: core("read", "full active agent message projection from Conversation.context, never the window", "wire:idle-queue-models-resources"),
  get_commands: host("resources", "read", "trusted Pi skills/templates with sourceInfo and no TUI commands", "wire:idle-queue-models-resources"),
} as const satisfies Record<CommandKind, CommandRow>;

const live = (source: string, reference: string, probe?: string) =>
  ({ scope: "piEvents", source, loss: "streamFailure", reference, probe }) as const;
const absent = (reason: string) => ({ scope: "no", reason, loss: "none", reference: "profile:failures" }) as const;
export const events = {
  agent_start: live("request hook and committed inputs, linked across generation handoffs", "wire:tools-and-two-turns", "durable:ordered-fast-tool-matches-pi"),
  agent_end: live("low-level boundary with authoritative attempt messages and backoff decision", "oneshot:json:retry", "durable:ordered-generation-retry-matches-pi"),
  agent_settled: live("drain of the Pi-owned chain including verified threshold outcome and late inputs, not submission settlement or ordinary Durable idle", "wire:post-answer-threshold-delays-settlement", "durable:profile-threshold-holds-busy-and-drains-late-input"),
  turn_start: live("generation boundary before provider and user/tool message emission", "wire:tools-and-two-turns", "durable:ordered-fast-tool-matches-pi"),
  turn_end: live("authoritative assistant and terminal round tool results", "wire:tools-and-two-turns", "durable:ordered-fast-tool-matches-pi"),
  message_start: live("prepared system/user entries and provider attempt start", "wire:tools-and-two-turns", "durable:ordered-fast-tool-matches-pi"),
  message_update: live("provider stream tap, block index + cumulative usage; no partial message", "wire:tools-and-two-turns", "durable:ordered-fast-tool-matches-pi"),
  message_end: live("committed assistant/tool result or matched aborted invocation's terminal provider event", "wire:busy-admission-abort", "durable:ordered-active-abort-without-a-committed-assistant"),
  tool_execution_start: live("execution adapter or pre-commit lookup/validation rejection; not first appearance in watch", "wire:tools-and-two-turns", "durable:ordered-fast-tool-matches-pi"),
  tool_execution_update: live("tool output tap before pacing and retention", "wire:tools-and-two-turns", "durable:ordered-fast-tool-matches-pi"),
  tool_execution_end: live("tool terminal result from task commit; reject phase time is not execution duration", "wire:tools-and-two-turns", "durable:ordered-unoffered_tool-has-no-execution-duration"),
  queue_update: live("staging commits, marked delivery and explicit idle no-op clear", "wire:local-queue-order", "durable:ordered-queue-matches-pi-delivery"),
  thinking_level_changed: live("effective clamped level from committed AgentDoc", "wire:idle-queue-models-resources", "durable:ordered-thinking-and-title-metadata"),
  session_info_changed: live("ordered commit via injected title store", "wire:branch-cursor-title", "durable:ordered-thinking-and-title-metadata"),
  compaction_start: live("manual/owned task creation or actual cut selection for profile-owned automatic compaction", "wire:manual-compaction-retry", "durable:profile-pre-prompt-threshold-matches-pi"),
  compaction_end: live("anchored placement before continuation/settlement, or verified failure/abort", "wire:manual-compaction-retry", "durable:profile-threshold_then_overflow-uses-one-policy"),
  auto_retry_start: live("generation retry checkpoint/backoff and provider error", "oneshot:json:retry", "durable:ordered-generation-retry-matches-pi"),
  auto_retry_end: live("generation retry success or exhaustion and completed attempt count", "oneshot:json:retry", "durable:ordered-generation-retry_failed-matches-pi"),
  summarization_retry_scheduled: live("compactor retry/backoff before next attempt", "wire:manual-compaction-retry", "durable:ordered-manual-compaction-matches-pi-retry"),
  summarization_retry_attempt_start: live("compactor retry attempt and reason; never branchSummary in v1", "wire:manual-compaction-retry", "durable:ordered-manual-compaction-matches-pi-retry"),
  summarization_retry_finished: live("compactor attempt series finished or gave up", "wire:manual-compaction-retry", "durable:ordered-manual-compaction-matches-pi-retry"),
  entry_appended: absent("no Pi entry producer; don't substitute native transcript changes or fabricate Pi-internal context edits"),
  bash_execution_update: absent("no direct RPC shell; model tool events are separate"),
} as const satisfies Record<JsonAgentSessionEvent["type"], ReturnType<typeof live | typeof absent>>;

export const rpcOnly = {
  extension_error: absent("no TypeScript extension runtime"),
  extension_ui_request: absent("no extension UI runtime"),
  extension_ui_response: absent("no extension UI requests to answer"),
} as const satisfies Record<RpcOnlyEvent["type"] | ExtensionUI["type"], ReturnType<typeof absent>>;

type Update = Event<"message_update">["assistantMessageEvent"]["type"];
export const updateVariants = {
  start: "schema only; normal wire uses message_start",
  text_start: "wire:tools-and-two-turns",
  text_delta: "wire:tools-and-two-turns",
  text_end: "wire:tools-and-two-turns",
  thinking_start: "wire:tools-and-two-turns",
  thinking_delta: "wire:tools-and-two-turns",
  thinking_end: "wire:tools-and-two-turns",
  toolcall_start: "wire:tools-and-two-turns",
  toolcall_delta: "wire:tools-and-two-turns",
  toolcall_end: "wire:tools-and-two-turns",
  done: "schema only; normal wire uses message_end",
  error: "schema only; normal wire uses message_end",
} as const satisfies Record<Update, string>;

// Require a typed success as well as a request for every classified upstream command.
type MissingResponse = Exclude<CommandKind, Extract<RpcResponse, { success: true }>["command"]>;
export const allCommandsHaveDeclaredResponses: [MissingResponse] extends [never] ? true : never = true;

export type HostCapability = Extract<CommandRow, { scope: "host" }>["capability"];

// Fixture pre-dispatch gate. This accepts parsed, typed commands only; not a hush RPC server.
export function rejectUnavailable(command: RpcCommand, capabilities: ReadonlySet<HostCapability>): Failure | undefined {
  const row = commands[command.type];
  const error = row.scope === "no" ? `Unsupported in hush v1: ${command.type}` :
    row.scope === "host" && !capabilities.has(row.capability) ? `Unsupported capability: ${row.capability}` : undefined;
  return error === undefined ? undefined : {
    type: "response", command: command.type, success: false, error,
    ...(command.id === undefined ? {} : { id: command.id }),
  };
}

export function rejectionFixtures(): Failure[] {
  return Object.entries(commands).flatMap(([command, row]): Failure[] => {
    switch (row.scope) {
      case "no": return [{ id: `unsupported:${command}`, type: "response", command, success: false, error: `Unsupported in hush v1: ${command}` }];
      case "host": return [{ id: `absent:${command}`, type: "response", command, success: false, error: `Unsupported capability: ${row.capability}` }];
      default: return [];
    }
  });
}
