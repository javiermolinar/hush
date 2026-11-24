import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fauxAssistantMessage } from "@earendil-works/pi-ai";
import { ConversationBusy } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { ConversationIdle, ctx, InboxDoc, openDurable, toolRound } from "./durable-probe.mjs";
import { LocalQueue, PendingInputs } from "./local-queue.mts";
import { ordered, pi, textOf } from "./reference-probe.mjs";
import { commands, events, rejectionFixtures, rejectUnavailable, updateVariants } from "./surface.mts";

for (const scenario of ["chat", "error", "aborted", "length", "retry"]) {
  for (const mode of ["text", "json"]) {
    test(`oneshot:${mode}:${scenario}`, { timeout: 10000 }, async (t) => {
      const p = await pi(t, mode, scenario);
      const out = await p.finish();
      if (mode === "text") {
        const expected = {
          chat: [0, "reference answer\n", ""],
          retry: [0, "recovered\n", ""],
          length: [1, "partial\n", "Response was truncated: the model reached its output token limit.\n"],
          error: [1, "", "fixture model error\n"],
          aborted: [1, "", "fixture abort\n"],
        }[scenario];
        assert.deepEqual([out.code, out.stdout, out.stderr], expected);
      } else {
        assert.equal(out.code, 0, out.stderr);
        assert.equal(out.stderr, "");
        assert.equal(out.records[0]?.type, "session");
        assert.equal(out.records[0]?.version, 3);
        assert.ok(!existsSync(join(p.dir, "sessions")), "in-memory reference session wrote a session file");
        ordered(out.records, ["agent_start", "turn_start", "message_start", "message_end", "agent_end", "agent_settled"]);
        const end = out.records.findLast((r) => r.type === "message_end" && r.message.role === "assistant");
        assert.equal(end?.message.stopReason, { error: "error", aborted: "aborted", length: "length" }[scenario] ?? "stop");
        if (scenario === "retry") ordered(out.records, ["auto_retry_start", "auto_retry_end", "agent_settled"]);
      }
    });
  }
}

test("wire:tools-and-two-turns", { timeout: 10000 }, async (t) => {
  const p = await pi(t, "json", "tools");
  const out = await p.finish();
  assert.equal(out.code, 0, out.stderr);
  const events = out.records;
  const updates = events.filter((r) => r.type === "message_update");
  assert.ok(updates.length > 0);
  for (const { message, usage, assistantMessageEvent } of updates) {
    assert.equal(message, undefined);
    assert.equal(assistantMessageEvent.partial, undefined);
    assert.equal(typeof usage.totalTokens, "number");
  }
  for (const [kind, index] of [["thinking", 0], ["text", 1], ["toolcall", 2]]) {
    assert.ok(updates.some((r) => r.assistantMessageEvent.type === `${kind}_delta` &&
      r.assistantMessageEvent.contentIndex === index));
  }
  ordered(events, ["turn_start", "tool_execution_start", "tool_execution_update", "tool_execution_end",
    "turn_end", "turn_start", "turn_end", "agent_end", "agent_settled"]);
  const finished = events.find((r) => r.type === "tool_execution_end");
  assert.equal(finished.toolCallId, "fixture-call");
  assert.equal(finished.isError, false);
  assert.equal(finished.result.content[0].text, "finished");
  assert.equal(events.at(-1).aborted, false);
});

