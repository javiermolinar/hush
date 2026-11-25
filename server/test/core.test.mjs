import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { setImmediate } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { diffRevisions } from "@earendil-works/chord/delta";
import { fauxAssistantMessage, fauxToolCall } from "@earendil-works/pi-ai";
import { AgentDoc } from "@earendil-works/pi-durable";
import { createCoreServer } from "../../dist/server/src/index.js";
import { encode } from "../../dist/server/src/wire.js";
import { context, openNative, projectState } from "../../protocol/v1/native-probe.mjs";
import { assertFrame, Replica } from "../../protocol/v1/replay.mjs";
import { dial } from "./client.mjs";

const result = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const code = (frame, expected) => { assert.equal(frame.ok, false); assert.equal(frame.error.code, expected); };
const hello = client => client.send("hello", { versions: [1], capabilities: { piEvents: [1], images: [1] } }).then(result);
const wp = { min: 1, max: 2 };

async function fixture(t, { native, server } = {}) {
  const dir = await mkdtemp(join(tmpdir(), "hsv-"));
  const path = join(dir, "sock");
  const setup = await openNative(native);
  const transport = createCoreServer(setup.h, server);
  await transport.listen(path);
  t.after(async () => { await transport.close(); await setup.close(); await rm(dir, { recursive: true, force: true }); });
  return { ...setup, transport, path, threadId: String(setup.conv.id) };
}
async function client(t, path, rep) {
  const wire = await dial(path, frame => { assertFrame(frame); if (rep) rep.accept(frame); });
  t.after(() => wire.close());
  if (rep) {
    const send = wire.send;
    wire.send = (method, params) => send(method, params, req => rep.accept(req));
  }
  return wire;
}
async function pageAll(wire, threadId, range, limit = 11) {
  const rows = [];
  const descending = range.kind === "before";
  for (;;) {
    const page = result(await wire.send("history", { threadId, range, limit }));
    rows[descending ? "unshift" : "push"](...page.entries);
    if (!page.nextCursor) return rows;
    range = { kind: "cursor", cursor: page.nextCursor };
  }
}

test("negotiation, strict frames, and uninstalled operations never touch the Harness", { timeout: 8000 }, async t => {
  let called = 0;
  const { h, path, threadId } = await fixture(t, { server: {
    capabilities: { piEvents: { subscribe: async () => { called++; return {}; } } },
  } });
  const before = await h.commit(tx => tx.scanConversations({}, 10), context);
  const wire = await client(t, path);
  code(await wire.send("thread.new", {}), "not_negotiated");
  code(await wire.send("hello", { versions: [2, 3] }), "unsupported_version");
  assert.deepEqual((await hello(wire)).capabilities, {});
  code(await wire.send("hello", { versions: [1] }), "already_negotiated");
  const absent = await wire.send("ext.piEvents.subscribe", { threadId });
  code(absent, "unsupported_capability");
  assert.equal(absent.error.capability, "piEvents");
  const images = await wire.send("ext.images.submit", {
    threadId, requestId: "never-admit", text: "", whenBusy: "reject", images: [{ mimeType: "image/png", data: "AA==" }],
  });
  code(images, "unsupported_capability");
  assert.equal(images.error.capability, "images");
  code(await wire.send("nonexistent", {}), "unknown_method");
  code(await wire.send("submit", { threadId, text: "ignored", whenBusy: "reject" }), "invalid_request");
  code(await wire.send("subscribe", { threadId, window: { min: 2, max: 1 } }), "invalid_request");
  code(await wire.send("thread.abort", { threadId: "9999999999999999" }), "invalid_request");
  assert.deepEqual(await h.commit(tx => tx.scanConversations({}, 10), context), before);
  assert.equal((await h.inspect(context)).submissions.length, 0);
  assert.equal(called, 0);

  const bad = [
    '{"type":"request","id":"x","i\\u0064":"y","method":"hello","params":{"versions":[1]}}\n',
    '{"type":"request","id":"x","method":"hello","params":{"versions":[1],"data":"\\ud800"}}\n',
    '{"type":"request","id":"x","method":"hello","params":{"versions":[1e999]}}\n',
    '\ufeff{"type":"request","id":"x","method":"hello","params":{"versions":[1]}}\n',
    Buffer.from([0xff, 0x0a]),
    Buffer.alloc(8 * 1024 * 1024 + 3, 0x20),
  ];
  for (const bytes of bad) {
    const fresh = await client(t, path);
    fresh.socket.write(bytes);
    await fresh.ended.catch(() => {});
    assert.equal(fresh.frames.length, 0);
  }
  const raw = await client(t, path);
  const message = Buffer.from('{"type":"request","id":"x","method":"hello","params":{"versions":[1]}}\r\n');
  raw.socket.write(message.subarray(0, 8));
  raw.socket.write(message.subarray(8));
  result(await raw.wait(frame => frame.id === "x"));
  raw.socket.write(message);
  await raw.ended;
  assert.equal(raw.frames.length, 1);
});

