import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { copyFile, lstat, mkdir, mkdtemp, readFile, readdir, rename, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { createServer } from "node:http";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { setTimeout } from "node:timers/promises";
import { fileURLToPath, pathToFileURL } from "node:url";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { getSystemMessageText } from "@earendil-works/pi-ai";
import { AgentDoc, LiveDoc } from "@earendil-works/pi-durable";
import { assertFrame } from "../../protocol/v1/replay.mjs";
import { dial } from "../../server/test/client.mjs";
import { loadModels, startBackend } from "../../dist/hush-backend.mjs";
import { displayTitle, topicTitle } from "../../dist/backend/src/thread-presentation.js";

import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";

const bundle = new URL("../../dist/hush-backend.mjs", import.meta.url);
const grant = ({ agentDir, cwd }) => new WorkspaceTrust(agentDir).change(cwd, "grant", AbortSignal.timeout(5000));
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const code = (f, expected) => { assert.equal(f.ok, false); assert.equal(f.error.code, expected); };
const fixtureToken = "this-test-api-key-is-not-a-credential";
const json = (path, value) => writeFile(path, JSON.stringify(value), { mode: 0o600 });
const doc = (path, value) => writeFile(path, value, { mode: 0o600 });

async function layout(t, { ephemeral = false, idleMs = 3_000 } = {}) {
  const dir = await mkdtemp("/tmp/hsb-");
  t.after(() => rm(dir, { recursive: true, force: true }));
  const cwd = join(dir, "project"), agentDir = join(dir, "agent"), dataDir = ephemeral ? undefined : join(dir, "data");
  await Promise.all([mkdir(cwd, { mode: 0o700 }), mkdir(agentDir, { mode: 0o700 })]);
  const privateDir = ephemeral ? join(dir, "private") : dataDir;
  await mkdir(privateDir, { mode: 0o700 });
  const socket = join(privateDir, "hush.sock");
  return { dir, config: { cwd, agentDir, dataDir, socket, idleMs, ephemeral, faux: true }, privateDir };
}

async function peer(t, path) {
  const connection = await dial(path, assertFrame);
  t.after(() => connection.close());
  const hello = ok(await connection.send("hello", { versions: [1], capabilities: { piEvents: [1], resources: [1] } }));
  assert.deepEqual(hello.capabilities, {});
  return { connection, hello };
}

async function submit(connection, threadId, text, requestId) {
  return ok(await connection.send("submit", { threadId, text, requestId, whenBusy: "reject" }));
}
async function wait(connection, threadId, submissionId) {
  return ok(await connection.send("submission.wait", { threadId, submissionId }));
}

async function boot(t, dir, config, launched = {}) {
  const entry = join(dir, "only-backend.mjs");
  await copyFile(bundle, entry);
  await copyFile(new URL(bundle.href + ".wasm-pack"), entry + ".wasm-pack");
  if (!config.ephemeral && config.faux) {
    const agentDir = join(dir, ".hush");
    await mkdir(agentDir, { mode: 0o700, recursive: true });
    await grant({ agentDir, cwd: config.cwd });
  }
  const storage = config.ephemeral ? ["--no-session", "--socket", config.socket]
    : config.dataDir ? ["--data-dir", config.dataDir] : [];
  const args = [entry, "--cwd", config.cwd, ...(config.faux ? ["--faux"] : []), "--idle-ms", String(config.idleMs), ...storage];
  const child = spawn(process.execPath, args, {
    cwd: dir, env: { HOME: dir, PATH: "/usr/bin:/bin", NODE_NO_WARNINGS: "1", LANG: "C", ...launched.env },
    stdio: ["ignore", "pipe", "pipe"],
  });
  const exit = once(child, "exit");
  let dead = false;
  exit.then(() => { dead = true; });
  let stderr = "";
  child.stderr.on("data", chunk => { stderr += chunk; });
  const lines = createInterface({ input: child.stdout })[Symbol.asyncIterator]();
  t.after(async () => {
    if (!dead) child.kill("SIGKILL");
    await exit;
  });
  const row = await lines.next();
  if (launched.failure) {
    assert.equal(row.done, true);
    assert.deepEqual(await exit, [1, null]);
    assert.match(stderr, launched.failure);
  } else assert.deepEqual(JSON.parse(row.value ?? "null"), { ready: true, socket: config.socket }, stderr);
  return { child, exit, get stderr() { return stderr; }, get dead() { return dead; } };
}

test("runtime stop closes the host and permits a replacement over retained history", async t => {
  const { config } = await layout(t, { idleMs: 10_000 });
  let app = await startBackend({ ...config });
  t.after(() => app.close());
  let wire = await dial(config.socket);
  t.after(() => wire.close());
  const first = ok(await wire.send("hello", { versions: [1], capabilities: { hushProfile: [1], storeIdentity: [1], runtime: [1] } }));
  assert.deepEqual(first.capabilities, { hushProfile: 1, storeIdentity: 1, runtime: 1 });
  const threadId = ok(await wire.send("thread.new", {})).id;
  assert.deepEqual(ok(await wire.send("ext.runtime.stop", {})), {});
  await app.closed;
  await assert.rejects(dial(config.socket));

  app = await startBackend({ ...config });
  wire = await dial(config.socket);
  t.after(() => wire.close());
  const second = ok(await wire.send("hello", { versions: [1], capabilities: { runtime: [1] } }));
  assert.notEqual(second.epoch, first.epoch);
  const threads = ok(await wire.send("thread.list", { limit: 100 }));
  assert.ok(threads.threads.some(row => row.id === threadId));
});

test("managed format-1 compatibility is opt-in and preserves identity and history", { timeout: 15_000 }, async t => {
  const { dir, config } = await layout(t, { idleMs: 10_000 });
  const agentDir = join(dir, ".hush"), dataDir = join(agentDir, "runtime"), socket = join(dataDir, "hush.sock");
  const options = { ...config, agentDir, dataDir, socket };
  await new WorkspaceTrust(agentDir).change(config.cwd, "grant", AbortSignal.timeout(5000));
  let app = await startBackend(options);
  t.after(() => app.close());
  let connection = await dial(socket);
  t.after(() => connection.close());
  const hello = () => connection.send("hello", { versions: [1], capabilities: { storeIdentity: [1], threadDirectory: [2] } });
  ok(await hello());
  const identity = ok(await connection.send("ext.storeIdentity.get", {}));
  const { id } = ok(await connection.send("ext.threadDirectory.new", { title: "Retained format-1 thread" }));
  // Seed plain predecessor-compatible history, not a format-2 plan/policy
  // turn relabeled as a legacy record. Native admission would set that loadout.
  const thread = await app.harness.conversation(Number(id), context);
  await thread.configure({ extensions: [], tools: [] }, context);
  const seeded = await thread.submit({ type: "input", content: "existing prompt", requestId: "legacy-1" }, context);
  assert.equal((await seeded.wait(context)).status, "done");
  const input = { submissionId: String(seeded.id) };
  await app.close();

  // Seed the legacy manifest shape on an otherwise identical schema and binding.
  const marker = join(dataDir, "store-profile");
  const { format, registry, cutover, ...binding } = JSON.parse(await readFile(marker, "utf8"));
  assert.equal(format, 2);
  await json(marker, { ...binding, format: 1 });
  const before = await readFile(marker);
  await assert.rejects(startBackend(options), { code: "store_profile" });
  await json(join(agentDir, "settings.json"), { allowLegacyStore: true, codemode: true });

  for (const requestId of ["legacy-2", "legacy-3"]) {
    app = await startBackend(options);
    connection = await dial(socket);
    t.after(() => connection.close());
    ok(await hello());
    assert.deepEqual(ok(await connection.send("ext.storeIdentity.get", {})), identity);
    const { threads } = ok(await connection.send("ext.threadDirectory.list", { limit: 100 }));
    assert.ok(threads.some(row => row.id === id && row.title === "Retained format-1 thread"));
    assert.equal((await wait(connection, id, input.submissionId)).status, "done");
    const next = await submit(connection, id, "continue existing thread", requestId);
    assert.equal((await wait(connection, id, next.submissionId)).status, "done");
    await app.close();
    assert.deepEqual(await readFile(marker), before);
  }

  // The setting does not adopt a standalone store at a custom data path.
  const custom = join(agentDir, "custom");
  await rename(dataDir, custom);
  await assert.rejects(startBackend({ ...options, dataDir: custom, socket: join(custom, "hush.sock") }), { code: "store_profile" });
  await rename(custom, dataDir);

  // Even a valid format-1 marker with opt-in cannot bypass an unknown fence.
  await json(join(agentDir, "store-cutover", "state.json"), { phase: "prepared" });
  await assert.rejects(startBackend(options), { code: "store_cutover" });
  assert.deepEqual(await readFile(marker), before);
});

test("native catalog search finds OpenRouter beyond the first 64 models with only an env key", { timeout: 10_000 }, async t => {
  const { dir, config } = await layout(t);
  await boot(t, dir, { ...config, faux: false }, { env: {
    ANTHROPIC_API_KEY: fixtureToken, OPENAI_API_KEY: fixtureToken, OPENROUTER_API_KEY: fixtureToken,
  } });
  const connection = await dial(config.socket);
  t.after(() => connection.close());
  const hello = ok(await connection.send("hello", { versions: [1], capabilities: { modelCatalog: [1, 2] } }));
  assert.deepEqual(hello.capabilities, { modelCatalog: 2 });
  const { id: threadId } = ok(await connection.send("thread.new", {}));
  const list = async (params = {}) => ok(await connection.send("ext.modelCatalog.list", { threadId, ...params }));
  const base = await list();
  assert.equal(base.models.length, 64);
  assert.ok(base.models.every(row => row.provider !== "openrouter"), "fixture must crowd OpenRouter out of the default page");
  assert.deepEqual(await list({ query: "  " }), base);
  const queried = await list({ query: "  OpEnRoUtEr " });
  assert.equal(queried.models.length, 64);
  assert.ok(queried.models.every(row => row.provider === "openrouter" && row.available && row.reason === null));
  assert.deepEqual(queried.current, base.current);
  assert.deepEqual(queried.default, base.default);
  assert.deepEqual((await list({ query: "no-fixture-model-matches-this" })).models, []);
  for (const query of [null, "a\nb", "界".repeat(43)]) {
    code(await connection.send("ext.modelCatalog.list", { threadId, query }), "invalid_request");
  }
  code(await connection.send("ext.modelCatalog.list", {}), "invalid_request");
  code(await connection.send("ext.modelCatalog.new", { model: base.default, thinkingLevel: "off" }), "unknown_method");

  const draftPeer = await dial(config.socket);
  t.after(() => draftPeer.close());
  assert.deepEqual(ok(await draftPeer.send("hello", { versions: [1], capabilities: { modelCatalog: [3, 2, 1] } })).capabilities, { modelCatalog: 3 });
  const before = ok(await draftPeer.send("thread.list", { limit: 100 }));
  const draft = ok(await draftPeer.send("ext.modelCatalog.list", { query: "openrouter" }));
  assert.deepEqual(draft, { ...queried, current: null });
  assert.deepEqual(ok(await draftPeer.send("ext.modelCatalog.list", {})), { ...base, current: null });
  code(await draftPeer.send("ext.modelCatalog.list", { threadId: null }), "invalid_request");
  code(await draftPeer.send("ext.modelCatalog.new", { model: { provider: "fixture", modelId: "not-installed" }, thinkingLevel: "off" }), "invalid_request");
  assert.deepEqual(ok(await draftPeer.send("thread.list", { limit: 100 })), before);

  const legacy = await dial(config.socket);
  t.after(() => legacy.close());
  assert.deepEqual(ok(await legacy.send("hello", { versions: [1], capabilities: { modelCatalog: [1] } })).capabilities, { modelCatalog: 1 });
  assert.deepEqual(ok(await legacy.send("ext.modelCatalog.list", { threadId })), base);
  code(await legacy.send("ext.modelCatalog.list", { threadId, query: "openrouter" }), "invalid_request");
});

test("standalone backend resolves stored OpenAI Codex OAuth without adjacent modules", { timeout: 10_000 }, async t => {
  const { dir, config } = await layout(t);
  const entry = join(dir, "only-backend.mjs");
  await copyFile(bundle, entry);
  await json(join(config.agentDir, "auth.json"), { "openai-codex": {
    type: "oauth", access: "fixture-bearer", refresh: "fixture-refresh", expires: Date.now() + 86_400_000,
  } });
  // A fresh credential exercises the same request-time loader as production,
  // but derivation needs neither an actual bearer token nor a network refresh.
  const script = `
    const [entry, agentDir] = process.argv.slice(1);
    const { loadModels } = await import(entry);
    const { runtime } = await loadModels({ agentDir, faux: false, ephemeral: true });
    const result = await runtime.getAuth('openai-codex');
    if (result?.source !== 'OAuth' || result.auth.apiKey !== 'fixture-bearer') {
      throw new Error('Codex OAuth did not derive fixture auth');
    }
  `;
  const child = spawn(process.execPath, ["--input-type=module", "--eval", script, pathToFileURL(entry).href, config.agentDir], {
    cwd: dir, env: { HOME: dir, PATH: "/usr/bin:/bin", NODE_NO_WARNINGS: "1" }, stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = "", stderr = "";
  child.stdout.on("data", chunk => stdout += chunk);
  child.stderr.on("data", chunk => stderr += chunk);
  const exit = once(child, "close");
  t.after(() => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); });
  assert.deepEqual(await exit, [0, null], stderr);
  assert.equal(stdout + stderr, "");
});

test("custom models resolve live Hush credentials without exposing them on the wire", { timeout: 10000 }, async t => {
  const { config } = await layout(t);
  const fixtureModel = "hush-model-test";
  let seenAuth;
  const provider = createServer((req, res) => {
    if (req.method === "POST") seenAuth = req.headers.authorization;
    res.writeHead(401, { "content-type": "application/json" });
    res.end(JSON.stringify({ error: { message: "Synthetic provider error with " + fixtureToken } }));
  });
  provider.listen(0, "127.0.0.1");
  await once(provider, "listening");
  t.after(() => new Promise(resolve => provider.close(resolve)));
  await json(join(config.agentDir, "settings.json"), { retry: { enabled: false } });
  await json(join(config.agentDir, "models.json"), { providers: {
    "hush-fixture": { baseUrl: "http://127.0.0.1:" + provider.address().port + "/v1", api: "openai-completions",
      models: [{ id: fixtureModel, name: "Fixture model", reasoning: true, contextWindow: 20000, maxTokens: 500 }] },
  } });
  await json(join(config.agentDir, "auth.json"), { "hush-fixture": { type: "api_key", key: fixtureToken } });
  const { runtime } = await loadModels({ ...config, faux: false, ephemeral: true });
  const model = runtime.getModel("hush-fixture", fixtureModel);
  assert.ok(model);
  assert.equal((await runtime.getAuth(model)).auth.apiKey, fixtureToken);
  await grant(config);
  const app = await startBackend({ ...config, faux: false, model: { provider: "hush-fixture", modelId: fixtureModel } });
  t.after(() => app.close());
  const { connection } = await peer(t, config.socket);
  const threadId = String(app.root.id);
  ok(await connection.send("subscribe", { threadId }));
  const receipt = await submit(connection, threadId, "A local authentication check", "secret-scrub");
  const report = await wait(connection, threadId, receipt.submissionId);
  assert.equal(report.status, "unanswered");
  assert.equal(seenAuth, "Bearer " + fixtureToken);
  ok(await connection.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 100 }));
  assert.doesNotMatch(JSON.stringify(connection.frames) + await readFile(join(config.dataDir, "hush.log"), "utf8"), new RegExp(fixtureToken));
});

