import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, realpath, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { DatabaseSync } from "node:sqlite";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { BACKGROUND_CONTEXT as ctx } from "@earendil-works/chord/context";
import { applyImmutable } from "@earendil-works/chord/delta";
import { fauxAssistantMessage, fauxProvider } from "@earendil-works/pi-ai/providers/faux";
import { defineDoc } from "@earendil-works/pi-durable";
import { dial } from "../../server/test/client.mjs";
import { assertFrame } from "../../protocol/v1/replay.mjs";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { startBackend } from "../../dist/hush-backend.mjs";
import { call, planProvider } from "./plan-provider.mjs";

const ok = value => { assert.equal(value.ok, true, JSON.stringify(value)); return value.result; };
const deny = (reply, code) => { assert.equal(reply.ok, false, JSON.stringify(reply)); assert.equal(reply.error.code, code, JSON.stringify(reply)); };
const notices = "hush.worker.notice", spawnName = "hush_spawn_reviews", joinName = "hush_join_reviews";
const ledger = defineDoc({ kind: "hush.delegated-reviews", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({ cohorts: [] }), checkpointWhen: () => true });
const releases = defineDoc({ kind: "hush.review-releases", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({ byHandle: {} }), checkpointWhen: () => true });
const policy = defineDoc({ kind: "hush.assigned-tool-policy", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true });
const scope = { tracked: ["a.txt"], untracked: ["test.txt"] };
const query = (threadId, requestId, options = {}) => ({ threadId, requestId, scope,
  assignments: [{ name: "First", brief: "Review the API" }], ...options });
const history = async (wire, threadId, range = { kind: "before", beforeEntryId: null }, limit = 50) =>
  ok(await wire.send("history", { threadId, range, limit }));
const ask = async (wire, threadId, text, requestId, whenBusy = "reject") => {
  const { submissionId } = ok(await wire.send("submit", { threadId, text, requestId, whenBusy }));
  return ok(await wire.send("submission.wait", { threadId, submissionId }));
};
const start = (wire, q) => wire.send("ext.delegation.start", q);
const info = async (wire, { threadId, requestId }) => ok(await wire.send("ext.delegation.get", { threadId, requestId }));
const stop = (wire, threadId, handle) => wire.send("ext.delegation.stop", { threadId, handle });
const state = async (wire, threadId, window) => ok(await wire.send("subscribe", { threadId, window })).state;
const isBrief = text => typeof text === "string" && text.startsWith("Code review assignment\n");
const brief = text => JSON.parse(text.slice("Code review assignment\n".length));
const has = (frame, text) => frame.type === "patch" && JSON.stringify(frame.ops).includes(text);
const waitFor = (wire, text, after = 0) => wire.wait(frame => wire.frames.indexOf(frame) >= after && has(frame, text));

async function layout(t) {
  const dir = await realpath(await mkdtemp("/tmp/hush-delegation-")), home = join(dir, "home"), agentDir = join(home, ".hush"),
    repo = join(dir, "repo"), cwd = join(repo, "src"), dataDir = join(dir, "db");
  await mkdir(cwd, { recursive: true });
  await mkdir(agentDir, { recursive: true, mode: 0o700 });
  const git = (...args) => execFileSync("git", args, { cwd: repo, env: { PATH: process.env.PATH, HOME: home, GIT_CONFIG_NOSYSTEM: "1" } });
  const save = (path, text) => writeFile(join(cwd, path), text);
  git("init", "-q"); git("config", "user.name", "Fixture"); git("config", "user.email", "fixture@example.test");
  await save("a.txt", "BASE\n"); git("add", "."); git("commit", "-qm", "base");
  await save("a.txt", "STAGED\n"); git("add", "."); await save("a.txt", "WORKING\n");
  await save("test.txt", "UNTRACKED\n");
  await save("unrelated.txt", "UNADMITTED DATA\n");
  await new WorkspaceTrust(agentDir).change(cwd, "grant", AbortSignal.timeout(4_000));
  const config = { cwd, agentDir, dataDir, socket: join(dataDir, "hush.sock"), idleMs: 60_000, ephemeral: false, faux: false };
  const peers = [], release = [];
  let backend;
  t.after(async () => {
    for (const fn of release) fn();
    for (const wire of peers) wire.close();
    await backend?.close();
    await rm(dir, { recursive: true, force: true });
  });
  const peer = async (v = 1, extras = {}) => {
    const wire = await dial(config.socket, frame => { if (!/^ext\.(delegation|workerActivity)\./.test(frame.method ?? "")) assertFrame(frame); });
    peers.push(wire);
    const hello = ok(await wire.send("hello", { versions: [1], capabilities: { ...(v && { delegation: [v] }), ...extras } }));
    assert.equal(hello.capabilities.delegation, v === 1 ? 1 : undefined);
    return wire;
  };
  const boot = async model => { backend = await startBackend({ ...config, model }); return backend; };
  const hold = () => { const gate = Promise.withResolvers(); release.push(gate.resolve); return gate; };
  return { config, boot, peer, hold, home, save, release };
}

async function reviewProvider(t, env, answer, options) {
  return planProvider(t, env.config.agentDir, async turn => {
    if (!isBrief(turn.text)) return answer(turn);
    assert.deepEqual(turn.tools.toSorted(), ["hush_review_read", "hush_review_report"]);
    assert.equal(turn.messages.filter(m => m.role === "user").length, 1);
    assert.doesNotMatch(JSON.stringify(turn.messages), /parent-secret|previous-parent-answer|UNADMITTED DATA|NEW_WORKING|Current working directory/);
    return answer(turn, brief(turn.text));
  }, options);
}

for (const mode of [undefined, "sequential"]) test(`explicit native review reserves once and runs ${mode ?? "default sequential"}`, { timeout: 12_000 }, async t => {
  const env = await layout(t), begun = env.hold(), advance = env.hold();
  const called = [];
  let secondMayStart = false;
  const provider = await reviewProvider(t, env, async (turn, input) => {
    if (!input) { assert.ok(!turn.tools.includes(spawnName)); return "previous-parent-answer"; }
    if (!turn.results.length) {
      called.push(input.name);
      if (input.name === "First") { begun.resolve(); await advance.promise; }
      else assert.ok(secondMayStart, "a sequential sibling started before settlement");
      return [call("hush_review_read", { path: input.name === "First" ? "a.txt" : "test.txt" })];
    }
    if (turn.results.length === 1) {
      const file = JSON.parse(turn.results[0].content);
      if (input.name === "First") assert.deepEqual([file.base.text, file.staged.text, file.working.text], ["BASE\n", "STAGED\n", "WORKING\n"]);
      else assert.equal(file.working.text, "UNTRACKED\n");
      return [call("hush_review_report", { report: `${input.name.toUpperCase()}_RESULT` })];
    }
    if (input.name === "First") secondMayStart = true;
    return "Finished.";
  });
  const backend = await env.boot(provider.model);
  const [a, b, old] = await Promise.all([env.peer(), env.peer(), env.peer(false)]);
  const { id } = ok(await a.send("thread.new", {}));
  const before = await ask(a, id, "parent-secret", "seed");
  assert.equal(before.status, "done");
  await state(a, id, { min: 1, max: 1 }); await state(old, id, { min: 1, max: 1 });
  const opts = { assignments: [{ name: "First", brief: "Review only a" }, { name: "Second", brief: "Review the explicit test" }], ...(mode && { mode }) };
  const q = query(id, "request", opts);
  deny(await start(old, q), "unsupported_capability");
  deny(await start(a, { ...q, prompt: "Create a plan for another task" }), "invalid_request");
  deny(await b.send("ext.delegation.get", { threadId: id, requestId: "request" }), "not_found");
  const [one, two] = (await Promise.all([start(a, q), start(b, q)])).map(ok);
  assert.deepEqual(one, two); assert.equal(one.mode, "sequential"); assert.equal(one.submissionId, undefined);
  await begun.promise;
  const tasks = () => backend.harness.commit(tx => tx.scanTasks({ conversationId: Number(id), kind: "hush.review-worker" }, 10), ctx);
  assert.equal((await tasks()).items.length, 2);
  assert.deepEqual(called, ["First"]);
  deny(await start(b, { ...q, assignments: [{ name: "Different", brief: "Changed" }] }), "invalid_request");
  deny(await start(b, query(id, "third")), "limit");
  await env.save("a.txt", "NEW_WORKING\n");
  advance.resolve();
  await waitFor(a, "SECOND_RESULT");
  const result = await info(b, q);
  assert.equal(result.target, "stale");
  assert.deepEqual(result.workers.map(w => [w.state, w.report]), [["succeeded", "FIRST_RESULT"], ["succeeded", "SECOND_RESULT"]]);
  assert.deepEqual(called, ["First", "Second"]);
  assert.equal(provider.requests.filter(r => !isBrief(r.text)).length, 1, "idle parent did not get a follow-up request");
  const retained = (await history(b, id)).entries.filter(row => row.kind === notices);
  assert.deepEqual(retained.map(e => e.id), result.workers.map(w => w.noticeId));
  const oldWindow = await state(old, id, { min: 1, max: 1 });
  assert.equal(oldWindow.window.lastEntryId, before.answerEntryId);
  assert.ok(oldWindow.window.beforeEntryId);
  const oldPage = await history(old, id);
  assert.ok(!oldPage.entries.some(row => row.kind === notices));
  assert.equal((await history(old, id, { kind: "before", beforeEntryId: oldWindow.window.beforeEntryId })).nextCursor, null);
  deny(await old.send("history", { threadId: id, range: { kind: "before", beforeEntryId: retained[1].id }, limit: 1 }), "invalid_cursor");
  const latest = (await history(b, id, { kind: "before", beforeEntryId: null }, 1)).nextCursor;
  assert.ok(latest);
  deny(await old.send("history", { threadId: id, range: { kind: "cursor", cursor: latest }, limit: 1 }), "invalid_cursor");
  assert.deepEqual(ok(await start(b, q)), one, "same receipt after source edit and settlement");
  assert.equal(ok(await stop(b, id, one.workers[0].handle)).worker.state, "succeeded", "Stop cannot rewrite committed results");
  const firstTask = (await tasks()).items[0].id;
  const workers = await backend.harness.commit(tx => tx.scanConversations({ ownerTaskId: firstTask }, 2), ctx);
  assert.equal(workers.items.length, 1);
  const child = String(workers.items[0].id);
  deny(await a.send("history", { threadId: child, range: { kind: "before", beforeEntryId: null }, limit: 1 }), "not_found");
  assert.ok(!ok(await a.send("thread.list", { limit: 100 })).threads.some(row => row.id === child));
});

