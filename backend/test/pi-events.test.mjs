import assert from "node:assert/strict";
import { once } from "node:events";
import { mkdir, mkdtemp, realpath, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT as ctx } from "@earendil-works/chord/context";
import AjvModule from "ajv/dist/2020.js";
import { startBackend } from "../../dist/hush-backend.mjs";
import schema from "../../protocol/v1/pi-1.6/pi-events.schema.json" with { type: "json" };
import { commands, events, rpcOnly, rejectionFixtures, updateVariants } from "../../protocol/v1/pi-1.6/surface.mts";
import { assertFrame } from "../../protocol/v1/replay.mjs";
import { dial } from "../../server/test/client.mjs";

const ok = f => { assert.equal(f.ok, true, JSON.stringify(f)); return f.result; };
const failure = (f, code) => { assert.equal(f.ok, false); assert.equal(f.error.code, code); };
const ajv = new AjvModule.Ajv2020({ strict: false, ownProperties: true }).addSchema(schema);
const shape = Object.fromEntries(Object.keys(schema.$defs).map(key => [key, ajv.compile({ $ref: `${schema.$id}#/$defs/${key}` })]));
function frameShape(f) {
  if (f.type === "pi_event" || f.type === "pi_end") {
    assert.equal(shape[f.type === "pi_event" ? "frame" : "end"](f), true, JSON.stringify(f));
  } else if (f.ok && f.method.startsWith("ext.piEvents.")) {
    assert.equal(shape.successEnvelope(f), true, JSON.stringify(f));
    const name = f.method.split(".").at(-1);
    assert.equal(shape[{ open: "ready", catalog: "catalogResult", command: "callResult" }[name]](f.result), true, JSON.stringify(f));
  } else assertFrame(f);
}

