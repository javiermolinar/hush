import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, readdir, realpath, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { promisify } from "node:util";
import { startBackend } from "../../dist/hush-backend.mjs";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { dial } from "../../server/test/client.mjs";
import { call, createRequest, draftArgs, planProvider, savedPlanId } from "./plan-provider.mjs";

const exec = promisify(execFile);
const ok = f => { assert.equal(f.ok, true, JSON.stringify(f)); return f.result; };
const refused = (f, code, message) => {
  assert.equal(f.ok, false, JSON.stringify(f));
  assert.equal(f.error.code, code, JSON.stringify(f));
  if (message) assert.match(f.error.message, message);
};

async function setup(t, answer, { inside = false, name } = {}) {
  const root = await realpath(await mkdtemp("/tmp/hush-plan-wt-"));
  const agentDir = join(root, "agent"), dataDir = join(root, "data"), project = join(root, "project"), socket = join(dataDir, "hush.sock");
  const cwd = inside ? join(project, "component") : project;
  const trust = new WorkspaceTrust(agentDir), peers = [];
  let app;
  const close = async () => {
    for (const p of peers.splice(0)) p.close();
    await app?.close();
    app = undefined;
  };
  t.after(async () => { await close(); await rm(root, { recursive: true, force: true }); });
  await Promise.all([agentDir, cwd].map(path => mkdir(path, { recursive: true, mode: 0o700 })));
  const git = async (...args) => (await exec("git", ["-C", project, ...args], { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } })).stdout.trim();
  await git("init", "-q");
  await writeFile(join(cwd, "prefs.txt"), "committed prefs\n");
  await git("add", ".");
  await git("-c", "user.name=Hush fixture", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "seed");
  const head = await git("rev-parse", "HEAD");
  await writeFile(join(cwd, "prefs.txt"), "unsaved prefs in source\n");
  const dirty = await git("status", "--porcelain");
  await trust.change(cwd, "grant", t.signal);
  let planId;
  const provider = await planProvider(t, agentDir, async turn => {
    if (turn.text === createRequest) {
      if (!turn.results.length) return [call("plan_save", draftArgs())];
      planId = savedPlanId(turn.results[0].content);
      return "Plan saved.";
    }
    if (turn.text.startsWith("Plan worktree name\n")) {
      assert.deepEqual(turn.tools, []);
      if (name) return name(turn);
      assert.deepEqual(JSON.parse(turn.text.split("\n")[1]), { title: "Interface preferences", tasks: ["Restore preferences"] });
      return "Preference round trip";
    }
    return answer(turn);
  });
  const boot = async () => { app = await startBackend({ model: provider.model, cwd, agentDir, dataDir, socket, idleMs: 30_000, ephemeral: false, faux: false }); };
  await boot();
  const attach = async () => {
    const p = await dial(socket);
    peers.push(p);
    ok(await p.send("hello", { versions: [1], capabilities: { plan: [2], threadDirectory: [2] } }));
    return { p, rpc: async (method, params) => ok(await p.send(method, params)) };
  };
  const { p, rpc } = await attach();
  const { id: threadId } = await rpc("thread.new", {});
  const { submissionId } = await rpc("submit", { threadId, text: createRequest, requestId: "draft", whenBusy: "reject" });
  assert.equal((await rpc("submission.wait", { threadId, submissionId })).status, "done");
  const pick = { planId, revision: 1, stepIds: ["restore"], requestId: "worktree-run" };
  return { cwd, project, git, head, dirty, trust, provider, threadId, pick, close, boot, attach, p, rpc };
}

async function settled(rpc, cwd, planId) {
  let v = await rpc("ext.plan.get", { cwd, planId });
  for (let i = 0; v.plan.runs.length && i < 16; i++) v = await rpc("ext.plan.get", { cwd, planId, after: v.revision });
  assert.deepEqual(v.plan.runs, []);
  return v.plan;
}

