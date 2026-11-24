import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fauxAssistantMessage, fauxToolCall } from "@earendil-works/pi-ai";
import { GenerationTask } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { ctx, openDurable, toolRound } from "./durable-probe.mjs";
import { LocalQueue } from "./local-queue.mts";
import { OrderedCompaction } from "./ordered-compaction.mts";
import { OrderedMetadata } from "./ordered-metadata.mts";
import { acceptTrace, OrderedRun, TraceJournal } from "./ordered-run.mts";
import { ordered, pi, textOf } from "./reference-probe.mjs";
import { events } from "./surface.mts";

function fixtureEvents(records) {
  return records.filter(row => events[row.type]?.scope === "piEvents");
}

function runProjection(records) {
  const message = m => {
    if (m.role === "system") return { role: m.role }; // Tool declarations differ by adapter.
    if (m.role === "user") return { role: m.role, content: textOf(m.content) };
    return { role: m.role, content: m.content,
      ...(m.role === "assistant" ? { stopReason: m.stopReason } : { callId: m.toolCallId, isError: m.isError }) };
  };
  return fixtureEvents(records).map(row => {
    switch (row.type) {
      case "agent_end":
        return { type: row.type, messages: row.messages.map(message), willRetry: row.willRetry };
      case "message_start":
      case "message_end":
      case "turn_end":
        return { type: row.type, message: message(row.message),
          ...(row.type === "turn_end" ? { toolResults: row.toolResults.map(message) } : {}) };
      case "message_update":
        assert.equal(row.message, undefined);
        assert.equal(row.assistantMessageEvent.partial, undefined);
        assert.ok(Number.isFinite(row.usage.totalTokens));
        // Usage counts reflect different provider prompts. Assert presence and compare deltas verbatim.
        return { type: row.type, assistantMessageEvent: row.assistantMessageEvent };
      case "tool_execution_end": {
        const { durationMs, ...normalized } = row;
        assert.ok(Number.isFinite(durationMs) && durationMs >= 0, "fixture tool executed");
        return normalized;
      }
      default:
        return row;
    }
  });
}

const readTrace = journal => {
  assert.ok(journal.ready);
  assert.equal(journal.failure, undefined);
  const accept = acceptTrace(journal.ready);
  return journal.frames.map(accept);
};

test("durable:ordered-fast-tool-matches-pi", { timeout: 10000 }, async (t) => {
  const piRun = await pi(t, "json", "tools");
  const reference = await piRun.finish();
  assert.equal(reference.code, 0, reference.stderr);
  const producer = new OrderedRun();
  const h = await openDurable([], {}, { producer });
  const watch = await h.conv.watch(ctx);
  const blocked = Promise.withResolvers(), release = Promise.withResolvers(), folded = Promise.withResolvers();
  t.after(async () => { release.resolve(); await watch.stop(); await h.close(); });
  const seen = [];
  watch.start(async change => {
    if (change.kind !== "value") return;
    seen.push(change.value);
    if (seen.length === 1) {
      blocked.resolve();
      await release.promise;
    } else {
      folded.resolve();
    }
  });
  assert.equal(producer.journal.frames.length, 0, "ready is not a replay");
  h.faux.setResponses([async () => { await blocked.promise; return toolRound(); }, fauxAssistantMessage("tool complete")]);
  const receipt = await h.conv.submit({ type: "input", content: "fixture prompt" }, ctx);
  assert.equal((await receipt.wait(ctx)).status, "done");
  await h.conv.waitForIdle(ctx);
  const projected = readTrace(producer.journal);
  assert.deepEqual(runProjection(projected), runProjection(reference.records));
  assert.equal(projected.filter(row => row.type === "tool_execution_update").length, 2);
  assert.equal(seen.length, 1, "the watch listener was blocked through the tool round");
  release.resolve();
  await folded.promise;
  assert.ok(seen.every(value => !(value.docs["pi.live"]?.tools?.some(slot => slot.status === "running"))),
    "neither delivered watch value showed the running tool");
});