test("local titles use the topic of the first exchange, not generic opening words", () => {
  assert.equal(topicTitle("There is a bug where other threads cannot be clicked with the mouse just using the /threads menu",
    "Fixed the tab-click bug: inactive threads now switch when clicked with the mouse."), "tab-click bug");
  assert.equal(topicTitle("It seems to be a bug. 1. Start a new thread and global skills are not showing.",
    "Fixed skills loading in new threads: global skills now appear."), "skills loading");
  assert.equal(topicTitle("Fix the café picker with detai…"), "café picker");
  assert.equal(displayTitle({ title: "My intentionally much longer title", topicTitle: "café picker" }), "My intentionally much longer title");
});

test("thread directory keeps friendly titles and pages by last committed message", { timeout: 15_000 }, async t => {
  const { config } = await layout(t, { idleMs: 10_000 });
  await grant(config);
  const app = await startBackend(config);
  t.after(() => app.close());
  const c = await dial(config.socket);
  t.after(() => c.close());
  const hello = ok(await c.send("hello", { versions: [1], capabilities: { threadDirectory: [1] } }));
  assert.equal(hello.capabilities.threadDirectory, 1);
  const method = "ext.threadDirectory.list";
  const basic = await dial(config.socket);
  t.after(() => basic.close());
  ok(await basic.send("hello", { versions: [1] }));
  code(await basic.send(method, { limit: 10 }), "unsupported_capability");
  code(await c.send(method, { limit: 101 }), "invalid_request");
  code(await c.send(method, { limit: 1, cursor: "invalid" }), "invalid_cursor");

  const older = ok(await c.send("thread.new", { cwd: config.cwd })).id;
  const newer = ok(await c.send("thread.new", { cwd: config.cwd })).id;
  const quiet = ok(await c.send("thread.new", { cwd: config.cwd })).id;
  const ask = async (thread, text, key) => {
    const r = await submit(c, thread, text, key);
    assert.equal((await wait(c, thread, r.submissionId)).status, "done");
  };
  await ask(newer, "Review terminal layout", "directory-second");
  await ask(older, "Fix the café picker\nwith details", "directory-first");
  await ask(older, "Follow-up that must not rename the thread", "directory-followup");
  const recent = ok(await c.send("history", { threadId: older, range: { kind: "before", beforeEntryId: null }, limit: 1 }));
  const stamp = recent.entries[0].messages.find(message => message.role === "assistant").timestampMs;

  let first, second;
  for (let i = 0; i < 50; i++) {
    first = ok(await c.send(method, { limit: 1 }));
    if (first.threads[0]?.lastMessageMs === stamp && !first.indexing) break;
    await setTimeout(20);
  }
  assert.deepEqual(first.threads, [{ id: older, title: "café picker", lastMessageMs: stamp }]);
  assert.ok(first.nextCursor);
  second = ok(await c.send(method, { limit: 10, cursor: first.nextCursor }));
  assert.equal(second.threads[0].id, newer);
  assert.equal(second.threads[0].title, "terminal layout");
  assert.deepEqual(second.threads.find(row => row.id === quiet), { id: quiet });
  assert.equal(second.nextCursor, null);

  c.close();
  basic.close();
  await app.close();
  const restarted = await startBackend(config);
  t.after(() => restarted.close());
  const reopened = await dial(config.socket);
  t.after(() => reopened.close());
  ok(await reopened.send("hello", { versions: [1], capabilities: { threadDirectory: [1] } }));
  code(await reopened.send(method, { limit: 10, cursor: first.nextCursor }), "invalid_cursor");
  // The first list after startup may be a partial page until the disposable
  // index is rebuilt. Check the contract while waiting for a complete page.
  const deadline = performance.now() + 5_000;
  let loaded = ok(await reopened.send(method, { limit: 10 }));
  while (loaded.indexing && performance.now() < deadline) {
    assert.equal(loaded.nextCursor, null, "a partial index cannot issue cursors");
    await setTimeout(20);
    loaded = ok(await reopened.send(method, { limit: 10 }));
  }
  assert.equal(loaded.indexing, false, "directory index did not finish rebuilding");
  assert.deepEqual(loaded.threads[0], first.threads[0], "directory is rebuilt from retained history");
});