test("runtime stop is negotiated, validated and acknowledged before shutdown", async t => {
  const stopped = Promise.withResolvers();
  let calls = 0;
  const { path } = await fixture(t, { server: { capabilities: { runtime: {
    stop: () => { calls++; stopped.resolve(); },
  } } } });
  const wire = await dial(path);
  t.after(() => wire.close());
  const hello = result(await wire.send("hello", { versions: [1], capabilities: { runtime: [1] } }));
  assert.deepEqual(hello.capabilities, { runtime: 1 });
  code(await wire.send("ext.runtime.stop", { force: true }), "invalid_request");
  code(await wire.send("ext.runtime.reload", {}), "unknown_method");
  assert.equal(calls, 0);
  assert.deepEqual(result(await wire.send("ext.runtime.stop", {})), {});
  await stopped.promise;
  assert.equal(calls, 1);

  const legacy = await dial(path);
  t.after(() => legacy.close());
  result(await legacy.send("hello", { versions: [1] }));
  const absent = await legacy.send("ext.runtime.stop", {});
  code(absent, "unsupported_capability");
  assert.equal(absent.error.capability, "runtime");
  assert.equal(calls, 1);
});

test("ordered admission: waits do not block, keyed retry is one input, scoped receipts, configure and switch", { timeout: 10000 }, async t => {
  const entered = Promise.withResolvers(), release = Promise.withResolvers();
  const { h, path, threadId, faux, conv } = await fixture(t, { native: {
    beforeToolResult: () => { entered.resolve(); return release.promise; },
  } });
  t.after(() => release.resolve());
  faux.setResponses([
    fauxAssistantMessage([fauxToolCall("fixture_echo", {}, { id: "pause" })], { stopReason: "toolUse" }),
    fauxAssistantMessage("answer"),
  ]);
  const rep = new Replica();
  const wire = await client(t, path, rep);
  await hello(wire);
  try {
    const a = result(await wire.send("subscribe", { threadId, window: wp }));
    const draft = { threadId, requestId: "same", text: "only-once", whenBusy: "reject" };
    const { submissionId } = result(await wire.send("submit", draft));
    await entered.promise;
    const terminal = wire.send("submission.wait", { threadId, submissionId });
    const idle = wire.send("thread.wait", { threadId });
    const found = result(await wire.send("submission.get", { threadId, requestId: draft.requestId }));
    assert.equal(found.status, "placed");
    assert.equal(found.submissionId, submissionId);
    const other = await client(t, path);
    await hello(other);
    assert.deepEqual(result(await other.send("submit", draft)), { submissionId });
    const { id } = result(await wire.send("thread.new", { cwd: "/different" }));
    code(await wire.send("submission.get", { threadId: id, submissionId }), "not_found");
    code(await wire.send("submission.wait", { threadId: id, submissionId }), "not_found");
    const b = result(await wire.send("subscribe", { threadId: id, window: wp }));
    assert.notEqual(a.subscriptionId, b.subscriptionId);
    assert.equal(b.state.agent.cwd, "/different");
    const page1 = result(await wire.send("thread.list", { limit: 1 }));
    assert.deepEqual(page1.threads.map(r => r.id), [threadId]);
    const page2 = result(await wire.send("thread.list", { limit: 1, cursor: page1.nextCursor }));
    assert.deepEqual(page2.threads.map(r => r.id), [id]);
    assert.equal(page2.nextCursor, null);

    await conv.configure({ compaction: { reserveTokens: 321 } }, context);
    result(await wire.send("thread.configure", { threadId, change: { thinkingLevel: "low", compactionEnabled: true } }));
    result(await wire.send("thread.configure", { threadId, change: { thinkingLevel: null, compactionEnabled: null } }));
    const doc = await conv.commit(tx => tx.doc(AgentDoc, conv.id).then(d => ({
      compaction: { ...d.compaction }, thinkingLevel: d.thinkingLevel,
    })), context);
    assert.deepEqual(doc, { compaction: { reserveTokens: 321 }, thinkingLevel: undefined });
    release.resolve();
    const done = result(await terminal);
    result(await idle);
    assert.equal(done.status, "done");
    const rows = await pageAll(wire, threadId, { kind: "after", afterEntryId: null, throughEntryId: done.answerEntryId });
    assert.equal(rows.filter(row => row.kind === "pi.user").length, 1);
    const fork = result(await wire.send("thread.fork", { threadId, atEntryId: done.entryId }));
    const prefix = await pageAll(wire, fork.id, { kind: "before", beforeEntryId: null });
    assert.equal(prefix.find(row => row.id === done.entryId).threadId, threadId);
    result(await wire.send("thread.reset", { threadId: fork.id, handoff: "next" }));
    assert.ok((await pageAll(wire, fork.id, { kind: "before", beforeEntryId: null })).some(row => row.kind === "pi.reset"));
    const compact = result(await wire.send("thread.compact", { threadId: fork.id }));
    assert.ok(compact.taskId);
    result(await wire.send("thread.abort", { threadId: fork.id }));
  } finally { release.resolve(); }
});

