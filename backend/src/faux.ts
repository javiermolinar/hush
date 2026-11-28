import { setTimeout } from "node:timers/promises";
import { fauxAssistantMessage, fauxProvider, fauxText, fauxThinking, fauxToolCall, type FauxResponseFactory } from "@earendil-works/pi-ai/providers/faux";
import { defineExtension, defineTool } from "@earendil-works/pi-durable";
import { getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";

export const fauxModel = { provider: "faux", modelId: "faux-1" };

export function makeFaux(ephemeral: boolean) {
  const faux = fauxProvider({ api: "hush-faux", tokenSize: { min: 16, max: 16 } });
  const reply: FauxResponseFactory = async ({ messages }, options) => {
    faux.appendResponses([reply]);
    const last = messages.findLast(message => message.role !== "system");
    if (last?.role === "toolResult") {
      if (last.toolName === "codemode" || last.toolName === "plan_save" ||
          last.toolName === "hush_fixture_fast" || last.toolCallId === "workspace-read") {
        return fauxAssistantMessage(last.content.filter(b => b.type === "text").map(b => b.text).join("\n"));
      }
      return fauxAssistantMessage(last.isError ? "The interrupted fixture tool did not rerun." : "The deadline fired.");
    }
    const text = last?.role !== "user" ? "" : typeof last.content === "string" ? last.content
      : last.content.flatMap(block => block.type === "text" ? [block.text] : []).join("");
    if (text === "!plan-fixture" && getCurrentTools(messages).some(t => t.name === "plan_save")) {
      return fauxAssistantMessage(fauxToolCall("plan_save", { baseRevision: 0, title: "Workspace profiles", steps: [
        { id: "save", title: "Save a profile", prompt: "Store a profile under a stable name. Check that the saved profile has the expected name." },
        { id: "restore", title: "Restore saved profiles", prompt: "Read the saved profile and restore its settings. Check that they round-trip.",
          requires: ["save"] },
        { id: "docs", title: "Document profile discovery", prompt: "Describe where profiles are read from." },
        { id: "review", title: "Review the profile code", prompt: "Review the save and restore code for data loss.",
          requires: ["restore"], delegate: { read_only: true } },
      ] }, { id: "plan-draft" }), { stopReason: "toolUse" });
    }
    if (text === "!error") return fauxAssistantMessage("", { stopReason: "error", errorMessage: "synthetic failure" });
    if (text === "!aborted") return fauxAssistantMessage("", { stopReason: "aborted", errorMessage: "synthetic abort" });
    if (text === "!length") return fauxAssistantMessage("partial faux answer", { stopReason: "length" });
    if (text === "!textless") return fauxAssistantMessage("");
    if (text === "!pause") {
      try { await setTimeout(60_000, undefined, { signal: options?.signal }); }
      catch (error) {
        if (!options?.signal?.aborted) throw error;
        // The faux factory's generic exception path would report a provider
        // error. Its cancellation should instead finish as an aborted stream.
        return fauxAssistantMessage("", { stopReason: "aborted", errorMessage: "Request was aborted" });
      }
    }
    if (!ephemeral) {
      if (text.startsWith("!codemode\n")) return fauxAssistantMessage(
        fauxToolCall("codemode", { code: text.slice("!codemode\n".length) }, { id: "codemode-fixture-" + faux.state.callCount }), { stopReason: "toolUse" });
      if (text === "!workspace-read") return fauxAssistantMessage(
        fauxToolCall("read", { path: "workspace-marker.txt" }, { id: "workspace-read" }), { stopReason: "toolUse" });
      if (text === "!workspace-write") return fauxAssistantMessage(
        fauxToolCall("write", { path: "workspace-marker.txt", content: "Written by the faux tool.\n" },
          { id: "workspace-write" }), { stopReason: "toolUse" });
      if (text === "!fast-tool") return fauxAssistantMessage([
        fauxThinking("fixture thinking"), fauxText("calling the fixture"), fauxToolCall("hush_fixture_fast", {}, { id: "fast-call" }),
      ], { stopReason: "toolUse" });
      if (text === "!unoffered-tool") return fauxAssistantMessage(fauxToolCall("hush_not_offered", {}, { id: "unoffered-call" }), { stopReason: "toolUse" });
      const sleep = /^!sleep ([1-9]\d{0,4})$/.exec(text);
      if (sleep || text === "!crash") {
        return fauxAssistantMessage(fauxToolCall("hush_fixture_wait", { ms: sleep ? Number(sleep[1]) : 0 },
          { id: "hush-fixture-" + faux.state.callCount }), { stopReason: "toolUse" });
      }
    }
    return fauxAssistantMessage("Faux: " + text);
  };
  faux.setResponses([reply]);
  return faux.provider;
}

export const fauxTools = defineExtension({ name: "hush-faux-tools", tools: [
  defineTool({
    name: "hush_fixture_fast", description: "Emit two synthetic chunks without blocking.", replay: "safe",
    parameters: { type: "object", properties: {}, additionalProperties: false },
    async execute(_args, api) {
      api.output("first");
      api.output(" second");
      return { output: [{ type: "text", text: "fixture tool finished" }] };
    },
  }),
  defineTool({
    name: "hush_fixture_wait", description: "Synthetic sleep for a local faux fixture.", replay: "unsafe",
    parameters: { type: "object", properties: { ms: { type: "integer", minimum: 0, maximum: 99_999 } },
      required: ["ms"], additionalProperties: false },
    async execute(args, api, context) {
      const ms = (args as { ms: number }).ms;
      if (ms === 0) {
        api.output("fixture entered");
        await setTimeout(60_000, undefined, { signal: context.abortSignal });
      } else if (api.wake === undefined) return { sleep: { until: api.now() + ms } };
      return { output: [{ type: "text", text: "fixture done" }] };
    },
  }),
] });
