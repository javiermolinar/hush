import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readdir, realpath, rm } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { join } from "node:path";
import { test } from "node:test";
import { promisify } from "node:util";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { startBackend } from "../../dist/hush-backend.mjs";
import { dial } from "../../server/test/client.mjs";
import { call, createRequest, draftArgs, planProvider, savedPlanId } from "../../backend/test/plan-provider.mjs";

const exec = promisify(execFile);
const repo = fileURLToPath(new URL("../../", import.meta.url));
const script = fileURLToPath(new URL("./plan-pty.py", import.meta.url));
const okay = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const git = (cwd, ...args) => exec("git", args, { cwd, env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } });

// A Hush host with a scripted provider, a peer and the built CLI.
async function fixture(t, answer, { repository = false } = {}) {
  const home = await realpath(await mkdtemp("/tmp/hush-plan-pty-"));
  const agentDir = join(home, ".hush"), dataDir = join(home, "data"), cwd = join(home, "work"), socket = join(dataDir, "hush.sock");
  let host, peer;
  t.after(async () => { peer?.close(); await host?.close(); await rm(home, { recursive: true, force: true }); });
  await Promise.all([mkdir(agentDir, { mode: 0o700 }), mkdir(cwd, { mode: 0o700 })]);
  if (repository) {
    await git(cwd, "init", "-q");
    await git(cwd, "-c", "user.name=Hush Fixture", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "fixture");
  }
  const trust = new WorkspaceTrust(agentDir);
  await trust.change(cwd, "grant", AbortSignal.timeout(5_000));
  let planId;
  const provider = await planProvider(t, agentDir, async turn => {
    if (turn.text === createRequest) {
      assert.match(turn.prompt, /don't ask for special wording/);
      if (!turn.results.length) return [call("plan_save", draftArgs())];
      planId = savedPlanId(turn.results[0].content);
      return "Saved the plan.";
    }
    return answer(turn);
  });
  const binary = process.env.HUSH_TEST_CLI || join(home, "hush");
  if (!process.env.HUSH_TEST_CLI) await exec("go", ["build", "-o", binary, "./app/cmd/hush"], { cwd: repo, timeout: 25_000 });
  host = await startBackend({ model: provider.model, faux: false, ephemeral: false, idleMs: 60_000, cwd, agentDir, dataDir, socket });
  peer = await dial(socket);
  okay(await peer.send("hello", { versions: [1], capabilities: { plan: [2], threadDirectory: [2] } }));
  const rpc = async (method, params) => okay(await peer.send(method, params));
  const ask = async (threadId, text) => {
    const { submissionId } = await rpc("submit", { threadId, text, requestId: text, whenBusy: "reject" });
    assert.equal((await rpc("submission.wait", { threadId, submissionId })).status, "done");
  };
  const pty = (mode, threadId, { cwd: dir = cwd, timeout = 16_000 } = {}) => exec("python3", [script, binary, socket, threadId ?? "", mode, createRequest],
    { cwd: dir, timeout, env: { HOME: home, PATH: process.env.PATH, TERM: "xterm-256color", LANG: "C.UTF-8" } });
  return { home, cwd, trust, provider, rpc, ask, pty, planId: () => planId };
}

const settled = async (rpc, cwd, planId) => {
  let v = await rpc("ext.plan.get", { cwd, planId });
  for (let i = 0; v.plan.runs.length && i < 16; i++) v = await rpc("ext.plan.get", { cwd, planId, after: v.revision });
  return v.plan;
};

test("terminal UI saves a plan from chat and opens its steps", { timeout: 45_000 }, async t => {
  const f = await fixture(t, () => assert.fail("only the plan request reaches the model"));
  const { id: threadId } = await f.rpc("thread.new", {});
  await f.pty("save", threadId);
  const plan = await settled(f.rpc, f.cwd, f.planId());
  assert.deepEqual(plan.steps.map(s => [s.id, s.status]), [["save", "pending"], ["restore", "pending"], ["review", "pending"]]);
  assert.deepEqual(await readdir(f.cwd), [], "saving a plan writes no files");
});

test("terminal UI browses plans at startup without creating a thread or sending the draft", { timeout: 45_000 }, async t => {
  const f = await fixture(t, () => assert.fail("browsing does not call the model"));
  const { id: threadId } = await f.rpc("thread.new", {});
  await f.ask(threadId, createRequest);
  const before = await f.rpc("thread.list", { limit: 32 }), requests = f.provider.requests.length;
  await f.pty("startup");
  assert.deepEqual(await f.rpc("thread.list", { limit: 32 }), before);
  assert.equal(f.provider.requests.length, requests);
});

test("terminal UI runs a selected step here without touching the composer", { timeout: 45_000 }, async t => {
  const f = await fixture(t, async turn => {
    assert.match(turn.text, /^Plan run\n/);
    assert.deepEqual(JSON.parse(turn.text.split("\n")[1]).selected, ["restore"]);
    assert.ok(turn.tools.includes("plan_report") && turn.tools.includes("write"));
    assert.ok(!turn.messages.some(m => m.content === "keep this draft"), "the draft was not sent");
    return turn.results.length ? "Restored the preferences." :
      [call("plan_report", { steps: [{ stepId: "restore", status: "done", detail: "Checked the settings round trip." }] })];
  });
  const { id: source } = await f.rpc("thread.new", {});
  await f.ask(source, createRequest);
  const { id: threadId } = await f.rpc("thread.new", {});
  const threads = await f.rpc("thread.list", { limit: 32 });
  await f.pty("chat", threadId);
  assert.deepEqual(await f.rpc("thread.list", { limit: 32 }), threads, "running here does not create a thread");
  const plan = await settled(f.rpc, f.cwd, f.planId());
  assert.deepEqual(plan.steps.map(s => [s.id, s.status]), [["save", "pending"], ["restore", "done"], ["review", "pending"]]);
  const { runs } = await f.rpc("ext.plan.history", { cwd: f.cwd, planId: f.planId() });
  assert.deepEqual(runs.map(r => [r.threadId, r.state, r.stepIds]), [[threadId, "finished", ["restore"]]]);
  const { entries } = await f.rpc("history", { threadId, limit: 32, range: { kind: "before", beforeEntryId: null } });
  assert.deepEqual(entries.filter(e => e.kind === "hush.plan.chat").map(e => e.messages[0].blocks[0].text),
    ["Plan run · r1 · Interface preferences · 1 step"]);
  assert.ok(!entries.some(e => e.kind === "pi.user"), "the run is not an ordinary chat submission");
});

test("terminal UI runs one step here and another in a new worktree that inherits the grant", { timeout: 45_000 }, async t => {
  let naming = 0;
  const f = await fixture(t, async turn => {
    if (turn.text.startsWith("Plan worktree name\n")) return ++naming && "Preference round trip";
    assert.match(turn.text, /^Plan run\n/);
    const [step] = JSON.parse(turn.text.split("\n")[1]).selected;
    assert.equal(turn.messages.some(m => m.content === createRequest), step === "save", "only Run here continues the source thread");
    return turn.results.length ? "Done." : [call("plan_report", { steps: [{ stepId: step, status: "done", detail: "Checked it." }] })];
  }, { repository: true });
  const { id: threadId } = await f.rpc("thread.new", {});
  await f.ask(threadId, createRequest);
  await f.pty("worktree", threadId, { timeout: 25_000 });
  const { threads } = await f.rpc("ext.threadDirectory.list", { limit: 10 });
  const work = threads.find(row => row.title === "Preference round trip");
  assert.equal(work?.checkout?.kind, "managed");
  assert.equal((await f.trust.review(work.checkout.cwd)).trusted, true, "the checkout inherited the source's grant");
  await settled(f.rpc, f.cwd, f.planId());
  const { runs } = await f.rpc("ext.plan.history", { cwd: f.cwd, planId: f.planId() });
  assert.deepEqual(runs.map(r => [r.threadId, r.state, r.stepIds, r.worktree]),
    [[work.id, "finished", ["restore"], true], [threadId, "finished", ["save"], undefined]]);
});

test("terminal UI opens a plan of another checkout at startup without running it", { timeout: 45_000 }, async t => {
  const f = await fixture(t, () => assert.fail("browsing does not call the model"), { repository: true });
  const other = join(f.home, "other-worktree");
  await git(f.cwd, "worktree", "add", "-q", "--detach", other);
  await f.trust.change(other, "grant", t.signal);
  const { id: threadId } = await f.rpc("thread.new", {});
  await f.ask(threadId, createRequest);
  const before = await f.rpc("thread.list", { limit: 32 }), requests = f.provider.requests.length;
  await f.pty("catalog", undefined, { cwd: other });
  assert.deepEqual(await f.rpc("thread.list", { limit: 32 }), before, "browsing another checkout's plan creates no thread");
  assert.equal(f.provider.requests.length, requests);
  assert.deepEqual((await f.rpc("ext.plan.get", { cwd: f.cwd, planId: f.planId() })).plan.runs, []);
});
