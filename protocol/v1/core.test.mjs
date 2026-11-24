import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { fauxAssistantMessage, fauxText, fauxThinking, fauxToolCall } from "@earendil-works/pi-ai";
import { AgentDoc, configure, ConversationBusy, MemoryStorage } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { context, historyPage, id, observeNative, openNative, projectState, projectThread } from "./native-probe.mjs";
import { assertFrame, readFrames, Replica } from "./replay.mjs";

const golden = (name) => readFrames(new URL(`./fixtures/${name}.jsonl`, import.meta.url));
const req = (id, method, params) => ({ type: "request", id, method, params });
const res = (id, method, result) => ({ type: "response", id, method, ok: true, result });
const wire = (frame) => JSON.parse(JSON.stringify(frame));
const throughWire = (client, frame) => client.accept(wire(frame));

function attach(epoch, snapshot, window) {
  const client = new Replica();
  client.accept(req("hello", "hello", { versions: [1] }));
  client.accept(res("hello", "hello", { version: 1, epoch, capabilities: {} }));
  client.accept(req("sub", "subscribe", { threadId: snapshot.state.thread.id, window }));
  throughWire(client, res("sub", "subscribe", snapshot));
  return client;
}

const replay = (client, frames) => frames.map((frame) => client.accept(frame));

test("golden: negotiation and all Chord ops, gap, replay, overflow and failed source", () => {
  const rep = new Replica();
  const frames = golden("replication");
  const effects = [];
  const snapshotBefore = structuredClone(frames.find((f) => f.method === "subscribe" && f.ok).result.state);
  for (const f of frames) {
    const effect = rep.accept(f);
    if (f.type === "patch" || f.type === "state_end") effects.push(effect);
    if (f.type === "patch" && f.subscriptionId === "state-a" && f.nextRev === 3) {
      assert.deepEqual(rep.active.state.tools.map((slot) => slot.callId), ["tool-b", "tool-a"]);
      assert.equal(rep.active.state.tools[1].output, "BC!");
      assert.equal(rep.active.state.streaming.blocks[0].text, "start more");
    }
    if (f.type === "patch" && f.subscriptionId === "state-a" && f.nextRev === 4) {
      assert.equal(rep.active.state.streaming, undefined);
      assert.deepEqual(rep.active.state.tools.map((slot) => slot.callId), ["tool-a"]);
    }
    if (f.type === "patch" && f.subscriptionId === "state-b" && rep.active.stale) {
      assert.equal(rep.active.state.agent.cwd, "/active");
    }
  }
  assert.deepEqual(effects, ["applied", "applied", "applied", "applied", "stale", "ignored", "applied",
    "stale", "stale", "stale", "stale", "stale"]);
  assert.deepEqual(new Set(frames.filter((f) => f.ops).flatMap((f) => f.ops.map(([code]) => code))), new Set("rsdatpm"));
  assert.deepEqual(frames.find((f) => f.method === "subscribe" && f.ok).result.state, snapshotBefore);
  assert.deepEqual(rep.results.filter((f) => !f.ok).slice(0, 4).map((f) => f.error.code),
    ["not_negotiated", "unsupported_version", "already_negotiated", "unsupported_capability"]);
  assert.ok(rep.active.stale);
  assert.equal(rep.pending.size, 0);
});

test("golden: mid-tool restart, independent history gap, fork and invalid anchors", () => {
  const rep = new Replica();
  replay(rep, golden("restart-before"));
  assert.equal(rep.active.state.tools[0].status, "running");
  rep.disconnect();
  const after = golden("restart-after");
  for (const f of after) {
    const effect = rep.accept(f);
    if (f.type === "patch" && f.epoch === "old-run") assert.equal(effect, "ignored");
    if (f.type === "response" && f.id === "gap3") {
      assert.equal(rep.active.rev, 1, "history cannot advance replica revision");
      const interrupted = rep.ordered("1").find((entry) => entry.kind === "pi.tool-result").messages[0];
      assert.equal(interrupted.callId, "tool-a");
      assert.equal(interrupted.isError, true);
    }
  }
  assert.deepEqual(rep.ordered("1").map((e) => e.id), ["2", "3", "6", "9", "12", "16", "18", "20", "23"]);
  assert.deepEqual(rep.ordered("24").map((e) => e.id), ["3", "6", "9", "12"]);
  assert.equal(rep.ordered("24")[0].threadId, "1", "fork preserves entry origin");
  assert.equal(rep.ordered("1")[1].messages[0].blocks[0].text, "First answer\u2028more");
  assert.equal(rep.active.state.thread.id, "24");
  assert.equal(rep.results.filter((f) => !f.ok && f.error.code === "invalid_cursor").length, 2);
});

