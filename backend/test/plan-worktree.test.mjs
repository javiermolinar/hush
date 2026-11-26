import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, readdir, realpath, rename, rm, symlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { promisify } from "node:util";
import { startBackend } from "../../dist/hush-backend.mjs";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { dial, request } from "../../server/test/client.mjs";
import { call, createRequest, draftArgs, planProvider } from "./plan-provider.mjs";

const exec = promisify(execFile), prepare = "ext.planWorktree.prepare", start = "ext.planWorktree.start";
const ok = f => { assert.equal(f.ok, true, JSON.stringify(f)); return f.result; };
const caps = { plan: [1], planWorkspace: [1], planChat: [1], planChatActivity: [1], planCatalog: [1], planWorktree: [1], threadDirectory: [2] };

async function setup(t, answer, { name = "Preference round trip", inside = false, alternate = false } = {}) {
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
  const git = async (...args) => (await exec("git", ["-C", project, ...args])).stdout.trim();
  await git("init", "-q");
  await writeFile(join(cwd, "prefs.txt"), "committed prefs\n");
  if (inside) await writeFile(join(project, "prefs.txt"), "root prefs, not selected\n");
  await git("add", ".");
  await git("-c", "user.name=Hush fixture", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "seed");
  const head = await git("rev-parse", "HEAD");
  await writeFile(join(cwd, "prefs.txt"), "unsaved prefs in source\n");
  await writeFile(join(cwd, "local-only.txt"), "keep in the source\n");
  const dirty = await git("status", "--porcelain");
  await trust.change(cwd, "grant", t.signal);
  const provider = await planProvider(t, agentDir, async turn => {
    if (turn.text === createRequest) return turn.results.length ? "Plan saved." : [call("hush_plan_propose", draftArgs())];
    if (turn.text.startsWith("Plan worktree name\n")) {
      assert.deepEqual(turn.tools, []);
      assert.deepEqual(JSON.parse(turn.text.split("\n")[1]), { title: "Interface preferences", tasks: ["Restore preferences"] });
      return name;
    }
    return answer(turn);
  }, { alternate });
  const boot = async () => { app = await startBackend({ model: provider.model, cwd, agentDir, dataDir, socket, idleMs: 30_000, ephemeral: false, faux: false }); };
  await boot();
  const attach = async (capabilities = caps) => {
    const p = await dial(socket);
    peers.push(p);
    const hello = ok(await p.send("hello", { versions: [1], capabilities }));
    return { p, hello, rpc: async (method, params) => ok(await p.send(method, params)) };
  };
  const { p, rpc } = await attach();
  const { id: threadId } = await rpc("thread.new", {});
  const { submissionId } = await rpc("submit", { threadId, text: createRequest, requestId: "draft", whenBusy: "reject" });
  assert.equal((await rpc("submission.wait", { threadId, submissionId })).status, "done");
  const { plan } = await rpc("ext.plan.wait", { threadId });
  const pick = { planId: plan.id, revision: plan.revision, stepIds: ["restore"], requestId: "allocate" };
  return { cwd, root, dataDir, agentDir, socket, git, head, dirty, trust, provider, threadId, pick, close, boot, attach, p, rpc };
}

const refused = (f, code) => { assert.equal(f.ok, false, JSON.stringify(f)); assert.equal(f.error.code, code, JSON.stringify(f)); };

