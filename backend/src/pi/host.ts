import { randomUUID } from "node:crypto";
import { BACKGROUND_CONTEXT as background, withAbortSignal, type Context } from "@earendil-works/chord/context";
import type { Api, Model, ModelThinkingLevel } from "@earendil-works/pi-ai";
import { clampThinkingLevel, getSupportedThinkingLevels } from "@earendil-works/pi-ai/models";
import { resolveCliModel, type ModelRuntime, type RpcSessionState, type ScopedModel } from "@earendil-works/pi-coding-agent";
import {
  configure, ConversationBusy, DEFAULT_COMPACTION_POLICY, DEFAULT_RETRY_POLICY, InboxDoc,
  type AgentChange, type Conversation, type ConversationId, type ConversationRetryPolicy, type Cursor, type EntryId, type Harness, type Tx,
} from "@earendil-works/pi-durable";
import type { NativeHost, PiBinding, PiCommand, PiCommandAction, PiEventsHost, PiEventSink, PiHeader, PiOpen } from "../../../server/src/index.js";
import { durableID, encode, WireError } from "../../../server/src/wire.js";
import { QueueFull } from "../../../protocol/v1/pi-1.6/local-queue.mts";
import { PromptCancelled } from "../../../protocol/v1/pi-1.6/threshold-fence.mts";
import type { HostCapability } from "../../../protocol/v1/pi-1.6/surface.mts";
import { resolveModelScopeFromModels } from "../pi-source.js";
import type { ProjectResources } from "../resources.js";
import { check, Rejected, rejected, success } from "./commands.js";
import { forkPoints, forkText, lastText, ProfileDoc, publicModel, sessionMessages, stats } from "./projection.js";
import { PiSources, ThreadSource } from "./source.js";

const ownerless = { kind: "ownerless" } as const;
const services = ["models", "resources", "titles"] as const satisfies readonly HostCapability[];
const serviceSet = new Set<HostCapability>(services);
type Profile = Awaited<ReturnType<typeof profile>>;
const profile = (h: Harness, id: ConversationId, ctx: Context) => h.snapshot(ProfileDoc, id, ctx);
const reference = (model: Model<Api>) => ({ provider: model.provider, modelId: model.id });
const modelKey = (model: Model<Api>) => `${model.provider}\u0000${model.id}`;
const noSession = () => new WireError("unsupported_capability", "piSessions");

/** Profile-owned admission and projection. A bare core server has none of this policy. */
export class PiHost implements PiEventsHost {
  readonly #leases = new Set<ConversationId>();
  readonly #retry: ConversationRetryPolicy;

  constructor(private readonly h: Harness, private readonly sources: PiSources, private readonly runtime: ModelRuntime,
    private readonly resources: ProjectResources, private readonly defaults: NonNullable<NativeHost["defaults"]>,
    private readonly prepareAgent: (cwd: string) => Promise<AgentChange>, retry: Partial<ConversationRetryPolicy> | undefined,
    private readonly ephemeral: boolean, private readonly faux: boolean) {
    this.#retry = { ...DEFAULT_RETRY_POLICY, ...retry };
  }

  // The real provider's opaque exception is not a public model record. Preserve
  // the fact and class of failure, but don't expose request/credential details.
  #diagnostic = (text: string) => this.faux ? text : "Model request failed or was aborted";

  get active() { return this.sources.threads.size > 0; }