test("directory v2 observes running and parked work and restores it after reopen", { timeout: 15_000 }, async t => {
  const { config } = await layout(t, { idleMs: 10_000 });
  await grant(config);
  let backend = await startBackend(config);
  t.after(() => backend.close());
  const connect = async version => {
    const c = await dial(config.socket);
    t.after(() => c.close());
    const hello = ok(await c.send("hello", { versions: [1], capabilities: { threadDirectory: [version], intro: [2] } }));
    assert.equal(hello.capabilities.threadDirectory, version);
    return c;
  };
  let c = await connect(2);
  const v1 = await connect(1);
  const method = "ext.threadDirectory.get";
  const id = ok(await c.send("ext.threadDirectory.new", { title: "Parking test" })).id;
  code(await v1.send(method, { threadIds: [id] }), "unknown_method");
  code(await v1.send("ext.threadDirectory.list", { includeArchived: true, limit: 10 }), "invalid_request");
  code(await c.send(method, { threadIds: [id, id] }), "invalid_request");
  code(await c.send(method, { threadIds: ["9999999999999999"] }), "invalid_request");
  code(await c.send(method, { threadIds: [id], after: Number.MAX_SAFE_INTEGER }), "invalid_request");

  const read = async (wanted, title = "Parking test") => {
    for (let i = 0; i < 80; i++) {
      const report = ok(await c.send(method, { threadIds: [id, "123456789"] }));
      const row = report.threads[0];
      if (row?.id === id && report.threads.length === 1 && row.status === wanted && row.title === title) return report;
      await setTimeout(15);
    }
    assert.fail(`missing directory state ${wanted} ${title}`);
  };
  await read("idle");
  const running = await submit(c, id, "!pause", "title-running");
  await read("running");
  code(await c.send("ext.threadDirectory.archive", { threadId: id, archived: true }), "busy");
  ok(await c.send("thread.abort", { threadId: id }));
  assert.equal((await wait(c, id, running.submissionId)).status, "unanswered");
  await read("stopped");
  await submit(c, id, "!sleep 60000", "title-parked");
  const parked = await read("parked");
  const waiting = c.send(method, { threadIds: [id], after: parked.revision });
  // A passive get does not hold the request line or wait for a second connection.
  ok(await c.send("ext.intro.rename", { threadId: id, title: "Renamed while parked" }));
  const advanced = ok(await waiting);
  assert.ok(advanced.revision > parked.revision);
  await read("parked", "Renamed while parked");
  const list = ok(await c.send("ext.threadDirectory.list", { limit: 10 }));
  assert.equal(list.threads.find(row => row.id === id).status, "parked");

  c.close();
  v1.close();
  await backend.close();
  backend = await startBackend(config);
  c = await connect(2);
  await read("parked", "Renamed while parked");
  ok(await c.send("thread.abort", { threadId: id }));
  await read("stopped", "Renamed while parked");
});

