// Reference fixture driver: pinned Pi's actual print/RPC serializers, not a hush backend.
import { existsSync } from "node:fs";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import {
  createAgentSessionFromServices,
  createAgentSessionRuntime,
  createAgentSessionServices,
  ModelRuntime,
  runPrintMode,
  runRpcMode,
  SessionManager,
  SettingsManager,
} from "@earendil-works/pi-coding-agent";
import {
  fauxAssistantMessage,
  fauxProvider,
  fauxText,
  fauxThinking,
  fauxToolCall,
  InMemoryCredentialStore,
} from "@earendil-works/pi-ai";

const [mode, scenario = "chat"] = process.argv.slice(2);
const cwd = process.cwd();
const agentDir = join(cwd, "agent");
const sessions = join(cwd, "sessions");
await mkdir(join(agentDir, "prompts"), { recursive: true });
await writeFile(join(agentDir, "prompts", "fixture.md"), "Fixture template: $1\n");
const faux = fauxProvider({
  api: "fixture-faux",
  models: [{ id: "faux-1", reasoning: true }, { id: "faux-2", reasoning: false }],
  tokenSize: { min: 16, max: 16 },
  ...(scenario === "slow" ? { tokensPerSecond: 50 } : {}),
});
const reply = (v, overrides) => fauxAssistantMessage(v, { timestamp: 1, ...overrides });
const holdThreshold = async (_request, { signal }) => {
  while (!existsSync(join(cwd, "release-threshold-fixture"))) await delay(10, undefined, { signal });
  return reply("threshold summary");
};
const holdAnswer = async (request, options) => {
  await holdThreshold(request, options);
  return reply("answer with queued input");
};
// Use a timestamp beyond the fixture's compaction marker when the error occurs after a summary.
const overflowError = (afterCompaction = false) => reply("", {
  stopReason: "error", errorMessage: "Input exceeds the context window", timestamp: afterCompaction ? Date.now() + 60_000 : 1,
});
const toolTurn = () => reply([fauxThinking("test only"), fauxText("calling"), fauxToolCall("fixture_echo", {}, { id: "fixture-call" })], {
  stopReason: "toolUse",
});
const scenarios = {
  chat: [reply("reference answer")],
  queue: [reply("one"), reply("two"), reply("three"), reply("four")],
  slow: [reply("deliberately slow ".repeat(200))],
  retry: [reply("", { stopReason: "error", errorMessage: "529 overloaded" }), reply("recovered")],
  retry_failed: [reply("", { stopReason: "error", errorMessage: "529 overloaded" }),
    reply("", { stopReason: "error", errorMessage: "529 overloaded again" })],
  history: [reply("first ".repeat(100)), reply("second ".repeat(100)), reply("fixture summary")],
  branched: [reply("unused response")],
  overflow: [overflowError(), reply("anchored overflow summary"), reply("split-turn fixture context"), reply("recovered after overflow")],
  overflow_failed: [overflowError(), reply("", { stopReason: "error", errorMessage: "fixture summary failure" })],
  overflow_aborted: [overflowError(), holdThreshold],
  overflow_again: [overflowError(), reply("anchored overflow summary"), reply("split-turn fixture context"), overflowError(true)],
  threshold_pre_prompt: [reply("pre-prompt summary"), reply("split-turn fixture context"), reply("pre-prompt answer")],
  threshold_tools: [toolTurn(), reply("between-turn summary"), reply("after threshold tools")],
  threshold_queued: [holdAnswer, reply("queued turn summary"), reply("queued turn answer")],
  threshold_then_overflow: [reply("pre-prompt summary"), reply("pre-prompt split-turn context"), overflowError(true),
    reply("anchored overflow summary"), reply("overflow split-turn context"), reply("recovered after overflow")],
  threshold: [reply("threshold answer"), reply("threshold summary")],
  threshold_hold: [reply("threshold answer"), holdThreshold, reply("after late input")],
  threshold_failure: [reply("threshold answer"), reply("", { stopReason: "error", errorMessage: "fixture summary failure" })],
  threshold_retry: [reply("threshold answer"), reply("", { stopReason: "error", errorMessage: "529 overloaded" }),
    reply("threshold summary")],
  history_retry: [
    reply("first ".repeat(100)), reply("second ".repeat(100)),
    reply("", { stopReason: "error", errorMessage: "529 overloaded" }), reply("fixture summary"),
  ],
  error: [reply("", { stopReason: "error", errorMessage: "fixture model error" })],
  aborted: [reply("", { stopReason: "aborted", errorMessage: "fixture abort" })],
  length: [reply("partial", { stopReason: "length" })],
  textless: [reply("  previous answer  "), reply([fauxThinking("no visible text")])],
  tools: [toolTurn(), reply("tool complete")],
  unoffered_tool: [reply([fauxToolCall("not_registered", {}, { id: "rejected-call" })], { stopReason: "toolUse" }),
    reply("continued after rejection")],
  invalid_tool: [reply([fauxToolCall("fixture_echo", { unexpected: true }, { id: "rejected-call" })], { stopReason: "toolUse" }),
    reply("continued after rejection")],
};
if (!(scenario in scenarios)) throw new Error(`unknown fixture scenario: ${scenario}`);
faux.setResponses(scenarios[scenario]);
const modelRuntime = await ModelRuntime.create({
  credentials: new InMemoryCredentialStore(),
  modelsPath: null,
  modelsStorePath: join(agentDir, "models-cache.json"),
  allowModelNetwork: false,
  refreshOnCreate: false,
});
modelRuntime.registerNativeProvider(faux.provider);
const sessionsManager = process.env.HUSH_FIXTURE_PERSIST
  ? SessionManager.create(cwd, sessions)
  : SessionManager.inMemory(cwd);