for (const [scenario, name, args] of [
  ["unoffered_tool", "not_registered", {}], ["invalid_tool", "fixture_echo", { unexpected: true }],
]) {
  test(`durable:ordered-${scenario}-has-no-execution-duration`, { timeout: 10000 }, async t => {
    const upstream = await pi(t, "json", scenario);
    const { code, stderr, records } = await upstream.finish();
    assert.equal(code, 0, stderr);
    const sources = [], producer = new OrderedRun();
    const h = await openDurable(sources, {}, { producer });
    t.after(() => h.close());
    h.faux.setResponses([
      fauxAssistantMessage([fauxToolCall(name, args, { id: "rejected-call" })], { stopReason: "toolUse" }),
      fauxAssistantMessage("continued after rejection"),
    ]);
    const receipt = await h.conv.submit({ type: "input", content: "fixture prompt" }, ctx);
    assert.equal((await receipt.wait(ctx)).status, "done");
    await h.conv.waitForIdle(ctx);
    const projected = readTrace(producer.journal);
    // Durable and Pi use different diagnostic text. Compare identity, order and the
    // attempted-vs-executed disposition; don't normalize a real duration to absent.
    const tools = rows => rows.filter(r => r.type.startsWith("tool_execution_")).map(r => ({
      type: r.type, id: r.toolCallId, name: r.toolName, args: r.args, error: r.isError,
      hasDuration: Object.hasOwn(r, "durationMs"),
    }));
    assert.deepEqual(tools(projected), tools(records));
    const [start, end] = tools(projected);
    assert.equal(start.type, "tool_execution_start");
    assert.equal(end.type, "tool_execution_end");
    assert.equal(end.error, true);
    assert.equal(end.hasDuration, false);
    assert.ok(!sources.some(row => row.type === "tool.before" || row.type === "tool.execute"));
    ordered(projected, ["tool_execution_start", "tool_execution_end", "turn_end", "turn_start", "agent_end", "agent_settled"]);
  });
}

for (const [scenario, settles] of [["retry", "done"], ["retry_failed", "unanswered"]]) {
  test(`durable:ordered-generation-${scenario}-matches-pi`, { timeout: 10000 }, async t => {
    const p = await pi(t, "json", scenario);
    const reference = await p.finish();
    assert.equal(reference.code, 0, reference.stderr);
    const producer = new OrderedRun();
    const h = await openDurable([], {}, { producer });
    t.after(() => h.close());
    h.faux.setResponses([fauxAssistantMessage("", { stopReason: "error", errorMessage: "529 overloaded" }),
      scenario === "retry" ? fauxAssistantMessage("recovered")
        : fauxAssistantMessage("", { stopReason: "error", errorMessage: "529 overloaded again" })]);
    assert.equal((await (await h.conv.submit({ type: "input", content: "fixture prompt" }, ctx)).wait(ctx)).status, settles);
    await h.conv.waitForIdle(ctx);
    assert.deepEqual(runProjection(readTrace(producer.journal)), runProjection(reference.records));
  });
}

test("durable:ordered-settled-is-after-drain-not-receipt", { timeout: 10000 }, async (t) => {
  const producer = new OrderedRun();
  const drain = Promise.withResolvers(), release = Promise.withResolvers();
  const h = await openDurable([], {}, { producer, async afterRun() { drain.resolve(); await release.promise; } });
  t.after(async () => { release.resolve(); await h.close(); });
  h.faux.setResponses([fauxAssistantMessage("committed answer")]);
  const receipt = await h.conv.submit({ type: "input", content: "fixture prompt" }, ctx);
  await drain.promise;
  assert.equal((await receipt.wait(ctx)).status, "done");
  assert.equal(readTrace(producer.journal).filter(row => row.type === "agent_settled").length, 0);
  release.resolve();
  await h.conv.waitForIdle(ctx);
  const rows = readTrace(producer.journal);
  assert.deepEqual(rows.at(-1), { type: "agent_settled", aborted: false });
  assert.equal(rows.filter(row => row.type === "agent_end").length, 1);
});