test("opted-in title model sees a bounded first exchange once, outside native turns", { timeout: 15_000 }, async t => {
  const { config } = await layout(t, { idleMs: 10_000 });
  const titles = [];
  let chats = 0;
  const provider = createServer(async (req, res) => {
    const chunks = [];
    for await (const data of req) chunks.push(data);
    const body = JSON.parse(Buffer.concat(chunks).toString());
    const title = body.model === "small";
    if (title) titles.push(body);
    else chats++;
    res.writeHead(200, { "content-type": "text/event-stream" });
    const chunk = (delta, finish_reason) => res.write("data: " + JSON.stringify({ id: "chatcmpl-title", object: "chat.completion.chunk",
      created: 1, model: body.model, choices: [{ index: 0, delta, finish_reason }] }) + "\n\n");
    chunk({ content: title ? "  Cursor   paging regression review extra words  " : "Follow the cursor after a committed entry." }, null);
    chunk({}, "stop");
    res.end("data: [DONE]\n\n");
  }).listen(0, "127.0.0.1");
  t.after(() => new Promise(resolve => provider.close(resolve)));
  await once(provider, "listening");
  const models = ["chat", "small"].map(id => ({ id, name: id, reasoning: false, contextWindow: 20000, maxTokens: 500 }));
  await json(join(config.agentDir, "models.json"), { providers: { fixture: { baseUrl: `http://127.0.0.1:${provider.address().port}/v1`,
    api: "openai-completions", models } } });
  await json(join(config.agentDir, "auth.json"), { fixture: { type: "api_key", key: fixtureToken } });
  await json(join(config.agentDir, "settings.json"), { compaction: { enabled: false }, retry: { enabled: false } });
  const options = { ...config, faux: false, model: { provider: "fixture", modelId: "chat" }, threadTitleModel: { provider: "fixture", modelId: "small" } };
  await grant(config);
  let server = await startBackend(options);
  t.after(() => server.close());
  const peer = async () => {
    const c = await dial(config.socket);
    t.after(() => c.close());
    ok(await c.send("hello", { versions: [1], capabilities: { threadDirectory: [2], intro: [2] } }));
    return c;
  };
  let c = await peer();
  const id = ok(await c.send("thread.new", {})).id;
  const ask = async (text, key) => {
    const r = await submit(c, id, text, key);
    assert.equal((await wait(c, id, r.submissionId)).status, "done");
  };
  await ask("Review paging " + "details ".repeat(70), "title-first");
  let row;
  for (let i = 0; i < 50; i++) {
    [row] = ok(await c.send("ext.threadDirectory.get", { threadIds: [id] })).threads;
    if (row?.title === "Cursor paging regression review") break;
    await setTimeout(20);
  }
  assert.equal(row?.title, "Cursor paging regression review");
  assert.equal(row.status, "done");
  assert.equal(titles.length, 1);
  const request = titles[0];
  assert.match(request.messages[0].content, /2–4 word title/);
  assert.match(request.messages.at(-1).content, /"user":"Review paging/);
  assert.match(request.messages.at(-1).content, /"answer":"Follow the cursor/);
  assert.ok(request.messages.at(-1).content.length < 250);
  assert.equal(request.tools, undefined);
  assert.equal(chats, 1);
  c.close();
  await server.close();
  server = await startBackend(options);
  c = await peer();
  const list = () => c.send("ext.threadDirectory.list", { limit: 10 }).then(ok);
  assert.equal((await list()).threads.find(row => row.id === id).title, "Cursor paging regression review");
  await ask("Continue the review", "title-second");
  assert.equal((await list()).threads.find(row => row.id === id).title, "Cursor paging regression review");
  ok(await c.send("ext.intro.rename", { threadId: id, title: "Manual title wins" }));
  assert.equal((await list()).threads.find(row => row.id === id).title, "Manual title wins");
  assert.equal(titles.length, 1);
  assert.equal(chats, 2);
});

test("intro v2 names threads and scopes recovery to a settled failure", { timeout: 10_000 }, async t => {
  const { config } = await layout(t, { idleMs: 10_000 });
  await grant(config);
  let app = await startBackend(config);
  t.after(() => app.close());
  const attach = async versions => {
    const c = await dial(config.socket);
    t.after(() => c.close());
    const hello = ok(await c.send("hello", { versions: [1], capabilities: { intro: versions, threadDirectory: [1] } }));
    assert.equal(hello.capabilities.intro, versions.includes(2) ? 2 : 1);
    return c;
  };
  let client = await attach([1, 2]);
  const legacy = await attach([1]);
  const threadId = ok(await client.send("thread.new", { cwd: config.cwd })).id;
  const scope = { threadId };
  async function reportUntil(pred, scope) {
    for (let i = 0; i < 100; i++) {
      const report = ok(await client.send("ext.intro.get", scope));
      if (pred(report)) return report;
      await setTimeout(10);
    }
    assert.fail("intro report did not reach the expected state");
  }
  const ask = async (text, key) => {
    const { submissionId } = await submit(client, threadId, text, key);
    return wait(client, threadId, submissionId);
  };
  assert.equal((await ask("Fix the café picker\nwith details", "intro-v2-title")).status, "done");
  const outcome = await ask("!error", "intro-v2-error");
  assert.equal(outcome.reason, "model_error");
  const first = await reportUntil(r => r.thread?.issue && r.attention[0]?.title === "café picker", scope);
  const issue = first.thread.issue;
  assert.equal(first.thread.id, threadId);
  assert.equal(first.thread.title, "café picker");
  assert.equal(issue.reason, "model_error");
  assert.equal(issue.detail, "synthetic failure");
  assert.equal(issue.retry, true);
  assert.equal(issue.switchModel, true);
  assert.ok(issue.endedAtMs > 0);
  assert.deepEqual(first.attention, [{ ...scope, title: first.thread.title, status: "stopped", issue }]);
  const old = ok(await legacy.send("ext.intro.get", scope));
  assert.equal(old.thread, undefined);
  assert.deepEqual(old.attention, [{ title: `thread #${threadId}`, status: "stopped", reason: "model error" }]);
  code(await legacy.send("ext.intro.retry", { ...scope, taskId: issue.taskId }), "unknown_method");

  const conv = await app.harness.conversation(Number(threadId), context);
  await conv.commit(async tx => {
    (await tx.doc(LiveDoc, conv.id)).runFailure.detail = `429 Rate limited: Authorization: Bearer ${fixtureToken}`;
  }, context);
  const sanitized = await reportUntil(r => r.thread?.issue?.detail === "rate limited (429)", scope);
  assert.equal(sanitized.attention[0].issue.detail, sanitized.thread.issue.detail);
  assert.doesNotMatch(JSON.stringify(sanitized), new RegExp(fixtureToken));

  code(await client.send("ext.intro.rename", { ...scope, title: "\t " }), "invalid_request");
  assert.deepEqual(ok(await client.send("ext.intro.rename", { ...scope, title: "  Debug\n the picker  " })), { title: "Debug the picker" });
  const named = await reportUntil(r => r.attention[0]?.title === "Debug the picker", scope);
  assert.equal(named.thread.title, "Debug the picker");
  const target = { ...scope, taskId: issue.taskId };
  code(await client.send("ext.intro.dismiss", { ...target, threadId: String(app.root.id) }), "not_found");
  const retry = ok(await client.send("ext.intro.retry", target));
  assert.equal((await wait(client, threadId, retry.submissionId)).reason, "model_error");
  const second = await reportUntil(r => r.thread?.issue?.taskId && r.thread.issue.taskId !== issue.taskId, scope);
  code(await client.send("ext.intro.retry", target), "not_found");
  code(await client.send("ext.intro.dismiss", target), "not_found");
  const dismissed = { ...scope, taskId: second.thread.issue.taskId };
  assert.deepEqual(ok(await client.send("ext.intro.dismiss", dismissed)), {});
  code(await client.send("ext.intro.retry", dismissed), "not_found");
  assert.equal((await wait(client, threadId, retry.submissionId)).status, "unanswered", "dismiss must not change settlement");

  client.close();
  legacy.close();
  await app.close();
  app = await startBackend(config);
  client = await attach([2]);
  const reopened = ok(await client.send("ext.intro.get", scope));
  assert.deepEqual(reopened.thread, { id: threadId, title: "Debug the picker", dismissedFailureId: dismissed.taskId });
  assert.deepEqual(reopened.attention, []);
  assert.equal(reopened.sessions.quiet, 2);
  const directory = ok(await client.send("ext.threadDirectory.list", { limit: 10 }));
  assert.equal(directory.threads.find(row => row.id === threadId).title, "Debug the picker");

  ok(await client.send("thread.configure", { ...scope, change: { model: { provider: "fixture", modelId: "missing-model" } } }));
  assert.equal((await ask("answer this after changing the model", "intro-v2-missing")).reason, "no_model");
  const missing = await reportUntil(r => r.thread?.issue?.reason === "no_model", scope);
  assert.equal(missing.thread.issue.detail, "fixture/missing-model is unavailable");
  assert.equal(missing.thread.issue.retry, false);
  assert.equal(missing.thread.issue.switchModel, true);
  ok(await client.send("thread.configure", { ...scope, change: { model: { provider: "faux", modelId: "faux-1" } } }));
  const ready = await reportUntil(r => r.thread?.issue?.retry, scope);
  const continued = ok(await client.send("ext.intro.retry", { ...scope, taskId: ready.thread.issue.taskId }));
  assert.equal((await wait(client, threadId, continued.submissionId)).status, "done");

  const sleep = await submit(client, threadId, "!sleep 12000", "intro-v2-abort");
  await reportUntil(r => r.attention.some(a => a.status === "parked"), scope);
  ok(await client.send("thread.abort", scope));
  await wait(client, threadId, sleep.submissionId);
  const stopped = await reportUntil(r => !r.attention.length && r.sessions.quiet === 2, scope);
  assert.equal(stopped.thread.issue, undefined);
  assert.equal(stopped.thread.dismissedFailureId, undefined);
  const resumed = await app.harness.conversation(Number(threadId), context);
  for (const reason of ["aborted", "reset", "nothing_to_answer"]) {
    await resumed.commit(async tx => {
      (await tx.doc(LiveDoc, resumed.id)).runFailure = { taskId: Number(continued.submissionId), reason, inputs: [] };
    }, context);
    const report = ok(await client.send("ext.intro.get", scope));
    assert.equal(report.thread.issue, undefined);
    assert.deepEqual(report.attention, []);
  }
});

test("config keeps Hush runtime defaults and lets --data-dir override the file", async t => {
  const { config, dir } = await layout(t);
  const path = join(dir, "backend.json");
  const { parseConfig } = await import("../../dist/backend/src/config.js");
  await json(path, { "data-dir": config.dataDir });
  assert.equal((await parseConfig(["--config", path])).socket, config.socket);
  const other = join(dir, "other-data");
  assert.equal((await parseConfig(["--config", path, "--data-dir", other])).socket, join(other, "hush.sock"));
  const ephemeral = await parseConfig(["--no-session", "--socket", config.socket]);
  assert.equal(ephemeral.dataDir, undefined);
  assert.equal(ephemeral.socket, config.socket);
  for (const args of [["--profile", "pi"], ["--pi-events"], ["--agent-dir", dir]]) {
    await assert.rejects(parseConfig(args));
  }
});

test("distinct --data-dir paths get independent live backends and histories", { timeout: 14000 }, async t => {
  const { config, dir } = await layout(t, { idleMs: 6000 });
  const envAgent = join(dir, "env-agent");
  const variant = suffix => {
    const dataDir = join(dir, "data-" + suffix);
    return { ...config, dataDir, socket: join(dataDir, "hush.sock") };
  };
  const a = variant("a"), b = variant("b");
  await Promise.all([envAgent].map(path => mkdir(path, { mode: 0o700 })));
  async function open(profile) {
    const host = await boot(t, dir, profile, { env: { PI_CODING_AGENT_DIR: envAgent } });
    const { connection } = await peer(t, profile.socket);
    const { id } = ok(await connection.send("thread.list", { limit: 1 })).threads[0];
    return { host, connection, id };
  }
  const clients = [await open(a), await open(b)]; // Both keep their own writer lock simultaneously.
  const text = ["profile-a-only", "profile-b-only"];
  const range = { kind: "before", beforeEntryId: null };
  async function checkHistory({ connection, id }, i) {
    const rows = ok(await connection.send("history", { threadId: id, range, limit: 100 })).entries;
    const inputs = rows.filter(e => e.kind === "pi.user").flatMap(e => e.messages.flatMap(m => m.blocks.map(b => b.text)));
    assert.deepEqual(inputs, [text[i]]);
  }
  await Promise.all(clients.map(async (client, i) => {
    const receipt = await submit(client.connection, client.id, text[i], "same-key");
    assert.equal((await wait(client.connection, client.id, receipt.submissionId)).status, "done");
    await checkHistory(client, i);
  }));
  assert.deepEqual(await readdir(envAgent), []);
  clients[1].connection.close();
  clients[1].host.child.kill("SIGTERM");
  assert.deepEqual(await clients[1].host.exit, [0, null]);
  await checkHistory(await open(b), 1);
  await checkHistory(clients[0], 0);
});

test("standalone bundle, private socket and lock, deadline outlives disconnected client then exits", { timeout: 14000 }, async t => {
  const { config, dir, privateDir } = await layout(t, { idleMs: 130 });
  const host = await boot(t, dir, config, { env: { HUSH_TEST_SECRET: fixtureToken } });
  assert.equal((await lstat(privateDir)).mode & 0o777, 0o700);
  assert.equal((await lstat(config.socket)).mode & 0o777, 0o600);
  const { connection } = await peer(t, config.socket);
  await boot(t, dir, config, { failure: /data_locked/ });
  await setTimeout(350); // More than the idle grace with a connected, idle peer.
  assert.equal(host.dead, false, "backend exited with a client still attached");
  const { id: threadId } = ok(await connection.send("thread.list", { limit: 1 })).threads[0];
  ok(await connection.send("subscribe", { threadId }));
  const receipt = await submit(connection, threadId, "!sleep 1000", "pending-deadline");
  await connection.wait(f => f.type === "patch" && JSON.stringify(f.ops).includes("sleeping"));
  connection.close();
  await setTimeout(350);
  assert.equal(host.dead, false, "backend exited before its pending deadline");
  assert.deepEqual(await host.exit, [0, null]);
  const fresh = await boot(t, dir, { ...config, idleMs: 3000 });
  const after = await peer(t, config.socket);
  const terminal = await wait(after.connection, threadId, receipt.submissionId);
  assert.equal(terminal.status, "done");
  const history = ok(await after.connection.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 100 }));
  assert.match(JSON.stringify(history), /The deadline fired/);
  const log = await readFile(join(config.dataDir, "hush.log"), "utf8");
  assert.doesNotMatch(log + fresh.stderr + JSON.stringify(after.connection.frames), new RegExp(fixtureToken));
});

