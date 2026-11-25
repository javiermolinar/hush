import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai";
import { fauxAssistantMessage, fauxText, fauxThinking } from "@earendil-works/pi-ai/providers/faux";
import { createRegistry, defineExtension, defineTask, Harness } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { ThreadDirectory } from "../../dist/backend/src/thread-directory.js";
import { ThreadPresentationDoc } from "../../dist/backend/src/thread-presentation.js";

test("directory searches workspace names, titles and committed chat before paging", { timeout: 10_000 }, async t => {
  const dir = await mkdtemp("/tmp/hush-thread-search-");
  const signal = t.signal;
  const workspace = join(dir, "deployment_tools"), other = join(dir, "other", "deployment_tools");
  let h, directory;
  try {
    await Promise.all([mkdir(workspace), mkdir(other, { recursive: true })]);
    h = await Harness.open(await openNodeSqliteStorage(join(dir, "history.sqlite")), { registry: createRegistry(), models: createModels() }, context);
    const ownership = { kind: "ownerless" };
    const create = (cwd, title) => h.createConversation({ ownership, agent: { cwd }, init: async (tx, id) => {
      (await tx.doc(ThreadPresentationDoc, id)).title = title;
    } }, context);
    const append = (thread, ...model) => h.commit(tx => tx.appendEntry(thread.id, { kind: "fixture", model }), context);
    const user = (content, timestamp) => ({ role: "user", content, timestamp });
    const old = await create(workspace, "Status report");
    const phrase = 'rollout "window"';
    const before = await append(old, user("Early note: " + "padding ".repeat(200) + phrase, 1));
    const fork = await old.fork(before.id, { ownership }, context);
    await append(old, fauxAssistantMessage([fauxText("first block"), fauxThinking("hidden reasoning"), fauxText("post-fork answer")], { timestamp: 2 }));
    await append(old, user('<skill name="review" location="ignored">fixture skill body</skill>', 3));
    await append(old, { role: "toolResult", toolCallId: "fixture", toolName: "fixture", isError: false,
      content: [fauxText("only in tool output")], timestamp: 3 });
    const recent = await create(other, "Récent review");
    await append(recent, user("The current topic is separate", 4));
    const unknown = await create(join(dir, "absent"), "Missing folder");
    directory = new ThreadDirectory(h, dir, async () => ({}), {});
    const list = (query = {}) => directory.list({ limit: 10, ...query }, signal, 2, 2);
    const ids = page => page.threads.map(row => row.id);
    // The first page waits at most 120 ms for the startup index, then returns
    // a partial page; slow hosts can exceed that, so wait for the full index.
    let initial = await list();
    while (initial.indexing) {
      await delay(20);
      initial = await list();
    }
    assert.equal(initial.threads[0].id, String(recent.id));
    assert.equal(initial.workspaces[0].name, "deployment_tools");
    assert.equal(initial.workspaces[ids(initial).indexOf(String(unknown.id))], null);

    const byName = await list({ query: "DEPLOYMENT TOOLS" });
    assert.deepEqual(ids(byName), [recent, old, fork].map(thread => String(thread.id)));
    assert.notEqual(byName.workspaces[0].id, byName.workspaces[1].id, "same-name directories stay distinct");
    assert.deepEqual(byName.workspaces[1], byName.workspaces[2], "forks share the workspace");
    assert.deepEqual(ids(await list({ query: "RÉ" })), [String(recent.id)]);
    assert.deepEqual(ids(await list({ query: "post-fork answer" })), [String(old.id)]);
    assert.deepEqual(ids(await list({ query: "hidden reasoning" })), []);
    assert.deepEqual(ids(await list({ query: "only in tool output" })), []);
    assert.deepEqual(ids(await list({ query: "fixture skill body" })), []);

    const search = { query: phrase, workspace, limit: 1 };
    const first = await list(search);
    assert.deepEqual(ids(first), [String(old.id)], "an older content match survives the page cap");
    assert.deepEqual(first.workspaceMatches, ["inside"]);
    assert.ok(first.nextCursor);
    assert.deepEqual(ids(await list({ ...search, query: phrase.toUpperCase(), cursor: first.nextCursor })), [String(fork.id)]);
    await assert.rejects(list({ ...search, query: "different query", cursor: first.nextCursor }), { code: "invalid_cursor" });
    await assert.rejects(list({ ...search, browseAll: true, cursor: first.nextCursor }), { code: "invalid_cursor" });

    await append(recent, user(`Recently committed ${phrase}`, 5));
    assert.deepEqual(ids(await list({ ...search, browseAll: true })), [String(recent.id)], "new chat becomes searchable");
    await directory.management.archive({ threadId: String(recent.id), archived: true }, signal);
    assert.deepEqual(ids(await list({ ...search, browseAll: true })), [String(old.id)], "archived matches stay hidden");
  } finally {
    await directory?.close();
    await h?.close(context);
    await rm(dir, { recursive: true, force: true });
  }
});

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
    const list = () => directory.list({ limit: 10 }, new AbortController().signal, 2);
    let page = await list();
    while (page.indexing) { // a slow host can outlast the first page's wait for the index
      await delay(20);
      page = await list();
    }
    assert.equal(page.threads.find(row => row.id === String(root.id))?.status, "idle");
    assert.equal(page.threads.find(row => row.id === String(thread.id))?.status, "scheduled");
  } finally {
    await directory?.close();
    await h?.close(context);
    await rm(dir, { recursive: true, force: true });
  }
});
