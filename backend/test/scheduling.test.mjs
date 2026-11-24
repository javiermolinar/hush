import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { dial } from "../../server/test/client.mjs";

const bundle = fileURLToPath(new URL("../../dist/hush-backend.mjs", import.meta.url));
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };

test("durable occurrence: deadlines, exact prerequisite, user decisions, and atomic receipts/reports", { timeout: 35_000 }, async () => {
  const home = await mkdtemp("/tmp/hush-schedule-test-");
  let host, exit, peer, other, stderr, socket;
  const offer = () => ({ versions: [1], capabilities: { schedules: [1], threadDirectory: [2] } });
  const boot = async () => {
    stderr = "";
    host = spawn(process.execPath, [bundle, "--faux", "--schedule-late-ms", "1100"], {
      cwd: home, env: { HOME: home, PATH: process.env.PATH, NODE_NO_WARNINGS: "1" },
      stdio: ["ignore", "pipe", "pipe"],
    });
    host.stderr.setEncoding("utf8").on("data", text => { stderr += text; });
    exit = once(host, "exit");
    exit.catch(() => {});
    const lines = createInterface({ input: host.stdout });
    const line = await Promise.race([once(lines, "line"), exit.then(() => { throw new Error(stderr); })]);
    lines.close();
    ({ socket } = JSON.parse(line[0]));
    peer = await dial(socket);
    other = await dial(socket);
    const hello = ok(await peer.send("hello", offer()));
    assert.equal(hello.capabilities.schedules, 1);
    // Omission really gates actions, even on the same host.
    ok(await other.send("hello", { versions: [1] }));
    assert.equal((await other.send("ext.schedules.list", { threadId: "1" })).error.code, "unsupported_capability");
    other.close();
    other = await dial(socket);
    ok(await other.send("hello", { versions: [1], capabilities: { schedules: [1] } }));
  };
  const stop = async (signal = "SIGTERM") => {
    peer?.close();
    other?.close();
    if (host && host.exitCode === null && host.signalCode === null) host.kill(signal);
    if (exit) assert.deepEqual(await exit, signal === "SIGKILL" ? [null, "SIGKILL"] : [0, null], stderr);
    host = exit = undefined;
  };
  const call = async (method, params, w = peer) => ok(await w.send(method, params));
  const schedule = async (op, params, w = peer) => call("ext.schedules." + op, params, w);
  let threadId, targetThreadId, depThread;
  const newThread = async () => (await call("thread.new", {})).id;
  const submit = async (threadId, text, requestId) => (await call("submit", { threadId, text, requestId, whenBusy: "reject" })).submissionId;
  const add = async (text, dueMs, extra = {}) => schedule("add", { threadId, targetThreadId, dueMs, zone: "+01:00", text, ...extra });
  const change = (op, id, fields = {}, w = peer) => schedule(op, { threadId, id, ...fields }, w);
  const row = async id => (await schedule("list", { threadId })).items.find(item => item.id === id);
  const until = async (get, msg) => {
    for (const deadline = Date.now() + 6_000; Date.now() < deadline; await delay(25)) {
      const out = await get();
      if (out) return out;
    }
    assert.fail(msg + ": " + JSON.stringify(await schedule("list", { threadId })) + " stderr=" + stderr);
  };
  const status = async (item, state) => until(async () => {
    const v = await row(item.id);
    return v?.state === state && v;
  }, `${item.id} did not reach ${state}`);
  const history = async threadId => (await call("history", { threadId, limit: 100, range: { kind: "before", beforeEntryId: null } })).entries;
  const content = entry => entry.messages.flatMap(m => m.blocks.map(b => b.text ?? "")).join("\n");
  const reports = async item => (await history(threadId)).filter(e => e.kind === "hush.schedule-report" && content(e).includes(item.id));
  const verify = async (item, text) => {
    const matching = (await history(targetThreadId)).filter(e => e.kind === "pi.user" && content(e) === text);
    assert.equal(matching.length, 1, text);
    const msgs = await reports(item);
    assert.equal(msgs.length, 1);
    assert.match(content(msgs[0]), new RegExp("Faux: " + text));
    assert.equal(msgs[0].id, (await row(item.id)).reportEntryId);
  };
  try {
    await boot();
    threadId = await newThread(); targetThreadId = await newThread(); depThread = await newThread();
    // Ordinary target idleness is not a substitute for the named prerequisite.
    const delayed = await submit(depThread, "!sleep 1800", "delay-dep");
    const due = Date.now() + 150;
    const dependent = await add("uses delayed settlement", due, { prerequisite: { threadId: depThread, submissionId: delayed } });
    await delay(1300);
    assert.equal((await row(dependent.id)).state, "pending");
    const settled = await status(dependent, "done");
    assert.ok(settled.eligibleMs > due + 1100, JSON.stringify(settled));
    await verify(dependent, "uses delayed settlement");

    const failed = await submit(depThread, "!error", "bad-dep");
    const stopped = await submit(targetThreadId, "!pause", "stop-dep");
    await call("thread.abort", { threadId: targetThreadId });
    for (const [dep, submissionId] of [[depThread, failed], [targetThreadId, stopped], [depThread, "99999999"], [targetThreadId, delayed]]) {
      const item = await add("must never dispatch", Date.now(), { prerequisite: { threadId: dep, submissionId } });
      const result = await status(item, "failed");
      assert.equal(result.reason, "prerequisite_failed");
      assert.equal(result.submissionId, undefined);
      assert.equal((await reports(item)).length, 1);
    }

    const future = await add("reopen and deliver", Date.now() + 1200);
    assert.equal((await row(future.id)).state, "pending");
    await stop("SIGKILL"); await boot();
    await status(future, "done");
    await verify(future, "reopen and deliver");
    const unattended = await add("without observers", Date.now() + 450);
    peer.close(); other.close();
    await delay(700);
    peer = await dial(socket); other = await dial(socket);
    ok(await peer.send("hello", offer()));
    ok(await other.send("hello", offer()));
    assert.equal((await row(unattended.id)).state, "done");
    await verify(unattended, "without observers");

    await call("ext.threadDirectory.archive", { threadId: targetThreadId, archived: true });
    const archived = await add("archived target", Date.now());
    assert.equal((await status(archived, "needs_attention")).reason, "target_archived");
    assert.equal((await status(archived, "needs_attention")).targetThreadId, targetThreadId);
    await change("run", archived.id);
    assert.equal((await status(archived, "needs_attention")).reason, "target_archived");
    await change("cancel", archived.id);
    await call("ext.threadDirectory.archive", { threadId: targetThreadId, archived: false });

    const late = await add("run after explicit decision", Date.now() - 10_000);
    assert.equal((await status(late, "needs_attention")).reason, "late");
    await stop(); await boot();
    assert.equal((await row(late.id)).state, "needs_attention");
    await change("run", late.id);
    await status(late, "done");
    await verify(late, "run after explicit decision");
    const move = await add("rescheduling uses the new instant", Date.now() - 10_000);
    await status(move, "needs_attention");
    const revised = await change("reschedule", move.id, { dueMs: Date.now() + 300, zone: "UTC" });
    assert.equal(revised.targetThreadId, targetThreadId);
    assert.equal(revised.zone, "UTC");
    await status(move, "done");
    const permitted = await add("allows late admission", Date.now() - 10_000, { allowLate: true });
    await status(permitted, "done");

    const busy = await submit(targetThreadId, "!sleep 4000", "unrelated-target-input");
    // Kill at a committed sleep, not while an unsafe tool is still entering
    // its first invocation. The latter correctly leaves a held follow-up
    // after interruption under Durable's existing afterFailedRun policy.
    await until(async () => (await call("ext.threadDirectory.list", { limit: 100 })).threads
      .some(t => t.id === targetThreadId && t.status === "parked"), "target tool did not durably park");
    const queued = await add("one queued followup", Date.now());
    const admitted = await status(queued, "admitted");
    const snap = await call("subscribe", { threadId: targetThreadId });
    assert.ok(snap.state.queue.some(q => q.submissionId === admitted.submissionId && q.mode === "followUp"));
    const losing = await change("cancel", queued.id, {}, other);
    assert.equal(losing.submissionId, admitted.submissionId);
    assert.equal((await call("submission.get", { threadId: targetThreadId, submissionId: busy })).status, "placed");
    await stop("SIGKILL"); await boot();
    assert.equal((await row(queued.id)).submissionId, admitted.submissionId);
    await status(queued, "done");
    await verify(queued, "one queued followup");
    await stop("SIGKILL"); await boot();
    await verify(queued, "one queued followup");

    const race = await add("race vs cancel", Date.now() + 180);
    const [_, vote] = await Promise.all([
      until(async () => Date.now() >= race.dueMs, "deadline"),
      delay(Math.max(0, race.dueMs - Date.now())).then(() => change("cancel", race.id, {}, other)),
    ]);
    if (vote.state === "cancelled") {
      assert.equal((await history(targetThreadId)).some(e => content(e) === "race vs cancel"), false);
    } else {
      assert.ok(vote.submissionId, JSON.stringify(vote));
      await status(race, "done");
      await verify(race, "race vs cancel");
    }
    assert.equal((await reports(race)).length, 1);
    assert.equal((await history(targetThreadId)).some(e => content(e) === "must never dispatch"), false);
    assert.equal((await history(threadId)).some(e => e.kind === "pi.user"), false);
    await stop();
  } finally {
    if (host) await stop().catch(() => {});
    await rm(home, { recursive: true, force: true });
  }
});
