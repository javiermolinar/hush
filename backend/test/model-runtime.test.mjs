import assert from "node:assert/strict";
import { once } from "node:events";
import { chmod, mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { InMemoryCredentialStore } from "@earendil-works/pi-ai";
import { dial } from "../../server/test/client.mjs";
import { startBackend } from "../../dist/hush-backend.mjs";
import { standaloneModelDefault } from "../../dist/backend/src/standalone-settings.js";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { createHushModels } from "../../dist/backend/src/models.js";
import { hushCredentialStore } from "../../dist/backend/src/credentials.js";

const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const save = (path, value) => writeFile(path, JSON.stringify(value), { mode: 0o600 });
const custom = id => ({ id, name: id, reasoning: true, contextWindow: 120_000, maxTokens: 4096 });

test("private model overlays retain builtin auth, thinking and safe configured alternatives", async t => {
  const dir = await mkdtemp("/tmp/hush-modelfile-");
  t.after(() => rm(dir, { recursive: true, force: true }));
  const path = join(dir, "models.json");
  const original = process.env.HUSH_TEST_MODEL_KEY;
  process.env.HUSH_TEST_MODEL_KEY = "fixture-alternative";
  t.after(() => {
    if (original === undefined) delete process.env.HUSH_TEST_MODEL_KEY;
    else process.env.HUSH_TEST_MODEL_KEY = original;
  });
  const { credentials } = await hushCredentialStore(dir, { createFile: true });
  const load = () => createHushModels(credentials, path);
  await save(path, { providers: {
    "openai-codex": { modelOverrides: { "gpt-6-sol": { name: "Pinned label", thinkingLevelMap: { high: null }, cost: { input: 1 } } } },
    fixture: { apiKey: "${HUSH_TEST_MODEL_KEY}", headers: { "X-Source": "local-fixture" },
      baseUrl: "http://127.0.0.1:1/v1", api: "openai-completions", models: [{ id: "test" }] },
  } });
  const runtime = await load();
  assert.equal(runtime.getProvider("openai-codex").auth.oauth.login instanceof Function, true);
  const overridden = runtime.getModel("openai-codex", "gpt-6-sol");
  assert.equal(overridden.name, "Pinned label");
  const baseline = await createHushModels(credentials);
  assert.deepEqual(overridden.thinkingLevelMap, { ...baseline.getModel("openai-codex", "gpt-6-sol").thinkingLevelMap, high: null });
  assert.equal(overridden.cost.input, 1);
  const model = runtime.getModel("fixture", "test");
  assert.equal(model.contextWindow, 128_000);
  assert.equal(model.maxTokens, 16_384);
  assert.equal((await runtime.getAuth(model)).auth.headers["X-Source"], "local-fixture");
  assert.equal((await runtime.getAuth("fixture")).auth.apiKey, "fixture-alternative");
  await credentials.modify("fixture", async () => ({ type: "api_key", key: "$FIXTURE_TOKEN", env: { FIXTURE_TOKEN: "fixture-local-record" } }));
  assert.equal((await runtime.getAuth("fixture")).auth.apiKey, "fixture-local-record");
  await credentials.delete("fixture");
  assert.equal((await runtime.getAuth("fixture")).auth.apiKey, "fixture-alternative");
  await chmod(path, 0o644);
  await assert.rejects(load, { code: "configuration" });
  await chmod(path, 0o600);
  await save(path, { providers: { fixture: { apiKey: "!do-not-run", baseUrl: "http://127.0.0.1:1/v1",
    api: "openai-completions", models: [custom("test")] } } });
  await assert.rejects(load, { code: "configuration" });
});

test("overlay fields merge; provider api is scoped to explicit model definitions", async t => {
  const dir = await mkdtemp("/tmp/hush-merge-models-");
  t.after(() => rm(dir, { recursive: true, force: true }));
  const path = join(dir, "models.json");
  const baseline = await createHushModels(new InMemoryCredentialStore());
  const provider = "amazon-bedrock", modelId = "anthropic.claude-fable-5";
  const original = baseline.getModel(provider, modelId);
  await save(path, { providers: {
    [provider]: { modelOverrides: { [modelId]: { thinkingLevelMap: { off: null } } } },
    anthropic: { api: "openai-completions", models: [{ id: "my-proxy-model" }] },
    fixture: {
      api: "openai-completions", baseUrl: "https://fixture.invalid/v1",
      compat: { openRouterRouting: { only: ["first"] }, chatTemplateKwargs: { first: true } },
      models: [{ id: "chat", inputLimits: { maxRequestBytes: 100_000, images: { maxPerMessage: 2, resize: { maxWidth: 512, maxHeight: 1024 } } },
        promptCache: { short: 60, long: 600 }, samplingParams: { temperature: 0.3, top_p: 0.9 },
        samplingParamsByThinkingLevel: { off: { a: 1 }, xhigh: { a: 1, b: 2 } },
        compat: { openRouterRouting: { order: ["first"] }, chatTemplateKwargs: { second: true } } }],
      modelOverrides: { chat: { inputLimits: { images: { maxPerRequest: 4, resize: { maxWidth: 128 } } },
        promptCache: { short: 30 }, samplingParams: { temperature: 0 }, samplingParamsByThinkingLevel: { xhigh: { a: null } },
        compat: { openRouterRouting: { allow_fallbacks: false }, chatTemplateKwargs: { second: null } } } },
    },
  } });
  const runtime = await createHushModels(new InMemoryCredentialStore(), path);
  assert.deepEqual(runtime.getModel(provider, modelId).thinkingLevelMap, { ...original.thinkingLevelMap, off: null });
  assert.deepEqual(standaloneModelDefault({ defaultProvider: provider, defaultModel: modelId, defaultThinkingLevel: "xhigh" }, runtime),
    { model: { provider, modelId }, thinkingLevel: "xhigh" });
  assert.equal(runtime.getModel("anthropic", "my-proxy-model").api, "openai-completions");
  for (const entry of baseline.getModels("anthropic")) {
    assert.equal(runtime.getModel("anthropic", entry.id).api, entry.api);
  }
  const model = runtime.getModel("fixture", "chat");
  assert.deepEqual(model.inputLimits, { maxRequestBytes: 100_000,
    images: { maxPerMessage: 2, maxPerRequest: 4, resize: { maxWidth: 128, maxHeight: 1024 } } });
  assert.deepEqual(model.promptCache, { short: 30, long: 600 });
  assert.deepEqual(model.samplingParams, { temperature: 0, top_p: 0.9 });
  assert.deepEqual(model.samplingParamsByThinkingLevel, { off: { a: 1 }, xhigh: { a: null, b: 2 } });
  assert.deepEqual(model.compat, {
    openRouterRouting: { only: ["first"], order: ["first"], allow_fallbacks: false },
    chatTemplateKwargs: { first: true, second: null },
  });
});

test("private models.json accepts comments; diagnostics identify fields without echoing their values or IDs", async t => {
  const dir = await mkdtemp("/tmp/hush-jsonc-models-");
  t.after(() => rm(dir, { recursive: true, force: true }));
  const path = join(dir, "models.json");
  const url = "https://fixture.invalid/v1//path";
  await writeFile(path, `\ufeff{ /* note */ "providers": { // note\r\n "fixture": {
    "baseUrl": ${JSON.stringify(url)}, "api": "openai-completions",
    "models": [{ "id": "chat", "name": ${JSON.stringify('quoted "//" /* text */')} }]
  } } } // eof`, { mode: 0o600 });
  const load = () => createHushModels(new InMemoryCredentialStore(), path);
  const model = (await load()).getModel("fixture", "chat");
  assert.equal(model.baseUrl, url);
  assert.equal(model.name, 'quoted "//" /* text */');
  for (const [body, message] of [
    ["{ /* not closed", /malformed JSON/],
    ['{"providers": {"hidden-fixture": {"oauth": "radius"}}}', /providers\.\*\.oauth is not supported/],
    ['{"providers": {"hidden-fixture": {"modelOverrides":{"hidden-fixture":{"contextWindow":150000.5}}}}}',
      /providers\.\*\.modelOverrides\.\*\.contextWindow/],
    ['{"providers": {"hidden-fixture": {"baseUrl":"https://hidden-fixture:secret@fixture.invalid"}}}', /providers\.\*\.baseUrl/],
  ]) {
    await writeFile(path, body);
    await assert.rejects(load, error => {
      assert.equal(error.code, "configuration");
      assert.match(error.message, message);
      assert.doesNotMatch(error.message, /hidden-fixture|150000|secret/);
      return true;
    });
  }
});

test("an invalid overlay preserves history and receipts without accepting work until restart", { timeout: 12_000 }, async t => {
  const dir = await realpath(await mkdtemp("/tmp/hush-bad-models-"));
  const agentDir = join(dir, "profile"), dataDir = join(dir, "runtime"), socket = join(dataDir, "hush.sock");
  const path = join(agentDir, "models.json"), settings = join(agentDir, "settings.json");
  let app, wire;
  const close = async () => { wire?.close(); await app?.close(); };
  t.after(async () => { await close(); await rm(dir, { recursive: true, force: true }); });
  await mkdir(agentDir, { mode: 0o700 });
  await new WorkspaceTrust(agentDir).change(dir, "grant", AbortSignal.timeout(5_000));
  const config = { agentDir, dataDir, socket, cwd: dir, idleMs: 10_000, ephemeral: false };
  const boot = async faux => {
    app = await startBackend({ ...config, faux });
    wire = await dial(socket);
    return ok(await wire.send("hello", { versions: [1], capabilities: {
      storeIdentity: [1], threadDirectory: [2], modelCatalog: [1], settings: [1], schedules: [1], checkpoints: [1],
    } }));
  };
  await boot(true);
  const threadId = String(app.root.id);
  const history = () => wire.send("history", { threadId, limit: 20, range: { kind: "before", beforeEntryId: null } }).then(ok);
  const input = { threadId, text: "existing history", requestId: "existing", whenBusy: "reject" };
  const receipt = ok(await wire.send("submit", input));
  assert.equal(ok(await wire.send("submission.wait", { threadId, ...receipt })).status, "done");
  const before = await history(), identity = ok(await wire.send("ext.storeIdentity.get", {}));
  await close();

  await save(settings, { defaultProvider: "fixture", defaultModel: "chat", defaultThinkingLevel: "off" });
  const saved = await readFile(settings);
  await writeFile(path, '{ "providers": { "fixture": { "oauth": "radius" } } }', { mode: 0o600 });
  const { capabilities } = await boot(false);
  assert.deepEqual(capabilities, { storeIdentity: 1, threadDirectory: 2 });
  assert.equal(app.runtime.getModels().length, 0);
  assert.deepEqual(ok(await wire.send("ext.storeIdentity.get", {})), identity);
  assert.deepEqual(await history(), before);
  assert.deepEqual(ok(await wire.send("submit", input)), receipt);
  assert.equal(ok(await wire.send("submission.get", { threadId, ...receipt })).status, "done");
  let rows;
  for (let i = 0; i < 20; i++) {
    rows = ok(await wire.send("ext.threadDirectory.list", { limit: 10 }));
    if (!rows.indexing) break;
    await new Promise(resolve => setTimeout(resolve, 5));
  }
  assert.ok(rows.threads.some(row => row.id === threadId && row.title));
  for (const [method, params] of [
    ["submit", { ...input, requestId: "not-admitted" }], ["thread.new", {}], ["thread.compact", { threadId }],
    ["ext.threadDirectory.new", { worktree: true }],
  ]) {
    const refused = await wire.send(method, params);
    assert.equal(refused.error.code, "invalid_request");
    assert.match(refused.error.message, /models\.json.*providers\.\*\.oauth/);
  }
  assert.deepEqual(await history(), before);
  assert.deepEqual(await readFile(settings), saved);
  assert.match(await readFile(join(dataDir, "hush.log"), "utf8"), /models\.json: .*providers\.\*\.oauth/);
  await close();
  await save(path, { providers: { fixture: { api: "openai-completions", baseUrl: "http://127.0.0.1:1/v1", models: [{ id: "chat" }] } } });
  assert.equal((await boot(false)).capabilities.settings, 1);
  assert.deepEqual(ok(await wire.send("ext.storeIdentity.get", {})), identity);
  assert.deepEqual(await history(), before);
});

test("standalone uses provider context usage and Durable's committed compaction boundary", { timeout: 12_000 }, async t => {
  const requests = [];
  const provider = createServer(async (req, res) => {
    const parts = [];
    for await (const part of req) parts.push(part);
    const call = JSON.parse(Buffer.concat(parts));
    requests.push(call);
    const chunk = (choice, usage) => "data: " + JSON.stringify({ id: "fixture", object: "chat.completion.chunk",
      created: 1, model: call.model, choices: choice ? [{ index: 0, ...choice }] : [], ...(usage && { usage }) }) + "\n\n";
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.end(chunk({ delta: { content: "verified fixture reply" }, finish_reason: null }) + chunk({ delta: {}, finish_reason: "stop" }) +
      chunk(null, { prompt_tokens: 119, completion_tokens: 4, total_tokens: 123, prompt_tokens_details: { cached_tokens: 20 } }) +
      "data: [DONE]\n\n");
  }).listen(0, "127.0.0.1");
  await once(provider, "listening");
  const dir = await realpath(await mkdtemp("/tmp/hush-context-"));
  const agentDir = join(dir, "profile"), dataDir = join(dir, "runtime"), socket = join(dataDir, "hush.sock");
  let app, wire;
  t.after(async () => {
    wire?.close();
    try { await app?.close(); }
    finally {
      provider.closeAllConnections();
      await new Promise(resolve => provider.close(resolve));
      await rm(dir, { recursive: true, force: true });
    }
  });
  await mkdir(agentDir, { mode: 0o700 });
  await new WorkspaceTrust(agentDir).change(dir, "grant", AbortSignal.timeout(5000));
  await save(join(agentDir, "auth.json"), { fixture: { type: "api_key", key: "fixture-key" } });
  await save(join(agentDir, "models.json"), { providers: { fixture: {
    baseUrl: `http://127.0.0.1:${provider.address().port}/v1`, api: "openai-completions", models: [custom("chat")],
  } } });
  const options = { agentDir, dataDir, socket, cwd: dir, idleMs: 10_000,
    model: { provider: "fixture", modelId: "chat" }, faux: false, ephemeral: false };
  const connect = async () => {
    app = await startBackend(options);
    wire = await dial(socket);
    ok(await wire.send("hello", { versions: [1], capabilities: { intro: [2], storeIdentity: [1] } }));
    return ok(await wire.send("ext.storeIdentity.get", {}));
  };
  const identity = await connect();
  const threadId = String(app.root.id), scope = { threadId };
  const ask = async (text, requestId) => {
    const { submissionId } = ok(await wire.send("submit", { threadId, text, requestId, whenBusy: "reject" }));
    assert.equal(ok(await wire.send("submission.wait", { threadId, submissionId })).status, "done");
  };
  const context = async () => ok(await wire.send("ext.intro.get", scope)).context;
  assert.equal((await context()).reportedTokens, undefined);
  await ask("First fixture turn", "first");
  assert.equal((await context()).reportedTokens, 123);
  // Leave enough real text for the existing 20k keep-recent policy. The
  // provider reports small usage to prevent an autonomous threshold cut.
  await ask("latest " + "detail ".repeat(16_000), "second");
  const { taskId } = ok(await wire.send("thread.compact", scope));
  assert.ok(taskId);
  ok(await wire.send("thread.wait", scope));
  const history = async () => ok(await wire.send("history", { threadId, limit: 20, range: { kind: "before", beforeEntryId: null } })).entries;
  assert.ok((await history()).some(entry => entry.kind === "pi.compaction"), "Durable did not place the summary");
  assert.equal((await context()).reportedTokens, undefined, "pre-cut usage is not current usage");
  wire.close();
  await app.close();
  assert.deepEqual(await connect(), identity);
  assert.equal(String(app.root.id), threadId);
  assert.ok((await history()).some(entry => entry.kind === "pi.compaction"));
  const current = await app.root.context(BACKGROUND_CONTEXT);
  assert.equal(current.head?.kind, "pi.compaction");
  assert.ok(requests.length > 2);
});