test("wire:idle-queue-models-resources", { timeout: 11000 }, async (t) => {
  const p = await pi(t, "rpc", "chat");
  assert.deepEqual(await p.send("steer", { message: "idle steer\u2028still one record" }), { disposition: "queued" });
  assert.deepEqual(await p.send("follow_up", { message: "idle follow-up" }), { disposition: "queued" });
  const state = await p.send("get_state");
  assert.equal(state.isStreaming, false);
  assert.equal(state.pendingMessageCount, 2);
  assert.ok(!Object.hasOwn(state, "sessionFile"));
  assert.equal(p.records.some((r) => r.type === "agent_start"), false);
  assert.deepEqual(await p.send("clear_queue"), { steering: ["idle steer\u2028still one record"], followUp: ["idle follow-up"] });
  assert.equal((await p.send("get_state")).pendingMessageCount, 0);
  const { models } = await p.send("get_available_models");
  const model = models.find((m) => m.provider === "faux" && m.id === "faux-2");
  assert.equal(typeof model?.contextWindow, "number");
  assert.deepEqual(await p.send("set_model", { provider: "faux", modelId: "faux-2" }), model);
  await p.send("set_model", { provider: "faux", modelId: "missing" }, false);
  assert.deepEqual(await p.send("get_available_thinking_levels"), { levels: ["off"] });
  assert.equal(await p.send("cycle_thinking_level"), null);
  assert.equal((await p.send("cycle_model")).model.id, "faux-1");
  await p.send("set_thinking_level", { level: "off" });
  assert.notEqual((await p.send("cycle_thinking_level")).level, "off");
  const { commands } = await p.send("get_commands");
  assert.ok(commands.some((c) => c.name === "fixture" && c.source === "prompt" && c.sourceInfo.scope === "user"));
  const cursor = p.records.length;
  assert.deepEqual(await p.send("prompt", { message: "/fixture needle" }), { disposition: "started" });
  await p.until((r) => r.type === "agent_settled", cursor);
  const { messages } = await p.send("get_messages");
  const user = messages.find((m) => m.role === "user");
  assert.match(JSON.stringify(user.content), /Fixture template: needle/);
  assert.deepEqual(await p.send("get_last_assistant_text"), { text: "reference answer" });
  assert.equal((await p.finish()).code, 0);
  assert.equal(p.records.some((r) => r.type === "session"), false);
  assert.ok(p.records.some((r) => r.type === "thinking_level_changed"));
});

test("wire:branch-cursor-title", { timeout: 11000 }, async (t) => {
  const p = await pi(t, "rpc", "chat", { persist: true });
  const cursor = p.records.length;
  assert.deepEqual(await p.send("prompt", { message: "first user turn" }), { disposition: "started" });
  await p.until((r) => r.type === "agent_settled", cursor);
  await p.send("set_session_name", { name: "fixture title" });
  const original = await p.send("get_state");
  assert.ok(original.sessionFile.startsWith(p.dir + "/"));
  assert.equal(original.sessionName, "fixture title");
  await p.until((r) => r.type === "session_info_changed" && r.name === "fixture title");
  const { entries, leafId } = await p.send("get_entries");
  assert.ok(entries.length > 1);
  assert.deepEqual((await p.send("get_entries", { since: entries[0].id })).entries, entries.slice(1));
  await p.send("get_entries", { since: "missing" }, false);
  const tree = await p.send("get_tree");
  assert.equal(tree.leafId, leafId);
  assert.equal(tree.tree[0].entry.parentId, null);
  const stats = await p.send("get_session_stats");
  assert.equal(stats.userMessages, 1);
  assert.equal(stats.assistantMessages, 1);
  assert.equal(typeof stats.tokens.total, "number");
  const selection = await p.send("get_fork_messages");
  assert.equal(selection.messages.length, 1);
  assert.equal(selection.messages[0].text, "first user turn");
  assert.deepEqual(await p.send("clone"), { cancelled: false });
  assert.deepEqual(await p.send("get_last_assistant_text"), { text: "reference answer" });
  assert.deepEqual(await p.send("switch_session", { sessionPath: original.sessionFile }), { cancelled: false });
  assert.equal((await p.send("get_state")).sessionName, "fixture title");
  assert.deepEqual(await p.send("fork", { entryId: selection.messages[0].entryId }), { text: "first user turn", cancelled: false });
  assert.ok(!(await p.send("get_messages")).messages.some((m) => m.role === "user"));
  assert.deepEqual(await p.send("new_session"), { cancelled: false });
  // The published type/doc say null, but pinned rpc-mode serializes undefined by omission.
  assert.deepEqual(await p.send("get_last_assistant_text"), {});
  assert.equal((await p.finish()).code, 0);
});