test("a named checkout runs the selected tasks only after explicit trust and records them on the existing plan", { timeout: 20_000 }, async t => {
  const reports = [{ stepId: "restore", state: "done", detail: "Wrote preferences in the new checkout." }];
  const h = await setup(t, async ({ text, tools, results, messages }) => {
    assert.match(text, /^Plan implementation\n/);
    assert.deepEqual(JSON.parse(text.split("\n")[1]).selected, ["restore"]);
    assert.ok(messages.every(m => m.content !== createRequest), "the source history wasn't imported");
    assert.ok(tools.includes("write"));
    if (!results.length) return [call("write", { path: "prefs.txt", content: "implemented in worktree\n" })];
    if (results.length === 1) return [call("hush_plan_report", { tasks: reports })];
    return "Restored preferences in the worktree.";
  });
  const { p, rpc, cwd, pick, threadId, git, trust } = h;
  const checkouts = () => git("worktree", "list", "--porcelain");
  const list = () => rpc("thread.list", { limit: 32 });
  const before = await list();
  const { p: old, hello } = await h.attach({ plan: [1], planWorkspace: [1], planWorktree: [1] });
  assert.equal(hello.capabilities.planWorktree, undefined);
  refused(await old.send(prepare, { ...pick, threadId }), "unsupported_capability");
  refused(await p.send(prepare, { ...pick, threadId, cwd }), "invalid_request");
  refused(await p.send(prepare, { ...pick, threadId, revision: 2 }), "invalid_request");
  refused(await p.send(prepare, { ...pick, threadId, stepIds: ["no-such-task"] }), "invalid_request");
  await trust.change(cwd, "revoke", t.signal);
  const requests = h.provider.requests.length;
  refused(await p.send(prepare, { ...pick, threadId }), "invalid_request");
  assert.equal(h.provider.requests.length, requests, "untrusted or stale selection cannot even ask for a name");
  assert.equal((await checkouts()).match(/^worktree /gm).length, 1);
  assert.deepEqual(await list(), before);
  await trust.change(cwd, "grant", t.signal);

  const allocation = await rpc(prepare, { ...pick, cwd });
  assert.deepEqual(await rpc(prepare, { ...pick, cwd }), allocation);
  refused(await p.send(prepare, { ...pick, threadId }), "invalid_request");
  const { checkout, title } = allocation;
  assert.equal(checkout.cwd, checkout.root);
  assert.match(checkout.branch, /^hush\/preference-round-trip-[0-9a-f-]{36}$/);
  assert.equal(title, "Preference round trip");
  assert.equal(await git("-C", checkout.cwd, "branch", "--show-current"), checkout.branch);
  assert.equal(await git("-C", checkout.cwd, "rev-parse", "HEAD"), h.head);
  assert.equal(await readFile(join(checkout.cwd, "prefs.txt"), "utf8"), "committed prefs\n");
  assert.deepEqual((await readdir(checkout.cwd)).sort(), [".git", "prefs.txt"]);
  assert.equal(await git("status", "--porcelain"), h.dirty);
  assert.deepEqual(await rpc("ext.planWorkspace.history", { threadId, planId: pick.planId }), { runs: [], next: null });
  assert.equal((await trust.review(checkout.cwd)).trusted, false);
  const params = { ...pick, threadId: allocation.threadId, checkout: checkout.cwd };
  refused(await p.send("ext.planChat.run", { ...pick, threadId: allocation.threadId }), "not_found");
  refused(await p.send(start, { ...params, checkout: cwd }), "not_found");
  refused(await p.send(start, { ...params, stepIds: ["save"] }), "not_found");
  refused(await p.send(start, params), "invalid_request");
  assert.equal(h.provider.requests.length, requests + 1, "prepare asked only for a name, even on retry without trust");

  await h.close();
  await h.boot();
  const b = await h.attach(), lost = await h.attach();
  assert.deepEqual(await b.rpc(prepare, { ...pick, cwd }), allocation, "prepared allocation is durable");
  await trust.change(checkout.cwd, "grant", t.signal);
  lost.p.socket.pause(); // Dispatch on a second peer without delivering its reply to the caller.
  lost.p.socket.write(JSON.stringify(request("lost-start", start, params)) + "\n");
  const one = await b.rpc(start, params);
  const q = { threadId, planId: pick.planId };
  let settled = await b.rpc("ext.planWorkspace.inspect", q);
  while (settled.run.state === "running") settled = await b.rpc("ext.planWorkspace.inspect", { ...q, after: settled.revision });
  assert.equal(settled.run.state, "done");
  assert.equal(settled.run.ownerThreadId, allocation.threadId);
  assert.deepEqual(settled.reports, reports);
  assert.deepEqual(settled.progress, []);
  const activity = await b.rpc("ext.planChatActivity.wait", { threadId: allocation.threadId });
  assert.deepEqual(activity.run, settled.run);
  assert.equal(activity.plan.id, pick.planId);
  assert.equal((await b.rpc("ext.plan.wait", { threadId: allocation.threadId })).plan, null);
  assert.ok(lost.p.frames.every(f => f.id !== "lost-start"));
  lost.p.close();
  await h.close();
  await h.boot();
  const resumed = await h.attach();
  assert.deepEqual(await resumed.rpc(start, params), one, "retry after an unobserved reply and restart must not run twice");
  const log = await resumed.rpc("ext.planWorkspace.history", q);
  assert.deepEqual(log.runs.map(r => [r.id, r.threadId, r.stepIds, r.worktree]), [[one.runId, allocation.threadId, ["restore"], true]]);
  const { planWorktree: _worktree, ...legacyCaps } = caps;
  const legacy = await h.attach(legacyCaps);
  assert.equal((await legacy.rpc("ext.planWorkspace.history", q)).runs[0].worktree, undefined);
  const { entries } = await resumed.rpc("history", { threadId: allocation.threadId, limit: 50, range: { kind: "before", beforeEntryId: null } });
  assert.equal(entries.filter(e => e.kind === "hush.plan.chat").length, 1);
  assert.ok(entries.every(e => e.threadId === allocation.threadId));
  assert.equal(await readFile(join(checkout.cwd, "prefs.txt"), "utf8"), "implemented in worktree\n");
  assert.equal(await readFile(join(cwd, "prefs.txt"), "utf8"), "unsaved prefs in source\n");
  assert.equal(await git("status", "--porcelain"), h.dirty);
  assert.equal(await git("rev-parse", "HEAD"), h.head);
});

