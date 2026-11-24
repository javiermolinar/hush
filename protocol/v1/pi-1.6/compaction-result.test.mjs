import assert from "node:assert/strict";
import { setImmediate } from "node:timers/promises";
import { test } from "node:test";
import { fauxAssistantMessage } from "@earendil-works/pi-ai";
import { AssistantEntry, CompactionEntry, CompactionTask, hookTask, UserEntry } from "@earendil-works/pi-durable";
import { observeCompactionSelections, resolveCompaction } from "./compaction-result.mts";
import { ctx, openDurable } from "./durable-probe.mjs";

const usage = {
  input: 600, output: 13, cacheRead: 0, cacheWrite: 0, totalTokens: 613,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};
const user = (content) => ({ role: "user", content, timestamp: 1 });
const assistant = (text) => ({ ...fauxAssistantMessage(text, { timestamp: 1 }), usage });

async function fixture(t, policy = {}, { seed = true, afterRun, onReport } = {}) {
  const h = await openDurable([], { compaction: { enabled: false, keepRecentTokens: 16, ...policy } }, { afterRun, onReport });
  t.after(h.close);
  const selections = new Map();
  const watch = observeCompactionSelections(h.harness, (id, selection) => selections.set(id, selection));
  t.after(() => watch.detach());
  const resolve = (id, placement) => resolveCompaction(h.harness, h.conv, id, (id) => selections.get(id), ctx, placement);
  const entries = seed ? await h.conv.commit(async (tx) => {
    const ids = [];
    for (const [token, message] of [
      [UserEntry, user("old ".repeat(400))],
      [AssistantEntry, assistant("a".repeat(360))],
      [UserEntry, user("k".repeat(240))],
      [AssistantEntry, assistant("r".repeat(128))],
    ]) {
      ids.push((await tx.appendEntry(token, h.conv.id, { model: [message] })).id);
    }
    return ids;
  }, ctx) : [];
  return { ...h, resolve, selections, entries };
}

const task = (h, id) => h.harness.waitForTask(id, ctx).then((t) => t.state.outcome);
const compact = (h) => h.conv.compact(undefined, ctx);

test("durable:compaction-result-anchored", { timeout: 10000 }, async (t) => {
  const h = await fixture(t);
  const last = h.entries.at(-1);
  h.faux.setResponses([
    fauxAssistantMessage("", { stopReason: "error", errorMessage: "529 overloaded" }),
    fauxAssistantMessage("short summary"),
  ]);
  const id = await compact(h);
  const outcome = await task(h, id);
  assert.equal(h.faux.state.callCount, 2);
  assert.equal(outcome.status, "completed");
  assert.ok(outcome.result.submissionId);
  // Advance the conversation before reading the result; this must not affect either estimate.
  await h.conv.commit((tx) => tx.appendEntry(UserEntry, h.conv.id, { model: [user("X".repeat(2000))] }), ctx);
  const projected = await h.resolve(id);
  assert.equal(projected.kind, "applied");
  assert.equal(projected.reason, "manual");
  assert.deepEqual(h.selections.get(id), { conversationId: h.conv.id, tail: last, firstKept: last });
  assert.deepEqual(projected.result, {
    summary: "short summary", firstKeptEntryId: String(last), tokensBefore: 613, estimatedTokensAfter: 36,
  });
  assert.equal((await h.conv.context(ctx, { at: projected.entryId })).head.kind, CompactionEntry.kind);
  // A post-compaction estimate must not reuse provider usage from before the new head.
  const late = (await h.conv.context(ctx)).entries.at(-1).id;
  h.faux.setResponses([fauxAssistantMessage("second summary")]);
  const second = await h.resolve(await compact(h));
  assert.equal(second.kind, "applied");
  assert.deepEqual(second.result, {
    summary: "second summary", firstKeptEntryId: String(late), tokensBefore: 536, estimatedTokensAfter: 504,
  });
});