test("wire:abandoned-branch-and-compacted-context", { timeout: 11000 }, async (t) => {
  const p = await pi(t, "rpc", "branched", { persist: true });
  const entries = await p.send("get_entries");
  const getText = e => e.type === "message" && textOf(e.message.content);
  const abandoned = entries.entries.find(e => getText(e) === "abandoned answer");
  const prior = entries.entries.find(e => getText(e) === "superseded active-prefix answer");
  const shared = entries.entries.find(e => getText(e) === "old shared input");
  const keep = entries.entries.find(e => getText(e) === "kept input");
  const compaction = entries.entries.find(e => e.type === "compaction");
  assert.equal(abandoned.parentId, shared.id);
  assert.equal(prior.parentId, shared.id);
  assert.equal(compaction.firstKeptEntryId, keep.id);
  assert.ok(entries.entries.indexOf(abandoned) < entries.entries.indexOf(prior));
  const after = await p.send("get_entries", { since: abandoned.id });
  assert.deepEqual(after.entries, entries.entries.slice(entries.entries.indexOf(abandoned) + 1));
  const tree = await p.send("get_tree");
  assert.equal(tree.leafId, entries.leafId);
  const flatten = nodes => nodes.flatMap(n => [n, ...flatten(n.children)]);
  const sharedNode = flatten(tree.tree).find(n => n.entry.id === shared.id);
  assert.deepEqual(sharedNode.children.map(n => n.entry.id).sort(), [abandoned.id, prior.id].sort());
  assert.deepEqual(flatten(tree.tree).map(n => n.entry.id).sort(), entries.entries.map(e => e.id).sort());

  const state = await p.send("get_state");
  assert.equal((await p.send("switch_session", { sessionPath: state.sessionFile })).cancelled, false);
  const reopened = await p.send("get_entries");
  assert.deepEqual(reopened.entries.slice(0, entries.entries.length), entries.entries);
  assert.ok(reopened.entries.some(e => e.id === entries.leafId));
  const active = JSON.stringify(await p.send("get_messages"));
  for (const label of ["anchored fixture summary", "kept input", "kept answer", "active leaf answer"]) {
    assert.ok(active.includes(label), "missing from active context: " + label);
  }
  for (const label of ["abandoned answer", "old shared input", "superseded active-prefix answer"]) {
    assert.ok(!active.includes(label), "leaked into active context: " + label);
  }
  assert.deepEqual(await p.send("get_last_assistant_text"), { text: "active leaf answer" });
  assert.equal((await p.finish()).code, 0);
});

test("wire:normal-textless-reply-replaces-previous-last-text", { timeout: 11000 }, async (t) => {
  const p = await pi(t, "rpc", "textless");
  for (const [message, expected] of [["first prompt", { text: "previous answer" }], ["no visible text", {}]]) {
    const cursor = p.records.length;
    await p.send("prompt", { message });
    await p.until(r => r.type === "agent_settled", cursor);
    assert.deepEqual(await p.send("get_last_assistant_text"), expected);
  }
  assert.equal((await p.finish()).code, 0);
});

test("wire:busy-admission-abort", { timeout: 11000 }, async (t) => {
  const p = await pi(t, "rpc", "slow");
  assert.deepEqual(await p.send("prompt", { message: "start slow" }), { disposition: "started" });
  await p.until((r) => r.type === "message_start" && r.message.role === "assistant");
  await p.send("prompt", { message: "reject without streaming behavior" }, false);
  assert.deepEqual(await p.send("prompt", { message: "change course", streamingBehavior: "steer" }), {
    disposition: "queued",
  });
  assert.deepEqual(await p.send("follow_up", { message: "next" }), { disposition: "queued" });
  assert.deepEqual(await p.send("clear_queue"), { steering: ["change course"], followUp: ["next"] });
  await p.send("abort");
  assert.equal((await p.send("get_state")).isStreaming, false);
  const out = await p.finish();
  assert.equal(out.code, 0, out.stderr);
  assert.equal(new Set(out.records.filter((r) => r.type === "response").map((r) => r.id)).size, 7);
  assert.equal(out.records.filter((r) => r.type === "response").length, 7);
  ordered(out.records, [
    (r) => r.type === "message_end" && r.message.stopReason === "aborted",
    (r) => r.type === "agent_settled" && r.aborted === true,
  ]);
});

// Interleave arrival to distinguish per-mode FIFO from a single global FIFO.
const mixedQueue = [
  ["followUp", "follow one"], ["steer", "steer one"], ["followUp", "follow two"], ["steer", "steer two"],
];
const emptyQueue = { steering: [], followUp: [] };
const mixedProjection = { steering: ["steer one", "steer two"], followUp: ["follow one", "follow two"] };
const mixedTurnInputs = [["begin", "steer one"], ["steer two"], ["follow one"], ["follow two"]];