test("a parent-scoped roster and dossier preserve distinct, bounded code review evidence", { timeout: 12_000 }, async t => {
  const env = await layout(t), began = env.hold(), readNow = env.hold(), readDone = env.hold(), finish = env.hold();
  const assessment = { checks: [{ id: "source", description: "Inspect the value" }, { id: "tests", description: "Review the test result" },
    { id: "network", description: "Verify live behavior" }], evidence: [{ id: "local", label: "Caller test excerpt", text: "3 passed (supplied)" }] };
  const structured = { summary: "Pinned working version changes the value.", findings: [
    { priority: "P3", path: "a.txt", version: "base", line: 1, message: "Check the base label." },
    { priority: "P1", path: "a.txt", version: "working", line: 1, message: "Working label changes the default." }],
    checks: [{ id: "source", basis: "inspected", source: "a.txt", note: "Compared pinned versions." },
      { id: "tests", basis: "supplied", source: "local", note: "Read the supplied test excerpt." }],
    limitations: ["Live behavior was not checked."] };
  const provider = await reviewProvider(t, env, async (turn, input) => {
    assert.deepEqual(input.assessment, assessment);
    switch (turn.results.length) {
      case 0:
        began.resolve();
        await readNow.promise;
        return [call("hush_review_report", { report: "Do not accept unstructured results for this contract" })];
      case 1:
        assert.match(turn.results.at(-1).content, /Use a bounded structured report/);
        return [call("hush_review_report", structured)]; // no source inspection yet
      case 2:
        assert.match(turn.results.at(-1).content, /Cite lines in versions you read/);
        return [call("hush_review_read", { path: "a.txt" })];
      case 3:
        assert.equal(JSON.parse(turn.results.at(-1).content).working.text, "WORKING\n");
        readDone.resolve();
        await finish.promise;
        return [call("hush_review_report", { ...structured,
          checks: [{ id: "tests", basis: "supplied", source: "not-admitted", note: "Not a valid citation." }] })];
      case 4:
        assert.match(turn.results.at(-1).content, /only admitted evidence IDs/);
        return [call("hush_review_report", { ...structured, findings: [{ ...structured.findings[1], line: 2 }] })];
      case 5:
        assert.match(turn.results.at(-1).content, /Cite lines in versions you read/);
        return [call("hush_review_report", structured)];
      case 6:
        assert.match(turn.results.at(-1).content, /Advisory report recorded/);
        return [call("hush_review_report", structured)];
      default:
        assert.match(turn.results.at(-1).content, /Advisory report recorded/);
        return "Reviewed the pinned source.";
    }
  });
  await env.boot(provider.model);
  const caps = { reviewEvidence: [1], workerActivity: [1] };
  const [a, b, old, unnegotiated] = await Promise.all([env.peer(1, caps), env.peer(1, caps), env.peer(), env.peer(false, caps)]);
  const { id } = ok(await a.send("thread.new", {}));
  const other = ok(await a.send("thread.new", {})).id;
  const wait = (peer, after) => peer.send("ext.workerActivity.wait", { threadId: id, ...(after && { after }) });
  const inspect = (peer, handle, transcript = false, threadId = id) => peer.send("ext.workerActivity.inspect", { threadId, handle, transcript });
  const initial = ok(await wait(a));
  assert.deepEqual(initial.workers, []);
  deny(await wait(old), "unsupported_capability");
  deny(await wait(unnegotiated), "unsupported_capability");
  const q = query(id, "evidence", { assessment });
  deny(await start(old, q), "unsupported_capability");
  deny(await start(a, { ...q, assessment: { checks: [assessment.checks[0], assessment.checks[0]] } }), "invalid_request");
  const watching = wait(a, initial.cursor);
  const receipt = ok(await start(b, q)), [{ handle }] = receipt.workers;
  const first = ok(await watching);
  assert.equal(first.workers[0].handle, handle);
  await began.promise;
  const active = ok(await wait(b));
  assert.deepEqual(active.workers, [{ handle, name: "First", state: "running", activity: "Reviewing pinned snapshot" }]);
  assert.equal(ok(await inspect(b, handle)).transcript, undefined);
  deny(await inspect(old, handle), "unsupported_capability");
  deny(await inspect(b, handle, true, other), "not_found");
  deny(await start(a, { ...q, assessment: { ...assessment, evidence: [{ ...assessment.evidence[0], text: "Changed" }] } }), "invalid_request");
  const changed = wait(b, active.cursor);
  readNow.resolve();
  await readDone.promise;
  assert.match(ok(await changed).workers[0].activity, /Read pinned versions of a.txt/);
  const publicRead = ok(await inspect(a, handle, true));
  assert.match(JSON.stringify(publicRead.transcript), /Read pinned versions of a.txt/);
  assert.doesNotMatch(JSON.stringify(publicRead.transcript), /Caller test excerpt|WORKING|Code review assignment/);
  await env.save("a.txt", "NEW_WORKING\n");
  await state(a, id);
  const before = a.frames.length;
  finish.resolve();
  await waitFor(a, "Working label changes the default.", before);
  const retained = ok(await inspect(b, handle, true));
  assert.equal(retained.target, "stale");
  assert.equal(retained.worker.state, "succeeded");
  assert.deepEqual(retained.assessment, assessment);
  assert.deepEqual(retained.receipt, receipt);
  assert.match(retained.worker.report, /Pinned selection: plan-test\/chat · thinking off/);
  assert.match(retained.worker.report, /Report call: plan-test\/chat .*thinking off/);
  assert.match(retained.worker.report, /P1 · "a.txt":1 \(working\).*P3 · "a.txt":1 \(base\)/s);
  assert.match(retained.worker.report, /Performed: pinned source inspection.*Supplied evidence \[local\] \(not rerun\).*Unverified \(not assessed\)/s);
  assert.match(retained.worker.report, /No tests executed by this reviewer/);
  assert.ok(retained.transcript.entries.some(e => e.text === "Reviewed the pinned source."));
  assert.deepEqual(await info(b, q).then(v => v.workers), [retained.worker]);
  assert.deepEqual(ok(await start(b, q)), receipt);
  assert.equal((await history(b, id)).entries.filter(e => e.kind === notices).length, 1);
  const resumed = await env.peer(1, caps);
  const fresh = ok(await wait(resumed, initial.cursor));
  assert.deepEqual(fresh.workers.map(w => [w.handle, w.state]), [[handle, "succeeded"]]);
  assert.deepEqual(ok(await wait(resumed)), fresh);
  assert.equal(provider.requests.length, 8, "inspection, reconnect and exact retry did not start work");
});

const parallel = { mode: "parallel", assignments: [{ name: "Fast", brief: "Read the staged change" }, { name: "Slow", brief: "Read context" }] };
const refillPair = { mode: "parallel", assignments: [
  { name: "RefillOne", brief: "Review the first new assignment" }, { name: "RefillTwo", brief: "Review the second new assignment" },
] };
const isRefill = input => refillPair.assignments.some(assignment => assignment.brief === input.brief);
const refillReport = ({ name }) => `CAPACITY_${name}_REPORT`;
const refillReply = (turn, input) => turn.results.length ? "Finished." : [call("hush_review_report", { report: refillReport(input) })];

async function assertRefillSucceeded(wire, q, receipt) {
  const reports = q.assignments.map(refillReport);
  await Promise.all(reports.map(report => waitFor(wire, report)));
  assert.deepEqual((await info(wire, q)).workers.map(w => [w.handle, w.state, w.report]),
    receipt.workers.map(({ handle }, i) => [handle, "succeeded", reports[i]]));
}

test("a same-turn join parks, retains a successful sibling, and late and repeated joins use the same outcomes", { timeout: 12_000 }, async t => {
  const env = await layout(t), fast = env.hold(), slow = env.hold(), allowFast = env.hold(), allowSlow = env.hold();
  const prompt = "Review the selected change and synthesize.";
  const ongoing = new Set();
  let foreign, joined;
  const provider = await reviewProvider(t, env, async (turn, input) => {
    if (input) {
      assert.equal(turn.model, "staged-chat");
      if (input.name === "First") return turn.results.length ? "Finished." : [call("hush_review_report", { report: "FOREIGN_COMMITTED_REPORT" })];
      if (!turn.results.length) {
        ongoing.add(input.name);
        (input.name === "Fast" ? fast : slow).resolve();
        await (input.name === "Fast" ? allowFast : allowSlow).promise;
        ongoing.delete(input.name);
        return [call("hush_review_report", { report: "FAST_COMMITTED_REPORT" })];
      }
      return "Finished.";
    }
    if (!turn.tools.includes(spawnName)) return "Ordinary response; no delegation.";
    assert.equal(turn.text, prompt);
    assert.ok(turn.tools.includes(joinName));
    assert.equal(turn.model, "chat");
    assert.equal(turn.messages.filter(m => m.role === "user").length, 1, "notices never become input");
    const last = turn.results.at(-1)?.content;
    switch (turn.results.length) {
      case 0: return [call(joinName, { handles: [foreign] })];
      case 1:
        assert.match(last, /Only launched review handles/);
        return [call(spawnName)];
      case 2: {
        const { workers } = JSON.parse(last);
        return [call(joinName, { handles: workers.map(w => w.handle), timeoutMs: 100 })];
      }
      case 3: {
        const wait = JSON.parse(last);
        assert.equal(wait.wait, "timed_out");
        assert.ok(wait.outcomes.every(r => r.state !== "timed_out"));
        return [call(joinName, { handles: wait.outcomes.map(w => w.handle) })];
      }
      case 4: {
        joined = JSON.parse(last);
        assert.deepEqual(joined.outcomes.map(x => x.state), ["succeeded", "timed_out"]);
        assert.equal(joined.outcomes[0].report, "FAST_COMMITTED_REPORT");
        return [call(joinName, { handles: joined.outcomes.map(w => w.handle) })];
      }
      default:
        assert.deepEqual(JSON.parse(last), joined);
        return "Synthesized from the committed review.";
    }
  }, { alternate: true });
  const backend = await env.boot(provider.model);
  let [a, b] = await Promise.all([env.peer(), env.peer()]);
  const { id } = ok(await a.send("thread.new", {}));
  const other = ok(await a.send("thread.new", {})).id;
  // The other parent owns this real handle, but this turn cannot join it.
  const otherPick = { model: provider.staged, thinking: "high" };
  foreign = ok(await start(b, query(other, "foreign", { selection: otherPick }))).workers[0].handle;
  deny(await start(a, query(id, "auth", { selection: { model: { provider: "missing", modelId: "other" }, thinking: "off" } })), "invalid_request");
  deny(await b.send("ext.delegation.get", { threadId: id, requestId: "auth" }), "not_found");
  const initial = await state(a, id);
  const q = query(id, "parallel", { ...parallel, prompt, selection: { model: provider.staged, thinking: "high" }, workerTimeoutMs: 1200 });
  const receipt = ok(await start(a, q));
  assert.deepEqual(receipt.selection, q.selection);
  deny(await stop(b, other, receipt.workers[0].handle), "not_found");
  assert.deepEqual(await info(a, q).then(v => v.receipt), receipt);
  await Promise.all([fast.promise, slow.promise]);
  assert.deepEqual([...ongoing].sort(), ["Fast", "Slow"]);
  let view = initial;
  await a.wait(frame => {
    if (frame.type === "patch") view = applyImmutable(view, frame.ops);
    return view.tools.some(slot => slot.name === joinName && slot.status === "sleeping");
  });
  a.close();
  a = await env.peer();
  assert.deepEqual(ok(await start(a, q)), receipt);
  const live = await state(a, id);
  assert.equal(live.run.submissionIds[0], receipt.submissionId);
  const { items } = await backend.harness.commit(tx => tx.scanTasks({ conversationId: Number(id), kind: "hush.review-worker" }, 10), ctx);
  assert.equal(items.length, 2);
  await b.send("thread.configure", { threadId: id, change: { thinkingLevel: null } }).then(ok);
  const follow = ok(await b.send("submit", { threadId: id, text: "Ordinary queued request", requestId: "plain-after", whenBusy: "followUp" }));
  allowFast.resolve();
  const settled = ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId }));
  assert.equal(settled.status, "done");
  assert.equal(joined.wait, "settled");
  const { entries } = await history(a, id);
  assert.equal(entries.filter(e => e.kind === notices).length, 2);
  assert.equal(ok(await b.send("submission.wait", { threadId: id, submissionId: follow.submissionId })).status, "done");
  assert.ok((await history(a, id)).entries.filter(e => e.kind === "pi.user").length === 2);
  assert.equal(entries.filter(e => e.messages.some(m => m.blocks.some(b => b.text === "Synthesized from the committed review."))).length, 1);
  const fork = ok(await a.send("thread.fork", { threadId: id, atEntryId: settled.answerEntryId })).id;
  assert.ok(!(await history(a, fork)).entries.some(e => e.kind === notices));
  deny(await b.send("ext.delegation.get", { threadId: fork, requestId: q.requestId }), "not_found");
  assert.equal((await ask(a, fork, "No review was requested for this turn", "plain")).status, "done");
  assert.equal(provider.requests.filter(r => r.text === prompt && r.tools.includes(spawnName)).length, 6);
  allowSlow.resolve();
});

