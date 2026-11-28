import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai";
import { createRegistry, defineExtension, defineTask, Harness, SubmissionSettled } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { retiredTasks } from "../../dist/backend/src/retired-tasks.js";

// Stands in for a removed task: it owns a reviewer and parks on its settlement.
const removed = (name, initial, version) => defineTask({ name, version, initial: () => ({ phase: initial }), phases: {
  [initial]: (task, runtime, ctx) => runtime.commit(async tx => {
    const reviewer = await tx.createConversation({ ownership: { kind: "task", taskId: task.id } });
    return { status: "sleeping", signals: [SubmissionSettled.key("review-settled")], checkpoint: { phase: "settle", reviewer: reviewer.id } };
  }, ctx),
  settle: () => { throw new Error("the removed definition must not run after restart"); },
}, abort: () => { throw new Error("the removed definition must not run after restart"); } });

const kinds = [["hush.plan-review", "open", 1], ["hush.review-worker", "gate", 1], ["hush.plan-scoped-run", "next", 1],
  ["hush.plan-scoped-run", "next", 2], ["hush.plan-fixture-run", "next", 1]];
for (const [name, initial, version] of kinds) test(`a stored ${name} v${version} task drains after restart`, { timeout: 8_000 }, async t => {
  const removedReview = removed(name, initial, version);
  const ctx = withAbortSignal(t.signal, BACKGROUND_CONTEXT);
  const dir = await mkdtemp(join(tmpdir(), "hush-retired-tasks-"));
  const database = join(dir, "history.sqlite");
  let h;
  t.after(async () => { await h?.close(BACKGROUND_CONTEXT); await rm(dir, { force: true, recursive: true }); });
  const open = async extension => {
    const registry = createRegistry();
    registry.install(extension);
    return Harness.open(await openNodeSqliteStorage(database), { registry, models: createModels() }, ctx);
  };

  h = await open(defineExtension({ name: "removed", tasks: [removedReview] }));
  const parent = await h.root(ctx);
  const id = await h.commit(tx => tx.createTask(removedReview, { planId: "plan", revision: 1 },
    { ownership: { kind: "conversation" }, conversationId: parent.id, background: true, abandonOnRestart: true }), ctx);
  h.resume();
  for (let i = 0; (await h.commit(tx => tx.task(id), ctx))?.state.status !== "sleeping"; i++) {
    assert.ok(i < 400, "the stand-in review must park before the restart");
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  await h.close(ctx);
  h = undefined;

  // Durable keeps an abandoned task whose definition is missing until one is
  // installed. The retired definition lets it, and its parent, settle.
  h = await open(retiredTasks);
  h.resume();
  const settled = await h.waitForTask(id, ctx);
  assert.equal(settled.state.outcome.status, "aborted");
  const record = await h.commit(tx => tx.task(id), ctx);
  assert.equal(record.abortReason, "restart");
});