test("wire:local-queue-order", { timeout: 11000 }, async (t) => {
  const p = await pi(t, "rpc", "queue");
  for (const [mode, message] of mixedQueue) {
    assert.deepEqual(await p.send(mode === "followUp" ? "follow_up" : mode, { message }), { disposition: "queued" });
  }
  assert.equal((await p.send("get_state")).pendingMessageCount, 4);
  assert.equal(p.records.some((r) => r.type === "agent_start"), false);
  assert.deepEqual(await p.send("prompt", { message: "begin" }), { disposition: "started" });
  await p.until((r) => r.type === "agent_settled");
  assert.equal((await p.send("get_state")).pendingMessageCount, 0);
  const out = await p.finish();
  assert.equal(out.code, 0, out.stderr);
  const turns = [];
  for (const r of out.records) {
    if (r.type === "turn_start") turns.push([]);
    if (r.type === "message_start" && r.message.role === "user") turns.at(-1).push(textOf(r.message.content));
  }
  assert.deepEqual(turns, mixedTurnInputs);
});

test("wire:abort-retains-local-inputs", { timeout: 11000 }, async (t) => {
  const p = await pi(t, "rpc", "slow");
  await p.send("prompt", { message: "start slow" });
  await p.until((r) => r.type === "message_start" && r.message.role === "assistant");
  await p.send("steer", { message: "pending steer" });
  await p.send("follow_up", { message: "pending follow-up" });
  await p.send("abort");
  const state = await p.send("get_state");
  assert.equal(state.isStreaming, false);
  assert.equal(state.pendingMessageCount, 2);
  assert.deepEqual(await p.send("clear_queue"), { steering: ["pending steer"], followUp: ["pending follow-up"] });
  assert.equal((await p.finish()).code, 0);
});

test("wire:parse-error-and-crlf-do-not-change-request-correlation", { timeout: 10000 }, async t => {
  const p = await pi(t, "rpc", "chat");
  p.raw("{bad JSON}\n");
  await p.until(r => r.type === "response" && r.command === "parse");
  p.raw('{"type":"steer","id":"split","message":"before\u2028after\u2029done"}\r');
  p.raw("\n");
  const steer = await p.until(r => r.type === "response" && r.id === "split");
  assert.deepEqual(steer, { type: "response", command: "steer", id: "split", success: true, data: { disposition: "queued" } });
  const reply = await p.send("clear_queue");
  assert.deepEqual(reply.steering, ["before\u2028after\u2029done"]);
  p.raw('{"type":"get_state"}\n');
  const noID = await p.until(r => r.type === "response" && r.command === "get_state");
  assert.ok(noID.success && !Object.hasOwn(noID, "id"));
  const result = await p.finish();
  const parse = result.records.filter(r => r.type === "response" && r.command === "parse");
  assert.equal(parse.length, 1);
  assert.equal(parse[0].success, false);
  assert.ok(!Object.hasOwn(parse[0], "id") && typeof parse[0].error === "string");
  assert.equal(result.code, 0);
  assert.equal(result.stderr, "");
});

test("wire:post-answer-threshold-delays-settlement", { timeout: 10000 }, async t => {
  const p = await pi(t, "json", "threshold");
  const out = await p.finish();
  assert.equal(out.code, 0, out.stderr);
  ordered(out.records, [
    r => r.type === "message_end" && r.message.role === "assistant" && r.message.stopReason === "stop",
    "agent_end", r => r.type === "compaction_start" && r.reason === "threshold",
    r => r.type === "compaction_end" && r.reason === "threshold" && !r.willRetry && r.result?.summary.startsWith("threshold summary"),
    "agent_settled",
  ]);
  assert.equal(out.records.filter(r => r.type === "agent_start").length, 1);
});