test("durable:compaction-result-waits-for-placement", { timeout: 10000 }, async (t) => {
  const h = await fixture(t);
  const started = Promise.withResolvers();
  const release = Promise.withResolvers();
  // Release the fixture even if an assertion fails while generation is paused.
  t.after(() => release.resolve());
  h.faux.setResponses([async () => {
    started.resolve();
    await release.promise;
    return fauxAssistantMessage("active answer");
  }, fauxAssistantMessage("queued summary")]);
  const turn = await h.conv.submit({ type: "input", content: "active prompt" }, ctx);
  try {
    await started.promise;
    const id = await compact(h);
    const finished = await task(h, id);
    assert.equal(finished.status, "completed");
    const write = await h.harness.submission(finished.result.submissionId, ctx);
    assert.equal((await write.status(ctx)).status, "queued");
    assert.deepEqual(await h.resolve(id, "committed"), {
      kind: "unavailable", reason: "manual", message: "Summary write was not placed at the required boundary",
    });
    let done = false;
    const response = h.resolve(id).then((value) => { done = true; return value; });
    // Keep a rejection handled until we join it below, including if the negative assertion fails.
    void response.catch(() => {});
    await setImmediate();
    assert.equal(done, false);
    release.resolve();
    assert.equal((await turn.wait(ctx)).status, "done");
    const result = await response;
    assert.equal(result.kind, "applied");
    assert.equal(result.result.summary, "queued summary");
    assert.equal(result.entryId, (await write.wait(ctx)).entry);
    assert.ok(result.entryId > h.selections.get(id).tail);
    assert.ok(result.result.tokensBefore < 700, "before estimate excludes the concurrent answer");
  } finally {
    release.resolve();
  }
});

test("durable:compaction-result-blocking-threshold", { timeout: 10000 }, async (t) => {
  const h = await fixture(t, { enabled: true, reserveTokens: 127800 });
  h.faux.setResponses([fauxAssistantMessage("automatic summary"), fauxAssistantMessage("after summary")]);
  const submitted = await h.conv.submit({ type: "input", content: "next prompt" }, ctx);
  assert.equal((await submitted.wait(ctx)).status, "done");
  assert.equal(h.selections.size, 1);
  const [id] = h.selections.keys();
  const native = await task(h, id);
  assert.equal(native.status, "completed");
  assert.ok(native.result.entryId);
  assert.equal(native.result.submissionId, undefined);
  const outcome = await h.resolve(id);
  assert.equal(outcome.kind, "applied");
  assert.equal(outcome.reason, "threshold");
  assert.equal(outcome.entryId, native.result.entryId);
  assert.equal(outcome.result.summary, "automatic summary");
  assert.equal(outcome.result.tokensBefore, 616);
  assert.equal(h.faux.state.callCount, 2);
});

test("durable:background-compaction-outlives-ordinary-idle", { timeout: 10000 }, async t => {
  const h = await fixture(t, { enabled: true, reserveTokens: 0, backgroundTokens: 127800 });
  const started = Promise.withResolvers(), release = Promise.withResolvers();
  const makeResponse = ({ messages }) => {
    if (messages[0]?.role === "system" && typeof messages[0].content === "string" &&
        messages[0].content.startsWith("You are a context summarization assistant.")) {
      started.resolve();
      return release.promise.then(() => fauxAssistantMessage("background summary"));
    }
    return fauxAssistantMessage("answer while compaction runs");
  };
  try {
    // Scheduling does not promise the order of the two requests.
    h.faux.setResponses([makeResponse, makeResponse]);
    const input = await h.conv.submit({ type: "input", content: "next prompt" }, ctx);
    await started.promise;
    assert.equal((await input.wait(ctx)).status, "done");
    await h.conv.waitForIdle(ctx);
    const { tasks } = await h.harness.inspect(ctx);
    const compactor = tasks.find(row => row.record.kind === CompactionTask.definition.name);
    assert.ok(compactor?.record.background);
    assert.ok(!CompactionEntry.is((await h.conv.context(ctx)).head));
    release.resolve();
    assert.equal((await h.resolve(compactor.record.id)).kind, "applied");
  } finally { release.resolve(); }
});

test("durable:after-run-hook-cannot-schedule-builtin-compaction", { timeout: 10000 }, async t => {
  const reports = [];
  const h = await fixture(t, {}, {
    onReport(report) { reports.push(report); },
    afterRun() { return { tasks: [hookTask(CompactionTask, { reason: "threshold" })] }; },
  });
  h.faux.setResponses([fauxAssistantMessage("completed answer")]);
  const receipt = await h.conv.submit({ type: "input", content: "next prompt" }, ctx);
  assert.equal((await receipt.wait(ctx)).status, "done");
  await h.conv.waitForIdle(ctx);
  assert.ok(reports.some(report => String(report.error).includes("A hook cannot start the built-in task pi.compaction")), reports);
  assert.equal(h.selections.size, 0);
  assert.equal(h.faux.state.callCount, 1);
});

