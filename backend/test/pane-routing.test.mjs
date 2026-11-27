import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from "@earendil-works/pi-ai";
import { getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";
import { createRegistry, defineExtension, defineTool, Harness, MemoryStorage } from "@earendil-works/pi-durable";
import { createCoreServer } from "../../dist/server/src/index.js";
import { PaneRoutes } from "../../dist/server/src/pane-control.js";
import { PaneRouting } from "../../dist/backend/src/pane-routing.js";
import { paneToolParameters } from "../../dist/backend/src/pane-contract.js";
import { context } from "../../protocol/v1/native-probe.mjs";
import { dial } from "../../server/test/client.mjs";

const method = op => `ext.paneControl.${op}`;
const ok = f => { assert.equal(f.ok, true, JSON.stringify(f)); return f.result; };
const caps = { left: false, above: false, tabs: false, read: false };
const handle = "hp_ABCDEFGHIJKLMNOPQRSTUVWXYZ";
const launch = { op: "open", placement: "below", dir: "/fixture", directory: { device: "1", inode: "2", birth: "3" },
  launch: { mode: "argv", program: "printf", args: ["not executed"] } };
const paneFrames = peer => peer.frames.filter(frame => frame.type === "pane_call");

async function fixture(t) {
  const dir = await mkdtemp("/tmp/hpr-");
  const path = join(dir, "sock"), routes = new PaneRoutes(), routing = new PaneRouting(routes);
  const peers = [];
  const observations = [], effects = [], held = new Map();
  const pause = text => {
    const hold = { arrived: Promise.withResolvers(), release: Promise.withResolvers() };
    held.set(text, hold);
    return hold;
  };
  const faux = fauxProvider({ api: "fixture-pane-route", tokenSize: { min: 4, max: 4 } });
  const models = createModels();
  models.setProvider(faux.provider);
  const registry = createRegistry();
  const schema = paneToolParameters({ left: true, above: true, tabs: true, read: true });
  let h;
  const ext = defineExtension({ name: "fixture-pane-route", hooks: [routing.gate(() => h)], tools: Object.entries(schema).map(
    ([name, parameters]) => defineTool({ name, parameters, description: `Faux ${name}`, replay: "unsafe", callers: ["model"],
      async execute(_args, api, ctx) {
        assert.equal(name, "pane_open");
        const result = await routing.dispatch(launch, api, ctx);
        const duplicate = await routing.dispatch(launch, api, ctx);
        effects.push([result, duplicate]);
        return { output: [{ type: "text", text: result.outcome }] };
      },
    })) });
  registry.install(ext);
  h = await Harness.open(new MemoryStorage(), { models, registry, settings: {
    extensions: [ext], compaction: { enabled: false }, retry: { enabled: false },
  } }, context);
  const conv = await h.root(context, { agent: { model: { provider: "faux", modelId: "faux-1" }, cwd: "/fixture" } });
  const server = createCoreServer(h, { capabilities: { paneControl: routes, clientEnvironment: 1 } });
  await server.listen(path);
  t.after(async () => {
    for (const hold of held.values()) hold.release.resolve();
    for (const peer of peers) peer.close();
    await server.close();
    await h.close(context);
    await rm(dir, { recursive: true, force: true });
  });
  const respond = async ({ messages }) => {
    faux.appendResponses([respond]);
    const last = messages.findLast(m => m.role !== "system");
    const text = last.role === "user" ? (typeof last.content === "string" ? last.content : last.content.map(b => b.text ?? "").join("")) : `result:${last.content[0]?.text}`;
    const tools = getCurrentTools(messages);
    const declared = tools.map(t => t.name).sort();
    const places = tools.find(t => t.name === "pane_open")?.parameters.properties.placement.enum;
    observations.push({ text, declared, places, prompt: JSON.stringify(messages) });
    const hold = held.get(text);
    if (hold) {
      hold.arrived.resolve();
      await hold.release.promise;
    }
    if (text.includes("!open")) return fauxAssistantMessage(fauxToolCall("pane_open", { placement: "below", launch: launch.launch },
      { id: "test-pane-open" }), { stopReason: "toolUse" });
    return fauxAssistantMessage("fixture settled");
  };
  faux.appendResponses([respond]);
  const threadId = String(conv.id);
  async function peer(offer, supportedCaps) {
    const wire = await dial(path);
    peers.push(wire);
    wire.hello = ok(await wire.send("hello", { versions: [1], capabilities: offer }));
    if (supportedCaps) wire.route = ok(await wire.send(method("bind"), { capabilities: supportedCaps })).route;
    return wire;
  }
  function input(text, whenBusy = "reject") { return { threadId, text, requestId: text, whenBusy }; }
  async function submit(p, draft) {
    return (p.route ? ok(await p.send(method("submit"), { route: p.route, input: draft })) : ok(await p.send("submit", draft))).submissionId;
  }
  async function settle(p, id) { assert.equal(ok(await p.send("submission.wait", { threadId, submissionId: id })).status, "done"); }
  async function queued(p, id) { assert.equal(ok(await p.send("submission.get", { threadId, submissionId: id })).status, "queued"); }
  function seen(text) { const got = observations.find(r => r.text === text); assert.ok(got, text); return got; }
  return { threadId, pause, peer, input, submit, settle, queued, seen, effects, observations };
}

test("pane tools follow the placed input across clients, queueing and mixed steers", { timeout: 12000 }, async t => {
  const f = await fixture(t);
  const a = await f.peer({ paneControl: [1] }, { ...caps, left: true, tabs: true, read: true });
  const b = await f.peer({ paneControl: [1], clientEnvironment: [1] }, { ...caps, above: true });
  const legacy = await f.peer({ paneControl: [3], clientEnvironment: [1] });
  assert.deepEqual(a.hello.capabilities, { paneControl: 1 });
  assert.deepEqual(b.hello.capabilities, { paneControl: 1, clientEnvironment: 1 });
  assert.deepEqual(legacy.hello.capabilities, { clientEnvironment: 1 });
  assert.equal((await legacy.send(method("bind"), { capabilities: caps })).error.code, "unsupported_capability");
  await f.settle(legacy, await f.submit(legacy, f.input("legacy")));
  assert.deepEqual(f.seen("legacy").declared, []);
  const draft = f.input("Open a pane for a-!open");
  const wait = f.pause(draft.text);
  const first = await f.submit(a, draft);
  await wait.arrived.promise;
  assert.deepEqual(f.seen(draft.text).places, ["right", "below", "left", "tab"]);
  assert.ok(f.seen(draft.text).declared.includes("pane_read"));
  assert.match(f.seen(draft.text).prompt, /Plain language is sufficient/);
  assert.match(f.seen(draft.text).prompt, /Placements: right, below, left, tab/);
  const second = await f.submit(b, f.input("queued from b", "followUp"));
  await f.queued(b, second);
  assert.equal((await b.send(method("submit"), { route: a.route, input: f.input("spoofed from b") })).error.code, "invalid_request");
  assert.equal(ok(await b.send(method("submit"), { route: a.route, input: draft })).submissionId, first);
  wait.release.resolve();
  const call = await a.wait(frame => frame.type === "pane_call");
  assert.deepEqual(call.request, launch);
  assert.equal(paneFrames(b).length, 0);
  ok(await b.send(method("result"), { callId: call.id, result: { outcome: "rejected" } }));
  assert.equal(f.effects.length, 0, "a different peer resolved the pending call");
  ok(await a.send(method("result"), { callId: call.id, result: { outcome: "confirmed", pane: handle } }));
  await f.settle(a, first);
  await f.settle(b, second);
  assert.deepEqual(f.effects, [[{ outcome: "confirmed", pane: handle }, { outcome: "unknown" }]]);
  assert.equal(paneFrames(a).length, 1);
  assert.deepEqual(f.seen("queued from b").places, ["right", "below", "above"]);
  assert.ok(!f.seen("queued from b").declared.includes("pane_read"));

  const steer = f.pause("/pane start mixed !open");
  const third = await f.submit(a, f.input("/pane start mixed !open"));
  await steer.arrived.promise;
  const mixed = await f.submit(b, f.input("mixed steer", "steer"));
  await f.queued(b, mixed);
  steer.release.resolve();
  const thirdCall = await a.wait(frame => frame.type === "pane_call" && frame.id !== call.id);
  ok(await a.send(method("result"), { callId: thirdCall.id, result: { outcome: "confirmed", pane: handle } }));
  await f.settle(a, third);
  await f.settle(b, mixed);
  assert.deepEqual(f.seen("mixed steer").declared, []);
  assert.equal(paneFrames(b).length, 0);
  const history = ok(await legacy.send("history", { threadId: f.threadId, range: { kind: "before", beforeEntryId: null }, limit: 100 }));
  for (const data of [JSON.stringify(history), ...f.observations.map(r => r.prompt)]) {
    for (const secret of [a.route, b.route, call.id, thirdCall.id, "paneAdmission", "hush.clientInput"]) assert.ok(!data.includes(secret), secret);
  }
});

test("disconnect before and after dispatch cannot transfer or replay the route", { timeout: 12000 }, async t => {
  const f = await fixture(t);
  const a = await f.peer({ paneControl: [1] }, caps);
  const b = await f.peer({ paneControl: [1] }, caps);
  const legacy = await f.peer({});
  const wait = f.pause("block queue");
  const first = await f.submit(legacy, f.input("block queue"));
  await wait.arrived.promise;
  const queued = await f.submit(a, f.input("stale queued input", "followUp"));
  await f.queued(b, queued);
  a.socket.end(); // observe the server's EOF before releasing the queue
  await a.ended;
  wait.release.resolve();
  await f.settle(legacy, first);
  await f.settle(legacy, queued);
  assert.deepEqual(f.seen("stale queued input").declared, []);
  const draft = f.input("/pane lost reply !open");
  const inFlight = await f.submit(b, draft);
  const dispatch = await b.wait(frame => frame.type === "pane_call");
  b.close();
  await b.ended;
  const replacement = await f.peer({ paneControl: [1] }, caps);
  assert.notEqual(replacement.route, b.route);
  assert.equal(ok(await replacement.send(method("submit"), { route: b.route, input: draft })).submissionId, inFlight);
  await f.settle(replacement, inFlight);
  assert.equal(paneFrames(b).length, 1);
  assert.equal(paneFrames(replacement).length, 0);
  assert.deepEqual(f.effects, [[{ outcome: "unknown" }, { outcome: "rejected" }]]);
  assert.deepEqual(f.seen("result:unknown").declared, []);
  ok(await replacement.send(method("result"), { callId: dispatch.id, result: { outcome: "confirmed", pane: handle } }));
  assert.equal(paneFrames(replacement).length, 0);
  ok(await replacement.send(method("unbind"), {}));
  await f.settle(replacement, await f.submit(replacement, f.input("chat after unbind")));
  assert.deepEqual(f.seen("chat after unbind").declared, []);
});

test("v2 can downgrade a revoked native route once without inheriting the label", { timeout: 12000 }, async t => {
  const f = await fixture(t);
  const native = { ...caps, threadTabs: true, views: true };
  const tabOnly = { ...caps, threadTabs: true, noPanes: true };
  const a = await f.peer({ paneControl: [2] }, native);
  const original = a.route;
  const draft = f.input("original receipt");
  const id = await f.submit(a, draft);
  await f.settle(a, id);
  assert.equal((await a.send(method("bind"), { capabilities: tabOnly })).error.code, "invalid_request");
  ok(await a.send(method("unbind"), {}));
  assert.equal((await a.send(method("bind"), { capabilities: native })).error.code, "invalid_request");
  a.route = ok(await a.send(method("bind"), { capabilities: tabOnly })).route;
  assert.notEqual(a.route, original);
  assert.equal(ok(await a.send(method("submit"), { route: original, input: draft })).submissionId, id);
  assert.equal((await a.send(method("submit"), { route: original, input: f.input("old label cannot admit") })).error.code, "invalid_request");
  assert.equal((await a.send(method("submit"), { route: a.route, input: f.input("/pane native unavailable") })).error.code, "invalid_request");
  await f.settle(a, await f.submit(a, f.input("ordinary tab-only input")));
  assert.deepEqual(f.seen("ordinary tab-only input").declared, []);
  ok(await a.send(method("unbind"), {}));
  assert.equal((await a.send(method("bind"), { capabilities: tabOnly })).error.code, "invalid_request");
});

test("expired admission cannot dispatch and late or malformed confirmation never guesses", async () => {
  const registry = new PaneRoutes(), frames = [];
  const route = registry.bind(caps, f => { frames.push(f); return true; }, new AbortController().signal);
  const expired = { key: route.key, until: Date.now() - 1 };
  assert.equal(registry.get(expired), undefined);
  assert.deepEqual(await route.dispatch("expired", expired.until, launch), { outcome: "rejected" });
  assert.equal(frames.length, 0);
  const call = route.dispatch("one", route.admission().until, launch, undefined, "1");
  route.reply(frames[0].id, { outcome: "confirmed", pane: "native-pane-id" });
  assert.deepEqual(await call, { outcome: "unknown" });
  assert.deepEqual(await route.dispatch("one", route.admission().until, launch, undefined, "1"), { outcome: "unknown" });
  assert.deepEqual(await route.dispatch("two", route.admission().until, launch, undefined, "1"), { outcome: "rejected" });
  assert.equal(frames.length, 1);
  route.close();
});

test("aborting a routed thread view cancels local waiting without confirming or replaying", async () => {
  const frames = [], registry = new PaneRoutes();
  const route = registry.bind({ ...caps, noPanes: true, threadTabs: true }, f => { frames.push(f); return true; }, new AbortController().signal);
  const run = new AbortController();
  const request = { op: "thread_open", threadId: "7", where: "tab" };
  const result = route.dispatch("view", route.admission().until, request, run.signal, "1");
  assert.equal(frames[0].type, "pane_call");
  assert.deepEqual(frames[0].request, request);
  run.abort();
  assert.deepEqual(frames[1], { type: "pane_cancel", id: frames[0].id });
  assert.deepEqual(await result, { outcome: "unknown" });
  assert.equal(route.live, false);
  route.reply(frames[0].id, { outcome: "confirmed", threadId: "7", where: "tab" });
  assert.deepEqual(await route.dispatch("other", route.admission().until, request, undefined, "2"), { outcome: "rejected" });
  assert.equal(frames.length, 2);
});
