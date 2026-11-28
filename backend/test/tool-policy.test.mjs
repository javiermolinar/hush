import assert from "node:assert/strict";
import { mkdir, mkdtemp, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT as ctx } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai";
import { fauxAssistantMessage, fauxProvider, fauxToolCall } from "@earendil-works/pi-ai/providers/faux";
import { configure, createRegistry, defineExtension, defineSignal, defineTask, defineTool, Harness, ToolResultEntry } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { dial } from "../../server/test/client.mjs";
import { bindExecutionWorkspace } from "../../dist/backend/src/execution-workspace.js";
import { admitToolProfile, ToolPolicy } from "../../dist/backend/src/tool-policy.js";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { startBackend } from "../../dist/hush-backend.mjs";
import { call, createRequest, draftArgs, planProvider } from "./plan-provider.mjs";

const ok = value => { assert.equal(value.ok, true, JSON.stringify(value)); return value.result; };
const output = text => ({ output: [{ type: "text", text }] });

async function workspace(t) {
  const dir = await realpath(await mkdtemp("/tmp/hush-tool-policy-"));
  t.after(() => rm(dir, { force: true, recursive: true }));
  const cwd = join(dir, "workspace"), other = join(dir, "other"), agentDir = join(dir, ".hush"), dataDir = join(dir, "data");
  for (const path of [cwd, other, agentDir]) await mkdir(path, { mode: 0o700 });
  const trust = new WorkspaceTrust(agentDir);
  const grant = path => trust.change(path, "grant", AbortSignal.timeout(5_000));
  await Promise.all([grant(cwd), grant(other)]);
  await writeFile(join(cwd, "allowed.txt"), "allowed fixture text\n");
  await writeFile(join(cwd, "other.txt"), "in workspace but outside the claim\n");
  await writeFile(join(other, "allowed.txt"), "different checkout\n");
  await symlink(join(other, "allowed.txt"), join(cwd, "link.txt"));
  return { cwd, other, agentDir, dataDir, trust, dir };
}