test("durable:ordered-active-abort-without-a-committed-assistant", { timeout: 10000 }, async (t) => {
  const p = await pi(t, "rpc", "slow");
  await p.send("prompt", { message: "fixture prompt" });
  await p.until(r => r.type === "message_start" && r.message.role === "assistant");
  await p.send("abort");
  await p.until(r => r.type === "agent_settled");
  const reference = await p.finish();
  assert.equal(reference.code, 0, reference.stderr);

  for (const dropTerminal of [false, true]) {
    const started = Promise.withResolvers();
    const producer = new class extends OrderedRun {
      provider(signal, event) {
        if (!dropTerminal || event.type !== "error") super.provider(signal, event);
        if (event.type === "start") started.resolve();
      }
    }();
    const h = await openDurable([], { progress: { partialIntervalMs: 60000 } }, { producer, tokensPerSecond: 60 });
    try {
      h.faux.setResponses([fauxAssistantMessage("deliberately slow ".repeat(200))]);
      const receipt = await h.conv.submit({ type: "input", content: "fixture prompt" }, ctx);
      await started.promise;
      await h.conv.abort(ctx);
      assert.equal((await receipt.wait(ctx)).reason, "aborted");
      assert.ok(!(await h.conv.entries({}, 100, undefined, ctx)).items.some(row => row.kind === "pi.assistant"));
      if (dropTerminal) {
        assert.equal(producer.journal.failure, "abort has no authoritative assistant result");
        assert.ok(!producer.journal.frames.some(row => row.event.type === "agent_settled"));
      } else {
        // The prefix streamed before cancellation is timing-dependent, but the terminal reason and order are not.
        const lifecycle = rows => fixtureEvents(rows).flatMap(row => row.type === "message_update" ? []
          : [{ type: row.type, role: row.message?.role, stopReason: row.message?.stopReason,
            willRetry: row.willRetry, aborted: row.aborted }]);
        assert.deepEqual(lifecycle(readTrace(producer.journal)), lifecycle(reference.records));
      }
    } finally {
      await h.close();
    }
  }
});

test("durable:ordered-trace-gap-and-overflow-fail-closed", { timeout: 10000 }, async (t) => {
  const journal = new TraceJournal(3);
  const h = await openDurable([], {}, { producer: new OrderedRun(journal) });
  t.after(() => h.close());
  h.faux.setResponses([fauxAssistantMessage("legitimate, but too fast for the fixture bound")]);
  assert.equal((await (await h.conv.submit({ type: "input", content: "fixture prompt" }, ctx)).wait(ctx)).status, "done");
  await h.conv.waitForIdle(ctx);
  assert.equal(journal.failure, "overflow");
  assert.equal(journal.frames.length, 3);
  assert.ok(journal.frames.every(row => row.event.type !== "agent_end" && row.event.type !== "agent_settled"));
  const accept = acceptTrace(journal.ready);
  assert.equal(accept(journal.frames[0]).type, "agent_start");
  assert.throws(() => accept(journal.frames[2]), /ordered trace lost/);
  assert.throws(() => accept(journal.frames[1]), /ordered trace lost/);
  const newEpoch = new TraceJournal();
  newEpoch.arm();
  newEpoch.push({ type: "turn_start" });
  assert.notEqual(newEpoch.ready.epoch, journal.ready.epoch);
  assert.throws(() => acceptTrace(journal.ready)(newEpoch.frames[0]), /ordered trace lost/);
});

test("durable:ordered-queue-matches-pi-delivery", { timeout: 10000 }, async (t) => {
  const p = await pi(t, "rpc", "queue");
  const producer = new OrderedRun();
  const h = await openDurable([], {}, { producer, localQueue: true });
  t.after(() => h.close());
  const q = new LocalQueue(h.harness, h.conv);
  const staged = [
    ["followUp", "repeated"], ["steer", "repeated"], ["followUp", "follow two"], ["steer", "steer two"],
  ];
  h.faux.setResponses(["one", "two", "three", "four"].map(text => fauxAssistantMessage(text)));
  for (const [mode, text] of staged) {
    await p.send(mode === "steer" ? "steer" : "follow_up", { message: text });
    await q.enqueue(mode, { text }, ctx);
  }
  await p.send("prompt", { message: "begin" });
  await q.prompt({ text: "begin" }, undefined, ctx);
  await p.until(r => r.type === "agent_settled");
  await h.conv.waitForIdle(ctx);
  const emptied = await p.send("clear_queue");
  const removed = await q.clear(ctx);
  assert.deepEqual(emptied, removed);
  producer.confirmedEmptyClear(removed);
  const reference = await p.finish();
  assert.equal(reference.code, 0, reference.stderr);
  assert.deepEqual(runProjection(readTrace(producer.journal)), runProjection(reference.records));
});

