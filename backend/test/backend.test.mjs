import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { copyFile, lstat, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { createServer } from "node:http";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { setTimeout } from "node:timers/promises";
import { fileURLToPath, pathToFileURL } from "node:url";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { createAgentSession, DefaultResourceLoader, ModelRegistry, ModelRuntime, ProjectTrustStore, SessionManager, SettingsManager } from "@earendil-works/pi-coding-agent";
import { getSystemMessageText } from "@earendil-works/pi-ai";
import { AgentDoc, LiveDoc } from "@earendil-works/pi-durable";
import { assertFrame } from "../../protocol/v1/replay.mjs";
import { dial } from "../../server/test/client.mjs";
import { loadModels, startBackend } from "../../dist/hush-backend.mjs";
import { parseConfig } from "../../dist/backend/src/config.js";

const bundle = new URL("../../dist/hush-backend.mjs", import.meta.url);
const piDir = fileURLToPath(new URL("../../node_modules/@earendil-works/pi-coding-agent", import.meta.url));
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
  const storage = config.ephemeral ? ["--no-session", "--socket", config.socket]
    : config.dataDir ? ["--data-dir", config.dataDir] : [];
  const profile = config.profile === "hush" ? ["--profile", "hush"] : ["--agent-dir", config.agentDir];
  const args = [entry, "--cwd", config.cwd, ...profile, ...(config.faux ? ["--faux"] : []), "--idle-ms", String(config.idleMs), ...storage];
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
  let app = await startBackend({ ...config, profile: "hush" });
  t.after(() => app.close());
  let wire = await dial(config.socket);
  t.after(() => wire.close());
  const first = ok(await wire.send("hello", { versions: [1], capabilities: { hushProfile: [1], storeIdentity: [1], runtime: [1] } }));
  assert.deepEqual(first.capabilities, { hushProfile: 1, storeIdentity: 1, runtime: 1 });
  const threadId = ok(await wire.send("thread.new", {})).id;
  assert.deepEqual(ok(await wire.send("ext.runtime.stop", {})), {});
  await app.closed;
  await assert.rejects(dial(config.socket));

  app = await startBackend({ ...config, profile: "hush" });
  wire = await dial(config.socket);
  t.after(() => wire.close());
  const second = ok(await wire.send("hello", { versions: [1], capabilities: { runtime: [1] } }));
  assert.notEqual(second.epoch, first.epoch);
  const threads = ok(await wire.send("thread.list", { limit: 100 }));
  assert.ok(threads.threads.some(row => row.id === threadId));
});

