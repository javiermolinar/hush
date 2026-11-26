// Exercise public Pi Durable APIs only. This is not a Hush worker service.
import assert from "node:assert/strict";
import { BACKGROUND_CONTEXT as ctx } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai";
import { createProvider } from "@earendil-works/pi-ai/models";
import { createFauxCore, fauxAssistantMessage, fauxToolCall } from "@earendil-works/pi-ai/providers/faux";
import { AssistantEntry, configure, createRegistry, defineDoc, defineExtension, defineSignal, defineTask, defineTool,
  Harness, LiveDoc, SubmissionSettled } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";

const [database, phase] = process.argv.slice(2);
// Keep IPC referenced at the kill boundaries, not Durable's scheduler.
process.on("message", () => {});
const State = defineDoc({ kind: "fixture.assignment", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true });
const ready = defineSignal({ name: "fixture.release", version: 1, schema: { type: "boolean" } });
const result = defineSignal({ name: "fixture.result", version: 1, schema: { type: "integer" } });
const notice = "fixture.worker-notice";
const none = { type: "object", additionalProperties: false, properties: {} };
const output = text => ({ output: [{ type: "text", text }] });

const releaseTool = defineTool({ name: "fixture_release", description: "Wait for a fixture signal.", parameters: none, replay: "safe",
  async execute(_args, api) {
    if (!api.wake) return { sleep: { signals: [ready.key("release")] } };
    return output("released");
  },
});
const childTools = defineExtension({ name: "fixture-child", tools: [releaseTool] });
const assignment = defineTask({ name: "fixture.assignment", version: 1, initial: () => ({ phase: "open" }), phases: {
  open: (task, runtime, context) => runtime.commit(async tx => {
    const row = (await tx.doc(State, runtime.conversationId)).row;
    assert.equal(row.task, task.id);
    const child = await tx.createConversation({ ownership: { kind: "task", taskId: task.id } });
    await configure(tx, child.id, { extensions: [childTools], tools: [releaseTool], modelTools: [releaseTool] });
    row.child = child.id;
    return { status: "running", checkpoint: { phase: "submit", child: child.id } };
  }, context),
  submit: async (task, runtime, context) => {
    const child = await runtime.conversation(task.state.checkpoint.child, context);
    return runtime.commit(async tx => {
      const row = (await tx.doc(State, runtime.conversationId)).row;
      row.submission = await child.submit(tx, { type: "input", content: "child-only brief", requestId: "child-input",
        notify: { taskId: task.id, key: "settled" } });
      return { status: "sleeping", signals: [SubmissionSettled.key("settled")], checkpoint: { phase: "settle" } };
    }, context);
  },
  settle: (task, runtime, context) => runtime.commit(async tx => {
    const row = (await tx.doc(State, runtime.conversationId)).row;
    assert.equal(row.task, task.id);
    const submission = await tx.submission(row.submission);
    assert.equal(submission.conversationId, row.child);
    assert.equal(submission.status, "done");
    const entry = await tx.entry(AssistantEntry, submission.answer);
    assert.equal(entry.conversationId, row.child);
    const answer = entry.model[0].content.map(block => block.text).join("");
    assert.equal(answer, "child evidence");
    assert.equal(row.answer, undefined);
    if (row.join !== undefined) assert.equal(await tx.signal(row.join, result, "settled", entry.id), "delivered");
    row.answer = answer;
    await tx.appendEntry(runtime.conversationId, { kind: notice, data: { task: task.id, child: row.child, answer } });
    return { status: "terminal", outcome: { status: "completed", result: entry.id } };
  }, context),
} });
const spawn = defineTool({ name: "fixture_spawn", description: "Reserve one synthetic assignment.", parameters: none, replay: "safe",
  execute: (_args, api, context) => api.commit(async tx => {
    const state = await tx.doc(State, api.conversationId);
    state.row ??= { task: await tx.createTask(assignment, null, { ownership: { kind: "conversation" }, background: true }) };
    return output(String(state.row.task));
  }, context),
});
const join = defineTool({ name: "fixture_join", description: "Join the synthetic assignment.", parameters: {
  type: "object", required: ["handle"], properties: { handle: { type: "integer" } }, additionalProperties: false,
}, replay: "safe",
  async execute({ handle }, api, context) {
    const answer = await api.commit(async tx => {
      const row = (await tx.doc(State, api.conversationId)).row;
      assert.equal(row.task, handle);
      if (row.answer !== undefined) return row.answer;
      row.join = api.taskId;
    }, context);
    if (answer === undefined) return { sleep: { signals: [result.key("settled")] } };
    if (phase === "settle") {
      // Kill the fixture after the outcome/notice commit but before it can
      // commit the join's result. The replaying join only reads the receipt.
      process.send({ boundary: "join-result-not-committed" });
      await new Promise(() => {});
    }
    return output(answer);
  },
});

const faux = createFauxCore({}), models = createModels(), registry = createRegistry();
let serving = 0, peak = 0;
const stream = (...args) => {
  serving++;
  peak = Math.max(peak, serving);
  const events = faux.stream(...args);
  // Account for both simultaneous requests and requests left open during a
  // park. The probe below must start with all previously open slots released.
  events.result().then(() => { serving--; });
  return events;
};
models.setProvider(createProvider({ id: faux.provider, models: faux.models,
  auth: { apiKey: { name: "Faux", resolve: async () => ({ auth: {} }) } }, api: { stream, streamSimple: stream } }));