test("durable:compaction-result-overflow", { timeout: 10000 }, async (t) => {
  const h = await fixture(t, { enabled: true, reserveTokens: 1000 });
  h.faux.setResponses([
    fauxAssistantMessage("", { stopReason: "error", errorMessage: "Input exceeds the context window" }),
    fauxAssistantMessage("overflow summary"),
    fauxAssistantMessage("recovered"),
  ]);
  const input = await h.conv.submit({ type: "input", content: "next prompt" }, ctx);
  assert.equal((await input.wait(ctx)).status, "done");
  assert.equal(h.faux.state.callCount, 3);
  assert.equal(h.selections.size, 1);
  const [id] = h.selections.keys();
  const outcome = await h.resolve(id);
  assert.equal(outcome.kind, "applied");
  assert.equal(outcome.reason, "overflow");
  assert.equal(outcome.result.summary, "overflow summary");
});

test("durable:compaction-result-with-edited-history", { timeout: 10000 }, async (t) => {
  const h = await fixture(t);
  await h.conv.commit((tx) => tx.appendEntry(h.conv.id, {
    kind: "fixture.edit",
    edits: [{ target: h.entries[2], action: "replace", messages: [user("short edit!!")] }],
  }), ctx);
  h.faux.setResponses([fauxAssistantMessage("short summary")]);
  const outcome = await h.resolve(await compact(h));
  assert.equal(outcome.kind, "applied");
  // The previous provider total (613) predates the edit. Use current projected lengths:
  // 1600 / 4 + 360 / 4 + 12 / 4 + 128 / 4.
  assert.equal(outcome.result.tokensBefore, 525);
  assert.equal(outcome.result.estimatedTokensAfter, 36);
});

test("durable:compaction-result-superseded", { timeout: 10000 }, async (t) => {
  const h = await fixture(t, { keepRecentTokens: 120 });
  const started = Promise.withResolvers();
  const release = Promise.withResolvers();
  h.faux.setResponses([async () => {
    started.resolve();
    await release.promise;
    return fauxAssistantMessage("old summary");
  }, fauxAssistantMessage("new summary")]);
  const older = await compact(h);
  try {
    await started.promise;
    // Make the second summary retain a strictly later first entry.
    await h.conv.configure({ compaction: { enabled: false, keepRecentTokens: 16 } }, ctx);
    const newer = await compact(h);
    assert.equal((await h.resolve(newer)).kind, "applied");
    assert.ok(h.selections.get(older).firstKept < h.selections.get(newer).firstKept);
    release.resolve();
    const old = await task(h, older);
    assert.equal(old.status, "completed", "a successful summarization does not prove placement");
    const write = await h.harness.submission(old.result.submissionId, ctx);
    assert.equal((await write.wait(ctx)).reason, "stale");
    assert.deepEqual(await h.resolve(older), { kind: "not_applied", reason: "manual", cause: "stale" });
  } finally {
    release.resolve();
  }
});

test("durable:compaction-result-no-summary", { timeout: 10000 }, async (t) => {
  const h = await fixture(t, {}, { seed: false });
  assert.deepEqual(await h.resolve(await compact(h)), { kind: "not_applied", reason: "manual", cause: "no_summary" });
  assert.equal(h.faux.state.callCount, 0);
});

test("durable:compaction-result-refuses-missing-selection", { timeout: 10000 }, async (t) => {
  const h = await fixture(t);
  h.faux.setResponses([fauxAssistantMessage("short summary")]);
  const id = await compact(h);
  const outcome = await resolveCompaction(h.harness, h.conv, id, () => undefined, ctx);
  assert.equal(outcome.kind, "unavailable");
  assert.match(outcome.message, /not captured/);
});

test("durable:compaction-result-failure", { timeout: 10000 }, async (t) => {
  const h = await fixture(t);
  h.faux.setResponses([fauxAssistantMessage("", { stopReason: "error", errorMessage: "fixture failed" })]);
  const outcome = await h.resolve(await compact(h));
  assert.deepEqual(outcome, { kind: "failed", reason: "manual", message: "Summarization failed: fixture failed" });
  assert.equal((await h.conv.context(ctx)).head, undefined);
});

test("durable:compaction-result-aborted", { timeout: 10000 }, async (t) => {
  const h = await fixture(t);
  const started = Promise.withResolvers();
  h.faux.setResponses([async (_request, { signal }) => {
    started.resolve();
    if (!signal.aborted) await new Promise((resolve) => signal.addEventListener("abort", resolve, { once: true }));
    return fauxAssistantMessage("not applied");
  }]);
  const id = await compact(h);
  await started.promise;
  await h.harness.abortTask(id, ctx);
  assert.deepEqual(await h.resolve(id), { kind: "aborted", reason: "manual" });
  assert.equal((await h.conv.context(ctx)).head, undefined);
});
