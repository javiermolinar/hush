// Observe real pinned Durable producer boundaries without reading a folded view.
import { BACKGROUND_CONTEXT as ctx } from "@earendil-works/chord/context";
import { localQueueHooks } from "./local-queue.mts";
import {
  createAssistantMessageEventStream,
  createModels,
  fauxAssistantMessage,
  fauxProvider,
  fauxText,
  fauxThinking,
  fauxToolCall,
} from "@earendil-works/pi-ai";
import {
  CompactionTask,
  ConversationIdle,
  createRegistry,
  defineExtension,
  defineTool,
  GenerationTask,
  Harness,
  hook,
  InboxDoc,
  MemoryStorage,
  ToolTask,
} from "@earendil-works/pi-durable";

export { ctx, ConversationIdle, InboxDoc };

export async function openDurable(trace = [], settings = {}, options = {}) {
  const record = (type, detail = {}) => trace.push({ type, ...detail });
  const producer = options.producer;
  const faux = fauxProvider({ api: "fixture-faux", tokenSize: { min: 16, max: 16 },
    ...(options.fauxModels === undefined ? {} : { models: options.fauxModels }),
    ...(options.tokensPerSecond === undefined ? {} : { tokensPerSecond: options.tokensPerSecond }) });
  const models = createModels();
  const tap = (factory) => (model, request, options) => {
    const source = factory(model, request, options);
    const sink = createAssistantMessageEventStream();
    void (async () => {
      try {
        for await (const event of source) {
          producer?.provider(options?.signal, event);
          if (event.type.endsWith("_delta")) {
            record("provider.delta", {
              kind: event.type, contentIndex: event.contentIndex, delta: event.delta,
              usage: structuredClone(event.partial.usage),
            });
          }
          sink.push(event);
        }
        sink.end(await source.result());
      } catch (error) {
        // Failing the probe must not leave the scheduler waiting on a stream.
        sink.end(fauxAssistantMessage("", { stopReason: "error", errorMessage: String(error) }));
      }
    })();
    return sink;
  };
  // Both stream methods are public Provider operations. Durability, retry and summarization still run in Harness.
  models.setProvider({
    ...faux.provider,
    stream: tap(faux.provider.stream.bind(faux.provider)),
    streamSimple: tap(faux.provider.streamSimple.bind(faux.provider)),
  });
  const registry = createRegistry();
  registry.install(defineExtension({
    name: "fixture-observer",
    tools: [defineTool({
      name: "fixture_echo",
      description: "Emit a deterministic tool output with no external effects.",
      parameters: { type: "object", additionalProperties: false, properties: {} },
      replay: "safe",
      async execute(_args, api) {
        record("tool.execute", { callId: api.callId });
        await options.beforeToolResult?.();
        for (const chunk of ["first", " second"]) {
          record("tool.output", { callId: api.callId, chunk });
          producer?.toolOutput(api.conversationId, api.taskId, chunk);
          api.output(chunk);
        }
        return producer ? { output: [{ type: "text", text: "finished" }], details: {} } : {};
      },
    })],
    hooks: [
      hook(GenerationTask, {
        async beforeRequest(_request, api, context) {
          const recovered = options.beforeRequest && await options.beforeRequest(api, context);
          record("generation.beforeRequest", { task: api.taskId });
          producer?.beforeRequest(api.conversationId, api.taskId, context.abortSignal, recovered);
        },
        afterResponse(message, api) {
          record("generation.afterResponse", { task: api.taskId, stopReason: message.stopReason });
        },
        afterTools(assistant, results, api, context) {
          record("generation.afterTools", { task: api.taskId, resultCount: results.length });
          producer?.afterTools(api.conversationId, api.taskId, assistant, results);
          return options.afterTools?.(assistant, results, api, context);
        },
        afterRun(summary, api, context) {
          record("generation.afterRun", { task: api.taskId });
          return options.afterRun?.(summary, api, context);
        },
      }),
      hook(ToolTask, {
        beforeTool(call, api) {
          record("tool.before", { callId: call.id });
          producer?.beforeTool(api.conversationId, api.taskId, call);
        },
        afterTool(call, result, api) {
          record("tool.after", { callId: call.id });
          producer?.afterTool(api.conversationId, api.taskId, call, result);
        },
      }),
      hook(CompactionTask, {
        beforeCompact(input, api) {
          record("compaction.before", { task: api.taskId, reason: input.reason, firstKept: input.firstKept });
          options.beforeCompact?.(api.taskId);
        },
      }),
    ],
  }));
  let harness;
  if (options.localQueue) registry.install(localQueueHooks(() => harness));
  harness = await Harness.open(options.storage ?? new MemoryStorage(), {
    models, registry, onReport: options.onReport,
    settings: { compaction: { enabled: false }, retry: { baseDelayMs: 1, maxRetries: 1 }, ...settings },
  }, ctx);
  const conv = await harness.root(ctx, { agent: { model: { provider: "faux", modelId: "faux-1" } } });
  if (options.seed) await options.seed(conv);
  producer?.attach(harness, conv);
  harness.observeCommits({
    advance(publication) {
      // All operations in one publication are atomic; the API does not specify intra-commit order.
      const tasks = publication.changes.filter((c) => c.type === "task");
      for (const { value: task } of tasks) {
        record("task", {
          task: task.id,
          state: task.state.status,
          phase: task.state.checkpoint?.phase,
          attempt: task.state.checkpoint?.attempt,
          result: task.state.outcome?.status,
        });
      }
      for (const change of publication.changes) {
        if (change.type === "submission") record("receipt", { status: change.value.status });
        if (change.type === "entry") record("entry", { kind: change.value.kind });
      }
    },
    end(end) { record("storage.end", { kind: end.kind }); },
  });
  return { conv, harness, faux, close: () => harness.close(ctx) };
}

export function toolRound() {
  return fauxAssistantMessage([
    fauxThinking("test only"), fauxText("calling"), fauxToolCall("fixture_echo", {}, { id: "fixture-call" }),
  ], { stopReason: "toolUse" });
}