async function setup(t) {
  const dir = await realpath(await mkdtemp("/tmp/hush-pi-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const cwd = join(dir, "project"), agentDir = join(dir, "agent"), dataDir = join(dir, "state");
  await Promise.all([mkdir(cwd, { mode: 0o700 }), mkdir(agentDir, { mode: 0o700 })]);
  return { cwd, agentDir, dataDir, socket: join(dataDir, "hush.sock"), idleMs: 10_000, ephemeral: false, faux: true, piEvents: true };
}

async function open(t, config) {
  const app = await startBackend(config);
  t.after(() => app.close());
  return app;
}

async function piPeer(t, config, options = {}) {
  const wire = await dial(config.socket, frameShape);
  t.after(() => wire.close());
  const hello = ok(await wire.send("hello", { versions: [1], capabilities: { piEvents: [1] } }));
  assert.deepEqual(hello.capabilities, { piEvents: 1 });
  let ready = ok(await wire.send("ext.piEvents.open", { cwd: config.cwd, ...options }));
  let lastRequest = 0;
  return { wire, hello, get ready() { return ready; },
    async command(type, params, id = `req-${++lastRequest}`) {
      const result = ok(await wire.send("ext.piEvents.command", { epoch: ready.epoch, command: { type, id, ...params } }));
      assert.equal(result.response.command, type);
      assert.equal(result.response.id, id);
      if (result.ready) ready = result.ready;
      return result.response;
    },
    since() { return wire.frames.length; },
    records(since = 0) { return wire.frames.slice(since).flatMap(f => f.type === "pi_event" ? [f.event] : []); },
    until(since, type) { return wire.wait(f => wire.frames.indexOf(f) >= since && (f.type === "pi_end" || f.event?.type === type)); },
  };
}

const data = r => { assert.equal(r.success, true, JSON.stringify(r)); return r.data; };
const text = m => typeof m.content === "string" ? m.content : m.content?.flatMap(b => b.type === "text" ? [b.text] : []).join("") ?? "";
const ensureOrder = (records, ...types) => {
  let i = 0;
  for (const r of records) if (r.type === types[i]) i++;
  assert.equal(i, types.length, `missing ${types[i]} after ${types.slice(0, i).join(",")}; got ${records.map(r => r.type).join(",")}`);
};

test("piEvents schema classifies every pinned live and excluded event variant", () => {
  assert.deepEqual([...schema.$defs.event.properties.type.enum].sort(),
    Object.entries(events).filter(([, row]) => row.scope === "piEvents").map(([key]) => key).sort());
  const delta = schema.$defs.event.allOf.find(rule => rule.if.properties.type.const === "message_update");
  assert.deepEqual([...delta.then.properties.assistantMessageEvent.properties.type.enum].sort(),
    Object.entries(updateVariants).filter(([, reference]) => !reference.startsWith("schema only")).map(([type]) => type).sort());
});

async function prompt(p, message) {
  const start = p.since();
  assert.equal(data(await p.command("prompt", { message })).disposition, "started");
  assert.equal((await p.until(start, "agent_settled")).event?.type, "agent_settled");
  return p.records(start);
}

test("piEvents: ready and pre-fold tool chunks survive a deliberately blocked watch", { timeout: 20000 }, async t => {
  const cfg = await setup(t), app = await open(t, cfg);
  const p = await piPeer(t, cfg);
  const before = p.wire.frames.length;
  assert.equal(p.ready.seq, 0);
  assert.deepEqual(p.ready.services, ["models", "resources", "titles"]);
  assert.equal(p.records().length, 0);
  const blocked = Promise.withResolvers(), release = Promise.withResolvers(), folded = Promise.withResolvers();
  const conv = await app.harness.conversation(Number(p.ready.threadId), ctx);
  const view = await conv.watch(ctx), states = [];
  view.start(async ({ kind, value }) => {
    if (kind !== "value") return;
    states.push(value);
    if (states.length === 1) { blocked.resolve(); await release.promise; }
    else folded.resolve();
  });
  t.after(async () => { release.resolve(); await view.stop(); });
  try {
    const run = prompt(p, "!fast-tool");
    await blocked.promise;
    const rows = await run;
    ensureOrder(rows, "agent_start", "turn_start", "message_update", "tool_execution_start", "tool_execution_update", "tool_execution_update",
      "tool_execution_end", "turn_end", "turn_start", "turn_end", "agent_end", "agent_settled");
    const updates = rows.filter(r => r.type === "tool_execution_update");
    assert.deepEqual(updates.map(r => r.partialResult.content[0].text), ["first", " second"]);
    assert.ok(updates.every(r => r.toolCallId === "fast-call"));
    assert.ok(rows.some(r => r.assistantMessageEvent?.type === "thinking_delta" && r.assistantMessageEvent.contentIndex === 0));
    assert.ok(rows.some(r => r.assistantMessageEvent?.type === "toolcall_start" && r.assistantMessageEvent.id === "fast-call"));
    const done = rows.find(r => r.type === "tool_execution_end");
    assert.equal(done.result.content[0].text, "fixture tool finished");
    assert.equal(typeof done.durationMs, "number");
    assert.equal(states.length, 1);
    release.resolve();
    await folded.promise;
    assert.ok(states.every(v => !v.docs["pi.live"]?.tools?.some(tool => tool.status === "running")));
    const unoffered = (await prompt(p, "!unoffered-tool")).filter(r => r.type.startsWith("tool_execution_"));
    assert.deepEqual(unoffered.map(r => r.type), ["tool_execution_start", "tool_execution_end"]);
    assert.equal(unoffered[1].isError, true);
    assert.ok(!Object.hasOwn(unoffered[1], "durationMs"));
    const streamed = p.wire.frames.slice(before).filter(f => f.type === "pi_event");
    assert.deepEqual(streamed.map(f => f.seq), streamed.map((_, i) => i + 1));
    assert.ok(streamed.every(f => f.threadId === p.ready.threadId && f.epoch === p.ready.epoch));
    const native = await dial(cfg.socket, assertFrame);
    t.after(() => native.close());
    assert.deepEqual(ok(await native.send("hello", { versions: [1] })).capabilities, {});
    failure(await native.send("submit", { threadId: p.ready.threadId, text: "do not mix owners", requestId: "bad", whenBusy: "reject" }), "unsupported_capability");
    const listed = ok(await native.send("thread.list", { limit: 20 }));
    assert.ok(listed.threads.some(r => r.id === p.ready.threadId));
    failure(await native.send("ext.piEvents.open", { cwd: cfg.cwd }), "unsupported_capability");
  } finally { release.resolve(); await view.stop(); }
});

test("piEvents: every unsupported or absent command fails before a Harness or provider effect", { timeout: 20000 }, async t => {
  const cfg = await setup(t), app = await open(t, cfg);
  const wire = await dial(cfg.socket, frameShape);
  t.after(() => wire.close());
  assert.deepEqual(ok(await wire.send("hello", { versions: [1], capabilities: { piEvents: [2] } })).capabilities, {});
  failure(await wire.send("ext.piEvents.open", { cwd: cfg.cwd }), "unsupported_capability");
  const p = await piPeer(t, cfg);
  const commits = [];
  const observer = app.harness.observeCommits({ advance(batch) { commits.push(...batch.changes); }, end() {} });
  try {
    for (const expect of rejectionFixtures().filter(x => !p.ready.services.includes(commands[x.command]?.capability))) {
      assert.deepEqual(await p.command(expect.command, {}, expect.id), expect);
    }
    for (const name of ["extension_ui_response", "constructor", "does_not_exist"]) {
      assert.equal((await p.command(name)).success, false);
    }
    assert.match((await p.command("steer", { message: "x", images: [{}] })).error, /capability: images/);
    assert.match((await p.command("prompt", { message: 17 })).error, /Invalid fields/);
    assert.match((await p.command("set_auto_compaction", { enabled: "false" })).error, /Invalid fields/);
    failure(await p.wire.send("ext.piEvents.command", { epoch: "00000000-0000-0000-0000-000000000000", command: { type: "prompt", message: "wrong fence" } }), "invalid_request");
    assert.deepEqual(commits, []);
    assert.deepEqual((await app.harness.inspect(ctx)).tasks, []);
    const unsupportedEvents = new Set([...Object.keys(rpcOnly), ...Object.entries(events).filter(([, v]) => v.scope === "no").map(([key]) => key)]);
    assert.ok(p.records().every(r => !unsupportedEvents.has(r.type)));
    assert.equal(data(await p.command("get_state")).messageCount, 0);
    const noFence = await dial(cfg.socket, frameShape);
    t.after(() => noFence.close());
    await noFence.send("hello", { versions: [1], capabilities: { piEvents: [1] } });
    failure(await noFence.send("ext.piEvents.command", { epoch: p.ready.epoch, command: { type: "prompt", message: "not ready" } }), "invalid_request");
    failure(await noFence.send("ext.piEvents.open", { cwd: cfg.cwd, threadId: p.ready.threadId }), "busy");
  } finally { observer.detach(); }
});

test("piEvents: scope-local queue, stable title, fork semantics, and a fresh epoch after restart", { timeout: 20000 }, async t => {
  const cfg = await setup(t), app = await open(t, cfg);
  const p = await piPeer(t, cfg);
  const first = p.ready;
  assert.deepEqual(data(await p.command("steer", { message: "first steer" })), { disposition: "queued" });
  assert.deepEqual(data(await p.command("steer", { message: "second steer" })), { disposition: "queued" });
  assert.deepEqual(data(await p.command("follow_up", { message: "after" })), { disposition: "queued" });
  const trace = await prompt(p, "first prompt");
  assert.deepEqual(trace.filter(r => r.type === "message_start" && r.message.role === "user").map(r => text(r.message)), ["first prompt", "first steer", "second steer", "after"]);
  assert.equal(trace.at(-1).aborted, false);
  assert.equal(data(await p.command("get_state")).pendingMessageCount, 0);
  assert.deepEqual(data(await p.command("clear_queue")), { steering: [], followUp: [] });
  await prompt(p, "!textless");
  assert.deepEqual(data(await p.command("get_last_assistant_text")), {});
  data(await p.command("set_session_name", { name: "  persistent title  " }));
  assert.ok(p.records().some(r => r.type === "session_info_changed" && r.name === "persistent title"));
  const [point] = data(await p.command("get_fork_messages")).messages;
  assert.equal(point.text, "first prompt");
  const stats = data(await p.command("get_session_stats"));
  assert.equal(stats.sessionId, first.header.id);
  assert.equal(stats.userMessages, 5);
  assert.ok(stats.assistantMessages > 0);
  assert.ok(!Object.hasOwn(stats, "sessionFile"));
  assert.deepEqual(data(await p.command("clone")), { cancelled: false });
  assert.notEqual(p.ready.epoch, first.epoch);
  assert.notEqual(p.ready.header.id, first.header.id);
  assert.equal(p.ready.header.parentSession, first.header.id);
  assert.deepEqual(data(await p.command("fork", { entryId: point.entryId })), { cancelled: false, text: "first prompt" });
  assert.ok(data(await p.command("get_messages")).messages.every(m => m.role !== "user"));
  assert.deepEqual(data(await p.command("new_session", { parentSession: first.header.id })), { cancelled: false });
  assert.equal(p.ready.header.parentSession, first.header.id);
  assert.deepEqual(data(await p.command("get_last_assistant_text")), {});
  p.wire.close();
  await app.close();
  const reboot = await open(t, cfg);
  const q = await piPeer(t, cfg, { threadId: first.threadId });
  assert.notEqual(q.hello.epoch, p.hello.epoch);
  assert.notEqual(q.ready.epoch, first.epoch);
  assert.deepEqual(q.ready.header, first.header);
  assert.equal(q.records().length, 0, "opening is not replay");
  assert.equal(data(await q.command("get_state")).sessionName, "persistent title");
  assert.equal(data(await q.command("get_fork_messages")).messages[0].entryId, point.entryId);
  await prompt(q, "after restart");
  assert.equal(q.wire.frames.find(f => f.type === "pi_event").seq, 1);
  assert.ok(reboot.harness);
});

async function loopback(t, cfg, settings, reply) {
  const requests = [];
  const agent = createServer(async (req, res) => {
    try {
      const chunks = [];
      for await (const b of req) chunks.push(b);
      const body = JSON.parse(Buffer.concat(chunks));
      requests.push(body);
      const answer = await reply(body, requests.length);
      if (answer.status) {
        res.writeHead(answer.status, { "content-type": "application/json" }).end(JSON.stringify({ error: { message: answer.message ?? "529 overloaded" } }));
        return;
      }
      const frame = (choices, usage) => "data: " + JSON.stringify({ id: "fixture", object: "chat.completion.chunk", created: 1,
        model: body.model, choices, usage }) + "\n\n";
      res.writeHead(200, { "content-type": "text/event-stream" }).end(
        frame([{ index: 0, delta: answer.tool ? { tool_calls: [{ index: 0, id: "capture-call", type: "function", function: answer.tool }] }
          : { content: answer.text }, finish_reason: null }]) +
        frame([{ index: 0, delta: {}, finish_reason: answer.tool ? "tool_calls" : answer.finish ?? "stop" }]) +
        frame([], { prompt_tokens: answer.tokens ?? 400, completion_tokens: 5, total_tokens: (answer.tokens ?? 400) + 5 }) + "data: [DONE]\n\n");
    } catch (e) { res.destroy(e); }
  });
  agent.listen(0, "127.0.0.1");
  await once(agent, "listening");
  t.after(() => new Promise(resolve => { agent.close(resolve); agent.closeAllConnections(); }));
  const key = "fixture-private-token-not-a-real-key";
  await Promise.all([
    writeFile(join(cfg.agentDir, "models.json"), JSON.stringify({ providers: { "hush-fixture": {
      api: "openai-completions", baseUrl: `http://127.0.0.1:${agent.address().port}/v1`, apiKey: key,
      headers: { "x-fixture-token": key }, models: ["model-a", "model-b"].map((id, i) => ({
        id, name: id, reasoning: i === 0, input: ["text"], contextWindow: 22000, maxTokens: 200,
      })),
    } } })),
    writeFile(join(cfg.agentDir, "settings.json"), JSON.stringify({ defaultProvider: "hush-fixture", defaultModel: "model-a",
      retry: { enabled: true, maxRetries: 1, baseDelayMs: 10, provider: { maxRetries: 0 } },
      compaction: { enabled: false, reserveTokens: 21750, keepRecentTokens: 20 }, ...settings })),
  ]);
  return { requests, key };
}

test("piEvents: production generation retry, exact compaction result and summary projection", { timeout: 25000 }, async t => {
  const cfg = { ...await setup(t), faux: false };
  const { requests, key } = await loopback(t, cfg, {}, (body, n) => {
    if (n === 1 || n === 5) return { status: 529, message: "529 overloaded fixture-private-token-not-a-real-key" };
    return { text: n === 6 ? "verified-tape-summary" : "result for " + n };
  });
  const app = await open(t, cfg);
  const p = await piPeer(t, cfg, { models: ["hush-fixture/model-a", "hush-fixture/model-b"] });
  const models = data(await p.command("get_available_models")).models;
  assert.ok(models.some(m => m.id === "model-a"));
  assert.ok(!JSON.stringify(models).includes(key));
  assert.ok(!models.some(m => Object.hasOwn(m, "headers")));
  const trace = await prompt(p, "old content ".repeat(40));
  ensureOrder(trace, "agent_start", "turn_end", "agent_end", "auto_retry_start", "agent_start", "auto_retry_end", "agent_end", "agent_settled");
  assert.equal(trace.find(r => r.type === "auto_retry_start").attempt, 1);
  assert.equal(trace.find(r => r.type === "auto_retry_end").success, true);
  assert.ok(!JSON.stringify(trace).includes(key));
  await prompt(p, "discarded details ".repeat(40));
  await prompt(p, "kept recent content ".repeat(40));
  assert.equal(requests.length, 4);
  const start = p.since();
  const compact = data(await p.command("compact", { customInstructions: "Summarize the test turns" }));
  assert.equal(compact.summary, "verified-tape-summary");
  assert.ok(compact.tokensBefore > 0 && compact.estimatedTokensAfter > 0);
  ensureOrder(p.records(start), "compaction_start", "summarization_retry_scheduled", "summarization_retry_attempt_start", "summarization_retry_finished", "compaction_end");
  assert.deepEqual(p.records(start).find(r => r.type === "compaction_end").result, compact);
  assert.ok(!JSON.stringify(p.records(start)).includes(key));
  const projected = data(await p.command("get_messages")).messages;
  const summary = projected.find(m => m.role === "compactionSummary");
  assert.equal(summary.summary, compact.summary);
  assert.equal(summary.tokensBefore, compact.tokensBefore);
  assert.ok(!JSON.stringify(projected).includes("old content"));
  const initial = p.ready;
  p.wire.close();
  await app.close();
  await open(t, cfg);
  const q = await piPeer(t, cfg, { threadId: initial.threadId });
  assert.deepEqual(data(await q.command("get_messages")).messages.find(m => m.role === "compactionSummary"), summary);
  data(await q.command("set_thinking_level", { level: "low" }));
  assert.ok(data(await q.command("get_available_thinking_levels")).levels.includes("low"));
  const switched = data(await q.command("cycle_model"));
  assert.equal(switched.model.id, "model-b");
  assert.equal(switched.thinkingLevel, "off");
  assert.equal(switched.isScoped, false);
  assert.ok(q.records().some(r => r.type === "thinking_level_changed" && r.level === "low"));
  assert.ok(q.records().some(r => r.type === "thinking_level_changed" && r.level === "off"));
  assert.equal(data(await q.command("cycle_thinking_level")), null);
  assert.equal(data(await q.command("set_model", { provider: "hush-fixture", modelId: "model-a" })).id, "model-a");
  assert.equal((await q.command("set_model", { provider: "hush-fixture", modelId: "missing" })).success, false);
  assert.notEqual(data(await q.command("cycle_thinking_level")).level, "off");
  assert.equal(requests.length, 6);
});

for (const length of [false, true]) test(`piEvents: threshold fences and verified ${length ? "length" : "overflow"} continuation`, { timeout: 20000 }, async t => {
  const cfg = { ...await setup(t), faux: false };
  const { requests } = await loopback(t, cfg, {}, (body, n) => {
    if (n === 7) return length ? { text: "truncated output not reused", tokens: 30, finish: "length" }
      : { status: 400, message: "maximum context length exceeded" };
    if (n === 3 || n === 6 || n === 8) return { tokens: 10, text: `verified summary ${n}` };
    return { tokens: n === 4 || n === 9 ? 5 : 400, text: "answer " + n };
  });
  const app = await open(t, cfg);
  const p = await piPeer(t, cfg);
  await prompt(p, "seed A ".repeat(120));
  await prompt(p, "seed B ".repeat(120));
  data(await p.command("set_auto_compaction", { enabled: true }));
  assert.equal(data(await p.command("get_state")).autoCompactionEnabled, true);
  const pre = await prompt(p, "pre ".repeat(120));
  ensureOrder(pre, "compaction_start", "compaction_end", "agent_start", "agent_end", "agent_settled");
  assert.equal(pre.find(r => r.type === "compaction_end").reason, "threshold");
  assert.equal(pre.find(r => r.type === "compaction_end").result.summary, "verified summary 3");
  const post = await prompt(p, "post ".repeat(120));
  ensureOrder(post, "agent_start", "agent_end", "compaction_start", "compaction_end", "agent_settled");
  assert.equal(post.find(r => r.type === "compaction_end").result.summary, "verified summary 6");
  const overflow = await prompt(p, "overflow ".repeat(120));
  ensureOrder(overflow, "agent_start", "agent_end", "compaction_start", "compaction_end", "agent_start", "agent_end", "agent_settled");
  const recovered = overflow.find(r => r.type === "compaction_end");
  assert.equal(recovered.reason, "overflow");
  assert.equal(recovered.willRetry, true);
  assert.equal(recovered.result.summary, "verified summary 8");
  assert.equal(overflow.filter(r => r.type === "agent_settled").length, 1);
  assert.equal(data(await p.command("get_last_assistant_text")).text, "answer 9");
  assert.equal(requests.length, 9);
  assert.doesNotMatch(JSON.stringify(requests[8]), /truncated output not reused/);
  const conv = await app.harness.conversation(Number(p.ready.threadId), ctx);
  assert.equal((await conv.context(ctx)).entries.filter(e => e.model?.some(m => m.role === "user" && text(m).startsWith("overflow"))).length, 1);
});

test("piEvents: abort can reach held pre-prompt compaction without admitting the prompt", { timeout: 20000 }, async t => {
  const cfg = { ...await setup(t), faux: false };
  const blocked = Promise.withResolvers(), release = Promise.withResolvers();
  t.after(() => release.resolve());
  await loopback(t, cfg, {}, async (_body, n) => {
    if (n === 3) { blocked.resolve(); await release.promise; }
    return { text: "answer " + n };
  });
  await open(t, cfg);
  const p = await piPeer(t, cfg);
  await prompt(p, "seed one ".repeat(120));
  await prompt(p, "seed two ".repeat(120));
  data(await p.command("set_auto_compaction", { enabled: true }));
  const start = p.since();
  const held = p.command("prompt", { message: "not admitted" });
  void held.catch(() => {});
  await blocked.promise;
  assert.equal(data(await p.command("get_state")).isCompacting, true);
  assert.match((await p.command("prompt", { message: "also not admitted" })).error, /busy/);
  data(await p.command("abort"));
  assert.match((await held).error, /cancelled before admission/);
  release.resolve();
  const trace = p.records(start);
  ensureOrder(trace, "compaction_start", "compaction_end");
  assert.equal(trace.find(r => r.type === "compaction_end").aborted, true);
  assert.ok(!trace.some(r => r.type === "agent_start" || r.type === "agent_settled"));
  assert.ok(!JSON.stringify(data(await p.command("get_messages"))).includes("not admitted"));
});

test("piEvents: exhausted model and manual compaction retries have distinct terminal evidence", { timeout: 20000 }, async t => {
  const cfg = { ...await setup(t), faux: false };
  const { requests } = await loopback(t, cfg, {}, (_body, n) => n <= 2 || n >= 6 ? { status: 529 } : { text: `seed answer ${n}` });
  await open(t, cfg);
  const p = await piPeer(t, cfg);
  const failed = await prompt(p, "model retry exhaustion");
  ensureOrder(failed, "agent_end", "auto_retry_start", "agent_start", "agent_end", "auto_retry_end", "agent_settled");
  assert.deepEqual(failed.filter(r => r.type === "agent_end").map(r => r.willRetry), [true, false]);
  assert.equal(failed.find(r => r.type === "auto_retry_end").success, false);
  assert.equal(failed.filter(r => r.type === "agent_settled").length, 1);
  for (const label of ["seed A ", "seed B ", "seed C "]) await prompt(p, label.repeat(100));
  const before = p.since();
  assert.match((await p.command("compact")).error, /Compaction failed/);
  const compact = p.records(before);
  ensureOrder(compact, "compaction_start", "summarization_retry_scheduled", "summarization_retry_attempt_start",
    "summarization_retry_finished", "compaction_end");
  assert.equal(compact.find(r => r.type === "compaction_end").result, undefined);
  assert.equal(compact.some(r => r.type === "agent_settled"), false, "manual compaction is not an agent run");
  assert.equal(data(await p.command("get_state")).isCompacting, false);
  assert.equal(requests.length, 7);
});

test("piEvents: a tool successor sees the exact between-turns compaction before its request", { timeout: 20000 }, async t => {
  const cfg = { ...await setup(t), faux: false };
  await writeFile(join(cfg.cwd, "read-fixture.txt"), "one harmless test line\n");
  const { requests } = await loopback(t, cfg, {}, (_body, n) => {
    if (n === 3) return { tokens: 500, tool: { name: "read", arguments: '{"path":"read-fixture.txt"}' } };
    return { tokens: 20, text: n === 4 ? "verified before successor" : `answer ${n}` };
  });
  await open(t, cfg);
  const p = await piPeer(t, cfg);
  await prompt(p, "seed first ".repeat(120));
  await prompt(p, "seed second ".repeat(120));
  data(await p.command("set_auto_compaction", { enabled: true }));
  const trace = await prompt(p, "read a local fixture");
  ensureOrder(trace, "agent_start", "turn_start", "tool_execution_start", "tool_execution_end", "turn_end",
    "compaction_start", "compaction_end", "turn_start", "turn_end", "agent_end", "agent_settled");
  assert.equal(trace.find(r => r.type === "compaction_end").result.summary, "verified before successor");
  assert.match(JSON.stringify(requests[4].messages), /verified before successor/);
  assert.equal(requests.length, 5);
});