test("golden: same-key retry and settlement are independent of wire correlation", () => {
  const rep = new Replica();
  replay(rep, golden("submit-lost"));
  rep.disconnect();
  assert.deepEqual(rep.lost.map(({ recovery }) => recovery), ["keyed", "indeterminate"]);
  const frames = golden("submit-retry");
  replay(rep, frames);
  const retry = frames.find((f) => f.id === "retry" && f.type === "request");
  assert.deepEqual(retry.params, rep.lost[0].request.params);
  const summaries = rep.results.filter((f) => ["key", "retry", "peek", "wait"].includes(f.id));
  assert.deepEqual(summaries.map((f) => f.result.submissionId), ["7", "7", "7", "7"]);
  assert.deepEqual(summaries.map((f) => f.result.status), ["queued", undefined, "placed", "done"]);
  assert.equal(rep.results.find((f) => f.id === "cont-wait").result.reason, "nothing_to_answer");
  assert.equal(rep.pending.size, 0);
});

test("schema/replay rejects invalid ops, malformed responses and unadvertised successful extensions", () => {
  const sub = golden("replication").find((f) => f.method === "subscribe" && f.ok).result;
  const fixtures = new Replica();
  fixtures.accept(req("h", "hello", { versions: [1], capabilities: { piEvents: [1] } }));
  assert.throws(() => fixtures.accept(res("h", "hello", { version: 1, epoch: "boot", capabilities: { piEvents: 1 } })), /negotiate extensions/);
  assert.throws(() => assertFrame(res("events", "ext.piEvents.subscribe", {})), /oneOf|must be equal/);
  assert.throws(() => assertFrame(req("s", "submit", { threadId: "1", text: "x", whenBusy: "reject" })), /requestId/);
  assert.throws(() => assertFrame(req("r", "thread.new", { cwd: "\ud800" })), /surrogate/);
  const patch = (ops) => ({ type: "patch", subscriptionId: sub.subscriptionId, epoch: sub.epoch, baseRev: 0, nextRev: 1, ops });
  for (const ops of [[['#', 0, ['agent']]], [['s', 1, true]], [['s', ['__proto__'], true]]]) {
    assert.throws(() => assertFrame(patch(ops)), /oneOf|must/);
  }
  const client = attach(sub.epoch, sub);
  assert.equal(client.accept(patch([["s", ["agent","cwd"], "/uncommitted"], ["a", ["tools"], "invalid"]])), "stale");
  assert.equal(client.active.state.agent.cwd, undefined, "a rejected batch publishes no partial change");
  assert.throws(() => client.accept(res("no-such-id", "thread.abort", {})), /matching request/);
});