test("join reports unresolved evidence and still waits for a correlatable sibling", { timeout: 12_000 }, async t => {
  const env = await layout(t), joinNow = env.hold(), slowStarted = env.hold(), finishSlow = env.hold();
  let handles, single, both;
  const provider = await reviewProvider(t, env, async (turn, input) => {
    if (input) {
      if (isRefill(input)) return refillReply(turn, input);
      if (input.name === "Slow" && !turn.results.length) { slowStarted.resolve(); await finishSlow.promise; }
      return turn.results.length ? "Finished." : [call("hush_review_report", { report: `${input.name.toUpperCase()}_REPORT` })];
    }
    const last = turn.results.at(-1)?.content;
    switch (turn.results.length) {
      case 0: return [call(spawnName)];
      case 1:
        handles = JSON.parse(last).workers.map(w => w.handle);
        await joinNow.promise;
        return [call(joinName, { handles: [handles[0]] })];
      case 2:
        single = JSON.parse(last);
        return [call(joinName, { handles })];
      default:
        both = JSON.parse(last);
        return "Finished the parent turn.";
    }
  });
  const backend = await env.boot(provider.model), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  let view = await state(a, id);
  const q = query(id, "degraded", { ...parallel, prompt: "Explicitly launch both reviews and join them." });
  const receipt = ok(await start(a, q));
  await Promise.all([slowStarted.promise, waitFor(a, "FAST_REPORT")]);
  assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["succeeded", "running"]);
  await backend.harness.commit(async tx => {
    // The task and original notice remain, but the receipt can no longer correlate them.
    delete (await tx.doc(ledger, Number(id))).cohorts[0].rows[0].notice;
  }, ctx);
  joinNow.resolve();
  await a.wait(frame => {
    if (frame.type === "patch") view = applyImmutable(view, frame.ops);
    return view.tools.some(slot => slot.name === joinName && slot.status === "sleeping");
  });
  assert.equal(single.wait, "unresolved");
  assert.deepEqual(single.outcomes.map(w => [w.handle, w.state, w.noticeId, w.report]), [[handles[0], "unresolved", undefined, undefined]]);
  assert.match(single.outcomes[0].detail, /its task has ended; the slot is free/i);
  finishSlow.resolve();
  assert.equal(ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId })).status, "done");
  assert.equal(both.wait, "unresolved");
  assert.deepEqual(both.outcomes.map(w => [w.handle, w.state, w.report]), [
    [handles[0], "unresolved", undefined], [handles[1], "succeeded", "SLOW_REPORT"],
  ]);
  assert.ok(both.outcomes.every(w => w.snapshotId === receipt.snapshotId));
  assert.equal((await history(a, id)).entries.filter(e => e.kind === notices).length, 2);
  const refill = query(id, "refill-after-join", refillPair), refilled = ok(await start(a, refill));
  await assertRefillSucceeded(a, refill, refilled);
  assert.deepEqual((await info(a, q)).workers.map(w => ({ ...w, snapshotId: receipt.snapshotId })), both.outcomes);
  assertBriefStarts(provider, { q, receipt }, { q: refill, receipt: refilled });
});

test("an unreleased reservation's terminal notice does not authorize a join", { timeout: 10_000 }, async t => {
  const env = await layout(t), parentReady = env.hold(), joinNow = env.hold();
  let handle, rejected, released, joined;
  const provider = await reviewProvider(t, env, async (turn, input) => {
    assert.equal(input, undefined, "the stopped reservation did not start a worker");
    switch (turn.results.length) {
      case 0:
        assert.ok(turn.tools.includes(joinName));
        parentReady.resolve();
        await joinNow.promise;
        return [call(joinName, { handles: [handle] })];
      case 1:
        rejected = turn.results[0].content;
        return [call(spawnName)];
      case 2:
        released = JSON.parse(turn.results[1].content);
        return [call(joinName, { handles: [handle] })];
      default:
        joined = JSON.parse(turn.results[2].content);
        return "Finished the parent turn.";
    }
  });
  await env.boot(provider.model);
  const a = await env.peer(1, { workerActivity: [1] });
  const { id } = ok(await a.send("thread.new", {}));
  await state(a, id);
  const q = query(id, "held", { prompt: "Check a reserved review without releasing it." });
  const receipt = ok(await start(a, q));
  handle = receipt.workers[0].handle;
  await parentReady.promise;
  assert.equal((await info(a, q)).workers[0].state, "reserved");
  // Stop publishes a genuine terminal notice without releasing or opening the worker.
  await stop(a, id, handle).then(ok);
  await waitFor(a, "The worker was stopped.");
  const worker = ok(await a.send("ext.workerActivity.inspect", { threadId: id, handle })).worker;
  assert.equal(worker.state, "cancelled");
  assert.ok(worker.noticeId);
  joinNow.resolve();
  assert.equal(ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId })).status, "done");
  assert.match(rejected, /Only launched review handles/);
  assert.deepEqual(released, { snapshotId: receipt.snapshotId, workers: receipt.workers });
  assert.deepEqual(joined, { wait: "settled", outcomes: [{ ...worker, snapshotId: receipt.snapshotId }] });
  assert.deepEqual((await info(a, q)).workers, [worker]);
  assert.deepEqual((await history(a, id)).entries.filter(e => e.kind === notices).map(e => e.id), [worker.noticeId]);
  assert.equal(provider.requests.length, 4);
});

test("spawn rejects a contradictory held checkpoint without releasing its sibling", { timeout: 12_000 }, async t => {
  const env = await layout(t), spawnNow = env.hold(), parked = Promise.withResolvers();
  let spawnResult;
  const provider = await reviewProvider(t, env, async (turn, input) => {
    if (input) return "No advisory report.";
    if (turn.results.length) {
      spawnResult = turn.results[0].content;
      return "Finished the parent turn.";
    }
    assert.ok(turn.tools.includes(spawnName));
    await spawnNow.promise;
    return [call(spawnName)];
  });
  const backend = await env.boot(provider.model), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  const held = new Set();
  const observe = backend.harness.observeCommits({ advance({ changes }) {
    for (const c of changes) if (c.type === "task" && c.value.kind === "hush.review-worker" &&
        c.value.conversationId === Number(id) && c.value.state.status === "sleeping" && c.value.state.checkpoint.phase === "gate") {
      held.add(c.value.id);
      if (held.size === 2) parked.resolve();
    }
  }, end() {} });
  t.after(() => observe.detach());
  const q = query(id, "held-spawn", { ...parallel, prompt: "Launch both reserved reviewers." }), receipt = ok(await start(a, q));
  await parked.promise;
  await backend.harness.commit(async tx => {
    const { cohorts } = await tx.doc(ledger, Number(id));
    assert.equal(cohorts[0].released, false);
    const task = await tx.task(cohorts[0].rows[1].task);
    assert.equal(task.state.status, "sleeping");
    // Test-only fault injection into a parked record: no scheduler invocation
    // can process the changed checkpoint until it receives a release signal.
    tx.setTask({ ...task, state: { ...task.state, checkpoint: { phase: "open" } } });
  }, ctx);
  assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["reserved", "unresolved"]);
  spawnNow.resolve();
  assert.equal(ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId })).status, "done");
  assert.match(spawnResult, /original review reservation cannot be correlated/i);
  assert.equal((await backend.harness.snapshot(ledger, Number(id), ctx)).cohorts[0].released, false);
  assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["reserved", "unresolved"]);
  assert.equal(provider.requests.length, 2, "spawn started no child or replacement");
  assert.equal((await history(a, id)).entries.filter(e => e.kind === notices).length, 0);
});

// Stop is per assignment. A request acknowledged during a live review is
// not a terminal observation; only the attributed parent notice is.
test("Stop settles only the addressed worker and leaves its sibling alone", { timeout: 10_000 }, async t => {
  const env = await layout(t), left = env.hold(), right = env.hold(), releaseLeft = env.hold(), releaseRight = env.hold();
  const provider = await reviewProvider(t, env, async (turn, input) => {
    assert.ok(input);
    if (turn.results.length) return "Finished.";
    (input.name === "Fast" ? left : right).resolve();
    await (input.name === "Fast" ? releaseLeft : releaseRight).promise;
    return [call("hush_review_report", { report: "SIBLING_KEPT_WORKING" })];
  });
  await env.boot(provider.model);
  const a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  await state(a, id);
  const q = query(id, "stop-one", parallel), receipt = ok(await start(a, q));
  await Promise.all([left.promise, right.promise]);
  const stopped = ok(await stop(a, id, receipt.workers[0].handle)).worker;
  assert.ok(["stopping", "cancelled"].includes(stopped.state));
  await waitFor(a, "The worker was stopped.");
  assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["cancelled", "running"]);
  releaseRight.resolve();
  await waitFor(a, "SIBLING_KEPT_WORKING");
  assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["cancelled", "succeeded"]);
  releaseLeft.resolve();
});

const crashFixture = fileURLToPath(new URL("./review-delegation-crash-fixture.mjs", import.meta.url));
async function processBackend(env, model, boundary) {
  const proc = spawn(process.execPath, [crashFixture, JSON.stringify({ ...env.config, model }), boundary], { stdio: ["ignore", "pipe", "pipe"] });
  const exit = once(proc, "close");
  let stderr = "";
  proc.stderr.on("data", chunk => { stderr += chunk; });
  env.release.push(() => proc.kill("SIGKILL"));
  const output = createInterface({ input: proc.stdout });
  const ready = await output[Symbol.asyncIterator]().next();
  assert.equal(ready.value, "ready", stderr);
  return { proc, exit, error: () => stderr };
}

for (const boundary of ["snapshot", "reservation", "child-open", "child-start"]) {
  test(`restart after ${boundary} does not replace or replay an admitted worker`, { timeout: 10_000 }, async t => {
    const env = await layout(t);
    const provider = await reviewProvider(t, env, (turn, input) => {
      assert.ok(input);
      return turn.results.length ? "Finished." : [call("hush_review_report", { report: "FIRST_EXPLICIT_REVIEW" })];
    });
    const crashed = await processBackend(env, provider.model, boundary), before = await env.peer();
    const { id } = ok(await before.send("thread.new", {})), q = query(id, boundary);
    const lost = start(before, q).catch(() => null);
    assert.deepEqual(await crashed.exit, [null, "SIGKILL"], crashed.error());
    assert.equal(provider.requests.length, 0);
    const original = await lost;
    await env.boot(provider.model);
    const after = await env.peer();
    const baseline = await state(after, id);
    if (boundary === "snapshot") deny(await after.send("ext.delegation.get", { threadId: id, requestId: q.requestId }), "not_found");
    const receipt = ok(await start(after, q));
    if (original) assert.deepEqual(ok(original), receipt);
    if (!baseline.window.entries.some(e => e.kind === notices)) await waitFor(after, boundary === "snapshot" ? "FIRST_EXPLICIT_REVIEW" : "it was not replayed");
    const v = await info(after, q);
    assert.deepEqual(v.receipt, receipt);
    assert.equal(v.workers[0].state, boundary === "snapshot" ? "succeeded" : "failed");
    assert.equal(provider.requests.length, boundary === "snapshot" ? 2 : 0);
    assert.equal((await history(after, id)).entries.filter(e => e.kind === notices).length, 1);
    assert.deepEqual(ok(await start(after, q)), receipt);
  });
}

for (const boundary of ["inflight", "child-settled", "notice", "joined"]) {
  test(`restart at ${boundary} keeps one outcome and resolves the original join`, { timeout: 12_000 }, async t => {
    const env = await layout(t), started = env.hold(), allow = env.hold();
    let childStarts = 0, syntheses = 0;
    const prompt = "Use the reserved reviewer and join its result.";
    const provider = await reviewProvider(t, env, async (turn, input) => {
      if (input) {
        if (turn.results.length) return "Finished.";
        childStarts++;
        started.resolve();
        await allow.promise;
        return [call("hush_review_report", { report: "COMMITTED_CHILD_EVIDENCE" })];
      }
      assert.equal(turn.text, prompt);
      assert.ok(turn.tools.includes(joinName));
      if (!turn.results.length) return [call(spawnName)];
      const last = JSON.parse(turn.results.at(-1).content);
      if (turn.results.length === 1) return [call(joinName, { handles: last.workers.map(w => w.handle) })];
      assert.equal(last.wait, "settled");
      assert.equal(last.outcomes[0].state, boundary === "inflight" ? "failed" : "succeeded");
      if (boundary !== "inflight") assert.equal(last.outcomes[0].report, "COMMITTED_CHILD_EVIDENCE");
      syntheses++;
      return "Same-turn synthesis.";
    });
    const crashed = await processBackend(env, provider.model, boundary), a = await env.peer();
    const { id } = ok(await a.send("thread.new", {}));
    let view = await state(a, id);
    const q = query(id, "join-at-boundary", { prompt }), receipt = ok(await start(a, q));
    await started.promise;
    await a.wait(frame => {
      if (frame.type === "patch") view = applyImmutable(view, frame.ops);
      return view.tools.some(slot => slot.name === joinName && slot.status === "sleeping");
    });
    const entry = view.window.entries.find(e => e.kind === "pi.user").id;
    if (boundary === "inflight") crashed.proc.kill("SIGKILL");
    else allow.resolve();
    assert.deepEqual(await crashed.exit, [null, "SIGKILL"], crashed.error());
    allow.resolve();
    await env.boot(provider.model);
    const after = await env.peer();
    assert.deepEqual(ok(await start(after, q)), receipt);
    const result = ok(await after.send("submission.wait", { threadId: id, submissionId: receipt.submissionId }));
    assert.equal(result.status, "done");
    assert.equal(result.entryId, entry);
    assert.equal(syntheses, 1); assert.equal(childStarts, 1);
    const h = (await history(after, id)).entries;
    assert.equal(h.filter(e => e.kind === notices).length, 1);
    assert.equal(h.filter(e => e.kind === "pi.user").length, 1);
    assert.equal(h.flatMap(e => e.messages).filter(m => m.role === "toolResult" && m.name === joinName).length, 1);
  });
}