test("standalone backend reopens after SIGKILL without replaying a running unsafe tool", { timeout: 14000 }, async t => {
  const { config, dir } = await layout(t);
  const host = await boot(t, dir, config);
  const { connection, hello } = await peer(t, config.socket);
  const { id: threadId } = ok(await connection.send("thread.list", { limit: 1 })).threads[0];
  ok(await connection.send("subscribe", { threadId }));
  const receipt = await submit(connection, threadId, "!crash", "recover-me");
  await connection.wait(f => f.type === "patch" && JSON.stringify(f.ops).includes("fixture entered"));
  host.child.kill("SIGKILL");
  assert.deepEqual(await host.exit, [null, "SIGKILL"]);
  await connection.ended;
  await boot(t, dir, config);
  const fresh = await peer(t, config.socket);
  assert.notEqual(fresh.hello.epoch, hello.epoch);
  assert.deepEqual(await submit(fresh.connection, threadId, "!crash", "recover-me"), receipt);
  assert.equal((await wait(fresh.connection, threadId, receipt.submissionId)).status, "done");
  const records = ok(await fresh.connection.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 100 })).entries;
  assert.equal(records.filter(e => e.kind === "pi.user").length, 1);
  assert.equal(records.filter(e => e.kind === "pi.tool-result").length, 1);
  assert.equal(records.find(e => e.kind === "pi.tool-result").messages[0].isError, true);
});