test("socket replica converges for seeded structural changes; history fills a gap independently", { timeout: 15000 }, async t => {
  const { h, conv, path, threadId } = await fixture(t);
  const rep = new Replica();
  const wire = await client(t, path, rep);
  await hello(wire);
  const initial = result(await wire.send("subscribe", { threadId, window: wp }));
  const append = (n, text) => conv.commit(tx => tx.appendEntry(conv.id, {
    kind: "fixture.entry", model: [{ role: "user", content: text, timestamp: n }],
  }), context);
  let seed = 0x1e331;
  let committed;
  for (let n = 1; n <= 64; n++) {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0;
    const changes = 1 + seed % 5;
    for (let j = 0; j < changes; j++) {
      committed = await append(n, ("ð🤖\u2028" + seed).repeat(j + 1));
      if (seed % 3 === 0) await conv.configure({ cwd: "/fixture/" + seed }, context);
    }
    const after = String(committed.id);
    const rev = rep.active.rev;
    if (rep.active.state.window.lastEntryId !== after || (seed % 3 === 0 && rep.active.state.agent.cwd !== "/fixture/" + seed)) {
      await wire.wait(frame => frame.type === "patch" && frame.nextRev > rev && rep.active.state.window.lastEntryId === after);
    }
    // Initial size can differ from a window advanced by commits. Compare the entire
    // authoritative view by seeding a fresh observation at the replica's first visible entry.
    const seen = rep.active.state.window.entries[0].id;
    const snap = await h.observeConversation(conv.id, { advance() {}, end() {} }, context, { ...wp, from: Number(seen) });
    try { assert.deepEqual(rep.active.state, projectState(snap.value)); }
    finally { snap.detach(); }
    assert.equal(rep.active.stale, false);
  }
  const lastEntryId = String(committed.id);
  const newest = result(await wire.send("subscribe", { threadId, window: wp }));
  assert.notEqual(newest.subscriptionId, initial.subscriptionId);
  const [forward, backward] = await Promise.all([
    pageAll(wire, threadId, { kind: "after", afterEntryId: null, throughEntryId: lastEntryId }),
    pageAll(wire, threadId, { kind: "before", beforeEntryId: null }),
  ]);
  assert.deepEqual(forward, backward);
  assert.ok(forward.length > 100);
  assert.equal(rep.ordered(threadId).length, forward.length);
  assert.equal(rep.active.rev, 0);
  const page = result(await wire.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 1 }));
  const foreign = result(await wire.send("thread.new", {})).id;
  code(await wire.send("history", { threadId: foreign, range: { kind: "cursor", cursor: page.nextCursor }, limit: 1 }), "invalid_cursor");
  code(await wire.send("history", { threadId, range: { kind: "before", beforeEntryId: "9007199254740991" }, limit: 1 }), "invalid_cursor");
  code(await wire.send("thread.list", { limit: 1, cursor: page.nextCursor }), "invalid_cursor");
});