test("a worker stays private when its assigned policy is unavailable", { timeout: 12_000 }, async t => {
  const env = await layout(t);
  const provider = await reviewProvider(t, env, turn =>
    turn.results.length ? "Finished." : [call("hush_review_report", { report: "FIXTURE_REPORT" })]);
  const backend = await env.boot(provider.model), a = await env.peer(1, { workerActivity: [1] }), old = await env.peer(false);
  const { id } = ok(await a.send("thread.new", {}));
  await state(a, id);
  const q = query(id, "private-worker"), receipt = ok(await start(a, q));
  await waitFor(a, "FIXTURE_REPORT");

  const roster = after => a.send("ext.workerActivity.wait", { threadId: id, ...(after && { after }) });
  const baseline = ok(await roster());
  assert.equal(baseline.workers[0].state, "succeeded");
  const changed = roster(baseline.cursor);
  // The connection processes requests in order. This proves the equal-cursor
  // wait is outstanding before the only change (a child document retirement).
  deny(await roster(baseline.cursor), "limit");
  await history(a, id);
  const { child, submission } = await backend.harness.commit(async tx => {
    const { cohorts } = await tx.doc(ledger, Number(id));
    const [row] = cohorts[0].rows;
    assert.ok(row.child && row.submission);
    await tx.retireDoc(policy, row.child);
    return { child: String(row.child), submission: String(row.submission) };
  }, ctx);
  const update = ok(await changed);
  assert.notEqual(update.cursor, baseline.cursor);
  assert.deepEqual(update.workers, [{ ...receipt.workers[0], state: "unresolved", activity: "" }]);
  for (const peer of [a, old]) {
    deny(await peer.send("subscribe", { threadId: child }), "not_found");
    deny(await peer.send("history", { threadId: child, range: { kind: "before", beforeEntryId: null }, limit: 1 }), "not_found");
    deny(await peer.send("submission.get", { threadId: child, submissionId: submission }), "not_found");
    assert.ok(!ok(await peer.send("thread.list", { limit: 100 })).threads.some(row => row.id === child));
  }
  const ref = { threadId: id, handle: receipt.workers[0].handle };
  assert.equal(ok(await a.send("ext.workerActivity.inspect", ref)).worker.state, "unresolved");
  deny(await a.send("ext.workerActivity.inspect", { ...ref, transcript: true }), "invalid_request");
});

async function waitForTaskTerminal(backend, taskId) {
  const terminal = Promise.withResolvers();
  const observed = backend.harness.observeCommits({ advance({ changes }) {
    if (changes.some(c => c.type === "task" && c.value.id === taskId && c.value.state.status === "terminal")) terminal.resolve();
  }, end() {} });
  try {
    if ((await backend.harness.getTask(taskId, ctx)).state.status !== "terminal") await terminal.promise;
  } finally { observed.detach(); }
}

test("restart does not settle a worker with a contradictory checkpoint", { timeout: 12_000 }, async t => {
  const env = await layout(t), allow = env.hold();
  const provider = await reviewProvider(t, env, async (turn, input) => {
    assert.ok(input);
    if (isRefill(input)) return refillReply(turn, input);
    await allow.promise;
    return turn.results.length ? "Finished." : [call("hush_review_report", { report: "FIXTURE_REPORT" })];
  });
  const crashed = await processBackend(env, provider.model, "child-settled"), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  const q = query(id, "checkpoint-mismatch"), receipt = ok(await start(a, q));
  allow.resolve();
  assert.deepEqual(await crashed.exit, [null, "SIGKILL"], crashed.error());

  // Only the disposable coordinator checkpoint disagrees. The committed child
  // submission, ownership, admitted ledger and report remain intact.
  const sqlite = new DatabaseSync(join(env.config.dataDir, "history.sqlite"));
  let taskId;
  try {
    const { id: task, record } = sqlite.prepare(`select id, record from tasks where kind=json_quote('hush.review-worker')`).get();
    taskId = task;
    const { phase, submission } = JSON.parse(record).state.checkpoint;
    assert.equal(phase, "settle");
    assert.ok(Number.isSafeInteger(submission));
    sqlite.prepare(`update tasks set record=json_set(record, '$.state.checkpoint.submission', ?) where id=?`).run(submission + 1, taskId);
  } finally { sqlite.close(); }

  await waitForTaskTerminal(await env.boot(provider.model), taskId);
  const after = await env.peer(1, { workerActivity: [1] });
  const [worker] = (await info(after, q)).workers;
  assert.equal(worker.state, "unresolved");
  assert.equal(worker.noticeId, undefined);
  assert.equal(worker.report, undefined);
  assert.deepEqual(ok(await stop(after, id, receipt.workers[0].handle)).worker, worker);
  assert.equal((await history(after, id)).entries.filter(e => e.kind === notices).length, 0);
  await state(after, id);
  const refill = query(id, "refill-after-restart", refillPair), refilled = ok(await start(after, refill));
  await assertRefillSucceeded(after, refill, refilled);
  assert.deepEqual((await info(after, q)).workers, [worker]);
  assertBriefStarts(provider, { q, receipt }, { q: refill, receipt: refilled });
});

test("restart does not settle a held worker with an impossible open checkpoint", { timeout: 12_000 }, async t => {
  const env = await layout(t);
  const provider = await reviewProvider(t, env, (turn, input) => {
    if (input && isRefill(input)) return refillReply(turn, input);
    assert.equal(input, undefined, "no child can start without release");
    return "Finished without launching a review.";
  });
  const crashed = await processBackend(env, provider.model, "reservation"), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  const q = query(id, "held-checkpoint", { prompt: "Do not release the reserved reviewer." });
  const lost = start(a, q).catch(() => null);
  assert.deepEqual(await crashed.exit, [null, "SIGKILL"], crashed.error());
  await lost;

  const sqlite = new DatabaseSync(join(env.config.dataDir, "history.sqlite"));
  let taskId;
  try {
    const { id: task, record } = sqlite.prepare(`select id, record from tasks where kind=json_quote('hush.review-worker')`).get();
    taskId = task;
    assert.deepEqual(JSON.parse(record).state.checkpoint, { phase: "gate" });
    sqlite.prepare(`update tasks set record=json_set(record, '$.state.checkpoint.phase', 'open') where id=?`).run(taskId);
  } finally { sqlite.close(); }

  await waitForTaskTerminal(await env.boot(provider.model), taskId);
  const after = await env.peer();
  const receipt = ok(await start(after, q));
  const [worker] = (await info(after, q)).workers;
  assert.equal(worker.state, "unresolved");
  assert.equal(worker.noticeId, undefined);
  assert.equal(ok(await after.send("submission.wait", { threadId: id, submissionId: receipt.submissionId })).status, "done");
  assert.equal((await history(after, id)).entries.filter(e => e.kind === notices).length, 0);
  await state(after, id);
  const refill = query(id, "refill-after-held-restart", refillPair), refilled = ok(await start(after, refill));
  await assertRefillSucceeded(after, refill, refilled);
  assert.deepEqual((await info(after, q)).workers, [worker]);
  assertBriefStarts(provider, { q, receipt, count: 0 }, { q: refill, receipt: refilled });
  assert.equal(provider.requests.filter(r => !isBrief(r.text)).length, 1, "only the original parent turn resumed");
});

test("missing and corrupt worker evidence stays unresolved and holds capacity", { timeout: 12_000 }, async t => {
  const env = await layout(t);
  const provider = await reviewProvider(t, env, (turn, input) => {
    assert.ok(input);
    return turn.results.length ? "Finished." : [call("hush_review_report", { report: "PREVIOUSLY_COMMITTED" })];
  });
  const proc = await processBackend(env, provider.model, "none"), before = await env.peer();
  const { id } = ok(await before.send("thread.new", {})), q = query(id, "corrupt", parallel);
  let view = await state(before, id);
  const receipt = ok(await start(before, q));
  await before.wait(frame => {
    if (frame.type === "patch") view = applyImmutable(view, frame.ops);
    return view.window.entries.filter(e => e.kind === notices).length === 2;
  });
  assert.ok((await info(before, q)).workers.every(w => w.state === "succeeded"));
  proc.proc.kill("SIGKILL");
  await proc.exit;
  const sqlite = new DatabaseSync(join(env.config.dataDir, "history.sqlite"));
  try {
    const rows = sqlite.prepare(`select id from tasks where kind=json_quote('hush.review-worker') order by id`).all();
    assert.equal(rows.length, 2);
    sqlite.prepare(`update tasks set record=json_set(record, '$.input.fingerprint', 'broken') where id=?`).run(rows[0].id);
    sqlite.prepare(`delete from tasks where id=?`).run(rows[1].id);
  } finally { sqlite.close(); }
  await env.boot(provider.model);
  const after = await env.peer(1, { workerActivity: [1] });
  const activity = ok(await after.send("ext.workerActivity.wait", { threadId: id }));
  assert.deepEqual(activity.workers.map(w => [w.handle, w.state]), receipt.workers.map(w => [w.handle, "unresolved"]));
  for (const { handle } of receipt.workers) {
    const ref = { threadId: id, handle };
    deny(await after.send("ext.workerActivity.inspect", ref), "invalid_request");
    deny(await after.send("ext.workerActivity.inspect", { ...ref, transcript: true }), "invalid_request");
  }
  for (const _ of [1, 2]) {
    const v = await info(after, q);
    assert.ok(v.workers.every(w => w.state === "unresolved" && !w.report && !w.noticeId));
    for (const worker of v.workers) assert.match(worker.detail, /reservation is still held/i);
    assert.deepEqual(v.receipt, receipt);
  }
  assert.equal(ok(await stop(after, id, receipt.workers[0].handle)).worker.state, "unresolved");
  deny(await start(after, query(id, "must-not-refill")), "limit");
  assert.equal(provider.requests.length, 4, "no replacement request was sent");
});

test("worker activity shows orphaned tasks as unresolved when a cohort or its ledger is missing", { timeout: 12_000 }, async t => {
  const env = await layout(t);
  const provider = await reviewProvider(t, env, (turn, input) =>
    turn.results.length ? "Finished." : [call("hush_review_report", { report: `ORIGINAL_${input.name}_REPORT` })]);
  const backend = await env.boot(provider.model);
  const caps = { workerActivity: [1] };
  let a = await env.peer(1, caps);
  const { id } = ok(await a.send("thread.new", {}));
  const other = ok(await a.send("thread.new", {})).id;
  const roster = (peer, threadId = id, after) => peer.send("ext.workerActivity.wait", { threadId, ...(after && { after }) }).then(ok);
  assert.deepEqual((await roster(a)).workers, []);
  await state(a, id);
  const one = query(id, "first"), two = query(id, "second", { assignments: [{ name: "Second", brief: "Read context" }] });
  const [first] = ok(await start(a, one)).workers;
  await waitFor(a, "ORIGINAL_First_REPORT");
  const [second] = ok(await start(a, two)).workers;
  await waitFor(a, "ORIGINAL_Second_REPORT");
  const initial = await roster(a);
  const succeeded = w => ({ ...w, state: "succeeded", activity: "" });
  const orphan = ({ handle }) => ({ handle, name: "Missing review receipt", state: "unresolved", activity: "" });
  assert.deepEqual(initial.workers, [succeeded(first), succeeded(second)]);

  // Simulate lost evidence in the disposable store while the tasks remain.
  const update = roster(a, id, initial.cursor);
  await backend.harness.commit(async tx => { (await tx.doc(ledger, Number(id))).cohorts.shift(); }, ctx);
  const partial = await update;
  assert.deepEqual(partial.workers, [orphan(first), succeeded(second)], "do not hide or reorder the successful sibling");
  const lost = roster(a, id, partial.cursor);
  await backend.harness.commit(tx => tx.retireDoc(ledger, Number(id)), ctx);
  assert.deepEqual((await lost).workers, [orphan(first), orphan(second)]);
  assert.deepEqual((await roster(a, other)).workers, []);
  const ref = { threadId: id, handle: first.handle };
  const inspect = params => a.send("ext.workerActivity.inspect", { ...ref, ...params });
  const report = await inspect();
  deny(report, "invalid_request");
  assert.match(report.error.message, /original review receipt is missing/i);
  deny(await inspect({ transcript: true }), "invalid_request");
  deny(await inspect({ threadId: other }), "not_found");
  deny(await stop(a, id, first.handle), "not_found");
  deny(await start(a, query(id, "must-not-refill")), "invalid_request");
  assert.equal(await backend.harness.snapshot(ledger, Number(id), ctx), undefined, "inspection did not reconstruct the ledger");

  await backend.close();
  await env.boot(provider.model);
  a = await env.peer(1, caps);
  assert.deepEqual((await roster(a)).workers, [orphan(first), orphan(second)]);
  assert.equal((await history(a, id)).entries.filter(e => e.kind === notices).length, 2, "no replacement notice");
  assert.equal(provider.requests.length, 4, "no replacement worker");
});