if (scenario === "branched") {
  const user = content => ({ role: "user", content, timestamp: 1 });
  const common = sessionsManager.appendMessage(user("old shared input"));
  sessionsManager.appendMessage(reply("abandoned answer"));
  sessionsManager.branch(common);
  sessionsManager.appendMessage(reply("superseded active-prefix answer"));
  const keep = sessionsManager.appendMessage(user("kept input"));
  sessionsManager.appendMessage(reply("kept answer"));
  sessionsManager.appendCompaction("anchored fixture summary", keep, 2100);
  sessionsManager.appendMessage(user("new active input"));
  sessionsManager.appendMessage(reply("active leaf answer"));
}
const thresholdScenario = scenario.startsWith("threshold"), overflowScenario = scenario.startsWith("overflow");
if (overflowScenario || thresholdScenario) {
  const high = scenario === "threshold_pre_prompt" || scenario === "threshold_then_overflow";
  for (const [user, assistant] of [["old input ", "old answer "], ["kept input ", "kept answer "]]) {
    sessionsManager.appendMessage({ role: "user", content: user.repeat(high ? 300 : 240), timestamp: 1 });
    sessionsManager.appendMessage(reply(assistant.repeat(high ? 200 : 150)));
  }
}

const runtime = await createAgentSessionRuntime(async (input) => {
  const settingsManager = SettingsManager.inMemory({
    retry: { enabled: ["retry", "retry_failed", "history_retry", "threshold_retry"].includes(scenario), baseDelayMs: 1, maxRetries: 1 },
    compaction: { enabled: overflowScenario || thresholdScenario, keepRecentTokens: 250,
      // The seeded history is below 2200 tokens; the next request including system/tool metadata crosses it.
      ...(thresholdScenario ? { reserveTokens: 125800 } : {}) },
  });
  const services = await createAgentSessionServices({
    cwd: input.cwd,
    agentDir: input.agentDir,
    modelRuntime,
    settingsManager,
    resourceLoaderOptions: { noExtensions: true, noContextFiles: true, noThemes: true, noSkills: true },
  });
  const result = await createAgentSessionFromServices({
    services,
    sessionManager: input.sessionManager,
    sessionStartEvent: input.sessionStartEvent,
    model: faux.getModel(),
    tools: ["fixture_echo"],
    customTools: [{
      name: "fixture_echo",
      label: "Fixture",
      description: "Report two deterministic strings without side effects.",
      parameters: { type: "object", properties: {}, additionalProperties: false },
      async execute(_callId, _params, _signal, update) {
        update?.({ content: [{ type: "text", text: "first" }], details: {} });
        update?.({ content: [{ type: "text", text: " second" }], details: {} });
        return { content: [{ type: "text", text: "finished" }], details: {} };
      },
    }],
  });
  return { ...result, services, diagnostics: services.diagnostics };
}, {
  cwd,
  agentDir,
  sessionManager: sessionsManager,
});

if (mode === "rpc") await runRpcMode(runtime);
else process.exitCode = await runPrintMode(runtime, { mode, initialMessage: "fixture prompt" });