test("wire:manual-compaction-retry", { timeout: 11000 }, async (t) => {
  const p = await pi(t, "rpc", "history_retry");
  for (const text of ["old history ".repeat(130), "keep this ".repeat(50)]) {
    const cursor = p.records.length;
    await p.send("prompt", { message: text });
    await p.until((r) => r.type === "agent_settled", cursor);
  }
  await p.send("set_auto_compaction", { enabled: true });
  assert.equal((await p.send("get_state")).autoCompactionEnabled, true);
  const result = await p.send("compact", { customInstructions: "Use fixture summary." });
  assert.equal(result.summary, "fixture summary");
  assert.equal(typeof result.firstKeptEntryId, "string");
  assert.equal(typeof result.tokensBefore, "number");
  assert.equal(typeof result.estimatedTokensAfter, "number");
  ordered(p.records, ["compaction_start", "summarization_retry_scheduled", "summarization_retry_attempt_start",
    "summarization_retry_finished", "compaction_end"]);
  assert.equal((await p.finish()).code, 0);
});

test("durable:producer-observations", { timeout: 10000 }, async (t) => {
  const trace = [];
  const h = await openDurable(trace, { compaction: { enabled: false, keepRecentTokens: 20 } });
  t.after(() => h.close());
  const overload = () => fauxAssistantMessage("", { stopReason: "error", errorMessage: "529 overloaded" });
  h.faux.setResponses([overload(), toolRound(), fauxAssistantMessage("done ".repeat(40)), overload(), fauxAssistantMessage("summary")]);
  const input = await h.conv.submit({ type: "input", content: "fixture history ".repeat(40) }, ctx);
  assert.equal((await input.wait(ctx)).status, "done");
  await h.conv.waitForIdle(ctx);
  const compact = await h.conv.compact(undefined, ctx);
  const task = await h.harness.waitForTask(compact, ctx);
  assert.equal(task.state.outcome.status, "completed");
  // Durable returns the summary's placement receipt, not Pi's compaction payload.
  assert.deepEqual(Object.keys(task.state.outcome.result), ["submissionId"]);
  const placement = await h.harness.submission(task.state.outcome.result.submissionId, ctx);
  assert.equal((await placement.wait(ctx)).status, "done");
  const retry = (r) => r.type === "task" && r.state === "sleeping" && r.phase === "retry";
  ordered(trace, ["generation.beforeRequest", (r) => r.stopReason === "error", retry,
    "generation.beforeRequest", (r) => r.kind === "thinking_delta" && r.contentIndex === 0,
    (r) => r.kind === "toolcall_delta" && r.contentIndex === 2, "tool.before", "tool.execute",
    (r) => r.type === "tool.output" && r.chunk === "first", (r) => r.type === "tool.output" && r.chunk === " second",
    "tool.after", (r) => r.type === "entry" && r.kind === "pi.tool-result", "generation.afterTools",
    "generation.beforeRequest", (r) => r.type === "receipt" && r.status === "done", "generation.afterRun",
    (r) => r.type === "task" && r.state === "terminal", "compaction.before", retry,
    (r) => r.type === "entry" && r.kind === "pi.compaction"]);
  assert.ok(!trace.some((r) => r.type === "storage.end"), "observer ended during the trace");
});