test("worktree prep falls back from an unsafe name and never starts a replaced or stale checkout", { timeout: 20_000 }, async t => {
  const h = await setup(t, () => assert.fail("an implementation should not start"), { name: "../../escape\nother name" });
  const { rpc, p, pick, cwd, threadId, trust, git } = h;
  const allocation = await rpc(prepare, { ...pick, threadId });
  assert.equal(allocation.title, "Interface preferences");
  assert.match(allocation.checkout.branch, /^hush\/interface-preferences-[0-9a-f-]{36}$/);
  const checkout = allocation.checkout.cwd, saved = checkout + "-saved";
  const params = { ...pick, threadId: allocation.threadId, checkout };
  await rename(checkout, saved);
  await symlink(cwd, checkout);
  const denied = await p.send(start, params);
  refused(denied, "invalid_request");
  assert.match(denied.error.message, /worktree changed/);
  await rm(checkout);
  await rename(saved, checkout);
  await trust.change(checkout, "grant", t.signal);
  await git("-C", checkout, "checkout", "--detach", "HEAD");
  assert.match((await p.send(start, params)).error?.message ?? "", /worktree changed/);
  await git("-C", checkout, "checkout", allocation.checkout.branch);
  await rename(join(checkout, ".git"), join(checkout, ".saved-git"));
  assert.match((await p.send(start, params)).error?.message ?? "", /worktree changed/);
  await rename(join(checkout, ".saved-git"), join(checkout, ".git"));
  const plan = { threadId, planId: pick.planId };
  await rpc("ext.planWorkspace.setClosed", { ...plan, closed: true });
  await rpc("ext.planWorkspace.setClosed", { ...plan, closed: false });
  refused(await p.send(start, params), "invalid_request");
  assert.equal((await rpc("ext.planWorkspace.inspect", plan)).run, null);
  assert.ok(h.provider.requests.every(r => !r.text.startsWith("Plan implementation\n")));
});

test("threadless worktree keeps the staged model and the committed subdirectory", { timeout: 20_000 }, async t => {
  const h = await setup(t, async ({ model, text, results }) => {
    assert.equal(model, "staged-chat");
    assert.match(text, /^Plan implementation\n/);
    if (results.length) { assert.notEqual(results[0].isError, true); return "Updated the component."; }
    return [call("write", { path: "prefs.txt", content: "component implementation\n" })];
  }, { inside: true, alternate: true, name: "設定の調整" });
  const { p, rpc, cwd, pick, threadId, trust, provider } = h;
  const args = { ...pick, cwd, model: provider.staged, thinkingLevel: "high" };
  refused(await p.send(prepare, { ...args, thinkingLevel: undefined }), "invalid_request");
  refused(await p.send(prepare, { ...args, cwd: undefined, threadId }), "invalid_request");
  const allocation = await rpc(prepare, args), ck = allocation.checkout;
  assert.equal(provider.requests.filter(r => r.text.startsWith("Plan worktree name\n")).at(0)?.model, "staged-chat");
  assert.equal(ck.cwd, join(ck.root, "component"));
  assert.match(ck.branch, /^hush\/interface-preferences-[0-9a-f-]{36}$/, "a name without an ASCII slug falls back to the plan title");
  const { state } = await rpc("subscribe", { threadId: allocation.threadId });
  assert.equal(state.agent.cwd, ck.cwd);
  assert.deepEqual(state.agent.model, provider.staged);
  assert.equal(state.agent.thinkingLevel, "high");
  refused(await p.send(prepare, { ...args, thinkingLevel: "off" }), "invalid_request");
  await trust.change(ck.root, "grant", t.signal);
  const params = { ...pick, threadId: allocation.threadId, checkout: ck.cwd };
  refused(await p.send(start, params), "invalid_request");
  await trust.change(ck.cwd, "grant", t.signal);
  await rpc(start, params);
  const q = { threadId, planId: pick.planId };
  let settled = await rpc("ext.planWorkspace.inspect", q);
  while (settled.run.state === "running") settled = await rpc("ext.planWorkspace.inspect", { ...q, after: settled.revision });
  assert.equal(settled.run.state, "done");
  assert.equal(await readFile(join(ck.root, "prefs.txt"), "utf8"), "root prefs, not selected\n");
  assert.equal(await readFile(join(ck.cwd, "prefs.txt"), "utf8"), "component implementation\n");
  assert.equal(await readFile(join(cwd, "prefs.txt"), "utf8"), "unsaved prefs in source\n");
  assert.equal(await h.git("status", "--porcelain"), h.dirty);
});