test("an acknowledged Stop keeps an unresponsive worker's reservation until its owned work drains", { timeout: 10_000 }, async t => {
  const env = await layout(t), started = env.hold(), release = env.hold();
  const backend = await env.boot();
  // The scripted provider ignores abort until released. No subprocess,
  // shell, or side effects are required to make an invocation unresponsive.
  const faux = fauxProvider();
  faux.setResponses([async () => { started.resolve(); await release.promise; return fauxAssistantMessage("This is not a report."); }]);
  backend.runtime.setProvider(faux.provider);
  const a = await env.peer();
  const { id } = ok(await a.send("thread.new", { model: { provider: "faux", modelId: "faux-1" } }));
  await state(a, id);
  const q = query(id, "slow-stop"), receipt = ok(await start(a, q));
  await started.promise;
  const stopping = ok(await stop(a, id, receipt.workers[0].handle)).worker;
  assert.equal(stopping.state, "stopping");
  assert.equal(stopping.noticeId, undefined);
  assert.equal((await info(a, q)).workers[0].state, "stopping");
  deny(await start(a, query(id, "no-refill", parallel)), "limit");
  assert.ok(!(await history(a, id)).entries.some(e => e.kind === notices));
  release.resolve();
  await waitFor(a, "The worker was stopped.");
  assert.equal((await info(a, q)).workers[0].state, "cancelled");
});

test("an ordinary model guessing a hidden tool cannot authorize a worker", { timeout: 10_000 }, async t => {
  const env = await layout(t);
  const provider = await reviewProvider(t, env, (turn, input) => {
    assert.equal(input, undefined);
    assert.ok(!turn.tools.includes(spawnName) && !turn.tools.includes(joinName));
    if (turn.results.length) {
      assert.match(turn.results[0].content, /No review launch was authorized|not available/);
      return "No work launched.";
    }
    return [call(spawnName)];
  });
  const backend = await env.boot(provider.model);
  const a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  assert.equal((await ask(a, id, "Try starting an unapproved review", "hidden-call")).status, "done");
  assert.equal((await backend.harness.commit(tx => tx.scanTasks({ conversationId: Number(id), kind: "hush.review-worker" }, 2), ctx)).items.length, 0);
});

const startedBriefs = provider => provider.requests.filter(r => isBrief(r.text) && !r.results.length).map(r => brief(r.text).brief);
function assertBriefStarts(provider, ...cohorts) {
  // The provider sees the brief, not the handle. Give each fixture assignment a distinct brief.
  const started = startedBriefs(provider), seen = new Set();
  let total = 0;
  for (const { q, receipt, count = 1 } of cohorts) receipt.workers.forEach(({ handle }, i) => {
    const text = q.assignments[i].brief;
    assert.ok(!seen.has(text), "fixture briefs must be distinct");
    seen.add(text);
    assert.equal(started.filter(b => b === text).length, count, `provider starts for ${handle}`);
    total += count;
  });
  assert.equal(started.length, total, "no extra brief reached the provider");
}
const noticeIds = async (wire, id) => (await history(wire, id)).entries.filter(e => e.kind === notices).map(e => e.id);

for (const [mode, stoppedIndex] of [["parallel", 0], ["sequential", 0], ["sequential", 1]]) {
  test(`held ${mode} spawn and join after Stop of the ${stoppedIndex === 0 ? "first" : "second"} worker`, { timeout: 10_000 }, async t => {
    const env = await layout(t), parentReady = env.hold(), spawnNow = env.hold();
    const prompt = "Launch the two reserved reviews and join both.";
    const report = "SURVIVING_REVIEW_REPORT", survivor = 1 - stoppedIndex;
    let released, joined;
    const provider = await reviewProvider(t, env, async (turn, input) => {
      if (input) return turn.results.length ? "Finished." : [call("hush_review_report", { report })];
      assert.equal(turn.text, prompt);
      switch (turn.results.length) {
        case 0:
          assert.ok(turn.tools.includes(spawnName) && turn.tools.includes(joinName));
          parentReady.resolve();
          await spawnNow.promise;
          return [call(spawnName)];
        case 1:
          released = JSON.parse(turn.results[0].content);
          return [call(joinName, { handles: released.workers.map(w => w.handle) })];
        case 2:
          joined = JSON.parse(turn.results[1].content);
          return "Finished the parent turn.";
        default:
          assert.fail("expected one spawn and one join");
      }
    });
    await env.boot(provider.model);
    const a = await env.peer();
    const { id } = ok(await a.send("thread.new", {}));
    await state(a, id);
    const q = query(id, "held-stop", { ...parallel, mode, prompt }), receipt = ok(await start(a, q));
    await parentReady.promise;
    assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["reserved", "reserved"]);
    ok(await stop(a, id, receipt.workers[stoppedIndex].handle));
    await waitFor(a, "The worker was stopped.");
    const held = (await info(a, q)).workers;
    assert.equal(held[stoppedIndex].state, "cancelled");
    assert.equal(held[survivor].state, "reserved");
    assert.deepEqual(startedBriefs(provider), []);

    spawnNow.resolve();
    assert.equal(ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId })).status, "done");
    assert.deepEqual(released, { snapshotId: receipt.snapshotId, workers: receipt.workers });
    const { workers } = await info(a, q);
    assert.deepEqual(workers.map(w => w.state), stoppedIndex === 0 ? ["cancelled", "succeeded"] : ["succeeded", "cancelled"]);
    assert.equal(workers[survivor].report, report);
    assert.deepEqual(joined, { wait: "settled", outcomes: workers.map(w => ({ ...w, snapshotId: receipt.snapshotId })) });
    assert.deepEqual(await noticeIds(a, id), [held[stoppedIndex].noticeId, workers[survivor].noticeId]);
    assert.deepEqual(startedBriefs(provider), [q.assignments[survivor].brief]);
  });
}

function observeParentCommits(t, backend, id, visit) {
  const owner = Number(id);
  const observed = backend.harness.observeCommits({ advance({ changes }) {
    for (const { type, value } of changes) if (value?.conversationId === owner) visit(type, value);
  }, end() {} });
  t.after(() => observed.detach());
}

// Wait for the successor to reach the fence without depending on how it is woken.
const atReviewFence = (task, requestId) => task.kind === "hush.review-worker" && task.input.key === requestId &&
  ["sleeping", "waiting"].includes(task.state.status) && task.state.checkpoint?.phase === "gate";

test("a later review request starts only after its running predecessor publishes a success notice", { timeout: 10_000 }, async t => {
  const env = await layout(t), firstStarted = env.hold(), finishFirst = env.hold(), fenced = Promise.withResolvers();
  const [first, later] = parallel.assignments, order = [];
  const provider = await reviewProvider(t, env, async (turn, input) => {
    assert.ok(input);
    if (turn.results.length) return "Finished.";
    if (input.name === first.name) {
      firstStarted.resolve();
      await finishFirst.promise;
      return [call("hush_review_report", { report: "FIRST_REQUEST_REPORT" })];
    }
    assert.equal(input.name, later.name);
    order.push("later brief");
    return [call("hush_review_report", { report: "LATER_REQUEST_REPORT" })];
  });
  const backend = await env.boot(provider.model), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  await state(a, id);
  const earlier = query(id, "earlier-request", { assignments: [first] });
  const next = query(id, "later-request", { assignments: [later] });
  const firstHandle = ok(await start(a, earlier)).workers[0].handle;
  await firstStarted.promise;
  observeParentCommits(t, backend, id, (type, value) => {
    if (type === "task" && atReviewFence(value, next.requestId)) fenced.resolve(value.input.handle);
    if (type === "entry" && value.kind === notices && value.data.handle === firstHandle) order.push("first notice");
  });
  const laterHandle = ok(await start(a, next)).workers[0].handle;
  assert.equal(await fenced.promise, laterHandle);
  assert.equal((await info(a, earlier)).workers[0].state, "running");
  assert.equal((await info(a, next)).workers[0].state, "reserved");
  assert.deepEqual(startedBriefs(provider), [first.brief]);

  finishFirst.resolve();
  await waitFor(a, "LATER_REQUEST_REPORT");
  assert.equal((await info(a, earlier)).workers[0].state, "succeeded");
  assert.equal((await info(a, next)).workers[0].state, "succeeded");
  assert.deepEqual(order, ["first notice", "later brief"]);
  assert.deepEqual(startedBriefs(provider), [first.brief, later.brief]);
});

test("Stop cancels a fenced sequential successor without stopping its running predecessor", { timeout: 10_000 }, async t => {
  const env = await layout(t), firstStarted = env.hold(), finishFirst = env.hold(), fenced = Promise.withResolvers();
  const [first] = parallel.assignments, report = "PREDECESSOR_REVIEW_REPORT";
  const provider = await reviewProvider(t, env, async (turn, input) => {
    assert.equal(input?.name, first.name, "the fenced successor never starts");
    if (turn.results.length) return "Finished.";
    firstStarted.resolve();
    await finishFirst.promise;
    return [call("hush_review_report", { report })];
  });
  const backend = await env.boot(provider.model), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  await state(a, id);
  const q = query(id, "stop-fenced", { ...parallel, mode: "sequential" });
  observeParentCommits(t, backend, id, (type, value) => {
    if (type === "task" && atReviewFence(value, q.requestId)) fenced.resolve(value.input.handle);
  });
  const { workers: [, successor] } = ok(await start(a, q));
  await firstStarted.promise;
  assert.equal(await fenced.promise, successor.handle);
  assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["running", "reserved"]);
  ok(await stop(a, id, successor.handle));
  await waitFor(a, "The worker was stopped.");
  const live = (await info(a, q)).workers;
  assert.deepEqual(live.map(w => w.state), ["running", "cancelled"]);
  assert.deepEqual(await noticeIds(a, id), [live[1].noticeId]);

  finishFirst.resolve();
  await waitFor(a, report);
  const { workers } = await info(a, q);
  assert.deepEqual(workers.map(w => w.state), ["succeeded", "cancelled"]);
  assert.deepEqual(await noticeIds(a, id), [live[1].noticeId, workers[0].noticeId]);
  assert.deepEqual(startedBriefs(provider), [first.brief]);
});

async function heldReviewPair(t) {
  const env = await layout(t), spawnNow = env.hold(), parked = Promise.withResolvers();
  const tasks = new Map();
  let spawnResult;
  const provider = await reviewProvider(t, env, async (turn, input) => {
    if (input) return isRefill(input) ? refillReply(turn, input) : "No advisory report.";
    if (turn.results.length) spawnResult = turn.results[0].content;
    else if (await spawnNow.promise) return [call(spawnName)];
    return "Finished the parent turn.";
  });
  const backend = await env.boot(provider.model), a = await env.peer(1, { workerActivity: [1] });
  const { id } = ok(await a.send("thread.new", {}));
  const q = query(id, "held-pair", { ...parallel, prompt: "Launch both reserved reviewers." });
  observeParentCommits(t, backend, id, (type, task) => {
    if (type === "task" && atReviewFence(task, q.requestId)) {
      tasks.set(task.input.handle, task.id);
      if (tasks.size === 2) parked.resolve();
    }
  });
  const receipt = ok(await start(a, q));
  await parked.promise;
  return { backend, a, id, q, receipt, provider, tasks, spawnNow, spawnResult: () => spawnResult };
}

const rosterWorkers = async (wire, threadId) => ok(await wire.send("ext.workerActivity.wait", { threadId })).workers;
const workerStates = workers => workers.map(({ handle, name, state }) => ({ handle, name, state }));

