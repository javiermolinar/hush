import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, readdir, rm } from "node:fs/promises";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai";
import { createRegistry, defineDoc, defineDocFamily, Harness } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { dial } from "../../server/test/client.mjs";
import { startBackend } from "../../dist/hush-backend.mjs";
import { planInventoryPage } from "../../dist/backend/src/plan-catalog.js";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { call, createRequest, draftArgs, planProvider, savedPlanId } from "./plan-provider.mjs";

const okay = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const denied = (frame, code, message) => {
  assert.equal(frame.ok, false, JSON.stringify(frame));
  assert.equal(frame.error.code, code, JSON.stringify(frame));
  if (message) assert.match(frame.error.message, message);
};

async function layout(t) {
  const root = await mkdtemp("/tmp/hush-plan-test-");
  const cwd = join(root, "project"), other = join(root, "other"), agentDir = join(root, ".hush"), dataDir = join(root, "data");
  await Promise.all([cwd, other, agentDir].map(dir => mkdir(dir, { mode: 0o700 })));
  const trust = new WorkspaceTrust(agentDir);
  for (const dir of [cwd, other]) await trust.change(dir, "grant", AbortSignal.timeout(5_000));
  const config = { faux: false, ephemeral: false, idleMs: 120_000, cwd, agentDir, dataDir, socket: join(dataDir, "hush.sock") };
  let backend;
  const peers = [];
  const stop = async () => {
    for (const peer of peers.splice(0)) peer.close();
    await backend?.close();
    backend = undefined;
  };
  t.after(async () => { await stop(); await rm(root, { recursive: true, force: true }); });
  const boot = async (changes = {}) => {
    const options = { ...config, ...changes };
    backend = await startBackend(options);
    return async (versions = [2], expected = 2) => { // null: not negotiated
      const peer = await dial(options.socket);
      peers.push(peer);
      const hello = okay(await peer.send("hello", { versions: [1], capabilities: { plan: versions, planWorkspace: [1], planChat: [1], nativeStop: [1] } }));
      assert.equal(hello.capabilities.plan ?? null, expected);
      for (const removed of ["planWorkspace", "planChat", "planChatActivity", "planCatalog", "planWorktree", "planDraft", "planReview"])
        assert.equal(hello.capabilities[removed], undefined, removed);
      const rpc = async (method, params) => okay(await peer.send(method, params));
      return Object.assign(peer, { rpc });
    };
  };
  return { boot, stop, config, other, trust };
}

async function ask(peer, threadId, text, requestId, whenBusy = "reject") {
  const { submissionId } = await peer.rpc("submit", { threadId, text, requestId, whenBusy });
  const reply = await peer.rpc("submission.wait", { threadId, submissionId });
  assert.equal(reply.status, "done", JSON.stringify(reply));
  return reply.answerEntryId;
}

// Follow a watch until `done` holds.
async function until(read, done) {
  let v = await read();
  for (let i = 0; !done(v) && i < 16; ++i) v = await read(v.revision);
  assert.ok(done(v), JSON.stringify(v));
  return v;
}

const transcript = async (peer, threadId) => (await peer.rpc("history", { threadId, limit: 64, range: { kind: "before", beforeEntryId: null } })).entries;
const statuses = plan => Object.fromEntries(plan.steps.map(s => [s.id, s.reason ? `${s.status}: ${s.reason}` : s.status]));