test("an unavailable staged model falls back to the plan title, not a different provider", { timeout: 20_000 }, async t => {
  const h = await setup(t, () => assert.fail("must not substitute the default model"));
  const { p, rpc, cwd, pick, trust, provider } = h;
  const source = { ...pick, cwd, model: { ...provider.model, modelId: "not-installed" }, thinkingLevel: "off" };
  const before = provider.requests.length;
  const allocation = await rpc(prepare, source);
  assert.equal(allocation.title, "Interface preferences");
  assert.match(allocation.checkout.branch, /^hush\/interface-preferences-/);
  await trust.change(allocation.checkout.cwd, "grant", t.signal);
  const { error } = await p.send(start, { ...pick, threadId: allocation.threadId, checkout: allocation.checkout.cwd });
  assert.match(error?.message ?? "", /not installed/);
  assert.equal(provider.requests.length, before);
});

test("a lost prepare reply recovers one checkout and a failed Git allocation remains discoverable", { timeout: 20_000 }, async t => {
  const h = await setup(t, () => assert.fail("prepare must not start a run"));
  const { p, cwd, rpc, pick, root, git } = h;
  const tool = join(root, "shim"), blocked = join(root, "checkout-written"), released = join(root, "release"), fail = join(root, "fail-after-git");
  const realGit = (await exec("sh", ["-c", "command -v git"])).stdout.trim();
  const quote = s => `'${s.replaceAll("'", "'\\''")}'`;
  await mkdir(tool);
  await writeFile(join(tool, "git"), `#!/bin/sh
case " $* " in
  *" worktree add "*)
    ${quote(realGit)} "$@" || exit $?
    test ! -f ${quote(fail)} || exit 1
    : > ${quote(blocked)}
    n=0
    until test -f ${quote(released)}; do
      n=$((n+1)); test "$n" -lt 150 || exit 1
      sleep 0.025
    done
    ;;
  *) exec ${quote(realGit)} "$@" ;;
esac
`, { mode: 0o700 });
  const previous = process.env.PATH;
  process.env.PATH = `${tool}:${previous}`;
  t.after(() => { process.env.PATH = previous; });
  const waiting = (await h.attach()).p;
  const args = { ...pick, cwd };
  const requestLost = assert.rejects(waiting.send(prepare, args), /socket closed/);
  const checkouts = () => git("worktree", "list", "--porcelain");
  try {
    const bound = AbortSignal.any([t.signal, AbortSignal.timeout(3000)]);
    while (!(await readFile(blocked).then(() => true, () => false))) await delay(20, undefined, { signal: bound });
    assert.equal((await checkouts()).match(/^worktree /gm).length, 2);
    refused(await p.send(prepare, args), "busy");
    waiting.close();
    await requestLost;
  } finally { await writeFile(released, ""); }
  let recovered;
  for (let i = 0; i < 40; i++) {
    const reply = await p.send(prepare, args);
    if (reply.ok) { recovered = reply.result; break; }
    refused(reply, "busy");
    await delay(25, undefined, { signal: t.signal });
  }
  assert.ok(recovered, "the committed checkout is returned after disconnect instead of allocating a duplicate");
  assert.equal((await checkouts()).match(/^worktree /gm).length, 2);
  assert.equal(h.provider.requests.filter(r => r.text.startsWith("Plan worktree name\n")).length, 1);

  await writeFile(fail, "");
  const partial = { ...args, requestId: "failed-allocation" };
  const failed = await p.send(prepare, partial);
  refused(failed, "invalid_request");
  const id = failed.error.message.match(/inspect thread #(\d+) in \/threads/)?.[1];
  assert.ok(id, "even when Git fails after writing files, the reservation refers to a specific thread");
  const { threads: [{ id: row, checkout }] } = await rpc("ext.threadDirectory.get", { threadIds: [id] });
  assert.equal(row, id);
  assert.equal(checkout.kind, "managed");
  assert.equal(await readFile(join(checkout.cwd, "prefs.txt"), "utf8"), "committed prefs\n");
  await h.close();
  await h.boot();
  const b = await h.attach();
  assert.deepEqual(await b.rpc(prepare, args), recovered);
  assert.deepEqual((await b.p.send(prepare, partial)).error, failed.error);
  assert.equal((await checkouts()).match(/^worktree /gm).length, 3, "recovery does not allocate another branch or delete anything");
  await h.trust.change(checkout.cwd, "grant", t.signal);
  const stopped = await b.p.send(start, { ...pick, requestId: partial.requestId, threadId: id, checkout: checkout.cwd });
  refused(stopped, "invalid_request");
  assert.deepEqual(stopped.error, failed.error);
});