test("socket patch bytes match fresh projection at a full window and across evictions", { timeout: 10_000 }, async t => {
  const { h, conv, path, threadId } = await fixture(t);
  let timestamp = 100;
  const append = count => conv.commit(async tx => {
    for (let i = 0; i < count; i++) {
      await tx.appendEntry(conv.id, { kind: "fixture.entry",
        model: [{ role: "user", timestamp: timestamp++, content: "Identical text, distinct IDs. 😀" }] });
    }
  }, context);
  await append(100);
  const window = { min: 50, max: 100 };
  let newest;
  const reference = await h.observeConversation(conv.id, { advance(update) {
    if (update.kind === "value") newest = update.value;
  }, end() {} }, context, window);
  t.after(() => reference.detach());
  let before = projectState(reference.value);
  const wire = await client(t, path);
  await hello(wire);
  const { epoch, subscriptionId, state } = result(await wire.send("subscribe", { threadId, window }));
  assert.equal(encode(state), encode(before));
  assert.equal(state.window.entries.length, 50); // Durable starts at min
  const changes = [() => append(50), () => conv.configure({ cwd: "/fixture/second" }, context),
    () => append(1), () => append(12), () => append(49), () => append(100)];
  for (const [baseRev, change] of changes.entries()) {
    await change();
    const nextRev = baseRev + 1;
    const actual = await wire.wait(frame => frame.type === "patch" && frame.nextRev === nextRev);
    const next = projectState(newest);
    if (baseRev === 0) assert.equal(next.window.entries.length, 100);
    if (baseRev === 2) assert.equal(next.window.entries.length, 50);
    assert.equal(encode(actual), encode({ type: "patch", epoch, subscriptionId, baseRev, nextRev,
      ops: diffRevisions(before, next) }));
    before = next;
  }
});

test("paused Unix socket overflows without delaying a healthy client or a commit", { timeout: 15000 }, async t => {
  const { path, conv, threadId } = await fixture(t, { server: { pendingObservations: 16 } });
  const slow = await client(t, path);
  await hello(slow);
  const ready = result(await slow.send("subscribe", { threadId, window: { min: 1, max: 1 } }));
  const peers = await Promise.all(Array.from({ length: 25 }, async (_, i) => {
    const wire = await client(t, path);
    const cwd = "/healthy/" + i;
    await hello(wire);
    const thread = result(await wire.send("thread.new", { cwd })).id;
    const ready = result(await wire.send("subscribe", { threadId: thread, window: wp }));
    assert.equal(ready.state.agent.cwd, cwd);
    return { wire, thread, ready };
  }));
  slow.socket.pause();
  let entry;
  for (let n = 0; n < 80; n++) {
    entry = await conv.commit(tx => tx.appendEntry(conv.id, {
      kind: "fixture.slow", model: [{ role: "user", content: "🤖".repeat(16384) + n, timestamp: n }],
    }), context);
    // Let individual patches attempt writes, until writableNeedDrain stays set.
    await setImmediate();
    if (n % 10 === 0) {
      await Promise.all(peers.map(async ({ wire, thread }) => result(await wire.send("thread.configure", {
        threadId: thread, change: { thinkingLevel: n % 20 ? "low" : "off" },
      }))));
    }
  }
  for (const { wire, ready } of peers) {
    const patches = wire.frames.filter(f => f.type === "patch");
    assert.ok(patches.length > 0);
    assert.ok(patches.every(f => f.subscriptionId === ready.subscriptionId && !JSON.stringify(f.ops).includes("fixture.slow")));
    assert.ok(!wire.frames.some(f => f.type === "state_end"));
  }
  const still = result(await peers[0].wire.send("subscribe", { threadId, window: wp }));
  assert.equal(still.state.window.lastEntryId, String(entry.id));
  slow.socket.resume();
  const end = await slow.wait(frame => frame.type === "state_end");
  assert.equal(end.subscriptionId, ready.subscriptionId);
  assert.equal(end.reason, "overflow");
  assert.notEqual(result(await slow.send("subscribe", { threadId, window: wp })).subscriptionId, ready.subscriptionId);
});