test("invocation-private memory leaves no persisted transcript, backups or log on disconnect/kill", { timeout: 18000 }, async t => {
  for (const kill of [false, true]) {
    const { config, dir, privateDir } = await layout(t, { ephemeral: true });
    const host = await boot(t, dir, config);
    const { connection } = await peer(t, config.socket);
    const { id: threadId } = ok(await connection.send("thread.list", { limit: 1 })).threads[0];
    const marker = kill ? "kill-private-payload-263" : "disconnect-private-payload-254";
    const prompt = await submit(connection, threadId, marker, "private");
    assert.equal((await wait(connection, threadId, prompt.submissionId)).status, "done");
    const history = ok(await connection.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 100 }));
    assert.match(JSON.stringify(history), new RegExp(marker));
    assert.ok(!history.entries.some(e => e.kind === "pi.tool-result"));
    if (kill) {
      const active = await submit(connection, threadId, "!pause", "no-detached-work");
      assert.ok(active.submissionId);
      host.child.kill("SIGKILL");
    } else connection.close();
    assert.deepEqual(await host.exit, kill ? [null, "SIGKILL"] : [0, null]);
    assert.deepEqual((await readdir(privateDir)).filter(n => n !== "hush.sock"), []);
    assert.deepEqual(await readdir(config.agentDir), []);
    assert.doesNotMatch(host.stderr, new RegExp(marker));
  }
});