test("a worktree run creates one checkout that inherits the grant and runs only the selected steps", { timeout: 25_000 }, async t => {
  let turns = 0;
  const h = await setup(t, async ({ text, tools, results, messages }) => {
    assert.match(text, /^Plan run\n/);
    assert.deepEqual(JSON.parse(text.split("\n")[1]).selected, ["restore"]);
    assert.match(text, /Work in this checkout only/);
    assert.ok(messages.every(m => m.content !== createRequest), "the source history isn't imported");
    assert.ok(tools.includes("write") && tools.includes("plan_report"));
    if (!results.length) { turns++; return [call("write", { path: "prefs.txt", content: "implemented in worktree\n" })]; }
    if (results.length === 1) return [call("plan_report", { steps: [{ stepId: "restore", status: "done", detail: "Wrote preferences." }] })];
    return "Restored preferences in the worktree.";
  });
  const { p, rpc, cwd, pick, threadId, git, trust } = h;
  const checkouts = async () => (await git("worktree", "list", "--porcelain")).match(/^worktree /gm).length;
  const send = params => p.send("ext.plan.runInWorktree", params);
  const threads = async () => (await rpc("thread.list", { limit: 32 })).threads.length;
  const before = await threads();
  refused(await send({ ...pick, threadId, revision: 2 }), "invalid_request", /Plan changed/);
  refused(await send({ ...pick, threadId, stepIds: ["no-such-step"] }), "invalid_request", /unknown step/);
  refused(await send({ ...pick, threadId, cwd }), "invalid_request");
  await trust.change(cwd, "revoke", t.signal);
  const requests = h.provider.requests.length;
  refused(await send({ ...pick, threadId }), "invalid_request");
  assert.equal(h.provider.requests.length, requests, "an untrusted source cannot even ask for a name");
  assert.equal(await checkouts(), 1);
  assert.equal(await threads(), before);
  await trust.change(cwd, "grant", t.signal);

  const result = ok(await send({ ...pick, threadId }));
  const { checkout, title } = result;
  assert.equal(title, "Preference round trip");
  assert.equal(checkout.cwd, checkout.root);
  assert.match(checkout.branch, /^hush\/preference-round-trip-[0-9a-f-]{36}$/);
  assert.equal(await git("-C", checkout.cwd, "rev-parse", "HEAD"), h.head);
  assert.equal((await trust.review(checkout.cwd)).trusted, true, "a checkout Hush creates inherits the source's grant");
  refused(await send({ ...pick, threadId, stepIds: ["save"] }), "invalid_request", /different selection/);
  const plan = await settled(rpc, cwd, pick.planId);
  assert.equal(turns, 1);
  assert.deepEqual(plan.steps.map(s => [s.id, s.status]), [["save", "pending"], ["restore", "done"], ["review", "pending"]]);
  assert.equal(await readFile(join(checkout.cwd, "prefs.txt"), "utf8"), "implemented in worktree\n");
  assert.deepEqual((await readdir(checkout.cwd)).sort(), [".git", "prefs.txt"]);
  assert.equal(await readFile(join(cwd, "prefs.txt"), "utf8"), "unsaved prefs in source\n");
  assert.equal(await git("status", "--porcelain"), h.dirty);
  const { runs } = await rpc("ext.plan.history", { cwd, planId: pick.planId });
  assert.deepEqual(runs.map(r => [r.id, r.threadId, r.stepIds, r.state, r.worktree]), [[result.runId, result.threadId, ["restore"], "finished", true]]);
  const { entries } = await rpc("history", { threadId: result.threadId, limit: 50, range: { kind: "before", beforeEntryId: null } });
  assert.equal(entries.filter(e => e.kind === "hush.plan.chat").length, 1);

  await h.close();
  await h.boot();
  const resumed = await h.attach();
  assert.deepEqual(await resumed.rpc("ext.plan.runInWorktree", { ...pick, threadId }), result, "a retry after restart returns the same run");
  assert.equal(await checkouts(), 2);
  assert.equal(turns, 1);
});

test("a worktree run from a launch directory keeps its committed subdirectory", { timeout: 25_000 }, async t => {
  const h = await setup(t, async ({ results }) => results.length ? "Done." :
    [call("plan_report", { steps: [{ stepId: "restore", status: "blocked", detail: "Needs a saved profile first." }] })], { inside: true });
  const { rpc, cwd, pick } = h;
  const result = await rpc("ext.plan.runInWorktree", { ...pick, cwd });
  assert.equal(result.checkout.cwd, join(result.checkout.root, "component"));
  const { threads } = await rpc("thread.list", { limit: 32 });
  assert.ok(threads.some(row => row.id === result.threadId));
  const plan = await settled(rpc, cwd, pick.planId);
  assert.deepEqual(plan.steps.find(s => s.id === "restore"), { ...draftArgs().steps[1], status: "blocked", reason: "Needs a saved profile first." });
});

test("a worktree retry while the first call is still working shares it, whatever the step order", { timeout: 25_000 }, async t => {
  const naming = Promise.withResolvers(), release = Promise.withResolvers();
  t.after(() => release.resolve());
  const h = await setup(t, async ({ text, results }) => {
    assert.deepEqual(JSON.parse(text.split("\n")[1]).selected, ["save", "restore"]);
    return results.length ? "Done." : [call("plan_report", { steps: [{ stepId: "save", status: "done", detail: "Saved." },
      { stepId: "restore", status: "done", detail: "Restored." }] })];
  }, { name: async () => { naming.resolve(); await release.promise; return "Preference round trip"; } });
  const { rpc, cwd, threadId, git } = h;
  const checkouts = async () => (await git("worktree", "list", "--porcelain")).match(/^worktree /gm).length;
  const threads = async () => (await rpc("thread.list", { limit: 32 })).threads.length;
  const before = await threads();
  const params = { ...h.pick, stepIds: ["restore", "save"], threadId };
  const first = h.p.send("ext.plan.runInWorktree", params);
  await naming.promise; // the first call is creating the checkout
  const [b, c] = await Promise.all([h.attach(), h.attach()]);
  const retry = b.p.send("ext.plan.runInWorktree", { ...params, stepIds: ["save", "restore"] });
  refused(await c.p.send("ext.plan.runInWorktree", { ...params, stepIds: ["save"] }), "invalid_request", /different selection/);
  release.resolve();
  const [one, two] = (await Promise.all([first, retry])).map(ok);
  assert.deepEqual(two, one, "the retry got the first call's run");
  assert.equal(await checkouts(), 2, "one checkout");
  assert.equal(await threads(), before + 1, "one thread");
  const plan = await settled(rpc, cwd, h.pick.planId);
  assert.deepEqual(plan.steps.map(s => [s.id, s.status]), [["save", "done"], ["restore", "done"], ["review", "pending"]]);
  assert.deepEqual(await b.rpc("ext.plan.runInWorktree", { ...params, stepIds: ["save", "restore"] }), one, "a later reordered retry gets the same run");
});