test("native: projected view converges during tools, steer and eviction", { timeout: 10000 }, async (t) => {
  const entered = Promise.withResolvers(), release = Promise.withResolvers();
  const setup = await openNative({ beforeToolResult: () => { entered.resolve(); return release.promise; } });
  const { h, conv, faux } = setup;
  t.after(async () => { release.resolve(); await setup.close(); });
  const window = { min: 2, max: 3 };
  const main = await observeNative(h, conv.id, "live", "feed", { window });
  const slow = await observeNative(h, conv.id, "live", "timeout", { window, pendingLimit: 1 });
  const client = attach("live", main.snapshot, window);
  const lagging = attach("live", slow.snapshot, window);
  faux.setResponses([
    fauxAssistantMessage([fauxThinking("seed reasoning"), fauxText("tool work"),
      fauxToolCall("fixture_echo", {}, { id: "probe-call" })], { stopReason: "toolUse" }),
    fauxAssistantMessage("resolved with steering"),
  ]);
  const draft = { type: "input", requestId: "probe-prompt", content: "native chat", whenBusy: "reject" };
  const first = await conv.submit(draft, context);
  try {
    await entered.promise;
    assert.equal((await first.status(context)).status, "placed");
    await assert.rejects(conv.submit({ ...draft, requestId: "busy-rejected" }, context), ConversationBusy);
    const steer = await conv.submit({ type: "input", requestId: "probe-steer", content: "use this direction", whenBusy: "steer" }, context);
    assert.equal((await steer.status(context)).status, "queued");
    for (const frame of main.flush()) assert.equal(throughWire(client, frame), "applied");
    assert.equal(client.active.state.tools[0].callId, "probe-call");
    assert.equal(client.active.state.tools[0].status, "running");
    assert.equal(client.active.state.queue[0].submissionId, id(steer.id));
    const bad = slow.flush();
    assert.equal(bad.length, 1);
    assert.equal(bad[0].reason, "overflow");
    assert.equal(throughWire(lagging, bad[0]), "stale");
    release.resolve();
    assert.equal((await first.wait(context)).status, "done");
    assert.equal((await steer.wait(context)).status, "done");
    await conv.waitForIdle(context);
    for (const f of main.flush()) assert.equal(throughWire(client, f), "applied");
    const authoritative = await conv.watch(context, window);
    try {
      const expected = projectState(authoritative.value);
      // Window sizes depend on when the observation attached. Compare its currently visible suffix and all other state.
      assert.deepEqual({ ...client.active.state, window: null }, { ...expected, window: null });
      assert.equal(client.active.state.window.lastEntryId, expected.window.lastEntryId);
      assert.deepEqual(client.active.state.window.entries.slice(-2), expected.window.entries);
    } finally { await authoritative.stop(); }
    assert.equal(client.active.state.run, undefined);
    assert.deepEqual(client.active.state.queue, []);
    assert.ok(client.active.state.usage.totalTokens > 0);
    const last = client.active.state.window.lastEntryId;
    const scan = { kind: "after", afterEntryId: null, throughEntryId: last };
    const forward = [];
    for (let range = scan; range; ) {
      const page = await historyPage(conv, range, 2);
      forward.push(...page.entries);
      range = page.next;
    }
    assert.ok(forward.length > window.max);
    assert.deepEqual(forward, client.ordered(id(conv.id)), "replica merge equals exact append-order history");
    const back = [];
    for (let range = { kind: "before", beforeEntryId: null }; range; ) {
      const page = await historyPage(conv, range, 2);
      back.unshift(...page.entries);
      range = page.next;
    }
    assert.deepEqual(back, forward);
    const forkAt = forward[2].id;
    const fork = await conv.fork(Number(forkAt), { ownership: { kind: "ownerless" } }, context);
    const inherited = await historyPage(fork, { kind: "before", beforeEntryId: null }, 100);
    assert.deepEqual(inherited.entries, forward.slice(0, 3));
    await assert.rejects(historyPage(fork, { kind: "before", beforeEntryId: last }, 2), /invalid_cursor/);
  } finally { release.resolve(); main.detach(); slow.detach(); }
});

test("native: same-key submission survives close and SQLite reopen", { timeout: 10000 }, async (t) => {
  const dir = await mkdtemp(join(tmpdir(), "hush-protocol-receipt-"));
  const open = async () => openNative({ storage: await openNodeSqliteStorage(join(dir, "state.sqlite")) });
  let setup;
  t.after(async () => { await setup?.close(); await rm(dir, { recursive: true, force: true }); });
  setup = await open();
  setup.faux.setResponses([fauxAssistantMessage("only once")]);
  const draft = { type: "input", requestId: "lost-reply", content: "one accepted input", whenBusy: "reject" };
  const submission = await setup.conv.submit(draft, context);
  const original = await submission.wait(context);
  assert.equal(original.status, "done");
  await setup.close();
  setup = await open();
  const byKey = await setup.h.commit((tx) => tx.submissionByRequest(setup.conv.id, draft.requestId), context);
  assert.equal(byKey.id, submission.id);
  const retried = await setup.conv.submit(draft, context);
  assert.equal(retried.id, submission.id);
  assert.deepEqual(await retried.status(context), original);
  const page = await historyPage(setup.conv, { kind: "before", beforeEntryId: null }, 100);
  assert.equal(page.entries.filter((entry) => entry.kind === "pi.user").length, 1);
  assert.equal(setup.faux.state.callCount, 0);
});