test("standalone settings persist model defaults and reasoning display without rewriting existing threads", async t => {
  const { config } = await layout(t);
  const models = ["fixture-one", "fixture-two"].map(id => ({ id, name: id, reasoning: true, contextWindow: 20000, maxTokens: 500 }));
  await json(join(config.agentDir, "models.json"), { providers: { fixture: {
    baseUrl: "http://127.0.0.1:9/v1", api: "openai-completions", models,
  } } });
  await json(join(config.agentDir, "settings.json"), {
    defaultProvider: "fixture", defaultModel: "fixture-one", defaultThinkingLevel: "high", codemode: true,
  });
  const real = { ...config, faux: false };
  let app = await startBackend(real);
  let wire = await dial(config.socket);
  t.after(async () => { wire.close(); await app.close(); });
  const hello = ok(await wire.send("hello", { versions: [1], capabilities: { settings: [1], modelCatalog: [3] } }));
  assert.equal(hello.capabilities.settings, 1);
  assert.deepEqual(ok(await wire.send("ext.settings.get", {})), {
    model: { provider: "fixture", modelId: "fixture-one" }, thinkingLevel: "high", reasoning: "folded",
  });
  assert.deepEqual(ok(await wire.send("ext.settings.set", { reasoning: "expanded" })), {
    model: { provider: "fixture", modelId: "fixture-one" }, thinkingLevel: "high", reasoning: "expanded",
  });
  const changed = ok(await wire.send("ext.settings.set", {
    model: { provider: "fixture", modelId: "fixture-two" }, thinkingLevel: "low",
  }));
  assert.deepEqual(changed, { model: { provider: "fixture", modelId: "fixture-two" }, thinkingLevel: "low", reasoning: "expanded" });
  const catalog = ok(await wire.send("ext.modelCatalog.list", { threadId: String(app.root.id) }));
  assert.deepEqual(catalog.current, { provider: "fixture", modelId: "fixture-one" });
  assert.deepEqual(catalog.default, { provider: "fixture", modelId: "fixture-two" });
  const created = ok(await wire.send("thread.new", {}));
  const state = ok(await wire.send("subscribe", { threadId: created.id }));
  assert.deepEqual(state.state.agent.model, { provider: "fixture", modelId: "fixture-two" });
  assert.equal(state.state.agent.thinkingLevel, "low");
  code(await wire.send("ext.settings.set", { thinkingLevel: "max" }), "invalid_request");
  const saved = JSON.parse(await readFile(join(config.agentDir, "settings.json"), "utf8"));
  assert.equal(saved.codemode, true);
  assert.equal(saved.defaultModel, "fixture-two");
  assert.equal(saved.defaultThinkingLevel, "low");
  assert.deepEqual(saved.display, { reasoning: "expanded" });
  assert.equal((await lstat(join(config.agentDir, "settings.json"))).mode & 0o077, 0);
  wire.close();
  await app.close();
  app = await startBackend(real);
  wire = await dial(config.socket);
  ok(await wire.send("hello", { versions: [1], capabilities: { settings: [1] } }));
  assert.deepEqual(ok(await wire.send("ext.settings.get", {})), changed);
  wire.close();
  await app.close();
});