test("SIGKILL yields a fresh epoch, a stale cursor and one original admission across actual reopen", { timeout: 18000 }, async t => {
  const dir = await mkdtemp(join(tmpdir(), "hsc-"));
  const sock = join(dir, "sock");
  const db = join(dir, "db");
  const childFile = fileURLToPath(new URL("./crash-child.mjs", import.meta.url));
  const live = new Map();
  t.after(async () => {
    for (const [child, dead] of live) { child.kill("SIGKILL"); await dead; }
    await rm(dir, { recursive: true, force: true });
  });
  async function boot(mode) {
    const child = spawn(process.execPath, [childFile, db, sock, mode], { stdio: ["ignore", "pipe", "pipe"] });
    let err = "";
    child.stderr.on("data", bytes => { err += bytes; });
    const dead = once(child, "exit");
    live.set(child, dead);
    dead.then(() => live.delete(child));
    const lines = createInterface({ input: child.stdout })[Symbol.asyncIterator]();
    assert.equal((await lines.next()).value, "listening", err);
    return { child, dead, lines };
  }
  const first = await boot("crash");
  const wire = await client(t, sock);
  const old = await hello(wire);
  const threadId = result(await wire.send("thread.list", { limit: 1 })).threads[0].id;
  const draft = { threadId, requestId: "one", text: "before crash", whenBusy: "reject" };
  const { submissionId } = result(await wire.send("submit", draft));
  assert.equal((await first.lines.next()).value, "tool-entered");
  const older = result(await wire.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 1 }));
  assert.ok(older.nextCursor);
  first.child.kill("SIGKILL");
  assert.deepEqual(await first.dead, [null, "SIGKILL"]);
  await wire.ended;
  await rm(sock, { force: true });
  const second = await boot("recovered");
  const fresh = await client(t, sock);
  assert.notEqual((await hello(fresh)).epoch, old.epoch);
  code(await fresh.send("history", { threadId, range: { kind: "cursor", cursor: older.nextCursor }, limit: 1 }), "invalid_cursor");
  assert.deepEqual(result(await fresh.send("submit", draft)), { submissionId });
  result(await fresh.send("submission.wait", { threadId, submissionId }));
  const rows = await pageAll(fresh, threadId, { kind: "before", beforeEntryId: null });
  assert.equal(rows.filter(row => row.kind === "pi.user").length, 1);
  const tool = rows.filter(row => row.kind === "pi.tool-result").flatMap(row => row.messages);
  assert.equal(tool.length, 1);
  assert.equal(tool[0].callId, "crash-call");
  assert.equal(tool[0].isError, true);
  fresh.close();
  second.child.kill("SIGTERM");
  assert.deepEqual(await second.dead, [0, null]);
});

test("settings capability validates changes and detached replies", async t => {
  let current = { model: { provider: "fixture", modelId: "one" }, thinkingLevel: "high", reasoning: "folded" };
  const calls = [];
  const { path } = await fixture(t, { server: { capabilities: { settings: {
    get: async () => current,
    set: async change => { calls.push(change); return current = { ...current, ...change }; },
  } } } });
  const wire = await dial(path);
  t.after(() => wire.close());
  assert.deepEqual(result(await wire.send("hello", { versions: [1], capabilities: { settings: [1] } })).capabilities, { settings: 1 });
  assert.deepEqual(result(await wire.send("ext.settings.get", {})), current);
  const change = { model: { provider: "fixture", modelId: "two" }, thinkingLevel: "low", reasoning: "expanded" };
  assert.deepEqual(result(await wire.send("ext.settings.set", change)), change);
  assert.deepEqual(calls, [change]);
  for (const params of [{}, { reasoning: "live" }, { model: { provider: "fixture" } }, { unknown: true }]) {
    code(await wire.send("ext.settings.set", params), "invalid_request");
  }
  code(await wire.send("ext.settings.get", { unexpected: true }), "invalid_request");
  code(await wire.send("ext.settings.unknown", {}), "unknown_method");
  assert.equal(calls.length, 1);
  const plain = await dial(path);
  t.after(() => plain.close());
  result(await plain.send("hello", { versions: [1] }));
  code(await plain.send("ext.settings.get", {}), "unsupported_capability");
});
