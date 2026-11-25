// Cost of one Feed flush (server/src/index.ts) per streamed token: project the
// newest view, validate it, diff it against the previous projection with
// Chord, then check and encode the patch. The synthetic views reuse one
// EntryRecord array and replace only the live generation, as Durable does.
// Run directly for more samples: HUSH_BENCH_SAMPLES=2000 node server/test/feed-flush.test.mjs
import { test } from "node:test";
import { diffRevisions } from "@earendil-works/chord/delta";
import { projectView } from "../../dist/server/src/durable.js";
import { cleanJSON, encode, patchShape } from "../../dist/server/src/wire.js";
import { checkTarget, samples, summary } from "./perf.mjs";

const count = samples(1000), warmup = 500; // warm the JIT before timing
const target = 0.1; // ms p99 per flush, projection-memo

function entries(n, toolBytes) {
  const prompt = "Please review the cached handler. ".repeat(7).slice(0, 200);
  const answer = "The handler reads the cached value before the request. ".repeat(15).slice(0, 800);
  const output = "ok  github.com/javiermolinar/hush/sdk  0.7s\n".repeat(Math.ceil(toolBytes / 44)).slice(0, toolBytes);
  return Array.from({ length: n }, (_, i) => {
    const id = i + 1, timestamp = 1795435568245;
    const model = [[{ role: "user", content: prompt, timestamp }],
      [{ role: "assistant", content: [{ type: "text", text: answer }, { type: "toolCall", id: `call-${id}`, name: "bash", arguments: { command: "go test ./..." } }],
        stopReason: "toolUse", timestamp }],
      [{ role: "toolResult", toolCallId: `call-${id - 1}`, toolName: "bash", content: [{ type: "text", text: output }], isError: false, timestamp }]][i % 3];
    return Object.freeze({ id, conversationId: 1, kind: "model", model });
  });
}

const view = (records, text) => ({
  conversation: { id: 1 }, entries: records, before: undefined,
  docs: {
    "pi.agent": { model: { provider: "faux", modelId: "faux-1" }, thinkingLevel: "off", cwd: "/workspace" },
    "pi.live": { run: { taskId: 900, inputs: [901] }, generation: { attempt: 1, message: { content: [{ type: "text", text }] } } },
  },
});

function measure(n, toolBytes) {
  const records = entries(n, toolBytes), identity = { epoch: "bench-epoch", subscriptionId: "bench-sub" };
  const phases = { project_and_validate: [], diff: [], encode: [] }, total = [];
  let before = projectView(view(records, "")), rev = 0, text = "";
  for (let i = 0; i < warmup + count; i++) {
    text += " token";
    const t0 = performance.now();
    const next = projectView(view(records, text));
    const t1 = performance.now();
    const ops = diffRevisions(before, next);
    const t2 = performance.now();
    const delta = { type: "patch", ...identity, baseRev: rev, nextRev: rev + 1, ops };
    encode(cleanJSON(ops) && patchShape(delta) ? delta : { ...delta, ops: [["r", next]] });
    const t3 = performance.now();
    before = next;
    rev++;
    if (i < warmup) continue;
    total.push(t3 - t0);
    phases.project_and_validate.push(t1 - t0);
    phases.diff.push(t2 - t1);
    phases.encode.push(t3 - t2);
  }
  return { total: summary(total), p50_by_phase_ms: Object.fromEntries(Object.entries(phases).map(([k, v]) => [k, summary(v).p50_ms])) };
}

for (const [n, toolBytes] of [[10, 2 << 10], [10, 16 << 10], [100, 2 << 10], [100, 16 << 10]]) {
  test(`feed flush per streamed token: window ${n}, tool results ${toolBytes >> 10} KB`, t => {
    const result = measure(n, toolBytes);
    t.diagnostic(JSON.stringify(result));
    checkTarget(t, { name: "p99 per flush", value: result.total.p99_ms, limit: target });
  });
}