test("durable:ordered-abort-of-a-queued-handoff-does-not-invent-delivery", { timeout: 10000 }, async t => {
  const producer = new OrderedRun();
  const sending = Promise.withResolvers(), answer = Promise.withResolvers();
  const held = Promise.withResolvers(), release = Promise.withResolvers(), abortMarked = Promise.withResolvers();
  const h = await openDurable([], {}, { producer, localQueue: true,
    async afterRun() { held.resolve(); await release.promise; },
  });
  t.after(async () => { answer.resolve(); release.resolve(); await h.close(); });
  h.harness.observeCommits({
    advance(batch) {
      if (batch.changes.some(c => c.type === "task" && c.value.kind === GenerationTask.definition.name && c.value.abortRequested)) {
        abortMarked.resolve();
      }
    },
    end() {},
  });
  h.faux.setResponses([async () => { sending.resolve(); await answer.promise; return fauxAssistantMessage("answer"); }]);
  const queue = new LocalQueue(h.harness, h.conv);
  await queue.prompt({ text: "begin" }, undefined, ctx);
  await sending.promise;
  await queue.enqueue("followUp", { text: "selected but not delivered" }, ctx);
  answer.resolve();
  await held.promise;
  const pending = (await h.harness.inspect(ctx)).submissions.find(s => s.type === "input" && s.status === "queued");
  assert.ok(pending, "handoff was committed, but not placement");
  const stopped = h.conv.abort(ctx); // A native abort bypasses the profile's handoff reconciliation.
  void stopped.catch(() => {});
  await abortMarked.promise;
  release.resolve();
  await stopped;
  const receipt = await h.harness.submission(pending.id, ctx);
  const result = await receipt.status(ctx);
  assert.equal(result.status, "unanswered");
  assert.equal(result.reason, "aborted");
  assert.equal(result.entry, undefined);
  assert.match(producer.journal.failure, /handoff was interrupted/);
  assert.ok(!producer.journal.frames.some(({ event }) => event.type === "agent_settled" ||
    (event.type === "message_start" && event.message.role === "user" && textOf(event.message.content) === "selected but not delivered")));
  assert.equal(h.faux.state.callCount, 1);
});

test("durable:ordered-manual-compaction-matches-pi-retry", { timeout: 12000 }, async (t) => {
  const p = await pi(t, "rpc", "history_retry");
  const inputs = ["old history ".repeat(130), "keep this ".repeat(50)];
  for (const message of inputs) {
    const cursor = p.records.length;
    await p.send("prompt", { message });
    await p.until(r => r.type === "agent_settled", cursor);
  }
  const args = { customInstructions: "Use fixture summary." };
  const expected = await p.send("compact", args);
  const reference = await p.finish();
  assert.equal(reference.code, 0, reference.stderr);

  const h = await openDurable([], { compaction: { enabled: false, keepRecentTokens: 250 } });
  t.after(() => h.close());
  h.faux.setResponses([fauxAssistantMessage("first ".repeat(100)), fauxAssistantMessage("second ".repeat(100)),
    fauxAssistantMessage("", { stopReason: "error", errorMessage: "529 overloaded" }), fauxAssistantMessage("fixture summary")]);
  for (const content of inputs) {
    assert.equal((await (await h.conv.submit({ type: "input", content }, ctx)).wait(ctx)).status, "done");
    await h.conv.waitForIdle(ctx);
  }
  const producer = new OrderedCompaction(h.harness, h.conv);
  const id = await h.conv.compact(args.customInstructions, ctx);
  const resolved = await producer.end(id, ctx);
  assert.equal(resolved.kind, "applied");
  assert.equal(resolved.result.summary, expected.summary);
  assert.ok(Number.isFinite(resolved.result.tokensBefore));
  assert.ok(Number.isFinite(resolved.result.estimatedTokensAfter));
  assert.equal(resolved.result.firstKeptEntryId,
    String((await h.conv.context(ctx, { at: resolved.entryId })).head.head));
  // Pi has different session IDs and token estimation inputs. Compare lifecycle payloads, retry count and summary.
  const lifecycle = rows => rows.filter(r => /^(compaction_|summarization_retry_)/.test(r.type)).map(r =>
    r.type === "compaction_end" ? { ...r, result: { summary: r.result.summary } } : r);
  assert.deepEqual(lifecycle(readTrace(producer.journal)), lifecycle(reference.records));
});