registry.install(childTools);
const parent = defineExtension({ name: "fixture-parent", tools: [spawn, join], tasks: [assignment] });
registry.install(parent);
const calls = [];
const tool = (name, args = {}) => fauxAssistantMessage(fauxToolCall(name, args, { id: name }), { stopReason: "toolUse" });
const route = ({ messages }) => {
  const user = messages.filter(m => m.role === "user").map(m => m.content);
  assert.equal(user.length, 1, "notice must not become model input");
  const last = messages.findLast(m => m.role !== "system");
  const ask = typeof user[0] === "string" ? user[0] : user[0][0].text;
  calls.push(last.role === "toolResult" ? last.toolName : ask);
  if (last.role !== "toolResult") {
    if (ask === "parent brief") return tool(spawn.name);
    if (ask === "child-only brief") return tool(releaseTool.name);
    assert.equal(ask, "provider slot probe");
    return fauxAssistantMessage("capacity available");
  }
  if (last.toolName === spawn.name) return tool(join.name, { handle: Number(last.content[0].text) });
  if (last.toolName === releaseTool.name) return fauxAssistantMessage("child evidence");
  assert.equal(last.toolName, join.name);
  assert.equal(last.content[0].text, "child evidence");
  return fauxAssistantMessage("parent synthesis");
};
faux.setResponses(Array(8).fill(route));

let wake;
let onQuiet;
const h = await Harness.open(await openNodeSqliteStorage(database), { models, registry, onWake(value) {
  wake = value;
  if (value.active === false && value.at === undefined) onQuiet?.();
}, settings: { retry: { enabled: false }, compaction: { enabled: false } } }, ctx);
const root = await h.root(ctx, { agent: { model: { provider: "faux", modelId: "faux-1" }, extensions: [parent] } });
const row = () => h.snapshot(State, root.id, ctx).then(s => s.row);
const entries = () => root.entries({}, 32, undefined, ctx).then(page => page.items);
if (phase === "park") {
  const asleep = Promise.withResolvers();
  const watched = await h.observeTaskGraph({ advance({ value }) {
    if (Object.values(value.tasks).filter(task => task.state.status === "sleeping").length === 3) asleep.resolve();
  }, end() {} }, ctx);
  const submitted = await root.submit({ type: "input", content: "parent brief", requestId: "parent-input" }, ctx);
  await asleep.promise;
  if (wake.active) await new Promise(resolve => { onQuiet = resolve; });
  watched.detach();
  assert.deepEqual(wake, { active: false });
  assert.equal(serving, 0);
  const live = await h.snapshot(LiveDoc, root.id, ctx);
  assert.equal(live.tools[0].name, join.name);
  assert.equal(live.tools[0].status, "sleeping");
  assert.equal((await submitted.status(ctx)).entry, live.run.first);
  const probe = await h.createConversation({ ownership: { kind: "ownerless" }, agent: { model: { provider: "faux", modelId: "faux-1" } } }, ctx);
  assert.equal((await (await probe.submit({ type: "input", content: "provider slot probe" }, ctx)).wait(ctx)).status, "done");
  assert.deepEqual(calls.toSorted(), ["child-only brief", "fixture_spawn", "parent brief", "provider slot probe"]);
  assert.equal(serving, 0);
  assert.ok(peak > 0);
  process.send({ boundary: "parked", parentEntry: live.run.first, row: await row(), submissionId: submitted.id });
} else if (phase === "settle") {
  const saved = await row();
  const child = await h.snapshot(LiveDoc, saved.child, ctx);
  assert.equal(child.tools[0].status, "sleeping");
  assert.equal((await h.snapshot(LiveDoc, root.id, ctx)).tools[0].status, "sleeping");
  assert.deepEqual(calls, []);
  const posted = Promise.withResolvers();
  const watch = await h.observeConversation(root.id, { advance({ value }) {
    const found = value.entries.find(entry => entry.kind === notice);
    if (found) posted.resolve(found);
  }, end() {} }, ctx);
  assert.ok(!watch.value.entries.some(entry => entry.kind === notice));
  assert.equal(await h.signal(child.tools[0].taskId, ready, "release", true, ctx), "delivered");
  const entry = await posted.promise;
  assert.equal(entry.model, undefined);
  assert.equal(entry.data.child, saved.child);
  assert.equal((await h.getTask(saved.task, ctx)).state.status, "terminal");
  assert.equal((await row()).answer, "child evidence");
  assert.deepEqual(calls, ["fixture_release"]);
  watch.detach();
  process.send({ boundary: "settled", noticeId: entry.id });
} else {
  h.resume();
  const original = await h.commit(tx => tx.submissionByRequest(root.id, "parent-input"), ctx);
  const durable = await h.submission(original.id, ctx);
  const settled = await durable.wait(ctx);
  await root.waitForIdle(ctx);
  assert.equal(settled.status, "done");
  const history = await entries();
  assert.equal(history.filter(entry => entry.kind === "pi.user").length, 1);
  const notices = history.filter(entry => entry.kind === notice);
  assert.equal(notices.length, 1);
  assert.equal(history.filter(entry => entry.kind === "pi.tool-result").length, 2);
  assert.equal(history.find(entry => entry.id === settled.answer).model[0].content[0].text, "parent synthesis");
  assert.deepEqual(calls, phase === "resume" ? ["fixture_join"] : []);
  assert.equal(serving, 0);
  const saved = await row();
  const child = await h.conversation(saved.child, ctx);
  assert.equal((await child.entries({}, 32, undefined, ctx)).items.filter(entry => entry.kind === "pi.user").length, 1);
  await h.close(ctx);
  process.send({ boundary: "finished", noticeId: notices[0].id, submissionId: durable.id, parentEntry: settled.entry, row: saved });
  process.disconnect();
}