test("R6: losing a held worker row keeps the receipt member visible and prevents any release", { timeout: 10_000 }, async t => {
  const { backend, a, id, q, receipt, provider, tasks, spawnNow, spawnResult } = await heldReviewPair(t);
  const [first, second] = receipt.workers;
  await backend.harness.commit(async tx => { (await tx.doc(ledger, Number(id))).cohorts[0].rows.pop(); }, ctx);
  const expected = [{ ...first, state: "reserved" }, { ...second, state: "unresolved" }];
  const { workers } = await info(a, q);
  assert.deepEqual(workerStates(workers), expected);
  assert.deepEqual(await rosterWorkers(a, id), expected.map(w => ({ ...w, activity: "" })));
  deny(await a.send("ext.workerActivity.inspect", { threadId: id, handle: second.handle }), "invalid_request");

  const firstTransitions = [];
  observeParentCommits(t, backend, id, (type, task) => {
    if (type === "task" && task.id === tasks.get(first.handle)) firstTransitions.push(task.state.status);
  });
  const stopped = await stop(a, id, second.handle);
  spawnNow.resolve(true);
  assert.equal(ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId })).status, "done");
  assert.match(spawnResult(), /original review reservation cannot be correlated/i);
  assert.equal((await backend.harness.snapshot(ledger, Number(id), ctx)).cohorts[0].released, false);
  assert.deepEqual(firstTransitions, [], "spawn did not signal the intact reservation");
  assert.deepEqual(ok(stopped).worker, workers[1]);
  deny(await start(a, query(id, "must-not-refill", parallel)), "limit");
  assert.deepEqual(startedBriefs(provider), []);
});

test("R10: a row copied across cohorts cannot duplicate or displace receipt members", { timeout: 10_000 }, async t => {
  const env = await layout(t);
  const provider = await reviewProvider(t, env, (turn, input) =>
    turn.results.length ? "Finished." : [call("hush_review_report", { report: `COPIED_ROW_${input.name}_REPORT` })]);
  const backend = await env.boot(provider.model), a = await env.peer(1, { workerActivity: [1] });
  const { id } = ok(await a.send("thread.new", {}));
  await state(a, id);
  const one = query(id, "original-row"), two = query(id, "copied-row", { assignments: [{ name: "Second", brief: "Read the test" }] });
  const [first] = ok(await start(a, one)).workers;
  await waitFor(a, "COPIED_ROW_First_REPORT");
  const [second] = ok(await start(a, two)).workers;
  await waitFor(a, "COPIED_ROW_Second_REPORT");
  await backend.harness.commit(async tx => {
    const { cohorts: [original, other] } = await tx.doc(ledger, Number(id));
    other.rows.unshift({ ...original.rows[0] });
  }, ctx);
  const expected = [{ ...first, state: "unresolved" }, { ...second, state: "succeeded" }];
  assert.deepEqual(await rosterWorkers(a, id), expected.map(w => ({ ...w, activity: "" })));
  assert.deepEqual(workerStates((await info(a, one)).workers), [expected[0]]);
  assert.deepEqual(workerStates((await info(a, two)).workers), [expected[1]]);
  deny(await a.send("ext.workerActivity.inspect", { threadId: id, handle: first.handle }), "invalid_request");
  deny(await start(a, query(id, "ambiguous-row-refill", parallel)), "limit");
});

async function reviewPairMissingFirstRow(t) {
  const env = await layout(t), slowStarted = env.hold(), finishSlow = env.hold(), joinNow = env.hold();
  let joinResult;
  const provider = await reviewProvider(t, env, async (turn, input) => {
    if (input) {
      if (turn.results.length) return "Finished.";
      if (input.name === "Slow") { slowStarted.resolve(); await finishSlow.promise; }
      return [call("hush_review_report", { report: `ROW_LOSS_${input.name}_REPORT` })];
    }
    switch (turn.results.length) {
      case 0: return [call(spawnName)];
      case 1: {
        const handles = JSON.parse(turn.results[0].content).workers.map(w => w.handle);
        await joinNow.promise;
        return [call(joinName, { handles })];
      }
      default:
        joinResult = turn.results[1].content;
        return "Finished the parent turn.";
    }
  });
  const backend = await env.boot(provider.model), a = await env.peer(1, { workerActivity: [1] });
  const { id } = ok(await a.send("thread.new", {})), view = await state(a, id);
  const q = query(id, "lost-first-row", { ...parallel, prompt: "Launch both reviews and join them." });
  const receipt = ok(await start(a, q));
  await Promise.all([slowStarted.promise, waitFor(a, "ROW_LOSS_Fast_REPORT")]);
  assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["succeeded", "running"]);
  const firstTask = await backend.harness.commit(async tx => {
    const [removed] = (await tx.doc(ledger, Number(id))).cohorts[0].rows.splice(0, 1);
    assert.equal(removed.handle, receipt.workers[0].handle);
    return removed.task;
  }, ctx);
  return { env, provider, backend, a, id, q, receipt, firstTask, view, joinNow, finishSlow, joinResult: () => joinResult };
}

test("R7: join waits for the running sibling of a receipt member whose row was lost", { timeout: 10_000 }, async t => {
  const { a, id, q, receipt, view, joinNow, finishSlow, joinResult } = await reviewPairMissingFirstRow(t);
  let live = view;
  const parked = a.wait(frame => {
    if (frame.type === "patch") live = applyImmutable(live, frame.ops);
    return live.tools.some(slot => slot.name === joinName && slot.status === "sleeping");
  });
  joinNow.resolve();
  const parent = a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId }).then(ok);
  assert.equal(await Promise.race([parked.then(() => "parked"), parent.then(joinResult)]), "parked");
  assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["unresolved", "running"]);
  finishSlow.resolve();
  assert.equal((await parent).status, "done");
  const { wait, outcomes } = JSON.parse(joinResult());
  assert.equal(wait, "unresolved");
  assert.deepEqual(outcomes.map(w => [w.handle, w.state, w.report]), [
    [receipt.workers[0].handle, "unresolved", undefined],
    [receipt.workers[1].handle, "succeeded", "ROW_LOSS_Slow_REPORT"],
  ]);
  assert.ok(outcomes.every(w => w.snapshotId === receipt.snapshotId));
});

test("R8: a missing row and task still occupy capacity after restart", { timeout: 10_000 }, async t => {
  const { env, provider, backend, a, id, q, receipt, firstTask, joinNow, finishSlow } = await reviewPairMissingFirstRow(t);
  joinNow.resolve();
  finishSlow.resolve();
  await waitFor(a, "ROW_LOSS_Slow_REPORT");
  assert.equal(ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId })).status, "done");
  await backend.close();
  const sqlite = new DatabaseSync(join(env.config.dataDir, "history.sqlite"));
  try {
    assert.equal(sqlite.prepare("delete from tasks where id=?").run(firstTask).changes, 1);
  } finally { sqlite.close(); }
  await env.boot(provider.model);
  const after = await env.peer(1, { workerActivity: [1] });
  deny(await start(after, query(id, "must-not-refill", parallel)), "limit");
  const [first, second] = receipt.workers;
  const expected = [{ ...first, state: "unresolved" }, { ...second, state: "succeeded" }];
  assert.deepEqual(workerStates((await info(after, q)).workers), expected);
  assert.deepEqual(await rosterWorkers(after, id), expected.map(w => ({ ...w, activity: "" })));
  assert.deepEqual(startedBriefs(provider).sort(), q.assignments.map(a => a.brief).sort());
});

test("R9: Stop ends an intact task with a contradictory held checkpoint without publishing an outcome", { timeout: 10_000 }, async t => {
  const { backend, a, id, q, receipt, provider, tasks, spawnNow } = await heldReviewPair(t);
  const handle = receipt.workers[1].handle, taskId = tasks.get(handle);
  await backend.harness.commit(async tx => {
    const task = await tx.task(taskId);
    assert.equal(task.state.status, "sleeping");
    tx.setTask({ ...task, state: { ...task.state, checkpoint: { phase: "open" } } });
  }, ctx);
  assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["reserved", "unresolved"]);
  const { worker } = ok(await stop(a, id, handle));
  assert.equal(worker.state, "unresolved");
  assert.equal(worker.noticeId, undefined);
  const task = await backend.harness.getTask(taskId, ctx);
  assert.ok(task.state.status === "terminal" || task.abortRequested, "Stop must abort the identity-checked task");
  await waitForTaskTerminal(backend, taskId);
  const { workers } = await info(a, q);
  assert.deepEqual(workers.map(w => w.state), ["reserved", "unresolved"]);
  assert.deepEqual((await backend.harness.commit(tx => tx.scanConversations({ ownerTaskId: taskId }, 2), ctx)).items, []);
  assert.deepEqual(await noticeIds(a, id), []);
  assert.deepEqual(startedBriefs(provider), []);

  spawnNow.resolve(false);
  assert.equal(ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId })).status, "done");
  await state(a, id);
  const refill = query(id, "refill-after-stop", { assignments: refillPair.assignments.slice(0, 1) });
  const refilled = ok(await start(a, refill));
  assert.match(workers[1].detail, /its task has ended; the slot is free/i);
  assert.equal((await info(a, refill)).workers[0].state, "reserved", "the intact held worker still fences the refill");
  deny(await start(a, query(id, "third-slot")), "limit");
  ok(await stop(a, id, receipt.workers[0].handle));
  await assertRefillSucceeded(a, refill, refilled);
  assert.deepEqual((await info(a, q)).workers[1], workers[1]);
  assertBriefStarts(provider, { q, receipt, count: 0 }, { q: refill, receipt: refilled });
});

for (const sameCohort of [true, false]) {
  test(`${sameCohort ? "R4: a sequential sibling" : "R5b: a later review request"} starts after a predecessor ends without a notice`, { timeout: 10_000 }, async t => {
    const env = await layout(t), firstStarted = env.hold(), finishFirst = env.hold(), fenced = Promise.withResolvers();
    const [first, next] = parallel.assignments, order = [], report = "AFTER_UNRESOLVED_PREDECESSOR";
    const provider = await reviewProvider(t, env, async (turn, input) => {
      assert.ok(input);
      if (isRefill(input)) return refillReply(turn, input);
      if (input.name === first.name) {
        firstStarted.resolve();
        await finishFirst.promise;
        return "Finished the predecessor's child turn.";
      }
      if (turn.results.length) return "Finished.";
      assert.equal(input.name, next.name);
      order.push("successor brief");
      return [call("hush_review_report", { report })];
    });
    const backend = await env.boot(provider.model), a = await env.peer();
    const { id } = ok(await a.send("thread.new", {}));
    await state(a, id);
    const earlier = query(id, "unresolved-predecessor", { mode: "sequential", assignments: sameCohort ? [first, next] : [first] });
    const later = sameCohort ? earlier : query(id, "after-unresolved", { assignments: [next] });
    observeParentCommits(t, backend, id, (type, value) => {
      if (type === "task" && atReviewFence(value, later.requestId)) fenced.resolve(value.input.handle);
    });
    const receipt = ok(await start(a, earlier));
    await firstStarted.promise;
    const laterReceipt = sameCohort ? receipt : ok(await start(a, later));
    const successor = laterReceipt.workers[sameCohort ? 1 : 0];
    assert.equal(await fenced.promise, successor.handle);
    assert.equal((await info(a, earlier)).workers[0].state, "running");
    assert.equal((await info(a, later)).workers.at(-1).state, "reserved");
    assert.deepEqual(startedBriefs(provider), [first.brief]);

    const firstTask = await backend.harness.commit(async tx => {
      const { task: taskId } = (await tx.doc(ledger, Number(id))).cohorts[0].rows[0];
      const task = await tx.task(taskId), { checkpoint } = task.state;
      assert.equal(task.state.status, "sleeping");
      assert.equal(checkpoint.phase, "settle");
      // The child still signals settlement, but the coordinator cannot
      // correlate it. Its identity and the successor's fence are unchanged.
      tx.setTask({ ...task, state: { ...task.state, checkpoint: { ...checkpoint, submission: checkpoint.submission + 1 } } });
      return taskId;
    }, ctx);
    observeParentCommits(t, backend, id, (type, value) => {
      if (type === "task" && value.id === firstTask && value.state.status === "terminal") order.push("predecessor ended");
    });
    finishFirst.resolve();
    await waitForTaskTerminal(backend, firstTask);
    assert.equal((await info(a, earlier)).workers[0].state, "unresolved");
    await waitFor(a, report);
    const { workers } = await info(a, earlier);
    const outcomes = sameCohort ? workers : [workers[0], ...(await info(a, later)).workers];
    assert.deepEqual(outcomes.map(w => [w.state, w.report]), [["unresolved", undefined], ["succeeded", report]]);
    assert.equal(outcomes[0].noticeId, undefined);
    assert.deepEqual(await noticeIds(a, id), [outcomes[1].noticeId]);
    assert.deepEqual(order, ["predecessor ended", "successor brief"]);
    assert.deepEqual(startedBriefs(provider), [first.brief, next.brief]);

    const refill = query(id, "after-both-end", { ...refillPair, assignments: refillPair.assignments.slice(0, sameCohort ? 2 : 1) });
    const refilled = ok(await start(a, refill));
    const { cohorts } = await backend.harness.snapshot(ledger, Number(id), ctx);
    assert.equal(cohorts.at(-1).rows[0].predecessor, undefined, "new work should not fence on an ended task");
    await assertRefillSucceeded(a, refill, refilled);
    assert.deepEqual((await info(a, earlier)).workers[0], outcomes[0]);
    const originals = sameCohort ? [{ q: earlier, receipt }] : [{ q: earlier, receipt }, { q: later, receipt: laterReceipt }];
    assertBriefStarts(provider, ...originals, { q: refill, receipt: refilled });
  });
}