test("durable:ordered-thinking-and-title-metadata", { timeout: 10000 }, async t => {
  const p = await pi(t, "rpc", "chat");
  const dir = await mkdtemp(join(tmpdir(), "hush-pi-title-"));
  const db = join(dir, "title.sqlite");
  const open = async () => openDurable([], {}, {
    storage: await openNodeSqliteStorage(db), fauxModels: [{ id: "faux-1", reasoning: true }],
  });
  let h;
  t.after(async () => { await h?.close(); await rm(dir, { recursive: true, force: true }); });
  h = await open();
  await h.conv.configure({ thinkingLevel: (await p.send("get_state")).thinkingLevel }, ctx);
  let producer = await OrderedMetadata.attach(h.harness, h.conv, ctx);
  for (const level of ["off", "medium", "medium", "low"]) {
    await p.send("set_thinking_level", { level });
    await producer.setThinking(h.faux.getModel(), level, ctx);
  }
  for (const name of ["first fixture title", "renamed fixture title"]) {
    await p.send("set_session_name", { name });
    await producer.setTitle(name, ctx);
  }
  const out = await p.finish();
  assert.equal(out.code, 0, out.stderr);
  const metadata = rows => rows.filter(r => r.type === "thinking_level_changed" || r.type === "session_info_changed");
  assert.deepEqual(readTrace(producer.journal), metadata(out.records));
  await h.close();
  h = await open();
  producer = await OrderedMetadata.attach(h.harness, h.conv, ctx);
  assert.equal(await producer.getTitle(ctx), "renamed fixture title");
  assert.deepEqual(readTrace(producer.journal), [], "renegotiation is not event replay");
});

test("durable:ordered-overflow-compaction-precedes-agent-recovery", { timeout: 11000 }, async t => {
  const p = await pi(t, "rpc", "overflow");
  await p.send("prompt", { message: "force overflow and recover" });
  await p.until(r => r.type === "agent_settled");
  const out = await p.finish();
  assert.equal(out.code, 0, out.stderr);
  ordered(out.records, [
    r => r.type === "message_end" && r.message.stopReason === "error",
    r => r.type === "agent_end" && !r.willRetry,
    r => r.type === "compaction_start" && r.reason === "overflow",
    r => r.type === "compaction_end" && r.reason === "overflow" && r.willRetry &&
      r.result?.summary.startsWith("anchored overflow summary") && r.result.summary.includes("split-turn fixture context"),
    "agent_start", r => r.type === "message_end" && r.message.role === "assistant" && r.message.stopReason === "stop",
    "agent_settled",
  ]);
  assert.equal(out.records.filter(r => r.type === "agent_settled").length, 1);

  const retryRun = new OrderedRun(new TraceJournal(4096), { ownedOverflow: true });
  let compactor;
  const h = await openDurable([], { compaction: { enabled: true, keepRecentTokens: 250 } }, {
    producer: retryRun, beforeRequest: (api, ctx) => compactor?.beforeGenerationRequest(api.taskId, ctx),
  });
  t.after(() => h.close());
  h.faux.setResponses([fauxAssistantMessage("old answer ".repeat(150)), fauxAssistantMessage("kept answer ".repeat(150)),
    fauxAssistantMessage("", { stopReason: "error", errorMessage: "Input exceeds the context window" }),
    fauxAssistantMessage("anchored overflow summary"), fauxAssistantMessage("recovered after overflow")]);
  for (const content of ["old input ".repeat(240), "kept input ".repeat(240)]) {
    assert.equal((await (await h.conv.submit({ type: "input", content }, ctx)).wait(ctx)).status, "done");
    await h.conv.waitForIdle(ctx);
  }
  const cursor = retryRun.journal.frames.length;
  compactor = new OrderedCompaction(h.harness, h.conv, retryRun.journal);
  const receipt = await h.conv.submit({ type: "input", content: "force overflow and recover" }, ctx);
  assert.equal((await receipt.wait(ctx)).status, "done");
  await h.conv.waitForIdle(ctx);
  const projected = readTrace(retryRun.journal).slice(cursor);
  const key = rows => rows.flatMap(row => {
    if (["agent_start", "agent_end", "agent_settled", "compaction_start", "compaction_end"].includes(row.type)) {
      return [{ type: row.type, reason: row.reason, willRetry: row.willRetry, aborted: row.aborted }];
    }
    if (row.type === "message_end" && row.message.role === "assistant") {
      return [{ type: row.type, stopReason: row.message.stopReason }];
    }
    return [];
  });
  assert.deepEqual(key(projected), key(out.records));
  const placed = projected.find(row => row.type === "compaction_end");
  assert.equal(placed.result.summary, "anchored overflow summary");
  assert.equal(placed.result.firstKeptEntryId, String((await h.conv.context(ctx)).head.head));
});
