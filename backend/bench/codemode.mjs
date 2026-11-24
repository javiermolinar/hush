// Opt-in live comparison; sends only the generated fixture to the selected provider.
import assert from "node:assert/strict";
import { mkdir, mkdtemp, realpath, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { startBackend } from "../../dist/hush-backend.mjs";

const model = { provider: "openai", modelId: "gpt-5.4-mini" };
const thinkingLevel = "off";
assert.ok(process.env.OPENAI_API_KEY, "Set OPENAI_API_KEY to run this live comparison");
const pairs = Number(process.argv[2] ?? 3);
assert.ok(Number.isInteger(pairs) && pairs >= 1 && pairs <= 10, "Pass a pair count between 1 and 10");
const root = await realpath(await mkdtemp("/tmp/hcmb-"));
const cwd = join(root, "workspace");
const keys = ["pine-420", "elm-117", "ash-731", "oak-583"];
const files = keys.map((_, i) => `record-${i + 1}.txt`);
const prompt = `Inspect ${files.join(", ")} and return only the value from the line starting REVIEW_KEY= in each file, ` +
  "comma-separated in file order. Use one Codemode batch if available, otherwise use direct read tools (in parallel if possible). Do not use shell.";

// Union the top-level tool intervals: parallel reads count once, and Codemode
// includes its QuickJS worker, all nested inspections, and result shaping.
function toolWallTime(messages) {
  const intervals = messages.map(m => [m.timestamp - m.durationMs, m.timestamp]).sort((a, b) => a[0] - b[0]);
  let total = 0, through = 0;
  for (const [start, end] of intervals) {
    total += Math.max(0, end - Math.max(start, through));
    through = Math.max(through, end);
  }
  return total;
}

async function measure(codemode, pair) {
  const agentDir = join(root, `${pair}-${codemode ? "on" : "off"}`), dataDir = join(agentDir, "runtime");
  await mkdir(agentDir, { mode: 0o700 });
  await writeFile(join(agentDir, "settings.json"), JSON.stringify({ codemode,
    defaultProvider: model.provider, defaultModel: model.modelId, defaultThinkingLevel: thinkingLevel,
  }), { mode: 0o600 });
  const app = await startBackend({ profile: "hush", cwd, agentDir, dataDir, socket: join(dataDir, "hush.sock"),
    ephemeral: false, faux: false, idleMs: 120_000 });
  try {
    const roundTrips = [];
    const streamSimple = app.runtime.streamSimple.bind(app.runtime);
    app.runtime.streamSimple = (...args) => {
      const started = performance.now();
      const stream = streamSimple(...args);
      roundTrips.push(stream.result().then(message => ({ ms: performance.now() - started, message })));
      return stream;
    };
    await app.root.configure({ maxTurns: 4, compaction: { enabled: false } }, context);
    const agent = await app.root.agent(context);
    assert.equal(agent.model.modelId, model.modelId);
    assert.equal(agent.thinkingLevel, thinkingLevel);
    assert.deepEqual((await app.root.entries({}, 100, undefined, context)).items, []);
    const started = performance.now();
    const submission = await app.root.submit({ type: "input", content: prompt, whenBusy: "reject" }, context);
    const settled = await submission.wait(context);
    const endToEndMs = Math.round(performance.now() - started);
    const rounds = await Promise.all(roundTrips);
    const messages = (await app.root.entries({}, 100, undefined, context)).items.flatMap(e => e.model ?? []);
    const tools = messages.filter(m => m.role === "toolResult");
    assert.equal(settled.status, "done", JSON.stringify({ reason: settled.reason,
      providerStops: rounds.map(r => r.message.stopReason) }));
    const answer = rounds.at(-1).message.content.filter(b => b.type === "text").map(b => b.text).join("");
    assert.equal(answer.replace(/\s/g, ""), keys.join(","));
    assert.ok(tools.every(m => !m.isError && Number.isFinite(m.durationMs)));
    assert.deepEqual(tools.map(m => m.toolName), codemode ? ["codemode"] : files.map(() => "read"));
    if (codemode) assert.deepEqual(tools[0].details.calls.map(c => [c.name, c.status]), files.map(() => ["inspect_file", "ok"]));
    const tokens = Object.fromEntries(["input", "output", "cacheRead", "cacheWrite", "totalTokens"].map(key =>
      [key, rounds.reduce((total, r) => total + r.message.usage[key], 0)]));
    return { pair, codemode, roundTrips: rounds.length, tokens, modelMs: Math.round(rounds.reduce((ms, r) => ms + r.ms, 0)),
      toolWallMs: toolWallTime(tools), endToEndMs };
  } finally { await app.close(); }
}

try {
  await mkdir(cwd, { mode: 0o700 });
  await Promise.all(files.map((file, i) => {
    const row = n => `row ${n}: fixture observation for warehouse ${((n + 23) * (i + 3)) % 97}\n`;
    const lines = Array.from({ length: 80 }, (_, n) => row(n));
    lines.splice(12 + i * 13, 0, `REVIEW_KEY=${keys[i]}\n`);
    return writeFile(join(cwd, file), lines.join(""), { mode: 0o600 });
  }));
  console.log(JSON.stringify({ model, thinkingLevel, files: files.length, pairs }));
  for (let pair = 1; pair <= pairs; pair++) {
    for (const codemode of (pair % 2 ? [false, true] : [true, false])) {
      console.log(JSON.stringify(await measure(codemode, pair)));
    }
  }
} finally { await rm(root, { recursive: true, force: true }); }
