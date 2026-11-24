import assert from "node:assert/strict";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { withAbortSignal } from "@earendil-works/chord/context";
import { fauxAssistantMessage } from "@earendil-works/pi-ai";
import { AssistantEntry, CompactionTask, ConversationBusy, GenerationTask, UserEntry } from "@earendil-works/pi-durable";
import { ctx, openDurable, toolRound } from "./durable-probe.mjs";
import { LocalQueue } from "./local-queue.mts";
import { acceptTrace, OrderedRun } from "./ordered-run.mts";
import { ordered, pi, textOf } from "./reference-probe.mjs";
import { CompactionFence } from "./threshold-fence.mts";

async function history(conv, high) {
  await conv.commit(async tx => {
    for (const [user, assistant] of [["old input ", "old answer "], ["kept input ", "kept answer "]]) {
      await tx.appendEntry(UserEntry, conv.id, { model: [{ role: "user", content: user.repeat(high ? 300 : 240), timestamp: 1 }] });
      const usage = { input: 1500, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 1501,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
      const reply = fauxAssistantMessage(assistant.repeat(high ? 200 : 150), { timestamp: 1 });
      await tx.appendEntry(AssistantEntry, conv.id, { model: [high ? reply : { ...reply, usage }] });
    }
  }, ctx);
}

async function fixture(t, options = {}) {
  const producer = new OrderedRun(undefined, { hostSettlement: true });
  let profile;
  // The Durable fixture has a smaller system prompt than Pi. Its threshold uses
  // the same rule but reserves more tokens so this same seeded history crosses it.
  const reserveTokens = options.reserveTokens ?? 126000;
  const keepRecentTokens = options.keepRecentTokens ?? 250;
  const h = await openDurable([], {
    compaction: { enabled: false, reserveTokens, keepRecentTokens },
    retry: { enabled: options.retry ?? false, baseDelayMs: 1, maxRetries: 1 },
  }, {
    producer, localQueue: true, seed: options.empty ? undefined : conv => history(conv, options.highHistory),
    beforeCompact: task => { if (!options.dropSelection) profile.beforeCompact(task); },
    beforeRequest: (api, context) => profile.beforeRequest(api.taskId, context),
    afterTools: (assistant, results, api, context) => profile.afterTools(api.taskId, assistant, results, context),
    async afterRun(summary, api, context) {
      await profile.afterAnswer(api.taskId, summary.last, context);
      await options.holdAfterRun?.();
    },
  });
  t.after(() => h.close());
  const model = h.faux.getModel();
  profile = new CompactionFence(h.harness, h.conv, producer, {
    enabled: options.enabled ?? true, reserveTokens, keepRecentTokens,
    provider: model.provider, modelId: model.id, contextWindow: model.contextWindow,
  });
  const receipt = async (context = ctx, text = "fixture prompt") => {
    const admission = await profile.prompt({ text }, undefined, context);
    assert.equal(admission.disposition, "started");
    return { ...admission, submission: await h.harness.submission(admission.submissionId, ctx) };
  };
  const rows = () => {
    assert.equal(producer.journal.failure, undefined);
    return producer.journal.frames.map(acceptTrace(producer.journal.ready));
  };
  return { ...h, profile, producer, receipt, rows };
}

// Pi may add a separate split-turn summary and uses Pi UUIDs rather than Durable IDs.
// Compare lifecycle, command effects and the summary disposition, not different cut text or token totals.
function lifecycle(rows) {
  return rows.flatMap(row => {
    switch (row.type) {
      case "agent_start": case "agent_settled": case "turn_start": case "queue_update": case "compaction_start":
      case "summarization_retry_scheduled": case "summarization_retry_attempt_start": case "summarization_retry_finished":
        return [row];
      case "agent_end":
        return [{ type: row.type, willRetry: row.willRetry }];
      case "turn_end":
        return [{ type: row.type, stopReason: row.message.stopReason, toolResults: row.toolResults.map(r => r.toolCallId) }];
      case "compaction_end":
        return [{ type: row.type, reason: row.reason, aborted: row.aborted, willRetry: row.willRetry,
          errorMessage: row.errorMessage, applied: !!row.result }];
      case "message_end":
        if (row.message.role === "user" || row.message.role === "assistant") {
          return [{ type: row.type, role: row.message.role, text: textOf(row.message.content), stopReason: row.message.stopReason }];
        }
        return [];
      default:
        return [];
    }
  });
}

for (const scenario of ["threshold", "threshold_retry", "threshold_failure"]) {
  test(`durable:profile-post-answer-${scenario}-matches-pi`, { timeout: 12000 }, async t => {
    const reference = await pi(t, "json", scenario);
    const expected = await reference.finish();
    assert.equal(expected.code, 0, expected.stderr);
    const h = await fixture(t, { retry: scenario === "threshold_retry" });
    const failure = scenario === "threshold_retry" ? "529 overloaded" : "fixture summary failure";
    h.faux.setResponses([
      fauxAssistantMessage("threshold answer"),
      ...(scenario === "threshold" ? [] : [fauxAssistantMessage("", { stopReason: "error", errorMessage: failure })]),
      ...(scenario === "threshold_failure" ? [] : [fauxAssistantMessage("threshold summary")]),
    ]);
    const run = await h.receipt();
    assert.equal((await run.submission.wait(ctx)).status, "done");
    await run.settled;
    const rows = h.rows();
    assert.deepEqual(lifecycle(rows), lifecycle(expected.records));
    const compaction = rows.find(row => row.type === "compaction_end");
    const { head } = await h.conv.context(ctx);
    if (scenario === "threshold_failure") {
      assert.equal(head, undefined);
      assert.equal(compaction.result, undefined);
    } else {
      assert.equal(head.data.reason, "manual", "Durable keeps the native reason");
      assert.equal(String(head.head), compaction.result.firstKeptEntryId);
      assert.equal(compaction.result.summary, "threshold summary");
    }
  });
}

test("durable:profile-pre-prompt-threshold-matches-pi", { timeout: 10000 }, async t => {
  const p = await pi(t, "json", "threshold_pre_prompt");
  const reference = await p.finish();
  assert.equal(reference.code, 0, reference.stderr);
  const h = await fixture(t, { highHistory: true });
  h.faux.setResponses([fauxAssistantMessage("pre-prompt summary"), ({ messages }) => {
    assert.ok(messages.some(m => m.role === "user" && textOf(m.content).includes("pre-prompt summary")));
    return fauxAssistantMessage("pre-prompt answer");
  }]);
  const run = await h.receipt();
  assert.equal((await run.submission.wait(ctx)).status, "done");
  await run.settled;
  const rows = h.rows();
  assert.deepEqual(lifecycle(rows), lifecycle(reference.records));
  assert.deepEqual(rows.slice(0, 3).map(row => row.type), ["compaction_start", "compaction_end", "agent_start"]);
  assert.equal(h.faux.state.callCount, 2);
});

test("durable:profile-between-tool-turns-uses-placed-write-before-next-request", { timeout: 10000 }, async t => {
  const p = await pi(t, "json", "threshold_tools");
  const reference = await p.finish();
  assert.equal(reference.code, 0, reference.stderr);
  const h = await fixture(t);
  const writes = [];
  h.harness.observeCommits({
    advance(batch) { for (const c of batch.changes) {
      if (c.type === "submission" && c.value.type === "write") writes.push(c.value);
    } },
    end() {},
  });
  let requestMessages;
  h.faux.setResponses([toolRound(), fauxAssistantMessage("between-turn summary"), ({ messages }) => {
    requestMessages = messages;
    return fauxAssistantMessage("after threshold tools");
  }]);
  const run = await h.receipt();
  assert.equal((await run.submission.wait(ctx)).status, "done");
  await run.settled;
  const rows = h.rows();
  assert.deepEqual(lifecycle(rows), lifecycle(reference.records));
  ordered(rows, ["tool_execution_end", "turn_end", "compaction_start", "compaction_end", "turn_start", "agent_end", "agent_settled"]);
  assert.deepEqual(writes.map(w => w.status), ["queued", "done"]);
  assert.equal(writes[0].id, writes[1].id);
  assert.equal((await h.conv.context(ctx)).head.id, writes[1].entry);
  assert.ok(requestMessages.some(m => m.role === "user" && textOf(m.content).includes("between-turn summary")));
  assert.ok(requestMessages.some(m => m.role === "toolResult" && m.toolCallId === "fixture-call"));
});

test("durable:profile-queued-answer-turn-compacts-before-delivery", { timeout: 10000 }, async t => {
  const p = await pi(t, "rpc", "threshold_queued");
  await p.send("prompt", { message: "fixture prompt" });
  await p.until(row => row.type === "agent_start");
  const followUp = "queued before the answer";
  assert.deepEqual(await p.send("follow_up", { message: followUp }), { disposition: "queued" });
  const h = await fixture(t);
  const started = Promise.withResolvers(), release = Promise.withResolvers();
  try {
    h.faux.setResponses([async () => { started.resolve(); await release.promise; return fauxAssistantMessage("answer with queued input"); },
      fauxAssistantMessage("queued turn summary"), fauxAssistantMessage("queued turn answer")]);
    const run = await h.receipt();
    await started.promise;
    assert.deepEqual(await h.profile.prompt({ text: followUp }, "followUp", ctx), { disposition: "queued" });
    await writeFile(join(p.dir, "release-threshold-fixture"), "ready\n");
    release.resolve();
    await run.settled;
    await p.until(row => row.type === "agent_settled");
    const reference = await p.finish();
    assert.equal(reference.code, 0, reference.stderr);
    const rows = h.rows();
    assert.deepEqual(lifecycle(rows), lifecycle(reference.records));
    ordered(rows, ["turn_end", "compaction_start", "compaction_end", "turn_start", "queue_update",
      row => row.type === "message_end" && row.message.role === "user" && textOf(row.message.content) === followUp,
      "agent_end", "agent_settled"]);
    assert.equal(rows.filter(row => row.type === "agent_start").length, 1);
  } finally { release.resolve(); }
});

const overflowError = () => fauxAssistantMessage("", { stopReason: "error", errorMessage: "Input exceeds the context window" });

for (const scenario of ["overflow", "overflow_failed", "overflow_again", "threshold_then_overflow"]) {
  test(`durable:profile-${scenario}-uses-one-policy`, { timeout: 12000 }, async t => {
    const combined = scenario === "threshold_then_overflow";
    const text = combined ? "request with enough new context for a second cut ".repeat(120).trimEnd() : "fixture prompt";
    const p = await pi(t, "rpc", scenario);
    await p.send("prompt", { message: text });
    await p.until(row => row.type === "agent_settled");
    const reference = await p.finish();
    assert.equal(reference.code, 0, reference.stderr);
    const h = await fixture(t, combined ? { highHistory: true } : { reserveTokens: 16384 });
    const summary = scenario === "overflow_failed"
      ? fauxAssistantMessage("", { stopReason: "error", errorMessage: "fixture summary failure" })
      : fauxAssistantMessage("anchored overflow summary");
    let recoveryMessages;
    h.faux.setResponses([
      ...(combined ? [fauxAssistantMessage("pre-prompt summary")] : []), overflowError(), summary,
      ...(scenario === "overflow_failed" ? [] : [({ messages }) => {
        recoveryMessages = messages;
        return scenario === "overflow_again" ? overflowError() : fauxAssistantMessage("recovered after overflow");
      }]),
    ]);
    const run = await h.receipt(ctx, text);
    await run.settled;
    assert.equal((await run.submission.status(ctx)).status, "unanswered", "never rewrite the original native receipt");
    const rows = h.rows();
    assert.deepEqual(lifecycle(rows), lifecycle(reference.records));
    assert.equal(rows.filter(r => r.type === "agent_settled").length, 1);
    assert.equal(rows.filter(r => r.type === "compaction_start" && r.reason === "overflow").length, 1);
    assert.equal(rows.filter(r => r.type === "message_end" && r.message.role === "user").length, 1);
    if (recoveryMessages) {
      assert.ok(recoveryMessages.some(m => m.role === "user" && textOf(m.content).includes("anchored overflow summary")));
      assert.ok(!recoveryMessages.some(m => m.role === "assistant" && m.stopReason === "error"));
    }
    assert.equal(h.faux.state.callCount, (combined ? 1 : 0) + (scenario === "overflow_failed" ? 2 : 3));
  });
}

async function held(t, overflow = false) {
  const h = await fixture(t, overflow ? { reserveTokens: 16384 } : {});
  const summarizing = Promise.withResolvers(), release = Promise.withResolvers();
  h.faux.setResponses([overflow ? overflowError() : fauxAssistantMessage("threshold answer"), async (_request, { signal }) => {
    const cancel = () => release.resolve();
    signal.addEventListener("abort", cancel, { once: true });
    try {
      summarizing.resolve();
      signal.throwIfAborted();
      await release.promise;
      signal.throwIfAborted();
      return fauxAssistantMessage("threshold summary");
    } finally { signal.removeEventListener("abort", cancel); }
  }, fauxAssistantMessage("after late input")]);
  return { ...h, summarizing: summarizing.promise, release: release.resolve };
}

test("durable:profile-aborted-overflow-settles-without-continuation", { timeout: 10000 }, async t => {
  const p = await pi(t, "rpc", "overflow_aborted");
  await p.send("prompt", { message: "fixture prompt" });
  await p.until(row => row.type === "compaction_start" && row.reason === "overflow");
  const h = await held(t, true);
  try {
    const run = await h.receipt();
    await Promise.race([h.summarizing, run.settled.then(() => assert.fail("settled before compactor ran"))]);
    await Promise.all([p.send("abort"), h.profile.abort(ctx)]);
    await run.settled;
    assert.equal((await run.submission.status(ctx)).status, "unanswered");
    await p.until(row => row.type === "agent_settled");
    const reference = await p.finish();
    assert.equal(reference.code, 0, reference.stderr);
    assert.deepEqual(lifecycle(h.rows()), lifecycle(reference.records));
    assert.equal(h.faux.state.callCount, 2);
    assert.equal((await h.conv.context(ctx)).head, undefined);
  } finally { h.release(); }
});

for (const abort of [false, true]) {
  test(`durable:profile-threshold-${abort ? "abort-joins-and-retains-queue" : "holds-busy-and-drains-late-input"}`,
    { timeout: 12000 }, async t => {
      const reference = await pi(t, "rpc", "threshold_hold");
      await reference.send("prompt", { message: "fixture prompt" });
      await reference.until(row => row.type === "compaction_start");
      const referenceState = await reference.send("get_state");
      assert.ok(referenceState.isCompacting && referenceState.isStreaming);

      const h = await held(t);
      try {
        const run = await h.receipt();
        assert.equal((await run.submission.wait(ctx)).status, "done", "receipt is independent of the drain");
        await Promise.race([h.summarizing, run.settled.then(() => assert.fail("settled before compactor ran"))]);
        const tasks = (await h.harness.inspect(ctx)).tasks;
        assert.equal(tasks.length, 1);
        assert.equal(tasks[0].record.kind, CompactionTask.definition.name);
        assert.equal(tasks[0].record.background, false);
        assert.equal(h.rows().at(-1).type, "compaction_start");
        assert.equal(h.profile.busy, true);
        await reference.send("prompt", { message: "reject while draining" }, false);
        await assert.rejects(h.profile.prompt({ text: "reject while draining" }, undefined, ctx), ConversationBusy);
        const message = "late input";
        assert.deepEqual(await reference.send("prompt", { message, streamingBehavior: "followUp" }), { disposition: "queued" });
        assert.deepEqual(await h.profile.prompt({ text: message }, "followUp", ctx), { disposition: "queued" });
        assert.ok(!h.rows().some(row => row.type === "agent_settled" || row.type === "compaction_end"));
        assert.equal(h.faux.state.callCount, 2);

        if (abort) {
          await Promise.all([h.profile.abort(ctx), reference.send("abort")]);
        } else {
          await writeFile(join(reference.dir, "release-threshold-fixture"), "ready\n");
          h.release();
        }
        await run.settled;
        await reference.until(row => row.type === "agent_settled");
        const queues = await new LocalQueue(h.harness, h.conv).inspect(ctx);
        assert.deepEqual(queues.followUp, abort ? [message] : []);
        assert.equal((await reference.send("get_state")).pendingMessageCount, queues.followUp.length);
        const expected = await reference.finish();
        assert.equal(expected.code, 0, expected.stderr);
        assert.deepEqual(lifecycle(h.rows()), lifecycle(expected.records));
        assert.equal((await h.conv.context(ctx)).head !== undefined, !abort);
        assert.equal(h.faux.state.callCount, abort ? 2 : 3);
        assert.equal(h.profile.busy, false);
      } finally { h.release(); }
    });
}

test("durable:profile-abort-restores-exact-unplaced-handoff-before-settling", { timeout: 10000 }, async t => {
  const started = Promise.withResolvers(), answer = Promise.withResolvers();
  const draining = Promise.withResolvers(), release = Promise.withResolvers(), abortMarked = Promise.withResolvers();
  const h = await fixture(t, { enabled: false, async holdAfterRun() { draining.resolve(); await release.promise; } });
  try {
    h.faux.setResponses([async () => { started.resolve(); await answer.promise; return fauxAssistantMessage("finished"); }]);
    h.harness.observeCommits({
      advance(batch) {
        if (batch.changes.some(c => c.type === "task" && c.value.kind === GenerationTask.definition.name && c.value.abortRequested)) {
          abortMarked.resolve();
        }
      },
      end() {},
    });
    const run = await h.receipt();
    await started.promise;
    const texts = ["selected but unplaced", "behind selected input"];
    for (const text of texts) assert.deepEqual(await h.profile.prompt({ text }, "followUp", ctx), { disposition: "queued" });
    answer.resolve();
    await draining.promise;
    assert.equal((await run.submission.wait(ctx)).status, "done");
    const pending = (await h.harness.inspect(ctx)).submissions.find(row => row.type === "input" && row.status === "queued");
    assert.ok(pending);
    const queue = new LocalQueue(h.harness, h.conv);
    assert.deepEqual((await queue.inspect(ctx)).followUp, [texts[1]]);
    const aborted = h.profile.abort(ctx);
    void aborted.catch(() => {});
    await abortMarked.promise;
    release.resolve();
    await aborted;
    const receipt = await h.harness.submission(pending.id, ctx), stopped = await receipt.status(ctx);
    assert.equal(stopped.status, "unanswered");
    assert.equal(stopped.reason, "aborted");
    assert.equal(stopped.entry, undefined);
    assert.deepEqual((await queue.inspect(ctx)).followUp, texts);
    const rows = h.rows(), queues = rows.filter(row => row.type === "queue_update");
    assert.deepEqual(queues.map(row => row.followUp), [[texts[0]], texts], "restoration is not a duplicate enqueue");
    assert.deepEqual(rows.at(-1), { type: "agent_settled", aborted: true });
    assert.equal(rows.filter(r => r.type === "message_end" && r.message.role === "user").length, 1);
    assert.equal(h.faux.state.callCount, 1);
  } finally { answer.resolve(); release.resolve(); }
});

test("durable:profile-threshold-no-cut-has-no-compaction-events", { timeout: 10000 }, async t => {
  const h = await fixture(t, { empty: true, reserveTokens: 127999, keepRecentTokens: 20000 });
  h.faux.setResponses([fauxAssistantMessage("short normal answer")]);
  const run = await h.receipt();
  await run.settled;
  assert.equal(h.rows().at(-1).type, "agent_settled");
  assert.equal(h.faux.state.callCount, 1);
  assert.ok(!h.rows().some(row => row.type.startsWith("compaction_")));
});

test("durable:profile-threshold-policy-does-not-compact-on-disabled-or-below-limit", { timeout: 10000 }, async t => {
  for (const options of [{ enabled: false }, { reserveTokens: 1000 }]) {
    const h = await fixture(t, options);
    h.faux.setResponses([fauxAssistantMessage("normal answer")]);
    const run = await h.receipt();
    await run.settled;
    assert.equal(h.rows().at(-1).type, "agent_settled");
    assert.equal((await h.conv.context(ctx)).head, undefined);
    assert.equal(h.faux.state.callCount, 1);
  }
});

test("durable:profile-threshold-outside-proven-predicate-fails-closed", { timeout: 10000 }, async t => {
  const h = await fixture(t);
  h.faux.setResponses([fauxAssistantMessage("partial", { stopReason: "length" })]);
  const run = await h.receipt();
  assert.equal((await run.submission.wait(ctx)).status, "done");
  await assert.rejects(run.settled, /unproved stop reason/);
  assert.ok(!h.producer.journal.frames.some(({ event }) => event.type === "agent_settled"));
  assert.equal(h.faux.state.callCount, 1);
});

test("durable:profile-threshold-lost-selection-fails-without-settling", { timeout: 10000 }, async t => {
  const h = await fixture(t, { dropSelection: true });
  h.faux.setResponses([fauxAssistantMessage("threshold answer"), fauxAssistantMessage("threshold summary")]);
  const run = await h.receipt();
  assert.equal((await run.submission.wait(ctx)).status, "done");
  await assert.rejects(run.settled, /profile trace lost.*without observed selection/);
  assert.ok((await h.conv.context(ctx)).head, "a trace fault doesn't undo the native commit");
  assert.ok(!h.producer.journal.frames.some(({ event }) => event.type === "agent_settled" || event.type === "compaction_end"));
  await assert.rejects(h.profile.prompt({ text: "must not replay" }, undefined, ctx), /profile trace lost/);
  assert.equal(h.faux.state.callCount, 2);
});

test("durable:profile-threshold-cancelled-observer-does-not-replay-or-abort", { timeout: 10000 }, async t => {
  const h = await held(t);
  const disconnect = new AbortController();
  try {
    const run = await h.receipt(withAbortSignal(disconnect.signal, ctx));
    await Promise.race([h.summarizing, run.settled.then(() => assert.fail("settled before compactor ran"))]);
    assert.equal(h.rows().at(-1).type, "compaction_start");
    disconnect.abort(new Error("fixture observer disconnected"));
    await assert.rejects(run.settled, /fixture observer disconnected/);
    const { tasks } = await h.harness.inspect(ctx);
    assert.equal(tasks.length, 1);
    assert.equal(tasks[0].record.abortRequested, false);
    h.release();
    await h.conv.waitForIdle(ctx);
    assert.ok((await h.conv.context(ctx)).head, "ordinary native work can still commit after trace loss");
    assert.ok(!h.producer.journal.frames.some(({ event }) => event.type === "agent_settled" || event.type === "compaction_end"));
    await assert.rejects(h.profile.prompt({ text: "must not replay" }, undefined, ctx), /profile trace lost/);
    assert.equal(h.faux.state.callCount, 2);
  } finally { h.release(); }
});