test("profile:documented-inventory", async () => {
  const docs = new URL("../../../node_modules/@earendil-works/pi-coding-agent/docs/", import.meta.url);
  const [rpc, json] = await Promise.all(["rpc-commands.md", "json.md"].map(file => readFile(new URL(file, docs), "utf8")));
  assert.deepEqual([...rpc.matchAll(/^### ([a-z_]+)$/gm)].map(m => m[1]).sort(), Object.keys(commands).sort());
  const listed = [...json.matchAll(/^\| `([a-z_]+)` \|/gm)].map(m => m[1]);
  const inTable = new Set([...Object.keys(events), ...Object.keys(updateVariants)]);
  for (const name of listed) assert.ok(inTable.has(name), "unclassified documented event " + name);
  for (const name of [...Object.keys(events), ...Object.keys(updateVariants)]) {
    assert.ok(listed.includes(name) || json.includes(`"${name}"`), "missing documented event " + name);
  }
});

test("profile:failures", () => {
  // Expected reply vectors; the next fixture runs the gate before a Harness-backed handler.
  const failures = new Map(rejectionFixtures().map((record) => [record.command, record]));
  for (const [name, row] of Object.entries(commands)) {
    if (row.scope === "core") {
      assert.equal(failures.has(name), false);
      continue;
    }
    const expected = row.scope === "no" ? [`unsupported:${name}`, `Unsupported in hush v1: ${name}`]
      : [`absent:${name}`, `Unsupported capability: ${row.capability}`];
    assert.deepEqual(failures.get(name), {
      id: expected[0], type: "response", command: name, success: false, error: expected[1],
    });
  }
});

test("profile:unavailable-never-dispatches", { timeout: 10000 }, async t => {
  const h = await openDurable();
  t.after(() => h.close());
  const commits = [];
  h.harness.observeCommits({ advance(batch) { commits.push(batch.seq); }, end() {} });
  const params = {
    set_model: { provider: "faux", modelId: "faux-1" },
    set_thinking_level: { level: "medium" },
    set_steering_mode: { mode: "all" },
    set_follow_up_mode: { mode: "all" },
    set_auto_retry: { enabled: true },
    bash: { command: "not executed" },
    switch_session: { sessionPath: "must-not-be-opened.jsonl" },
    set_session_name: { name: "must not rename" },
    export_html: { outputPath: "must-not-be-created.html" },
  };
  let dispatched = 0;
  async function handle(cmd, capabilities) {
    const error = rejectUnavailable(cmd, capabilities);
    if (error !== undefined) return error;
    dispatched++;
    // If a denied command falls through, a real change (and provider call) would occur.
    await h.conv.submit({ type: "input", content: "fixture handler reached" }, ctx);
  }
  const none = new Set();
  for (const expected of rejectionFixtures()) {
    const { command: type, id } = expected;
    assert.deepEqual(await handle({ type, id, ...params[type] }, none), expected);
  }
  assert.equal(dispatched, 0);
  assert.deepEqual(commits, []);
  assert.equal(h.faux.state.callCount, 0);
  assert.deepEqual((await h.harness.inspect(ctx)).submissions, []);
  assert.equal(rejectUnavailable({ type: "get_available_models" }, new Set(["models"])), undefined);
  assert.equal(rejectUnavailable({ type: "get_state" }, none), undefined);
  assert.ok(!Object.hasOwn(rejectUnavailable({ type: "abort_bash" }, none), "id"));
});

test("durable:bare-idle-submission-differs-from-rpc-queue", { timeout: 10000 }, async (t) => {
  const h = await openDurable();
  t.after(() => h.close());
  const input = { type: "input", content: "idle steer", whenBusy: "steer" };
  await assert.rejects(h.conv.submit({ ...input, whenIdle: "reject" }, ctx), ConversationIdle);
  assert.equal(h.faux.state.callCount, 0);
  h.faux.setResponses([fauxAssistantMessage("started instead")]);
  const receipt = await h.conv.submit(input, ctx);
  assert.equal((await receipt.wait(ctx)).status, "done");
  assert.equal(h.faux.state.callCount, 1);
  const inbox = await h.harness.snapshot(InboxDoc, h.conv.id, ctx);
  assert.deepEqual(inbox.items, []);
});

const localQueue = (h) => new LocalQueue(h.harness, h.conv);
const userTexts = (messages) => messages.filter((m) => m.role === "user").map((m) => textOf(m.content));

function localQueueChanges(h) {
  const changes = [];
  h.harness.observeCommits({
    advance(commit) {
      const change = commit.changes.find((c) => c.type === "document" && c.kind === "change" &&
        c.conversationId === h.conv.id && c.record.kind === PendingInputs.definition.kind);
      if (change) changes.push({
        steering: change.value.steer.map((r) => r.text), followUp: change.value.followUp.map((r) => r.text),
      });
    },
    end() {},
  });
  return changes;
}

test("durable:local-queue-order", { timeout: 10000 }, async (t) => {
  const h = await openDurable([], {}, { localQueue: true });
  t.after(() => h.close());
  const q = localQueue(h);
  const turns = [];
  const changes = localQueueChanges(h);
  let firstQueue;
  let previousInputCount = 0;
  const reply = ({ messages }) => {
    const inputs = userTexts(messages);
    turns.push(inputs.slice(previousInputCount));
    previousInputCount = inputs.length;
    if (turns.length === 1) firstQueue = changes.at(-1);
    return fauxAssistantMessage("done");
  };
  h.faux.setResponses(mixedTurnInputs.map(() => reply));
  for (const [mode, text] of mixedQueue) {
    assert.deepEqual(await q.enqueue(mode, { text }, ctx), { disposition: "queued" });
  }
  assert.deepEqual(await q.inspect(ctx), mixedProjection);
  assert.equal(h.harness.wake().active, false);
  assert.equal(h.faux.state.callCount, 0);
  assert.deepEqual((await h.conv.context(ctx)).entries, []);
  assert.deepEqual((await h.harness.inspect(ctx)).submissions, []);
  const prompt = await q.prompt({ text: "begin" }, undefined, ctx);
  assert.equal(prompt.disposition, "started");
  await h.conv.waitForIdle(ctx);
  assert.equal((await (await h.harness.submission(prompt.submissionId, ctx)).wait(ctx)).status, "done");
  assert.deepEqual(turns, mixedTurnInputs);
  assert.deepEqual(firstQueue, { steering: ["steer two"], followUp: ["follow one", "follow two"] });
  assert.deepEqual(userTexts((await h.conv.context(ctx)).messages), mixedTurnInputs.flat());
  assert.deepEqual(await q.inspect(ctx), emptyQueue);
  assert.deepEqual(changes.slice(-5), [
    mixedProjection,
    { steering: ["steer two"], followUp: ["follow one", "follow two"] },
    { steering: [], followUp: ["follow one", "follow two"] },
    { steering: [], followUp: ["follow two"] },
    emptyQueue,
  ]);
});

test("durable:local-queue-clear-while-busy", { timeout: 10000 }, async (t) => {
  const h = await openDurable([], {}, { localQueue: true });
  const started = Promise.withResolvers();
  const release = Promise.withResolvers();
  t.after(async () => { release.resolve(); await h.close(); });
  const a = localQueue(h), b = localQueue(h);
  h.faux.setResponses([async () => {
    started.resolve();
    await release.promise;
    return fauxAssistantMessage("done");
  }]);
  await a.prompt({ text: "begin" }, undefined, ctx);
  await started.promise;
  await assert.rejects(b.prompt({ text: "reject" }, undefined, ctx), ConversationBusy);
  await b.enqueue("followUp", { text: "discard follow-up" }, ctx);
  assert.deepEqual(await b.prompt({ text: "discard steer" }, "steer", ctx), { disposition: "queued" });
  assert.deepEqual(await a.clear(ctx), { steering: ["discard steer"], followUp: ["discard follow-up"] });
  assert.deepEqual(await b.clear(ctx), emptyQueue);
  release.resolve();
  await h.conv.waitForIdle(ctx);
  assert.deepEqual(userTexts((await h.conv.context(ctx)).messages), ["begin"]);
  assert.equal(h.faux.state.callCount, 1);
});

test("durable:local-queue-steer-after-tool", { timeout: 10000 }, async (t) => {
  const executing = Promise.withResolvers();
  const release = Promise.withResolvers();
  const h = await openDurable([], {}, { localQueue: true, async beforeToolResult() {
    executing.resolve();
    await release.promise;
  } });
  t.after(async () => { release.resolve(); await h.close(); });
  const q = localQueue(h);
  const turns = [];
  const reply = ({ messages }) => {
    turns.push(userTexts(messages));
    return fauxAssistantMessage("done");
  };
  h.faux.setResponses([toolRound(), reply, reply]);
  await q.prompt({ text: "begin" }, undefined, ctx);
  await executing.promise;
  await q.enqueue("followUp", { text: "later" }, ctx);
  await q.enqueue("steer", { text: "next request" }, ctx);
  assert.equal(h.faux.state.callCount, 1);
  release.resolve();
  await h.conv.waitForIdle(ctx);
  assert.deepEqual(turns, [["begin", "next request"], ["begin", "next request", "later"]]);
});

test("durable:local-queue-abort-keeps-unselected-inputs", { timeout: 10000 }, async (t) => {
  const h = await openDurable([], {}, { localQueue: true });
  const q = localQueue(h);
  const started = Promise.withResolvers();
  const release = Promise.withResolvers();
  t.after(async () => { release.resolve(); await h.close(); });
  h.faux.setResponses([async (_request, { signal }) => {
    started.resolve();
    if (!signal.aborted) signal.addEventListener("abort", () => release.resolve(), { once: true });
    else release.resolve();
    await release.promise;
    return fauxAssistantMessage("aborted", { stopReason: "aborted" });
  }]);
  const out = await q.prompt({ text: "begin" }, undefined, ctx);
  await started.promise;
  await q.enqueue("steer", { text: "pending steer" }, ctx);
  await q.enqueue("followUp", { text: "pending follow-up" }, ctx);
  await q.abort(ctx);
  const result = await (await h.harness.submission(out.submissionId, ctx)).wait(ctx);
  assert.equal(result.status, "unanswered");
  assert.equal(result.reason, "aborted");
  assert.deepEqual(await q.clear(ctx), { steering: ["pending steer"], followUp: ["pending follow-up"] });
  assert.equal(h.faux.state.callCount, 1);
});

test("durable:local-queue-conflict-does-not-consume-staging", { timeout: 10000 }, async (t) => {
  const h = await openDurable([], { retry: { enabled: false } }, { localQueue: true });
  const started = Promise.withResolvers();
  const release = Promise.withResolvers();
  t.after(async () => { release.resolve(); await h.close(); });
  h.faux.setResponses([async () => {
    started.resolve();
    await release.promise;
    return fauxAssistantMessage("", { stopReason: "error", errorMessage: "fixture failed" });
  }]);
  const q = localQueue(h);
  await q.prompt({ text: "begin" }, undefined, ctx);
  await started.promise;
  const native = await h.conv.submit({ type: "input", whenBusy: "followUp", content: "native input" }, ctx);
  await q.enqueue("steer", { text: "local input" }, ctx);
  release.resolve();
  await h.conv.waitForIdle(ctx);
  await assert.rejects(q.prompt({ text: "reject rather than reorder" }, undefined, ctx), /native user inputs pending/);
  assert.equal((await native.status(ctx)).status, "queued");
  assert.deepEqual(await q.inspect(ctx), { steering: ["local input"], followUp: [] });
  assert.deepEqual(userTexts((await h.conv.context(ctx)).messages), ["begin"]);
  assert.equal(h.faux.state.callCount, 1);
});

test("durable:local-queue-reopens-without-waking-and-forks-empty", { timeout: 10000 }, async (t) => {
  const dir = await mkdtemp(join(tmpdir(), "hush-pending-queue-"));
  const db = join(dir, "fixture.sqlite");
  const open = async () => openDurable([], {}, { localQueue: true, storage: await openNodeSqliteStorage(db) });
  let h;
  t.after(async () => {
    await h?.close();
    await rm(dir, { recursive: true, force: true });
  });
  h = await open();
  const image = { type: "image", data: "fixture-encoded-image", mimeType: "image/png" };
  await localQueue(h).enqueue("followUp", { text: "queued image", images: [image] }, ctx);
  image.data = "mutated-by-caller";
  await h.close();
  h = await open();
  const q = localQueue(h);
  assert.deepEqual(await q.inspect(ctx), { steering: [], followUp: ["queued image"] });
  assert.equal(h.harness.wake().active, false);
  assert.equal(h.faux.state.callCount, 0);
  const imagesSeen = [];
  h.faux.setResponses([fauxAssistantMessage("first"), ({ messages }) => {
    for (const m of messages) {
      if (m.role === "user" && Array.isArray(m.content)) imagesSeen.push(...m.content.filter((b) => b.type === "image"));
    }
    return fauxAssistantMessage("second");
  }]);
  await q.prompt({ text: "begin" }, undefined, ctx);
  await h.conv.waitForIdle(ctx);
  assert.deepEqual(imagesSeen, [{ type: "image", data: "fixture-encoded-image", mimeType: "image/png" }]);
  assert.deepEqual(await q.inspect(ctx), emptyQueue);
  await q.enqueue("steer", { text: "parent pending" }, ctx);
  const last = (await h.conv.context(ctx)).entries.at(-1).id;
  const fork = await h.conv.fork(last, { ownership: { kind: "ownerless" } }, ctx);
  assert.deepEqual(await new LocalQueue(h.harness, fork).inspect(ctx), emptyQueue);
  assert.deepEqual(await q.inspect(ctx), { steering: ["parent pending"], followUp: [] });
});
