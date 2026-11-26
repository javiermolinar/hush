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
  assert.match(single.outcomes[0].detail, /reservation is still held/);
  finishSlow.resolve();
  assert.equal(ok(await a.send("submission.wait", { threadId: id, submissionId: receipt.submissionId })).status, "done");
  assert.equal(both.wait, "unresolved");
  assert.deepEqual(both.outcomes.map(w => [w.handle, w.state, w.report]), [
    [handles[0], "unresolved", undefined], [handles[1], "succeeded", "SLOW_REPORT"],
  ]);
  assert.ok(both.outcomes.every(w => w.snapshotId === receipt.snapshotId));
  assert.equal((await history(a, id)).entries.filter(e => e.kind === notices).length, 2);
  deny(await start(a, query(id, "must-not-refill", parallel)), "limit");
  assert.deepEqual(provider.requests.filter(r => isBrief(r.text)).map(r => brief(r.text).name).sort(), ["Fast", "Fast", "Slow", "Slow"]);
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
  const policy = defineDoc({ kind: "hush.assigned-tool-policy", version: 1, scope: "conversation", history: "latest", fork: "initial",
    initial: () => ({}), checkpointWhen: () => true });
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
  const provider = await reviewProvider(t, env, async turn => {
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
  deny(await start(after, query(id, "must-not-refill", parallel)), "limit");
  assert.equal(provider.requests.length, 2, "the original child was not replayed");
});

test("restart does not settle a held worker with an impossible open checkpoint", { timeout: 12_000 }, async t => {
  const env = await layout(t);
  const provider = await reviewProvider(t, env, (_turn, input) => {
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
  deny(await start(after, query(id, "must-not-refill", parallel)), "limit");
  assert.equal(provider.requests.length, 1, "only the original parent turn resumed");
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
    assert.equal(ok(await after.send("ext.workerActivity.inspect", ref)).worker.state, "unresolved");
    deny(await after.send("ext.workerActivity.inspect", { ...ref, transcript: true }), "invalid_request");
  }
  for (const _ of [1, 2]) {
    const v = await info(after, q);
    assert.ok(v.workers.every(w => w.state === "unresolved" && !w.report && !w.noticeId));
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