test("a contradictory assigned profile cannot read another admitted snapshot from the same parent", { timeout: 10_000 }, async t => {
  const env = await layout(t), begun = env.hold(), readNow = env.hold(), readResult = Promise.withResolvers();
  const provider = await reviewProvider(t, env, async turn => {
    if (turn.results.length) {
      readResult.resolve(turn.results[0].content);
      return "Finished.";
    }
    begun.resolve();
    await readNow.promise;
    return [call("hush_review_read", { path: "other.txt" })];
  });
  const backend = await env.boot(provider.model), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  await env.save("other.txt", "DIFFERENT_SNAPSHOT_CONTENT\n");
  const other = await backend.reviewSnapshots.capture(Number(id), { tracked: [], untracked: ["other.txt"] }, ctx);
  const q = query(id, "original-snapshot"), receipt = ok(await start(a, q));
  assert.notEqual(receipt.snapshotId, other.id);
  await begun.promise;
  const taskId = await backend.harness.commit(async tx => {
    const { child, task } = (await tx.doc(ledger, Number(id))).cohorts[0].rows[0];
    const { pin } = await tx.doc(policy, child);
    assert.equal(pin.profile.target.snapshotId, receipt.snapshotId);
    pin.profile.target.snapshotId = other.id;
    return task;
  }, ctx);
  readNow.resolve();
  assert.match(await readResult.promise, /not available in this code review snapshot/i);
  await waitForTaskTerminal(backend, taskId);
  assert.equal((await info(a, q)).workers[0].state, "unresolved");
  assert.deepEqual(await noticeIds(a, id), []);
});

test("inspection cannot return a brief copied from a different assignment", { timeout: 10_000 }, async t => {
  const { backend, a, id, q, receipt } = await heldReviewPair(t);
  await backend.harness.commit(async tx => {
    const [first, second] = (await tx.doc(ledger, Number(id))).cohorts[0].rows;
    second.brief = first.brief;
  }, ctx);
  const { workers } = await info(a, q);
  assert.deepEqual(workers.map(w => w.state), ["reserved", "unresolved"]);
  const inspect = i => a.send("ext.workerActivity.inspect", { threadId: id, handle: receipt.workers[i].handle });
  deny(await inspect(1), "invalid_request");
  assert.equal(ok(await inspect(0)).brief, q.assignments[0].brief);
});

for (const missing of [false, true]) test(`a premature open checkpoint cannot bypass a ${missing ? "missing" : "live"} predecessor`, { timeout: 10_000 }, async t => {
  const env = await layout(t), firstStarted = env.hold(), finishFirst = env.hold(), parked = Promise.withResolvers();
  const provider = await reviewProvider(t, env, async (turn, input) => {
    if (turn.results.length) return "Finished.";
    if (input.name === "Fast") { firstStarted.resolve(); await finishFirst.promise; }
    return [call("hush_review_report", { report: `OPEN_GATE_${input.name}_REPORT` })];
  });
  const backend = await env.boot(provider.model), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  await state(a, id);
  const q = query(id, "premature-open", { ...parallel, mode: "sequential" });
  observeParentCommits(t, backend, id, (type, task) => {
    if (type === "task" && atReviewFence(task, q.requestId)) parked.resolve(task.id);
  });
  const receipt = ok(await start(a, q));
  await firstStarted.promise;
  const taskId = await parked.promise, reached = Promise.withResolvers();
  observeParentCommits(t, backend, id, (type, task) => {
    if (type !== "task" || task.id !== taskId) return;
    const { state } = task;
    if (atReviewFence(task, q.requestId) || state.checkpoint?.phase === "submit" || state.status === "terminal") reached.resolve(state);
  });
  await backend.harness.commit(async tx => {
    const { rows } = (await tx.doc(ledger, Number(id))).cohorts[0];
    const task = await tx.task(taskId);
    assert.equal(rows[1].task, taskId);
    if (missing) rows.shift();
    tx.setTask({ ...task, state: { status: "pending", checkpoint: { phase: "open" } } });
  }, ctx);
  backend.harness.resume();
  assert.equal((await reached.promise).status, missing ? "sleeping" : "waiting");
  assert.deepEqual((await backend.harness.commit(tx => tx.scanConversations({ ownerTaskId: taskId }, 2), ctx)).items, []);
  assert.deepEqual(startedBriefs(provider), [q.assignments[0].brief]);
  if (missing) {
    ok(await stop(a, id, receipt.workers[1].handle));
    await waitFor(a, "The worker was stopped.");
    assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["unresolved", "cancelled"]);
  } else {
    finishFirst.resolve();
    await waitFor(a, "OPEN_GATE_Slow_REPORT");
    assert.deepEqual((await info(a, q)).workers.map(w => w.state), ["succeeded", "succeeded"]);
  }
});

for (const duplicate of [false, true]) test(`${duplicate ? "duplicate task candidates cannot free a slot" : "a wrong row pointer cannot hide an orphan task"}`, { timeout: 10_000 }, async t => {
  const env = await layout(t);
  const provider = await reviewProvider(t, env, (turn, input) =>
    turn.results.length ? "Finished." : [call("hush_review_report", { report: `TASK_IDENTITY_${input.name}_REPORT` })]);
  const backend = await env.boot(provider.model);
  let a = await env.peer(1, { workerActivity: [1] });
  const { id } = ok(await a.send("thread.new", {}));
  await state(a, id);
  const one = query(id, "first-task"), two = query(id, "other-task", { assignments: [parallel.assignments[1]] });
  const [first] = ok(await start(a, one)).workers;
  await waitFor(a, "TASK_IDENTITY_First_REPORT");
  const [second] = ok(await start(a, two)).workers;
  await waitFor(a, "TASK_IDENTITY_Slow_REPORT");
  const { originalTask, otherTask } = await backend.harness.commit(async tx => {
    const { cohorts } = await tx.doc(ledger, Number(id));
    const [row] = cohorts[0].rows, { task: otherTask } = cohorts.pop().rows[0];
    const originalTask = row.task;
    if (!duplicate) row.task = otherTask;
    return { originalTask, otherTask };
  }, ctx);
  if (duplicate) {
    await backend.close();
    const sqlite = new DatabaseSync(join(env.config.dataDir, "history.sqlite"));
    try {
      const { input } = JSON.parse(sqlite.prepare("select record from tasks where id=?").get(originalTask).record);
      assert.equal(sqlite.prepare("update tasks set record=json_set(record, '$.input', json(?)) where id=?")
        .run(JSON.stringify(input), otherTask).changes, 1);
    } finally { sqlite.close(); }
    await env.boot(provider.model);
    a = await env.peer(1, { workerActivity: [1] });
    deny(await start(a, query(id, "refill")), "limit");
    deny(await a.send("ext.workerActivity.inspect", { threadId: id, handle: first.handle }), "invalid_request");
    assert.deepEqual(workerStates((await info(a, one)).workers), [{ ...first, state: "unresolved" }]);
  } else {
    deny(await start(a, query(id, "refill")), "invalid_request");
    assert.deepEqual(await rosterWorkers(a, id), [
      { ...first, state: "unresolved", activity: "" },
      { handle: second.handle, name: "Missing review receipt", state: "unresolved", activity: "" },
    ]);
  }
});

test("a surviving parent input prevents replacement after receipt and task loss", { timeout: 12_000 }, async t => {
  const env = await layout(t), prompt = "Launch the reviewer and join its report.";
  const provider = await reviewProvider(t, env, (turn, input) => {
    if (input) return turn.results.length ? "Finished." : [call("hush_review_report", { report: "ORIGINAL_RECEIPT_REPORT" })];
    assert.equal(turn.text, prompt);
    if (!turn.results.length) return [call(spawnName)];
    if (turn.results.length === 1) return [call(joinName, { handles: JSON.parse(turn.results[0].content).workers.map(w => w.handle) })];
    return "Finished the original parent turn.";
  });
  const backend = await env.boot(provider.model), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {})), q = query(id, "lost-admission", { prompt });
  const receipt = ok(await start(a, q));
  const original = ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId }));
  assert.equal(original.status, "done");
  assert.equal((await info(a, q)).workers[0].state, "succeeded");
  const requests = provider.requests.length;
  const taskId = await backend.harness.commit(async tx => {
    const { cohorts } = await tx.doc(ledger, Number(id));
    const [{ task }] = cohorts[0].rows;
    await tx.retireDoc(ledger, Number(id));
    // Bury the surviving request beyond the first history page.
    for (let i = 0; i < 130; i++) await tx.appendEntry(Number(id), { kind: "fixture.history" });
    return task;
  }, ctx);
  await backend.close();
  const sqlite = new DatabaseSync(join(env.config.dataDir, "history.sqlite"));
  try { assert.equal(sqlite.prepare("delete from tasks where id=?").run(taskId).changes, 1); }
  finally { sqlite.close(); }
  const reopened = await env.boot(provider.model), after = await env.peer();
  const entries = await reopened.harness.commit(tx => tx.scanEntries({ conversationId: Number(id), order: "ascending" }, 16), ctx);
  assert.equal(entries.items.filter(e => e.kind === "hush.review.request" && e.data.requestId === q.requestId).length, 1);
  deny(await after.send("ext.delegation.get", { threadId: id, requestId: q.requestId }), "invalid_request");
  for (const retry of [q, { ...q, prompt: undefined }, query(id, "new-key", parallel)]) {
    const rejected = await start(after, retry);
    deny(rejected, "invalid_request");
    assert.match(rejected.error.message, /original review receipt is unresolved/i);
  }
  assert.equal(provider.requests.length, requests);
  assert.equal(entries.items.filter(e => e.kind === notices).length, 1);
});

for (const fault of ["live phase", "restart phase", "restart expiry", "restart deadline"]) test(`worker recovery does not fabricate a timeout: ${fault}`, { timeout: 12_000 }, async t => {
  const restart = fault !== "live phase";
  const env = await layout(t), recorded = env.hold(), finish = env.hold(), report = "ON_TIME_REPORT";
  const provider = await reviewProvider(t, env, async (turn, input) => {
    assert.ok(input);
    if (!turn.results.length) return [call("hush_review_report", { report })];
    assert.match(turn.results[0].content, /Advisory report recorded/);
    recorded.resolve();
    await finish.promise;
    return "Finished before the deadline.";
  });
  const process = restart && await processBackend(env, provider.model, "child-settled");
  const backend = !restart && await env.boot(provider.model);
  const a = await env.peer(), { id } = ok(await a.send("thread.new", {}));
  await state(a, id);
  const q = query(id, "premature-timeout", { workerTimeoutMs: 120_000 });
  ok(await start(a, q));
  await recorded.promise;
  let after = a;
  if (process) {
    finish.resolve();
    assert.deepEqual(await process.exit, [null, "SIGKILL"], process.error());
    const sqlite = new DatabaseSync(join(env.config.dataDir, "history.sqlite"));
    let taskId;
    try {
      const { id, record } = sqlite.prepare(`select id, record from tasks where kind=json_quote('hush.review-worker')`).get();
      taskId = id;
      const { checkpoint } = JSON.parse(record).state;
      assert.equal(checkpoint.phase, "settle");
      assert.ok(checkpoint.deadline > Date.now());
      if (fault === "restart phase") {
        assert.equal(sqlite.prepare(`update tasks set record=json_set(record, '$.state.checkpoint.phase', 'timeout') where id=?`).run(id).changes, 1);
      } else if (fault === "restart deadline") {
        assert.equal(sqlite.prepare(`update tasks set record=json_set(record, '$.state.checkpoint.deadline', ?) where id=?`).run(checkpoint.deadline + 1, id).changes, 1);
      } else {
        assert.equal(sqlite.prepare(`update document_revisions set content=json_set(content, '$.worker.expiredAt', ?)
          where kind='base' and seq=(select max(r.seq) from document_revisions r join documents d on d.id=r.document_id
            where d.kind=json_quote('hush.review-child-progress') and d.owner_id=?)
          and document_id=(select id from documents where kind=json_quote('hush.review-child-progress') and owner_id=?)`)
          .run(checkpoint.deadline, checkpoint.child, checkpoint.child).changes, 1);
      }
    } finally { sqlite.close(); }
    await waitForTaskTerminal(await env.boot(provider.model), taskId);
    after = await env.peer();
  } else {
    const taskId = await backend.harness.commit(async tx => {
      const task = await tx.task((await tx.doc(ledger, Number(id))).cohorts[0].rows[0].task);
      const { checkpoint } = task.state;
      assert.equal(checkpoint.phase, "settle");
      assert.ok(checkpoint.deadline > Date.now());
      tx.setTask({ ...task, state: { ...task.state, checkpoint: { ...checkpoint, phase: "timeout" } } });
      return task.id;
    }, ctx);
    finish.resolve();
    await waitForTaskTerminal(backend, taskId);
  }
  const [worker] = (await info(after, q)).workers;
  const contradictory = fault === "restart expiry" || fault === "restart deadline";
  assert.equal(worker.state, contradictory ? "unresolved" : "succeeded");
  assert.equal(worker.report, contradictory ? undefined : report);
  assert.deepEqual(await noticeIds(after, id), contradictory ? [] : [worker.noticeId]);
  assert.deepEqual(startedBriefs(provider), [q.assignments[0].brief]);
});