test("native: crash mid-tool, resnapshot, and recover without re-executing the unsafe call", { timeout: 10000 }, async (t) => {
  const dir = await mkdtemp(join(tmpdir(), "hush-protocol-restart-"));
  const db = join(dir, "state.sqlite");
  const script = fileURLToPath(new URL("./crash-fixture.mjs", import.meta.url));
  const child = spawn(process.execPath, [script, db], { cwd: dir, env: { HOME: dir }, stdio: ["ignore", "pipe", "pipe"] });
  const ready = Promise.withResolvers();
  let stdout = "", stderr = "";
  child.stdout.setEncoding("utf8").on("data", (text) => {
    stdout += text;
    if (stdout === "tool-entered\n") ready.resolve();
  });
  child.stderr.setEncoding("utf8").on("data", (text) => { stderr += text; });
  child.once("error", ready.reject);
  const closed = new Promise((resolve) => child.once("close", (code, signal) => {
    ready.reject(new Error(`crash fixture stopped before readiness: ${code ?? signal} ${stderr}`));
    resolve({ code, signal });
  }));
  let setup;
  t.after(async () => {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    await closed;
    await setup?.close();
    await rm(dir, { recursive: true, force: true });
  });
  await ready.promise;
  assert.equal(child.kill("SIGKILL"), true);
  assert.deepEqual(await closed, { code: null, signal: "SIGKILL" });
  let executions = 0;
  setup = await openNative({ storage: await openNodeSqliteStorage(db), beforeToolResult() { executions++; } });
  const { h, conv, faux } = setup;
  faux.setResponses([fauxAssistantMessage("recovered after interruption")]);
  const window = { min: 2, max: 3 };
  const observed = await observeNative(h, conv.id, "new-epoch", "reopened", { window });
  const rep = attach("new-epoch", observed.snapshot, window);
  assert.equal(rep.active.state.window.entries.at(-1).messages[0].blocks[0].id, "crash-call");
  const admitted = await h.commit((tx) => tx.submissionByRequest(conv.id, "crash-prompt"), context);
  const settled = await (await h.submission(admitted.id, context)).wait(context);
  assert.equal(settled.status, "done");
  await conv.waitForIdle(context);
  for (const frame of observed.flush()) assert.equal(throughWire(rep, frame), "applied");
  assert.equal(executions, 0, "unsafe intent must not re-execute after reopen");
  const history = await historyPage(conv, { kind: "before", beforeEntryId: null }, 100);
  const interrupted = history.entries.find((e) => e.kind === "pi.tool-result").messages[0];
  assert.equal(interrupted.callId, "crash-call");
  assert.equal(interrupted.isError, true);
  assert.match(interrupted.blocks.map((b) => b.text).join("\n"), /interrupt/i);
  assert.equal(rep.active.state.window.lastEntryId, settled.answer.toString());
  observed.detach();
});

test("native: core mutations have public sources and distinct receipts", { timeout: 10000 }, async (t) => {
  const setup = await openNative({ storage: new MemoryStorage() });
  t.after(setup.close);
  const { h } = setup;
  const conv = await h.createConversation({ ownership: { kind: "ownerless" }, agent: { cwd: "/proof" } }, context);
  const rows = await h.commit((tx) => tx.scanConversations({}, 10), context);
  assert.ok(rows.items.some((row) => projectThread(row).id === id(conv.id)));
  await conv.configure({ compaction: { reserveTokens: 1024, enabled: true } }, context);
  await conv.commit(async (tx) => {
    const draft = await tx.doc(AgentDoc, conv.id);
    await configure(tx, conv.id, { thinkingLevel: "off", compaction: { ...draft.compaction, enabled: false } });
  }, context);
  const current = await h.snapshot(AgentDoc, conv.id, context);
  assert.deepEqual(current.compaction, { reserveTokens: 1024, enabled: false });
  await conv.commit(async (tx) => {
    const draft = await tx.doc(AgentDoc, conv.id);
    const policy = { ...draft.compaction };
    delete policy.enabled;
    await configure(tx, conv.id, { compaction: policy });
  }, context);
  assert.deepEqual((await h.snapshot(AgentDoc, conv.id, context)).compaction, { reserveTokens: 1024 });
  const compaction = await conv.compact(undefined, context);
  assertFrame(res("compact", "thread.compact", { taskId: id(compaction) }));
  await h.waitForTask(compaction, context);
  const input = await conv.submit({ type: "continue" }, context);
  const receipt = await input.wait(context);
  assert.equal(receipt.status, "unanswered");
  await conv.abort(context);
  await conv.reset("handoff", context);
  await conv.waitForIdle(context);
  const history = await historyPage(conv, { kind: "before", beforeEntryId: null }, 10);
  const last = history.entries.at(-1);
  assert.equal(last.kind, "pi.reset");
  assert.equal(last.headEntryId, last.id);
});