test("native catalog search finds OpenRouter beyond the first 64 models with only an env key", { timeout: 10_000 }, async t => {
  const { dir, config } = await layout(t);
  await boot(t, dir, { ...config, profile: "hush", faux: false }, { env: {
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

test("isolated Pi loader, default system prompt parity, trust transition, MCP and a one-section edit", { timeout: 16000 }, async t => {
  const oldPackage = process.env.PI_PACKAGE_DIR;
  process.env.PI_PACKAGE_DIR = piDir;
  t.after(() => { if (oldPackage) process.env.PI_PACKAGE_DIR = oldPackage; else delete process.env.PI_PACKAGE_DIR; });
  const { config } = await layout(t);
  const { cwd, agentDir } = config;
  await mkdir(join(cwd, ".pi", "skills", "hidden-skill"), { recursive: true, mode: 0o700 });
  await mkdir(join(cwd, ".pi", "prompts"));
  await Promise.all([
    json(join(agentDir, "settings.json"), { defaultThinkingLevel: "low", retry: { enabled: false }, compaction: { enabled: false } }),
    doc(join(agentDir, "AGENTS.md"), "global context fixture"),
    doc(join(cwd, "AGENTS.md"), "project context v1"),
    doc(join(cwd, ".pi", "SYSTEM.md"), "TRUSTED ROLE"),
    doc(join(cwd, ".pi", "APPEND_SYSTEM.md"), "TRUSTED ADDENDUM"),
    doc(join(cwd, ".pi", "skills", "hidden-skill", "SKILL.md"), "---\nname: hidden-skill\ndescription: Trust fixture skill\n---\nSkill data"),
    doc(join(cwd, ".pi", "prompts", "fixture.md"), "---\ndescription: Trust fixture prompt\n---\nexpanded for $1"),
    json(join(cwd, ".pi", "mcp.json"), { mcpServers: { project: {
      command: process.execPath, args: [fileURLToPath(new URL("./mcp-fixture.mjs", import.meta.url))], exposure: "direct",
    } } }),
  ]);
  const app = await startBackend(config);
  t.after(() => app.close());
  const { connection } = await peer(t, config.socket);
  const threadId = String(app.root.id);
  const snap = ok(await connection.send("subscribe", { threadId }));
  assert.equal(snap.state.agent.thinkingLevel, "low");
  code(await connection.send("ext.piEvents.subscribe", { threadId }), "unsupported_capability");
  const opts = { cwd, agentDir, settingsManager: SettingsManager.create(cwd, agentDir, { projectTrusted: false }),
    noExtensions: true, noThemes: true };
  const referenceLoader = new DefaultResourceLoader(opts);
  await referenceLoader.reload();
  const referenceRuntime = await ModelRuntime.create({ authPath: join(agentDir, "auth.json"), modelsPath: join(agentDir, "models.json"),
    refreshOnCreate: false });
  const { session } = await createAgentSession({ ...opts, modelRuntime: referenceRuntime,
    resourceLoader: referenceLoader, sessionManager: SessionManager.inMemory() });
  t.after(() => session.dispose());
  const getSystems = async () => ((await app.root.entries({}, 100, undefined, context)).items)
    .filter(e => e.kind === "pi.system").reverse().map(e => e.model[0]);
  const untrusted = await submit(connection, threadId, "/fixture Tester", "untrusted");
  assert.equal((await wait(connection, threadId, untrusted.submissionId)).status, "done");
  const [first] = await getSystems();
  assert.equal(getSystemMessageText(first), session.systemPrompt);
  assert.match(getSystemMessageText(first), /project context v1/);
  assert.doesNotMatch(getSystemMessageText(first), /TRUSTED ROLE|TRUSTED ADDENDUM|hidden-skill/);
  assert.ok(!(first.toolsAdded ?? []).some(t => t.name === "mcp__project__hello"));
  let all = (await app.root.entries({}, 100, undefined, context)).items;
  assert.equal(all.find(e => e.kind === "pi.user").model[0].content, "/fixture Tester");

  new ProjectTrustStore(agentDir).set(cwd, true);
  const trusted = await submit(connection, threadId, "/fixture Tester", "trusted");
  assert.equal((await wait(connection, threadId, trusted.submissionId)).status, "done");
  all = (await app.root.entries({}, 100, undefined, context)).items;
  assert.equal(all.find(e => e.kind === "pi.user").model[0].content, "expanded for Tester");
  const systems = await getSystems();
  const text = getSystemMessageText(systems.at(-1));
  assert.match(text, /TRUSTED ROLE/);
  assert.match(text, /TRUSTED ADDENDUM/);
  assert.match(text, /hidden-skill/);
  const mcpName = systems.flatMap(s => s.toolsAdded ?? []).map(t => t.name).find(n => /project.*hello/.test(n));
  assert.ok(mcpName);
  const mcp = (await app.root.agent(context)).tools.find(t => t.name === mcpName);
  assert.ok(mcp);

  const before = systems.length;
  await doc(join(cwd, "AGENTS.md"), "project context v2");
  // The environment watcher has at most two seconds to deliver the change.
  await setTimeout(2150);
  const edit = await submit(connection, threadId, "after the file edit", "edit");
  assert.equal((await wait(connection, threadId, edit.submissionId)).status, "done");
  const after = await getSystems();
  assert.equal(after.length, before + 1);
  assert.deepEqual(Object.keys(after.at(-1).sections), ["project_context"]);
  assert.match(after.at(-1).sections.project_context, /project context v2/);
  const duplicate = await submit(connection, threadId, "/fixture Tester", "trusted");
  assert.equal(duplicate.submissionId, trusted.submissionId);
  const mcpCall = await submit(connection, threadId, "!mcp", "invoke-mcp");
  assert.equal((await wait(connection, threadId, mcpCall.submissionId)).status, "done");
  const mcpHistory = ok(await connection.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 10 }));
  assert.match(JSON.stringify(mcpHistory), /mcp fixture ran/);
});

test("custom models and live auth lookup use the same Pi stores without exporting metadata secrets", { timeout: 10000 }, async t => {
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
  const real = { ...config, faux: false, ephemeral: true };
  const ours = await loadModels(real);
  const upstream = new ModelRegistry(await ModelRuntime.create({ authPath: join(config.agentDir, "auth.json"),
    modelsPath: join(config.agentDir, "models.json"), refreshOnCreate: false }));
  const pi = upstream.find("hush-fixture", fixtureModel);
  const backend = ours.models.find("hush-fixture", fixtureModel);
  assert.ok(backend);
  assert.deepEqual(backend, pi);
  assert.deepEqual(await ours.models.getApiKeyAndHeaders(backend), await upstream.getApiKeyAndHeaders(pi));
  assert.equal(await ours.models.getApiKeyForProvider("hush-fixture"), fixtureToken);
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
  await ours.runtime.settleCredentialWrites();
});

test("thread directory keeps friendly titles and pages by last committed message", { timeout: 15_000 }, async t => {
  const { config } = await layout(t, { idleMs: 10_000 });
  const app = await startBackend({ ...config, piEvents: true });
  t.after(() => app.close());
  const c = await dial(config.socket);
  t.after(() => c.close());
  const hello = ok(await c.send("hello", { versions: [1], capabilities: { threadDirectory: [1], piEvents: [1] } }));
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
  assert.deepEqual(first.threads, [{ id: older, title: "Fix the café picker with details", lastMessageMs: stamp }]);
  assert.ok(first.nextCursor);
  second = ok(await c.send(method, { limit: 10, cursor: first.nextCursor }));
  assert.equal(second.threads[0].id, newer);
  assert.equal(second.threads[0].title, "Review terminal layout");
  assert.deepEqual(second.threads.find(row => row.id === quiet), { id: quiet });
  assert.equal(second.nextCursor, null);

  const pi = await dial(config.socket);
  t.after(() => pi.close());
  ok(await pi.send("hello", { versions: [1], capabilities: { piEvents: [1] } }));
  const named = ok(await pi.send("ext.piEvents.open", { cwd: config.cwd, name: "Saved project title" })).threadId;
  let page;
  for (let i = 0; i < 50; i++) {
    page = ok(await c.send(method, { limit: 20 }));
    if (page.threads.some(row => row.id === named && row.title === "Saved project title")) break;
    await setTimeout(20);
  }
  assert.deepEqual(page.threads.find(row => row.id === named), { id: named, title: "Saved project title" });

  pi.close();
  c.close();
  basic.close();
  await app.close();
  const restarted = await startBackend({ ...config, piEvents: true });
  t.after(() => restarted.close());
  const reopened = await dial(config.socket);
  t.after(() => reopened.close());
  ok(await reopened.send("hello", { versions: [1], capabilities: { threadDirectory: [1] } }));
  code(await reopened.send(method, { limit: 10, cursor: first.nextCursor }), "invalid_cursor");
  const loaded = ok(await reopened.send(method, { limit: 10 }));
  assert.equal(loaded.indexing, false);
  assert.deepEqual(loaded.threads[0], first.threads[0], "directory is rebuilt from retained history");
});

test("directory v2 observes running and parked work and restores it after reopen", { timeout: 15_000 }, async t => {
  const { config } = await layout(t, { idleMs: 10_000 });
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
    chunk({ content: title ? "  Cursor   review  " : "Follow the cursor after a committed entry." }, null);
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
    if (row?.title === "Cursor review") break;
    await setTimeout(20);
  }
  assert.equal(row?.title, "Cursor review");
  assert.equal(row.status, "done");
  assert.equal(titles.length, 1);
  const request = titles[0];
  assert.match(request.messages[0].content, /at most six words/);
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
  assert.equal((await list()).threads.find(row => row.id === id).title, "Cursor review");
  await ask("Continue the review", "title-second");
  assert.equal((await list()).threads.find(row => row.id === id).title, "Cursor review");
  ok(await c.send("ext.intro.rename", { threadId: id, title: "Manual title wins" }));
  assert.equal((await list()).threads.find(row => row.id === id).title, "Manual title wins");
  assert.equal(titles.length, 1);
  assert.equal(chats, 2);
});

test("intro negotiation is read-only and reports trusted skills, context, run failures and parking", { timeout: 15000 }, async t => {
  const { config } = await layout(t);
  const makeSkill = async (dir, name) => {
    const path = join(dir, "skills", name);
    await mkdir(path, { recursive: true });
    await doc(join(path, "SKILL.md"), `---\nname: ${name}\ndescription: Report fixture\n---\nprivate-body-${name}\n`);
  };
  await makeSkill(config.agentDir, "global-review");
  await makeSkill(join(config.cwd, ".pi"), "project-review");
  await doc(join(config.cwd, "AGENTS.md"), "private project instructions " + "repeat ".repeat(300));
  const app = await startBackend(config);
  t.after(() => app.close());
  let watches = 0;
  const openGraph = app.harness.watchTaskGraph.bind(app.harness);
  app.harness.watchTaskGraph = (...args) => { watches++; return openGraph(...args); };
  const root = String(app.root.id), scope = { threadId: root };
  const { connection: base } = await peer(t, config.socket);
  code(await base.send("ext.intro.get", scope), "unsupported_capability");
  const connectIntro = async () => {
    const c = await dial(config.socket);
    t.after(() => c.close());
    const hello = ok(await c.send("hello", { versions: [1], capabilities: { intro: [1] } }));
    assert.equal(hello.capabilities.intro, 1);
    return c;
  };
  const report = await connectIntro(), another = await connectIntro();
  const before = ok(await report.send("ext.intro.get", scope));
  assert.deepEqual(before.sessions, { open: 1, running: 0, quiet: 1, partial: false });
  assert.equal(before.model.modelId, "faux-1");
  assert.ok(before.startedAtMs > 0);
  assert.ok(before.skills.includes("global-review"));
  assert.ok(!before.skills.includes("project-review"));
  assert.ok(before.context.window > 0 && before.context.renderedBytes > 2100);
  assert.equal(before.context.reportedTokens, undefined);
  assert.doesNotMatch(JSON.stringify(before), /private-body|private project instructions/);
  assert.deepEqual(ok(await another.send("ext.intro.get", scope)).skills, before.skills);
  assert.equal(watches, 1);
  assert.equal((await app.root.entries({}, 10, undefined, context)).items.length, 0, "report wrote durable history");
  code(await report.send("ext.intro.get", { threadId: "9007199254740992" }), "invalid_request");
  code(await report.send("ext.intro.get", { threadId: "123456789012" }), "not_found");
  code(await report.send("ext.intro.get", { ...scope, cwd: config.cwd }), "invalid_request");
  code(await report.send("ext.intro.get", { cwd: "." }), "invalid_request");
  code(await report.send("ext.intro.get", { cwd: join(config.cwd, "missing") }), "not_found");
  const otherWorkspace = join(config.cwd, "workspace");
  await makeSkill(join(otherWorkspace, ".pi"), "other-review");
  new ProjectTrustStore(config.agentDir).set(otherWorkspace, true);
  const planned = ok(await another.send("ext.intro.get", { cwd: otherWorkspace }));
  assert.equal(planned.cwd, otherWorkspace);
  assert.ok(planned.skills.includes("other-review"));
  assert.ok(!planned.skills.includes("project-review"));
  assert.equal(planned.context.window, before.context.window);
  assert.equal(planned.sessions.open, 1, "workspace preview created a conversation");
  code(await report.send("ext.intro.get", { ...scope, after: Number.MAX_SAFE_INTEGER }), "invalid_request");

  async function changedUntil(current, predicate, conn = report) {
    while (!predicate(current)) {
      const next = ok(await conn.send("ext.intro.get", { ...scope, after: current.revision }));
      assert.ok(next.revision > current.revision);
      current = next;
    }
    return current;
  }
  // A waiting report leaves the peer able to process EOF and other requests.
  const pending = changedUntil(before, r => r.skills.includes("project-review"));
  pending.catch(() => {});
  const trust = new ProjectTrustStore(config.agentDir);
  trust.set(config.cwd, true);
  const trusted = await pending;
  assert.ok(trusted.skills.includes("global-review"));
  assert.ok(trusted.context.renderedBytes > before.context.renderedBytes);

  code(await base.send("submit", { ...scope, text: "/skill:no-such-skill", requestId: "unknown-skill", whenBusy: "reject" }), "invalid_request");
  const good = await submit(base, root, "/skill:project-review with arguments", "intro-good");
  assert.equal((await wait(base, root, good.submissionId)).status, "done");
  const used = await changedUntil(trusted, r => r.context.reportedTokens > 0);
  assert.ok(used.context.reportedTokens > 0);
  assert.match(JSON.stringify((await app.root.entries({}, 10, undefined, context)).items), /private-body-project-review.*with arguments/);
  const bad = await submit(base, root, "!error", "intro-error");
  assert.equal((await wait(base, root, bad.submissionId)).status, "unanswered");
  const failed = await changedUntil(used, r => r.attention.some(a => a.status === "stopped"));
  assert.deepEqual(failed.attention, [{ title: "main", status: "stopped", reason: "model error" }]);
  assert.equal(failed.context.reportedTokens, undefined);
  const sleep = await submit(base, root, "!sleep 3500", "intro-sleep");
  const parked = await changedUntil(failed, r => r.attention.some(a => a.status === "parked"));
  assert.ok(parked.attention[0].untilMs > Date.now());
  assert.equal(parked.sessions.quiet, 0);
  assert.equal(watches, 1);
  const current = ok(await another.send("ext.intro.get", scope));
  const waiting = another.send("ext.intro.get", { ...scope, after: current.revision });
  waiting.catch(() => {});
  another.close();
  await another.ended;
  await base.send("thread.abort", scope);
  await wait(base, root, sleep.submissionId);
});

test("intro v2 names threads and scopes recovery to a settled failure", { timeout: 10_000 }, async t => {
  const { config } = await layout(t, { idleMs: 10_000 });
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
  const first = await reportUntil(r => r.thread?.issue && r.attention[0]?.title === "Fix the café picker with details", scope);
  const issue = first.thread.issue;
  assert.equal(first.thread.id, threadId);
  assert.equal(first.thread.title, "Fix the café picker with details");
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

test("config defaults follow the effective agent directory and explicit storage still wins", async t => {
  const { config, dir } = await layout(t);
  const path = join(dir, "backend.json");
  const other = join(dir, "other-agent");
  const explicit = join(dir, "explicit-data");
  await json(path, { "agent-dir": config.agentDir });
  const fromFile = await parseConfig(["--config", path]);
  assert.equal(fromFile.dataDir, join(config.agentDir, "hush"));
  const fromFlag = await parseConfig(["--config", path, "--agent-dir", other]);
  assert.equal(fromFlag.dataDir, join(other, "hush"));
  assert.equal(fromFlag.socket, join(other, "hush", "hush.sock"));
  await json(path, { "agent-dir": config.agentDir, "data-dir": explicit });
  const pinned = await parseConfig(["--config", path, "--agent-dir", other]);
  assert.equal(pinned.dataDir, explicit);
  const flag = await parseConfig(["--config", path, "--data-dir", config.dataDir]);
  assert.equal(flag.dataDir, config.dataDir);
  const ephemeral = await parseConfig(["--agent-dir", other, "--no-session", "--socket", config.socket]);
  assert.equal(ephemeral.dataDir, undefined);
  assert.equal(ephemeral.socket, config.socket);
});

test("distinct agent directories without --data-dir get independent live backends and histories", { timeout: 14000 }, async t => {
  const { config, dir } = await layout(t, { idleMs: 6000 });
  const envAgent = join(dir, "env-agent");
  const variant = suffix => {
    const agentDir = join(dir, "agent-" + suffix);
    return { ...config, agentDir, dataDir: undefined, socket: join(agentDir, "hush", "hush.sock") };
  };
  const a = variant("a"), b = variant("b");
  await Promise.all([envAgent, a.agentDir, b.agentDir].map(path => mkdir(path, { mode: 0o700 })));
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