for (const legacy of [false, true]) test(legacy ? "an unverifiable legacy timeout stays unresolved on restart" :
  "restart after a committed expiry recovers one timeout without restarting the worker", { timeout: 12_000 }, async t => {
  const env = await layout(t), begun = env.hold(), finish = env.hold();
  const provider = await reviewProvider(t, env, async (_turn, input) => {
    assert.ok(input);
    begun.resolve();
    await finish.promise;
    return "Too late.";
  });
  const crashed = await processBackend(env, provider.model, "timeout-decision"), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {})), q = query(id, "committed-expiry", { workerTimeoutMs: 1_600 });
  const receipt = ok(await start(a, q));
  await begun.promise;
  assert.deepEqual(await crashed.exit, [null, "SIGKILL"], crashed.error());
  finish.resolve();
  if (legacy) {
    const sqlite = new DatabaseSync(join(env.config.dataDir, "history.sqlite"));
    try {
      const { id, record } = sqlite.prepare(`select id, record from tasks where kind=json_quote('hush.review-worker')`).get();
      const { child } = JSON.parse(record).state.checkpoint;
      const { id: doc } = sqlite.prepare(`select id from documents where kind=json_quote('hush.review-child-progress') and owner_id=?`).get(child);
      sqlite.exec("begin");
      assert.equal(sqlite.prepare(`update tasks set record=json_remove(record, '$.state.checkpoint.expiredAt') where id=?`).run(id).changes, 1);
      assert.equal(sqlite.prepare(`update entries set record=json_remove(record, '$.data.progress') where conversation_id=?
        and json_extract(record, '$.kind')='hush.review.assignment'`).run(child).changes, 1);
      assert.ok(sqlite.prepare("delete from document_revisions where document_id=?").run(doc).changes);
      assert.equal(sqlite.prepare("delete from documents where id=?").run(doc).changes, 1);
      sqlite.exec("commit");
    } finally { sqlite.close(); }
  }
  const backend = await env.boot(provider.model), after = await env.peer();
  const taskId = (await backend.harness.snapshot(ledger, Number(id), ctx)).cohorts[0].rows[0].task;
  await waitForTaskTerminal(backend, taskId);
  assert.deepEqual(ok(await start(after, q)), receipt);
  const [worker] = (await info(after, q)).workers;
  assert.equal(worker.state, legacy ? "unresolved" : "timed_out");
  assert.deepEqual(await noticeIds(after, id), legacy ? [] : [worker.noticeId]);
  assert.deepEqual(startedBriefs(provider), [q.assignments[0].brief]);
});

for (const changeDeadline of [false, true]) test(changeDeadline ? "a changed checkpoint cannot publish a timeout after drain" :
  "Stop during drain preserves a previously committed worker expiry", { timeout: 10_000 }, async t => {
  const env = await layout(t), begun = env.hold(), finish = env.hold(), expired = Promise.withResolvers(), draining = Promise.withResolvers();
  const backend = await env.boot(), faux = fauxProvider();
  faux.setResponses([async () => { begun.resolve(); await finish.promise; return fauxAssistantMessage("Too late."); }]);
  backend.runtime.setProvider(faux.provider);
  const a = await env.peer();
  const { id } = ok(await a.send("thread.new", { model: { provider: "faux", modelId: "faux-1" } }));
  let child;
  const observation = backend.harness.observeCommits({ advance({ changes }) {
    for (const { type, value: task } of changes) {
      if (type !== "task") continue;
      const checkpoint = task.state.checkpoint;
      if (task.conversationId === Number(id) && task.kind === "hush.review-worker" && checkpoint?.phase === "timeout" &&
          checkpoint.expiredAt !== undefined) {
        child = checkpoint.child;
        expired.resolve(task.id);
      }
      if (task.conversationId === child && task.abortRequested) draining.resolve();
    }
  }, end() {} });
  t.after(() => observation.detach());
  await state(a, id);
  const q = query(id, "stop-after-expiry", { workerTimeoutMs: 1_200 }), receipt = ok(await start(a, q));
  await begun.promise;
  const taskId = await expired.promise;
  if (changeDeadline) {
    await draining.promise;
    await backend.harness.commit(async tx => {
      const task = await tx.task(taskId), { checkpoint } = task.state;
      assert.equal(task.state.status, "running");
      assert.equal(checkpoint.phase, "timeout");
      tx.setTask({ ...task, state: { ...task.state, checkpoint: { ...checkpoint, deadline: checkpoint.deadline + 1 } } });
    }, ctx);
  } else {
    const worker = ok(await stop(a, id, receipt.workers[0].handle)).worker;
    assert.equal(worker.state, "stopping");
    assert.equal(worker.noticeId, undefined);
    assert.equal((await backend.harness.getTask(taskId, ctx)).abortRequested, true);
  }
  finish.resolve();
  await waitForTaskTerminal(backend, taskId);
  const [settled] = (await info(a, q)).workers;
  assert.equal(settled.state, changeDeadline ? "unresolved" : "timed_out");
  assert.deepEqual(await noticeIds(a, id), changeDeadline ? [] : [settled.noticeId]);
});

test("a copied report body cannot be published for the wrong assignment", { timeout: 12_000 }, async t => {
  const env = await layout(t), bothReports = Promise.withResolvers(), finish = env.hold(), recorded = new Set();
  const provider = await reviewProvider(t, env, async (turn, input) => {
    assert.ok(input);
    if (!turn.results.length) return [call("hush_review_report", { summary: `Review by ${input.name}`, findings: [], checks: [], limitations: ["Scripted fixture only."] })];
    assert.match(turn.results.at(-1).content, /Advisory report recorded/);
    recorded.add(input.name);
    if (recorded.size === 2) bothReports.resolve();
    await finish.promise;
    return "Finished.";
  });
  const backend = await env.boot(provider.model), a = await env.peer(1, { reviewEvidence: [1] });
  const { id } = ok(await a.send("thread.new", {}));
  const q = query(id, "copied-report", { ...parallel, assessment: { checks: [{ id: "scope", description: "Check the original review scope" }] } });
  ok(await start(a, q));
  await bothReports.promise;
  const tasks = await backend.harness.commit(async tx => {
    const [first, second] = (await tx.doc(ledger, Number(id))).cohorts[0].rows;
    assert.match(first.report, /Review by Fast/);
    assert.match(second.report, /Review by Slow/);
    assert.notEqual(first.reportKey, second.reportKey);
    first.report = second.report;
    return [first.task, second.task];
  }, ctx);
  finish.resolve();
  await Promise.all(tasks.map(id => waitForTaskTerminal(backend, id)));
  const [first, second] = (await info(a, q)).workers;
  assert.equal(first.state, "unresolved");
  assert.equal(first.report, undefined);
  assert.equal(first.noticeId, undefined);
  assert.equal(second.state, "succeeded");
  assert.match(second.report, /Review by Slow/);
  assert.deepEqual(await noticeIds(a, id), [second.noticeId]);
});

test("a held worker cannot start or be joined on the strength of a release flag alone", { timeout: 12_000 }, async t => {
  const env = await layout(t), tryJoin = env.hold(), parked = Promise.withResolvers();
  let handle, result;
  const provider = await reviewProvider(t, env, async (turn, input) => {
    if (input) return "The held worker must not start.";
    if (!turn.results.length) { await tryJoin.promise; return [call(joinName, { handles: [handle] })]; }
    result = turn.results[0].content;
    return "Done without releasing the worker.";
  });
  const backend = await env.boot(provider.model), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {})), owner = Number(id);
  const q = query(id, "forged-release", { prompt: "Wait without launching a reviewer." });
  observeParentCommits(t, backend, id, (type, task) => {
    if (type === "task" && atReviewFence(task, q.requestId)) parked.resolve(task.id);
  });
  const receipt = ok(await start(a, q));
  handle = receipt.workers[0].handle;
  const taskId = await parked.promise;
  await backend.harness.commit(async tx => {
    const [cohort] = (await tx.doc(ledger, owner)).cohorts;
    assert.equal(cohort.released, false);
    cohort.released = true;
  }, ctx);
  tryJoin.resolve();
  assert.equal(ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId })).status, "done");
  assert.match(result, /Only launched review handles/);
  // Model the launch timer waking the held gate without a release signal.
  await backend.harness.commit(async tx => {
    const task = await tx.task(taskId);
    tx.setTask({ ...task, state: { status: "pending", checkpoint: task.state.checkpoint, wake: { reason: "timer" } } });
  }, ctx);
  backend.harness.resume();
  await waitForTaskTerminal(backend, taskId);
  assert.equal((await info(a, q)).workers[0].state, "unresolved");
  assert.deepEqual((await backend.harness.commit(tx => tx.scanConversations({ ownerTaskId: taskId }, 2), ctx)).items, []);
  assert.deepEqual(startedBriefs(provider), []);
  assert.deepEqual(await noticeIds(a, id), []);
});

for (const boundary of ["reservation", "release-decision"]) test(`restart at ${boundary} keeps the original release decision`, { timeout: 12_000 }, async t => {
  const env = await layout(t);
  const provider = await reviewProvider(t, env, (turn, input) => {
    assert.equal(input, undefined, "the pre-crash worker was not replayed");
    return turn.results.length ? "Finished without replay." : [call(spawnName)];
  });
  const crashed = await processBackend(env, provider.model, boundary), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {})), q = query(id, `crash-${boundary}`, { prompt: "Release the reserved reviewer." });
  const original = start(a, q).catch(() => null);
  assert.deepEqual(await crashed.exit, [null, "SIGKILL"], crashed.error());
  const observed = await original;
  const backend = await env.boot(provider.model), after = await env.peer();
  const receipt = ok(await start(after, q));
  if (observed) assert.deepEqual(receipt, ok(observed));
  const owner = Number(id), decision = (await backend.harness.snapshot(releases, owner, ctx))?.byHandle[receipt.workers[0].handle];
  if (boundary === "reservation") assert.equal(decision, undefined);
  else assert.match(decision, /^[a-f0-9]{64}$/);
  const [cohort] = (await backend.harness.snapshot(ledger, owner, ctx)).cohorts;
  await waitForTaskTerminal(backend, cohort.rows[0].task);
  assert.deepEqual(startedBriefs(provider), []);
  assert.equal((await info(after, q)).workers[0].state, "failed");
  assert.ok((await noticeIds(after, id)).length <= 1);
});

test("a plan task label cannot expose an owned code reviewer", { timeout: 12_000 }, async t => {
  const env = await layout(t);
  const provider = await reviewProvider(t, env, turn =>
    turn.results.length ? "Finished." : [call("hush_review_report", { report: "PRIVATE_REVIEW" })]);
  const backend = await env.boot(provider.model), a = await env.peer();
  const { id } = ok(await a.send("thread.new", {}));
  await state(a, id);
  ok(await start(a, query(id, "private-kind")));
  await waitFor(a, "PRIVATE_REVIEW");
  const { child, task } = (await backend.harness.snapshot(ledger, Number(id), ctx)).cohorts[0].rows[0];
  await backend.close();
  const sqlite = new DatabaseSync(join(env.config.dataDir, "history.sqlite"));
  try {
    assert.equal(sqlite.prepare(`update tasks set kind=json_quote('hush.plan-review'), record=json_set(record, '$.kind', 'hush.plan-review') where id=?`)
      .run(task).changes, 1);
  } finally { sqlite.close(); }
  await env.boot(provider.model);
  const after = await env.peer(), threadId = String(child);
  deny(await after.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 10 }), "not_found");
  assert.ok(!(ok(await after.send("thread.list", { limit: 100 })).threads.some(row => row.id === threadId)));
});