test("ordinary turns save, list and revise plans by ID, revision-checked", { timeout: 20_000 }, async t => {
  const { boot, stop, config, other } = await layout(t);
  const revise = "Add a docs step to the preferences plan", stale = "Rename the preferences plan", ordinary = "Write plan.txt";
  const elsewhere = "Read the preferences plan from here", closed = "Add a step to the closed plan";
  let planId;
  const provider = await planProvider(t, config.agentDir, async ({ text, tools, prompt, results }) => {
    assert.match(prompt, /don't ask for special wording/);
    assert.match(prompt, /Saving a plan does not authorize implementation/);
    assert.deepEqual(tools.toSorted(), ["bash", "edit", "plan_read", "plan_save", "read", "subagent", "thread_start", "write"],
      "plan_report is offered only in a plan run");
    const last = results.at(-1)?.content;
    if (text === createRequest) {
      if (!results.length) return [call("plan_save", draftArgs())];
      planId = savedPlanId(last);
      assert.match(last, /^Saved "Interface preferences" as plan [0-9a-f-]{36}, revision 1\. Open \/plan/);
      return "Saved.";
    }
    if (text === revise) {
      if (!results.length) return [call("plan_read")];
      if (results.length === 1) {
        const { plans } = JSON.parse(last);
        assert.deepEqual(plans.map(p => [p.id, p.revision, p.total, p.done]), [[planId, 1, 3, 0]]);
        return [call("plan_read", { planId })];
      }
      if (results.length === 2) {
        const plan = JSON.parse(last);
        assert.deepEqual(plan.steps.map(s => [s.id, s.status]), [["save", "pending"], ["restore", "pending"], ["review", "pending"]]);
        assert.deepEqual(plan.steps[2].delegate, { read_only: true });
        const next = draftArgs(plan.revision, "Interface preferences", planId);
        next.steps.push({ id: "docs", title: "Document preferences", prompt: "Describe where preferences are stored." });
        return [call("plan_save", next)];
      }
      assert.match(last, /revision 2\./);
      return "Revised.";
    }
    if (text === stale) {
      if (!results.length) return [call("plan_save", draftArgs(1, "Must not replace", planId))];
      assert.match(last, /revision is stale/);
      return "The plan changed.";
    }
    if (text === elsewhere) {
      if (!results.length) return [call("plan_read", { planId })];
      assert.match(last, /No plan .* in this workspace/);
      return "Not here.";
    }
    if (text === closed) {
      if (!results.length) return [call("plan_save", draftArgs(2, "Closed", planId))];
      assert.match(last, /This plan is closed/);
      return "Closed.";
    }
    assert.equal(text, ordinary);
    if (!results.length) return [call("write", { path: "plan.txt", content: "ordinary" })];
    return "Written.";
  });
  const options = { model: provider.model };
  let connect = await boot(options);
  const a = await connect();
  const owner = (await a.rpc("thread.new", {})).id, peer = (await a.rpc("thread.new", {})).id;
  const outside = (await a.rpc("thread.new", { cwd: other })).id;
  await ask(a, owner, createRequest, "create");
  assert.ok(planId);
  assert.deepEqual(await readdir(config.cwd), [], "saving a plan writes no files");
  const get = after => a.rpc("ext.plan.get", { cwd: config.cwd, planId, after });
  const v1 = await get();
  assert.deepEqual(v1.plan, { id: planId, revision: 1, title: "Interface preferences", workspace: config.cwd, runs: [],
    steps: draftArgs().steps.map(s => ({ ...s, status: "pending" })) });
  const watch = get(v1.revision);
  await ask(a, peer, revise, "revise"); // Any thread of the workspace can revise by ID.
  const v2 = await watch;
  assert.equal(v2.plan.revision, 2);
  assert.deepEqual(v2.plan.steps.map(s => s.id), ["save", "restore", "review", "docs"]);
  await ask(a, owner, stale, "stale");
  assert.deepEqual(await get(), v2);
  await ask(a, outside, elsewhere, "elsewhere");
  await ask(a, owner, ordinary, "ordinary");
  assert.equal(await readFile(join(config.cwd, "plan.txt"), "utf8"), "ordinary");

  const list = (cwd = config.cwd) => a.send("ext.plan.list", { cwd, page: 0 });
  const summary = { id: planId, title: "Interface preferences", revision: 2, workspace: config.cwd, total: 4, done: 0, blocked: 0,
    running: false, closed: false };
  assert.deepEqual(okay(await list()).plans, [summary]);
  assert.deepEqual(okay(await list(other)).plans, [summary], "plans are listed across workspaces");
  denied(await list(dirname(config.cwd)), "invalid_request");
  denied(await a.send("ext.plan.get", { cwd: config.cwd, planId: "missing" }), "not_found");
  denied(await a.send("ext.plan.close", { cwd: other, planId, closed: true }), "not_found", undefined);
  okay(await a.send("ext.plan.close", { cwd: config.cwd, planId, closed: true }));
  assert.equal((await get()).plan.closed, true);
  await ask(a, owner, closed, "closed");
  denied(await a.send("ext.plan.run", { threadId: owner, planId, revision: 2, stepIds: ["save"], requestId: "closed-run" }), "invalid_request", /closed/);
  okay(await a.send("ext.plan.close", { cwd: config.cwd, planId, closed: false }));

  await stop();
  connect = await boot(options);
  const b = await connect();
  assert.deepEqual((await b.rpc("ext.plan.get", { cwd: config.cwd, planId })).plan, v2.plan);
  assert.deepEqual((await b.rpc("ext.plan.list", { cwd: config.cwd, page: 0 })).plans, [summary]);
  denied(await (await connect([1], null)).send("ext.plan.list", { cwd: config.cwd, page: 0 }), "unsupported_capability");
});

test("a run works inline steps, delegates a step and frees its steps when it settles", { timeout: 25_000 }, async t => {
  const { boot, stop, config } = await layout(t);
  const gate = Promise.withResolvers(), busy = Promise.withResolvers(), reported = Promise.withResolvers(), finish = Promise.withResolvers();
  t.after(() => { gate.resolve(); finish.resolve(); });
  const delegated = `Plan "Interface preferences", step review: Review the round trip\n\n${draftArgs().steps[2].prompt}`;
  let planId, runs = 0;
  const provider = await planProvider(t, config.agentDir, async ({ text, tools, results, messages }) => {
    const last = results.at(-1)?.content;
    if (text === createRequest) return results.length ? (planId = savedPlanId(last), "Saved.") : [call("plan_save", draftArgs())];
    if (text === "Stay in this thread") { busy.resolve(); await gate.promise; return "Ready."; }
    if (text === delegated) {
      assert.deepEqual(tools, ["read"], "a read-only step delegate outside Git gets only read");
      return "Restore drops the theme.";
    }
    assert.match(text, /^Plan run\n/);
    const run = JSON.parse(text.split("\n")[1]);
    assert.ok(tools.includes("plan_report") && tools.includes("subagent") && tools.includes("write"));
    if (run.selected.join() === "save,review") {
      if (!results.length) {
        runs++;
        assert.equal(run.plan, planId);
        assert.deepEqual(run.steps.map(s => [s.id, s.status]), [["save", "pending"], ["restore", "pending"], ["review", "pending"]]);
        assert.ok(messages.some(m => m.content === "Stay in this thread"), "the run continues the thread");
        return [call("subagent", { step: "save" }), call("subagent", { step: "review", read_only: false })];
      }
      if (results.length === 2) {
        assert.match(results[0].content, /Step save has no delegate specification/);
        assert.match(results[1].content, /Pass only step/);
        return [call("subagent", { step: "review" })];
      }
      if (results.length === 3) {
        assert.equal(last, "Restore drops the theme.");
        return [call("write", { path: "prefs.txt", content: "theme=mocha\n" })];
      }
      if (results.length === 4) return [call("plan_report", { steps: [{ stepId: "restore", status: "done", detail: "Not selected." }] })];
      if (results.length === 5) {
        assert.match(last, /Step restore is not selected in this run/);
        return [call("plan_report", { steps: [{ stepId: "save", status: "done", detail: "Wrote prefs.txt." },
          { stepId: "review", status: "blocked", detail: "Restore drops the theme." }] })];
      }
      assert.equal(last, "Recorded.");
      reported.resolve();
      await finish.promise; // hold the turn open so the report is seen mid-run
      return "Saved preferences; the review found a problem.";
    }
    assert.equal(run.selected.join(), "restore,review");
    assert.deepEqual(run.steps.map(s => [s.id, s.status, s.reason]), [["save", "done", undefined], ["restore", "pending", undefined],
      ["review", "blocked", "Restore drops the theme."]]);
    if (!results.length) return [call("plan_report", { steps: [{ stepId: "restore", status: "done", detail: "Restore keeps the theme." },
      { stepId: "review", status: "done", detail: "Reviewed the fix." }] })];
    if (results.length === 1) return [call("plan_report", { steps: [{ stepId: "restore", status: "blocked", detail: "Late." }] })];
    assert.match(last, /Step restore is already done; done is final/);
    return "Done.";
  });
  const options = { model: provider.model };
  let connect = await boot(options);
  const a = await connect();
  const threadId = (await a.rpc("thread.new", {})).id, peer = (await a.rpc("thread.new", {})).id;
  await ask(a, threadId, createRequest, "create");
  const get = after => a.rpc("ext.plan.get", { cwd: config.cwd, planId, after });
  const activity = (after, owner = threadId) => a.rpc("ext.plan.activity", { threadId: owner, after });
  const selection = { threadId, planId, revision: 1, stepIds: ["review", "save"], requestId: "run-1" };
  const run = (overrides = {}) => a.send("ext.plan.run", { ...selection, ...overrides });
  assert.equal((await activity()).run, null);
  denied(await run({ revision: 2 }), "invalid_request", /Plan changed/);
  denied(await run({ stepIds: ["nope"] }), "invalid_request", /unknown step/);

  const { submissionId } = await a.rpc("submit", { threadId, text: "Stay in this thread", requestId: "busy", whenBusy: "reject" });
  await busy.promise;
  const [receipt, retry] = (await Promise.all([run(), run()])).map(okay);
  assert.deepEqual(receipt, retry, "the same request ID admits one run");
  denied(await run({ stepIds: ["save"] }), "invalid_request", /different selection/);
  const queued = await get();
  assert.deepEqual(queued.plan.runs, [{ id: receipt.runId, threadId, stepIds: ["save", "review"] }]);
  denied(await run({ threadId: peer, stepIds: ["save"], requestId: "overlap" }), "busy", /already in an active run/);
  denied(await a.send("ext.plan.close", { cwd: config.cwd, planId, closed: true }), "busy");
  assert.equal((await a.rpc("ext.plan.list", { cwd: config.cwd, page: 0 })).plans[0].running, true);
  const live = await activity();
  assert.equal(live.run.id, receipt.runId);
  assert.equal(live.run.state, "running");
  assert.deepEqual(live.run.steps.map(s => [s.id, s.status]), [["save", "pending"], ["review", "pending"]]);
  gate.resolve();
  assert.equal((await a.rpc("submission.wait", { threadId, submissionId })).status, "done");
  await reported.promise;
  // A report during the turn wakes the thread's activity watch; it doesn't wait for the turn to end.
  const timeout = new Promise((_, reject) => setTimeout(() => reject(new Error("the activity watch did not wake on a report")), 3_000).unref());
  const midRun = await Promise.race([activity(live.revision), timeout]);
  assert.equal(midRun.run.state, "running");
  assert.deepEqual(midRun.run.steps.map(s => [s.id, s.status]), [["save", "done"], ["review", "blocked"]]);
  finish.resolve();

  const settled = await until(get, v => v.plan.runs.length === 0);
  assert.equal(runs, 1);
  assert.deepEqual(statuses(settled.plan), { save: "done", restore: "pending", review: "blocked: Restore drops the theme." });
  assert.equal(await readFile(join(config.cwd, "prefs.txt"), "utf8"), "theme=mocha\n");
  const finished = await until(activity, v => v.run?.state === "finished");
  assert.equal(finished.run.id, receipt.runId);
  assert.deepEqual(finished.run.outcomes, [{ stepId: "save", status: "done", detail: "Wrote prefs.txt." },
    { stepId: "review", status: "blocked", detail: "Restore drops the theme." }]);
  const entries = await transcript(a, threadId);
  assert.equal(entries.filter(e => e.kind === "hush.plan.chat").at(-1).messages[0].blocks[0].text, "Plan run · r1 · Interface preferences · 2 steps");
  assert.match(entries.find(e => e.kind === "hush.plan.result").messages[0].blocks[0].text, /^Plan run finished · 1 done · 1 blocked\n/);

  denied(await run({ stepIds: ["save"], requestId: "again" }), "invalid_request", /done step/);
  const second = okay(await run({ stepIds: ["restore", "review"], requestId: "run-2" }));
  const complete = await until(get, v => v.plan.runs.length === 0 && v.plan.complete);
  assert.deepEqual(statuses(complete.plan), { save: "done", restore: "done", review: "done" });
  const log = await a.rpc("ext.plan.history", { cwd: config.cwd, planId });
  assert.deepEqual(log.runs.map(r => [r.id, r.state, r.stepIds, r.threadId]), [[second.runId, "finished", ["restore", "review"], threadId],
    [receipt.runId, "finished", ["save", "review"], threadId]]);
  assert.equal(log.next, null);
  assert.deepEqual((await a.rpc("ext.plan.list", { cwd: config.cwd, page: 0 })).plans[0], { id: planId, title: "Interface preferences",
    revision: 1, workspace: config.cwd, total: 3, done: 3, blocked: 0, running: false, closed: false });
  const fork = (await a.rpc("thread.fork", { threadId, atEntryId: entries.find(e => e.kind === "hush.plan.chat").id })).id;
  assert.equal((await activity(undefined, fork)).run, null, "a fork copies the transcript, not the thread's runs");

  await stop();
  connect = await boot(options);
  const b = await connect();
  assert.deepEqual(await b.rpc("ext.plan.history", { cwd: config.cwd, planId }), log);
  assert.equal((await b.rpc("ext.plan.activity", { threadId })).run.id, second.runId);
  assert.deepEqual(await b.rpc("ext.plan.run", selection), receipt, "the receipt survives a restart");
});

test("stored plan:1 plans convert on first read", { timeout: 15_000 }, async t => {
  const { boot, stop, config } = await layout(t);
  const connect = await boot({ faux: true });
  const threadId = (await (await connect()).rpc("thread.new", {})).id;
  await stop();
  const planId = "4d7f37c4-1d1d-4f53-9f4a-2c1f7f6b0e3a", cursor = "a".repeat(64);
  const family = (kind, scope = "session") => defineDocFamily({ kind, version: 1, family: true, scope, initial: () => ({}), checkpointWhen: () => true });
  const v1 = {
    head: family("hush.plan-head"), revision: family("hush.plan-revision"), log: family("hush.plan-run-log"), catalog: family("hush.plan-workspace"),
    ready: defineDoc({ kind: "hush.plan-workspace-indexed", version: 1, scope: "session", initial: () => ({}), checkpointWhen: () => true }),
    activity: defineDoc({ kind: "hush.plan-chat-activity", version: 1, scope: "conversation", history: "latest", fork: "initial",
      initial: () => ({}), checkpointWhen: () => true }),
  };
  const store = await openNodeSqliteStorage(join(config.dataDir, "history.sqlite"));
  const harness = await Harness.open(store, { registry: createRegistry(), models: createModels() }, context);
  try {
    await harness.commit(async tx => {
      Object.assign(await tx.doc(v1.head, planId, null), { cursor, revision: 1, workspace: config.cwd, members: [Number(threadId)], fixture: false,
        progress: [{ stepId: "save", state: "verified", effectiveThinking: "high", workerThreadId: "5" }],
        run: { id: "99", stepIds: ["extra"], state: "running", chat: true, ownerThreadId: threadId, submissionId: "7" },
        reports: [{ stepId: "restore", state: "done", detail: "Restored." }, { stepId: "docs", state: "blocked", detail: "Path undecided." },
          { stepId: "extra", state: "incomplete", detail: "Not started." }] });
      Object.assign(await tx.doc(v1.revision, `${planId}:1`, null), { sources: { inputs: [], partial: true }, plan: { id: planId, revision: 1,
        title: "Workspace profiles", steps: [
          { id: "save", title: "Save a profile", description: "Store a profile.", checks: ["Saved profile has a name"], thinking: "high",
            edit: { path: "profile.txt", contains: "name=" } },
          { id: "restore", title: "Restore profiles", description: "Read the profile.", checks: [], requires: ["save"] },
          { id: "docs", title: "Document discovery", description: "Describe the path.", checks: ["Path is documented"], blocker: "Path undecided" },
          { id: "extra", title: "Extra", description: "More.", checks: ["More"], after: "restore",
            scope: { write: ["a.txt"], acceptance: { label: "A", script: "return true" } } },
        ] } });
      Object.assign(await tx.doc(v1.log, planId, null), { runs: [
        { id: "50", revision: 1, threadId, stepIds: ["save"], state: "verified", workers: [], turns: [{ stepId: "save", entryId: "3", state: "verified" }] },
        { id: "60", revision: 1, threadId, stepIds: ["restore", "docs"], state: "done", chat: true, workers: [], entryId: "9",
          reports: [{ stepId: "restore", state: "done", detail: "Restored." }, { stepId: "docs", state: "blocked", detail: "Path undecided." }] },
        { id: "99", revision: 1, threadId, stepIds: ["extra"], state: "running", chat: true, workers: [] },
      ] });
      Object.assign(await tx.doc(v1.catalog, config.cwd, null), { cursor, rows: [{ id: planId, title: "Workspace profiles", revision: 1, total: 4,
        verified: 1, reportedDone: 1, status: "running", closed: false, activeRun: { id: "99", ownerThreadId: threadId, chat: true } }] });
      Object.assign(await tx.doc(v1.ready), { ready: true, runOrder: true });
      Object.assign(await tx.doc(v1.activity, Number(threadId)), { cursor, pending: [{ planId, revision: 1, runId: "99" }],
        last: { planId, revision: 1, run: { id: "60", stepIds: ["restore", "docs"], state: "done", chat: true }, progress: [], reports: [] } });
    }, context);
  } finally { await harness.close(context); }

  const a = await (await boot({ faux: true }))();
  const { plan } = await a.rpc("ext.plan.get", { cwd: config.cwd, planId });
  assert.deepEqual(plan.steps, [
    { id: "save", title: "Save a profile", prompt: "Store a profile.\nChecks:\n- Saved profile has a name", status: "done" },
    { id: "restore", title: "Restore profiles", prompt: "Read the profile.", requires: ["save"], status: "done" },
    { id: "docs", title: "Document discovery", prompt: "Describe the path.\nChecks:\n- Path is documented\nKnown blocker: Path undecided",
      status: "blocked", reason: "Path undecided." },
    { id: "extra", title: "Extra", prompt: "More.\nChecks:\n- More", status: "pending" },
  ]);
  assert.deepEqual(plan.runs, [], "a run from before the upgrade is not carried over");
  assert.deepEqual((await a.rpc("ext.plan.list", { cwd: config.cwd, page: 0 })).plans, [{ id: planId, title: "Workspace profiles", revision: 1,
    workspace: config.cwd, total: 4, done: 2, blocked: 1, running: false, closed: false }]);
  const { runs } = await a.rpc("ext.plan.history", { cwd: config.cwd, planId });
  assert.deepEqual(runs, [
    { id: "99", revision: 1, threadId, stepIds: ["extra"], state: "failed", outcomes: [], failure: "Interrupted by a Hush upgrade." },
    { id: "60", revision: 1, threadId, stepIds: ["restore", "docs"], state: "finished", entryId: "9", outcomes: [
      { stepId: "restore", status: "done", detail: "Restored." }, { stepId: "docs", status: "blocked", detail: "Path undecided." }] },
    { id: "50", revision: 1, threadId, stepIds: ["save"], state: "finished", outcomes: [{ stepId: "save", status: "done", detail: "Verified." }] },
  ]);
  const { run } = await a.rpc("ext.plan.activity", { threadId });
  assert.equal(run.id, "60");
  assert.deepEqual(run.steps, [{ id: "restore", title: "Restore profiles", status: "done" },
    { id: "docs", title: "Document discovery", status: "blocked", reason: "Path undecided." }]);
  // The converted plan runs like any other.
  const { runId } = await a.rpc("ext.plan.run", { threadId, planId, revision: 1, stepIds: ["extra"], requestId: "after-upgrade" });
  assert.ok(runId);
});

test("plan inventory lists open plans first with the caller's workspace leading", () => {
  const row = (id, closed = false) => ({ id, title: id, revision: 1, total: 1, done: 0, blocked: 0, running: false, closed });
  const page = planInventoryPage([["/here", [row("old"), row("shut", true), row("new")]], ["/there", [row("far"), row("gone", true)]]], 0);
  assert.deepEqual(page.plans.map(p => [p.id, p.workspace]), [["new", "/here"], ["old", "/here"], ["far", "/there"], ["shut", "/here"], ["gone", "/there"]]);
  assert.equal(page.more, false);
});

test("plan capability is unavailable on invocation-private hosts", { timeout: 12_000 }, async t => {
  const { boot, config } = await layout(t);
  const privateDir = join(config.agentDir, "private");
  await mkdir(privateDir, { mode: 0o700 });
  const connect = await boot({ ephemeral: true, dataDir: undefined, socket: join(privateDir, "hush.sock"), faux: true });
  const invocation = await connect([2], null);
  denied(await invocation.send("ext.plan.run", {}), "unsupported_capability");
});
