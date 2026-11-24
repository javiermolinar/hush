import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai";
import { createRegistry, defineExtension, defineTask, Harness } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { ThreadDirectory } from "../../dist/backend/src/thread-directory.js";

// Use a Durable task from the host fixture, not a production scheduling API.
// The directory must project a live sleeping background task after a restart.
test("directory projects a durable future background wake as scheduled", { timeout: 10_000 }, async t => {
  const dir = await mkdtemp("/tmp/hush-thread-task-");
  const registry = createRegistry();
  const finish = (_task, runtime, ctx) => runtime.commit(() => ({ status: "terminal", outcome: { status: "completed", result: null } }), ctx);
  const task = defineTask({ name: "fixture-thread-timer", version: 1, initial: () => ({ phase: "start" }),
    phases: { start: (task, runtime, ctx) => runtime.sleepUntil(task.input.until, { phase: "finish" }, ctx), finish }, abort: finish });
  registry.install(defineExtension({ name: "fixture-thread-timer", tasks: [task] }));
  const storage = () => openNodeSqliteStorage(join(dir, "history.sqlite"));
  const open = async () => Harness.open(await storage(), { registry, models: createModels() }, context);
  let h, directory;
  try {
    h = await open();
    const root = await h.root(context);
    const thread = await h.createConversation({ ownership: { kind: "ownerless" } }, context);
    await thread.commit(tx => tx.createTask(task, { until: Date.now() + 60_000 }, { ownership: { kind: "conversation" }, background: true }), context);
    h.resume();
    for (let i = 0; i < 50; i++) {
      if ((await h.inspect(context)).tasks[0]?.state.kind === "sleeping") break;
      await delay(10);
    }
    assert.equal((await h.inspect(context)).tasks[0]?.state.kind, "sleeping");
    await h.close(context);
    h = await open();
    directory = new ThreadDirectory(h, dir, async () => ({}), {});
    h.resume();
    const page = await directory.list({ limit: 10 }, new AbortController().signal, 2);
    assert.equal(page.indexing, false);
    assert.equal(page.threads.find(row => row.id === String(root.id))?.status, "idle");
    assert.equal(page.threads.find(row => row.id === String(thread.id))?.status, "scheduled");
  } finally {
    await directory?.close();
    await h?.close(context);
    await rm(dir, { recursive: true, force: true });
  }
});