// Dispatch real nested pi.tool tasks and park a replay-safe call after its
// execution intent. Presentation is deliberately broad in this fixture.
test("tool execution intersects the stored profile with direct and nested calls after restart", { timeout: 10_000 }, async t => {
  const { dir, cwd, other, trust, agentDir } = await workspace(t);
  const database = join(dir, "policy.sqlite"), ref = { provider: "faux", modelId: "faux-1" };
  const release = defineSignal({ name: "fixture.policy-wake", version: 1, schema: { type: "boolean" } });
  const anchor = defineTask({ name: "fixture.policy-owner", version: 1, initial: () => ({ phase: "hold" }), phases: {} });
  const args = { type: "object", additionalProperties: false, properties: { path: { type: "string" } } };
  const bad = [];
  const sentinel = name => defineTool({ name, description: "Fixture definition must not execute here.", parameters: args,
    ...(["read", "inspect_file"].includes(name) && { structuredOutputSchema: { type: "string" } }),
    async execute() { bad.push(name); return output("forbidden body ran"); } });
  let invocations = 0;
  const driver = defineTool({ name: "codemode", description: "Fixture nested dispatcher.", parameters: args, replay: "safe",
    async execute({ path }, api, context) {
      invocations++;
      if (!api.wake && path === "park") return { sleep: { signals: [release.key("wake")] } };
      const run = (name, path, key) => api.executeTool(name, { path }, context, { key });
      const direct = await run("inspect_file", path, "read");
      const denied = await run("bash", path, "forbidden");
      return output(JSON.stringify({ value: direct.structuredOutput, error: !!direct.isError, denied: !!denied.isError }));
    },
  });
  const ext = defineExtension({ name: "fixture-policy-tools", tools: [driver, ...["read", "inspect_file", "bash", "write", "external", "plan_save"].map(sentinel)] });
  const refFor = name => fauxToolCall(name, { path: name === "codemode" ? "allowed.txt" : "other.txt" }, { id: name });
  const route = ({ messages }) => {
    const last = messages.findLast(m => m.role !== "system");
    if (last.role === "toolResult") return fauxAssistantMessage("done");
    const text = typeof last.content === "string" ? last.content : last.content[0].text;
    return fauxAssistantMessage(text === "park" ? fauxToolCall("codemode", { path: "park" }, { id: "park" })
      : [refFor("read"), refFor("codemode"), refFor("bash"), refFor("write"), refFor("external"), refFor("plan_save"),
        fauxToolCall("read", { path: "link.txt" }, { id: "link" })], { stopReason: "toolUse" });
  };
  let h;
  t.after(() => h?.close(ctx));
  const open = async () => {
    const faux = fauxProvider(), models = createModels(), registry = createRegistry();
    faux.setResponses(Array(20).fill(route));
    models.setProvider(faux.provider);
    const policy = new ToolPolicy(() => h, trust, value => value ?? cwd, [agentDir]);
    registry.install(policy.protect(ext));
    h = await Harness.open(await openNodeSqliteStorage(database), { models, registry, settings: { compaction: { enabled: false } } }, ctx);
    return h.root(ctx, { agent: { cwd, model: ref, extensions: [ext] } });
  };
  let root = await open();
  const binding = await bindExecutionWorkspace(trust, cwd);
  const child = async profile => h.conversation(await root.commit(async tx => {
    const taskId = await tx.createTask(anchor, null, { ownership: { kind: "conversation" }, background: true });
    const child = await tx.createConversation({ ownership: { kind: "task", taskId } });
    await admitToolProfile(tx, child, profile);
    return child.id;
  }, ctx), ctx);
  const reader = await child({ kind: "read-only", workspace: binding });
  const worker = await child({ kind: "worker", workspace: binding, paths: ["allowed.txt", "link.txt"] });
  const history = conversation => conversation.entries({}, 64, undefined, ctx).then(p => p.items.filter(e => ToolResultEntry.is(e)).map(e => e.model[0]));
  for (const [actor, outsideClaim] of [[reader, false], [worker, true]]) {
    assert.equal((await (await actor.submit({ type: "input", content: "read", requestId: "initial" }, ctx)).wait(ctx)).status, "done");
    const results = new Map((await history(actor)).map(r => [r.toolCallId, r]));
    assert.equal(!!results.get("read").isError, outsideClaim);
    for (const name of ["link", "bash", "write", "external", "plan_save"]) assert.equal(results.get(name).isError, true, name);
    assert.deepEqual(JSON.parse(results.get("codemode").content[0].text), { value: "allowed fixture text\n", error: false, denied: true });
  }
  assert.deepEqual(bad, []);
  const asleep = Promise.withResolvers();
  const watch = await h.observeTaskGraph({ advance({ value }) {
    for (const task of Object.values(value.tasks)) if (task.conversationId === worker.id && task.state.status === "sleeping") asleep.resolve(task.id);
  }, end() {} }, ctx);
  const pending = await worker.submit({ type: "input", content: "park", requestId: "parked" }, ctx);
  const parked = await asleep.promise;
  watch.detach();
  assert.equal(invocations, 3);
  await h.close(ctx);
  root = await open();
  // Even a host-side reconfiguration to another trusted checkout cannot
  // rebind the assigned profile when the parked tool resumes.
  await h.commit(tx => configure(tx, worker.id, { cwd: other }), ctx);
  assert.equal(await h.signal(parked, release, "wake", true, ctx), "delivered");
  assert.equal((await (await h.submission(pending.id, ctx)).wait(ctx)).status, "done");
  const after = await h.conversation(worker.id, ctx);
  const parkedResult = (await history(after)).find(x => x.toolCallId === "park");
  assert.equal(parkedResult.isError, true);
  assert.doesNotMatch(JSON.stringify(parkedResult), /different checkout/);
  assert.equal(invocations, 3, "the woken implementation never ran");
  assert.deepEqual(bad, []);
});

test("a real Codemode script can inspect only its worker's admitted files", { timeout: 12_000 }, async t => {
  const { cwd, agentDir, dataDir, trust } = await workspace(t);
  await writeFile(join(agentDir, "settings.json"), '{"codemode":true}', { mode: 0o600 });
  const script = `const [allowed, outside] = await Promise.allSettled(
    ['allowed.txt', 'other.txt'].map(path => tools.inspect_file({path})));
    return [allowed.value, outside.status];`;
  const provider = await planProvider(t, agentDir, ({ results }) => results.length ? "done" : [call("codemode", { code: script })]);
  const backend = await startBackend({ cwd, agentDir, dataDir, socket: join(dataDir, "hush.sock"), model: provider.model,
    faux: false, ephemeral: false, idleMs: 60_000 });
  t.after(() => backend.close());
  const binding = await bindExecutionWorkspace(trust, cwd);
  const anchor = defineTask({ name: "fixture.codemode-owner", version: 1, initial: () => ({ phase: "hold" }), phases: {} });
  const child = await backend.harness.conversation(await backend.root.commit(async tx => {
    const taskId = await tx.createTask(anchor, null, { ownership: { kind: "conversation" }, background: true });
    const child = await tx.createConversation({ ownership: { kind: "task", taskId } });
    await admitToolProfile(tx, child, { kind: "worker", workspace: binding, paths: ["allowed.txt"] });
    await configure(tx, child.id, { extensions: [{ name: "codemode" }], tools: null, modelTools: null });
    return child.id;
  }, ctx), ctx);
  assert.equal((await (await child.submit({ type: "input", content: "Inspect the assigned text" }, ctx)).wait(ctx)).status, "done");
  const results = provider.requests.flatMap(r => r.results);
  assert.equal(results.length, 1);
  assert.match(results[0].content, /allowed fixture text.*rejected/s);
  assert.doesNotMatch(results[0].content, /outside the claim/);
});