  #available(): Model<Api>[] {
    return this.runtime.getAvailableSnapshot().filter(m => this.runtime.getPhysicalModel(m.provider, m.id));
  }

  async catalog(signal: AbortSignal) {
    signal.throwIfAborted();
    return { models: this.#available().map(publicModel) };
  }

  async open(options: PiOpen, sink: PiEventSink, signal: AbortSignal): Promise<PiBinding> {
    const ctx = withAbortSignal(signal, background);
    if (this.ephemeral && (options.threadId || options.session || options.continue || options.fork)) throw new WireError("invalid_request");
    if (options.name !== undefined && !options.name.trim()) throw new WireError("invalid_request");
    if (options.session && /[\\/]/.test(options.session)) throw noSession();
    let cwd = this.resources.directory(options.cwd);
    let selected: Conversation | undefined;
    let saved: Profile;
    if (options.threadId) {
      selected = await this.h.conversation(Number(options.threadId) as ConversationId, ctx);
      saved = selected && await profile(this.h, selected.id, ctx);
      if (!selected || !saved?.id) throw new WireError("not_found");
    } else if (options.session || options.continue) {
      const found = await this.#select(options.session, cwd, ctx);
      if (!found && options.session) {
        if (!/^[a-f0-9-]+$/i.test(options.session)) throw noSession();
        throw new WireError("not_found");
      }
      ({ conv: selected, saved } = found ?? { conv: undefined, saved: undefined });
    }
    if (options.fork && !selected) throw new WireError("not_found");
    if (selected && !options.fork && saved?.cwd) cwd = saved.cwd;
    const available = this.#available();
    const explicit = options.model ? resolveCliModel({ cliProvider: options.provider, cliModel: options.model,
      cliThinking: options.thinking, modelRuntime: this.runtime }) : undefined;
    const explicitModel = explicit?.model;
    if (explicit && (explicit.error || !explicitModel || !available.some(m => modelKey(m) === modelKey(explicitModel)))) {
      throw new WireError("invalid_request");
    }
    const eligible = options.provider && !explicitModel ? available.filter(m => m.provider === options.provider) : available;
    if (!eligible.length) throw new WireError("invalid_request");
    const project = await this.resources.load(cwd);
    const scope = resolveModelScopeFromModels(options.models ?? project.settings.getSettings().enabledModels ?? [], eligible).scopedModels;
    if (options.models?.length && !scope.length) throw new WireError("invalid_request");
    const choice = saved && selected && await selected.agent(ctx);
    const base = await this.defaults(cwd, ctx);
    const fromRef = (ref: AgentChange["model"]) => eligible.find(m => m.provider === ref?.provider && m.id === ref?.modelId);
    const preferred = !selected || options.models ? scope[0] : undefined;
    const model = explicitModel ?? preferred?.model ?? fromRef(choice?.model) ?? fromRef(base.model) ?? eligible[0];
    const savedLevel = choice?.model?.provider === model.provider && choice.model.modelId === model.id ? choice.thinkingLevel : undefined;
    const level = clampThinkingLevel(model, options.thinking ?? explicit?.thinkingLevel ?? (explicitModel ? undefined : preferred?.thinkingLevel)
      ?? savedLevel ?? project.settings.getModelThinkingLevel(model.provider, model.id) ?? base.thinkingLevel ?? "off");
    const policy = this.#policy(project.settings, model, saved?.autoCompaction);
    const agent = { ...await this.prepareAgent(cwd), cwd, model: reference(model), thinkingLevel: level,
      compaction: { ...policy.native, enabled: false } } satisfies AgentChange;
    if (selected && !options.fork) {
      // The profile marker excludes native writers. The lease excludes another Pi
      // writer, including a disconnected run that has not reached its drain fence.
      const conv = selected;
      return this.#bind(conv, scope, sink, ctx, async () => {
        await conv.commit(async tx => {
          await configure(tx, conv.id, agent);
          const doc = await tx.doc(ProfileDoc, conv.id);
          if (options.name !== undefined) doc.name = options.name.trim();
          if (options.systemPrompt !== undefined) doc.systemPrompt = options.systemPrompt;
          if (options.appendSystemPrompt !== undefined) doc.appendSystemPrompt = options.appendSystemPrompt;
        }, ctx);
      });
    }
    const at = selected && (await selected.entries({}, 1, undefined, ctx)).items[0]?.id;
    const conv = await this.#create(cwd, agent, ctx, {
      name: options.name?.trim(), parentSession: saved?.id,
      systemPrompt: options.systemPrompt ?? saved?.systemPrompt, appendSystemPrompt: options.appendSystemPrompt ?? saved?.appendSystemPrompt,
      autoCompaction: saved?.autoCompaction,
    }, at !== undefined && selected ? { conv: selected, at } : undefined);
    return this.#bind(conv, scope, sink, ctx);
  }

  async #select(id: string | undefined, cwd: string, ctx: Context) {
    // Use the last committed entry, not a model turn or a transient replica revision,
    // to pick the most recently written profile session in this project.
    let cursor: Cursor | undefined;
    let chosen: { conv: Conversation; saved: NonNullable<Profile>; last: number } | undefined;
    do {
      const page = await this.h.commit(tx => tx.scanConversations({ order: "descending" }, 100, cursor), ctx);
      for (const row of page.items) {
        const saved = await profile(this.h, row.id, ctx);
        if (!saved?.id || (!id && saved.cwd !== cwd)) continue;
        if (id && !saved.id.startsWith(id)) continue;
        const conv = await this.h.conversation(row.id, ctx);
        if (!conv) continue;
        const last = (await conv.entries({}, 1, undefined, ctx)).items[0]?.id ?? 0;
        if (id && chosen) throw new WireError("invalid_request"); // Ambiguous prefix.
        if (!chosen || last > chosen.last) chosen = { conv, saved, last };
      }
      cursor = page.next;
    } while (cursor);
    return chosen;
  }

  #policy(settings: Awaited<ReturnType<ProjectResources["load"]>>["settings"], model: Model<Api>, enabled?: boolean) {
    const native = settings.getCompactionSettings(model);
    return { native, profile: { ...DEFAULT_COMPACTION_POLICY, ...native,
      enabled: enabled ?? (!this.faux && native.enabled), provider: model.provider, modelId: model.id,
      contextWindow: model.contextWindow, maxTokens: model.maxTokens } };
  }

  async #create(cwd: string, agent: AgentChange, ctx: Context,
    fields: Pick<NonNullable<Profile>, "name" | "parentSession" | "systemPrompt" | "appendSystemPrompt" | "autoCompaction">,
    parent?: { conv: Conversation; at: EntryId }) {
    const options = {
      ownership: ownerless, agent,
      init: async (tx: Tx, id: ConversationId) => {
        const doc = await tx.doc(ProfileDoc, id);
        Object.assign(doc, Object.fromEntries(Object.entries(fields).filter(([, v]) => v !== undefined)), {
          id: randomUUID(), timestamp: new Date().toISOString(), cwd,
        });
      },
    };
    return parent ? parent.conv.fork(parent.at, options, ctx) : this.h.createConversation(options, ctx);
  }

  async #bind(conv: Conversation, scope: ScopedModel[], sink: PiEventSink, ctx: Context, setup?: () => Promise<void>): Promise<PiBinding> {
    if (this.#leases.has(conv.id)) throw new WireError("busy");
    this.#leases.add(conv.id);
    let source: ThreadSource | undefined;
    let closed = false;
    const close = () => {
      if (closed) return;
      closed = true;
      const released = source ? source.close() : Promise.resolve();
      void released.catch(() => {}).finally(() => {
        this.sources.threads.delete(conv.id);
        this.#leases.delete(conv.id);
      });
    };
    try {
      // Don't attach to a recovered, pending, or already-running attempt: the
      // initial observations needed by this observer can't be recovered.
      const current = await this.h.inspect(ctx);
      if (current.tasks.some(t => t.record.conversationId === conv.id) || current.submissions.some(s => s.conversationId === conv.id) ||
          (await this.h.snapshot(InboxDoc, conv.id, ctx))?.items.length) throw new WireError("busy");
      await setup?.();
      const saved = await profile(this.h, conv.id, ctx);
      const agent = await conv.agent(ctx);
      const initial = this.#available().find(m => m.provider === agent.model?.provider && m.id === agent.model.modelId);
      if (!saved?.id || !saved.timestamp || !saved.cwd || !initial || agent.compaction?.enabled !== false) throw new WireError("internal");
      let model = initial;
      const { id, timestamp, cwd, parentSession } = saved;
      const project = await this.resources.load(cwd);
      let autoOverride = saved.autoCompaction;
      const effectivePolicy = () => this.#policy(project.settings, model, autoOverride);
      let level = agent.thinkingLevel;
      const ordered = source = await ThreadSource.attach(this.h, conv, sink, effectivePolicy().profile,
        this.#retry, level, saved.name, ctx, this.#diagnostic);
      this.sources.threads.set(conv.id, source);
      const header: PiHeader = { type: "session", version: 3, id, timestamp, cwd, ...(parentSession ? { parentSession } : {}) };
      const requireIdle = () => { if (ordered.fence.busy) throw new Rejected("The session is busy"); };
      const selectModel = async (next: Model<Api>, requested?: ModelThinkingLevel) => {
        requireIdle();
        const updated = clampThinkingLevel(next, requested ?? project.settings.getModelThinkingLevel(next.provider, next.id)
          ?? project.settings.getDefaultThinkingLevel() ?? level);
        const policy = this.#policy(project.settings, next, autoOverride);
        await conv.configure({ model: reference(next), thinkingLevel: updated, compaction: { ...policy.native, enabled: false } }, ctx);
        ordered.fence.setPolicy(policy.profile);
        model = next;
        level = updated;
      };
      const selectLevel = async (requested: ModelThinkingLevel) => {
        const next = clampThinkingLevel(model, requested);
        await conv.configure({ thinkingLevel: next }, ctx);
        level = next;
      };
      const switchSession = async (nextSink: () => PiEventSink, parent?: { at?: EntryId; provenance?: string }) => {
        if (ordered.fence.busy) await ordered.fence.abort(background);
        const ref: AgentChange = { ...await this.prepareAgent(cwd), cwd, model: reference(model), thinkingLevel: level,
          compaction: { ...effectivePolicy().native, enabled: false } };
        const doc = await profile(this.h, conv.id, ctx);
        const next = await this.#create(cwd, ref, ctx, { parentSession: parent?.provenance,
          systemPrompt: doc?.systemPrompt, appendSystemPrompt: doc?.appendSystemPrompt, autoCompaction: autoOverride },
          parent?.at === undefined ? undefined : { conv, at: parent.at });
        return this.#bind(next, scope, nextSink(), ctx);
      };
      const command = async (raw: PiCommand, nextSink: () => PiEventSink): Promise<PiCommandAction> => {
        if (closed || ordered.trace.failure) throw new WireError("internal");
        try {
          const cmd = check(raw, serviceSet);
          const { id: rid } = cmd;
          switch (cmd.type) {
            case "prompt": case "steer": case "follow_up": {
              const input = { text: await this.resources.expandPi(await this.resources.load(cwd), cmd.message) };
              // Bound expanded content before any admission or staging.
              if (Buffer.byteLength(input.text) > 2 * 1024 * 1024) throw new Rejected("Input exceeds the profile limit");
              encode(input);
              if (cmd.type === "prompt") {
                if (!ordered.fence.busy) await conv.configure(await this.prepareAgent(cwd), ctx);
                const stage = await ordered.fence.beginPrompt(input, cmd.streamingBehavior, background);
                if ("started" in stage) {
                  return { wait: stage.started.then(({ disposition }) => success("prompt", rid, { disposition }), error => {
                    if (error instanceof PromptCancelled) return rejected(raw, error.message);
                    throw error;
                  }) };
                }
                return { response: success("prompt", rid, { disposition: stage.disposition }) };
              }
              const { disposition } = await ordered.queue.enqueue(cmd.type === "steer" ? "steer" : "followUp", input, ctx);
              return { response: success(cmd.type, rid, { disposition }) };
            }
            case "abort": return { wait: ordered.fence.abort(background).then(() => success("abort", rid)) };
            case "clear_queue": {
              const removed = await ordered.queue.clear(ctx);
              ordered.ordered.confirmedEmptyClear(removed);
              return { response: success("clear_queue", rid, removed) };
            }
            case "new_session": return { next: await switchSession(nextSink, { provenance: cmd.parentSession }),
              response: success("new_session", rid, { cancelled: false }) };
            case "fork": {
              if (!durableID(cmd.entryId)) throw new Rejected("Entry not found");
              const at = Number(cmd.entryId) as EntryId;
              const entry = (await conv.entries({ minEntryId: at, maxEntryId: at }, 1, undefined, ctx)).items[0];
              const text = entry && forkText(entry);
              if (!text) throw new Rejected("Entry is not a user fork point");
              const prior = (await conv.entries({ maxEntryId: (at - 1) as EntryId }, 1, undefined, ctx)).items[0];
              return { next: await switchSession(nextSink, { at: prior?.id, provenance: header.id }),
                response: success("fork", rid, { text, cancelled: false }) };
            }
            case "clone": {
              const at = (await conv.entries({}, 1, undefined, ctx)).items[0]?.id;
              if (at === undefined) throw new Rejected("Cannot clone session: no current entry selected");
              return { next: await switchSession(nextSink, { at, provenance: header.id }), response: success("clone", rid, { cancelled: false }) };
            }
            case "get_state": {
              const messages = await conv.context(ctx);
              const queues = ordered.ordered.queues;
              const state: RpcSessionState = { model: publicModel(model), thinkingLevel: level,
                isStreaming: ordered.trace.streaming, isCompacting: ordered.fence.isCompacting,
                steeringMode: "one-at-a-time", followUpMode: "one-at-a-time", sessionId: header.id,
                sessionName: (await profile(this.h, conv.id, ctx))?.name, autoCompactionEnabled: effectivePolicy().profile.enabled,
                messageCount: messages.messages.length,
                pendingMessageCount: queues.steering.length + queues.followUp.length };
              return { response: success("get_state", rid, state) };
            }
            case "get_available_models": return { response: success(cmd.type, rid, { models: this.#available().map(publicModel) }) };
            case "set_model": {
              const next = this.#available().find(m => m.provider === cmd.provider && m.id === cmd.modelId);
              if (!next) throw new Rejected(`Model not found: ${cmd.provider}/${cmd.modelId}`);
              await selectModel(next);
              return { response: success(cmd.type, rid, publicModel(next)) };
            }
            case "cycle_model": {
              const available = this.#available();
              const pool = scope.length ? scope.filter(s => available.some(m => modelKey(m) === modelKey(s.model))) : available.map(model => ({ model } as ScopedModel));
              const current = pool.findIndex(s => modelKey(s.model) === modelKey(model));
              if (!pool.length || pool.length === 1 && current === 0) return { response: success(cmd.type, rid, null) };
              const next = pool[(current + 1) % pool.length];
              await selectModel(next.model, next.thinkingLevel);
              return { response: success(cmd.type, rid, { model: publicModel(next.model), thinkingLevel: level, isScoped: Boolean(scope.length) }) };
            }
            case "get_available_thinking_levels": return { response: success(cmd.type, rid, { levels: getSupportedThinkingLevels(model) }) };
            case "set_thinking_level": await selectLevel(cmd.level); return { response: success(cmd.type, rid) };
            case "cycle_thinking_level": {
              const levels = getSupportedThinkingLevels(model);
              if (levels.length < 2) return { response: success(cmd.type, rid, null) };
              await selectLevel(levels[(levels.indexOf(level) + 1) % levels.length]);
              return { response: success(cmd.type, rid, { level }) };
            }
            case "compact": {
              const { done } = await ordered.fence.compact(cmd.customInstructions, background);
              return { wait: done.then(result => result.kind === "applied" ? success("compact", rid, result.result)
                : rejected(raw, result.kind === "aborted" ? "Compaction cancelled" : result.kind === "not_applied"
                  ? "Nothing to compact (session too small)" : "Compaction failed")) };
            }
            case "set_auto_compaction": {
              requireIdle();
              await conv.commit(async tx => { (await tx.doc(ProfileDoc, conv.id)).autoCompaction = cmd.enabled; }, ctx);
              autoOverride = cmd.enabled;
              ordered.fence.setPolicy(effectivePolicy().profile);
              return { response: success(cmd.type, rid) };
            }
            case "set_session_name": {
              const name = cmd.name.trim();
              if (!name || name.length > 4096) throw new Rejected("Session name must contain 1–4096 characters");
              await conv.commit(async tx => { (await tx.doc(ProfileDoc, conv.id)).name = name; }, ctx);
              return { response: success(cmd.type, rid) };
            }
            case "get_commands": return { response: success(cmd.type, rid, { commands: this.resources.commands(await this.resources.load(cwd)) }) };
            case "get_fork_messages": return { response: success(cmd.type, rid, { messages: await forkPoints(conv, ctx) }) };
            case "get_last_assistant_text": return { response: success(cmd.type, rid, { text: lastText(await conv.context(ctx)) }) };
            case "get_messages": {
              const messages = await sessionMessages(this.h, await conv.context(ctx), ctx);
              return { response: success(cmd.type, rid, { messages: messages.map(m => m.role === "assistant" && m.errorMessage !== undefined
                ? { ...m, errorMessage: this.#diagnostic(m.errorMessage) } : m) }) };
            }
            case "get_session_stats": return { response: success(cmd.type, rid, await stats(this.h, conv, header.id, model, ctx)) };
            // Exhaustive against the pinned command inventory. These never pass the gate.
            case "get_entries": case "get_tree": case "switch_session": case "set_steering_mode": case "set_follow_up_mode":
            case "set_auto_retry": case "abort_retry": case "bash": case "abort_bash": case "export_html":
              throw new Error("Unhandled admitted command");
            default: { const neverCommand: never = cmd; return neverCommand; }
          }
        } catch (error) {
          if (ordered.trace.failure) throw error;
          if (error instanceof Rejected || error instanceof QueueFull || error instanceof ConversationBusy) {
            return { response: rejected(raw, error instanceof ConversationBusy ? "The session is busy" : error.message) };
          }
          // An unexpected error is indeterminate, not a negative admission receipt.
          ordered.trace.fail("profile command lost");
          throw error;
        }
      };
      return { threadId: String(conv.id), header, services, command, close };
    } catch (error) { close(); throw error; }
  }
}
