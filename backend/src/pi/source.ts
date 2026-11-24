import { BACKGROUND_CONTEXT as background, type Context } from "@earendil-works/chord/context";
import type { Models, ModelThinkingLevel } from "@earendil-works/pi-ai";
import type { JsonAgentSessionEvent, ModelRuntime } from "@earendil-works/pi-coding-agent";
import {
  AgentDoc, CompactionTask, GenerationTask, ToolTask, defineExtension, hook,
  type Conversation, type ConversationId, type ConversationRetryPolicy, type Harness, type TaskId, type ToolRegistration,
} from "@earendil-works/pi-durable";
import type { PiEventSink } from "../../../server/src/index.js";
import { LocalQueue, localQueueHooks } from "../../../protocol/v1/pi-1.6/local-queue.mts";
import { OrderedRun, type TraceSink } from "../../../protocol/v1/pi-1.6/ordered-run.mts";
import { CompactionFence, type Policy } from "../../../protocol/v1/pi-1.6/threshold-fence.mts";
import { ProfileDoc, SummaryDoc } from "./projection.js";

/** A live producer and the logical run owner, not the socket's output queue. */
export class PiTrace implements TraceSink {
  failure?: string;
  streaming = false;
  #sink?: PiEventSink;

  constructor(sink: PiEventSink, private readonly diagnostic: (text: string) => string) { this.#sink = sink; }
  push(event: JsonAgentSessionEvent) {
    if (this.failure) return;
    if (event.type === "agent_start") this.streaming = true;
    else if (event.type === "agent_end") this.streaming = false;
    // These fields come from provider/compactor exceptions; unlike message
    // content they can contain the configured request credentials verbatim.
    const message = <T extends { role: string }>(m: T): T => {
      const diagnostic = "errorMessage" in m && m.errorMessage;
      return typeof diagnostic === "string" ? { ...m, errorMessage: this.diagnostic(diagnostic) } : m;
    };
    if ("errorMessage" in event && typeof event.errorMessage === "string") {
      event = { ...event, errorMessage: this.diagnostic(event.errorMessage) };
    } else if ("finalError" in event && typeof event.finalError === "string") {
      event = { ...event, finalError: this.diagnostic(event.finalError) };
    } else if (event.type === "agent_end") {
      event = { ...event, messages: event.messages.map(message) };
    } else if (event.type === "turn_end" || event.type === "message_start" || event.type === "message_end") {
      this.#sink?.emit({ ...event, message: message(event.message) });
      return;
    }
    this.#sink?.emit(event);
  }
  fail(reason: string) {
    if (this.failure !== undefined) return;
    this.failure = reason;
    this.#sink?.fail();
    this.unbind();
  }
  unbind() { this.#sink = undefined; }
}

export class ThreadSource {
  readonly outputDecoders = new Map<TaskId, TextDecoder>();

  private constructor(readonly trace: PiTrace, readonly ordered: OrderedRun, readonly fence: CompactionFence,
    readonly queue: LocalQueue, private readonly detach: () => void) {}

  static async attach(harness: Harness, conv: Conversation, sink: PiEventSink, policy: Policy,
    retry: ConversationRetryPolicy, level: ModelThinkingLevel, title: string | undefined, ctx: Context,
    diagnostic: (text: string) => string) {
    const trace = new PiTrace(sink, diagnostic);
    const queue = new LocalQueue(harness, conv);
    await queue.reconcile(ctx);
    const ordered = new OrderedRun(trace, { hostSettlement: true, ...await queue.seed(ctx), retry });
    const commit = ordered.attach(harness, conv);
    const fence = new CompactionFence(harness, conv, ordered, policy, retry, ({ entryId, result }, context) =>
      conv.commit(async tx => { await tx.doc(SummaryDoc, conv.id, String(entryId), result.tokensBefore); }, context));
    const metadata = harness.observeCommits({
      advance(batch) {
        for (const change of batch.changes) {
          if (change.type !== "document" || change.kind !== "change" || change.conversationId !== conv.id) continue;
          if (change.record.kind === AgentDoc.definition.kind) {
            const next = (change.value.thinkingLevel ?? "off") as ModelThinkingLevel;
            if (next !== level) trace.push({ type: "thinking_level_changed", level: level = next });
          } else if (change.record.kind === ProfileDoc.definition.kind) {
            const name = change.value.name;
            if (name !== undefined && typeof name !== "string") { trace.fail("invalid title commit"); return; }
            if (name !== title) trace.push({ type: "session_info_changed", name: title = name });
          }
        }
      },
      end() { trace.fail("metadata source ended"); },
    });
    return new ThreadSource(trace, ordered, fence, queue, () => { metadata.detach(); commit.detach(); });
  }

  async close() {
    this.trace.unbind();
    try { await this.fence.dispose(background); }
    finally { this.detach(); }
  }
}

export class PiSources {
  readonly threads = new Map<ConversationId, ThreadSource>();
  readonly modelAPI: Models;
  readonly #invocations = new WeakMap<AbortSignal, ThreadSource>();

  constructor(runtime: ModelRuntime) {
    const streamSimple: Models["streamSimple"] = (model, request, options) => {
      const response = runtime.streamSimple(model, request, options);
      const source = options?.signal && this.#invocations.get(options.signal);
      if (!source) return response; // Compaction uses a different invocation signal.
      return new Proxy(response, {
        get(target, key) {
          if (key === Symbol.asyncIterator) return async function* () {
            for await (const event of target) {
              source.ordered.provider(options?.signal, event);
              yield event;
            }
          };
          const value = Reflect.get(target, key, target);
          return typeof value === "function" ? value.bind(target) : value;
        },
      });
    };
    this.modelAPI = new Proxy(runtime, {
      get(target, key) {
        if (key === "streamSimple") return streamSimple;
        const value = Reflect.get(target, key, target);
        return typeof value === "function" ? value.bind(target) : value;
      },
    });
  }

  extensions(getHarness: () => Harness) {
    // Install the queue handoff before the threshold hook, so a queued successor
    // is a committed fact, not a guess from pending input or a folded view.
    return [localQueueHooks(getHarness), defineExtension({
      name: "hush.pi-ordered-source",
      hooks: [hook(GenerationTask, {
        beforeRequest: async (_request, api, ctx) => {
          const source = this.threads.get(api.conversationId);
          if (!source) return; // Durable may be recovering a run from an older epoch.
          await source.fence.beforeRequest(api.taskId, ctx);
          if (ctx.abortSignal) this.#invocations.set(ctx.abortSignal, source);
          source.ordered.beforeRequest(api.conversationId, api.taskId, ctx.abortSignal);
        },
        afterTools: async (answer, results, api, ctx) => {
          const source = this.threads.get(api.conversationId);
          if (!source) return;
          source.ordered.afterTools(api.conversationId, api.taskId, answer, results);
          await source.fence.afterTools(api.taskId, answer, results, ctx);
        },
        afterRun: async (summary, api, ctx) => {
          await this.threads.get(api.conversationId)?.fence.afterAnswer(api.taskId, summary.answer, ctx);
        },
      }), hook(ToolTask, {
        beforeTool: (call, api) => {
          if (!call.parent) this.threads.get(api.conversationId)?.ordered.beforeTool(api.conversationId, api.taskId, call);
        },
        afterTool: (call, result, api) => {
          const source = this.threads.get(api.conversationId);
          if (!source || call.parent) return;
          const final = source.outputDecoders.get(api.taskId)?.decode();
          source.outputDecoders.delete(api.taskId);
          if (final) source.ordered.toolOutput(api.conversationId, api.taskId, final);
          source.ordered.afterTool(api.conversationId, api.taskId, call, result);
        },
      }), hook(CompactionTask, {
        beforeCompact: (_input, api) => { this.threads.get(api.conversationId)?.fence.beforeCompact(api.taskId); },
      })],
    })];
  }

  // Tool APIs are plain objects. Observe the original output calls before the
  // Harness applies pacing/retention, without changing its output or tool result.
  tapTool = (tool: ToolRegistration): ToolRegistration => ({
    ...tool,
    execute: (args, api, ctx) => {
      const source = this.threads.get(api.conversationId);
      if (!source || !source.ordered.hasExecutableTool(api.taskId)) return tool.execute(args, api, ctx);
      const output = source.outputDecoders;
      return tool.execute(args, { ...api, output: (chunk, skipped) => {
        let text: string;
        if (typeof chunk === "string") {
          text = (output.get(api.taskId)?.decode() ?? "") + chunk;
          output.delete(api.taskId);
        } else {
          let decoder = output.get(api.taskId);
          if (!decoder) output.set(api.taskId, decoder = new TextDecoder());
          text = decoder.decode(chunk, { stream: true });
        }
        if (text) source.ordered.toolOutput(api.conversationId, api.taskId, text);
        api.output(chunk, skipped);
      } }, ctx);
    },
  });
}
