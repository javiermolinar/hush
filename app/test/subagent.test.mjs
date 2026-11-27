import assert from "node:assert/strict";
import { execFileSync, execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { call, planProvider } from "../../backend/test/plan-provider.mjs";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { startBackend } from "../../dist/hush-backend.mjs";
import { dial } from "../../server/test/client.mjs";

const exec = promisify(execFile);
const binary = process.env.HUSH_TEST_CLI || fileURLToPath(new URL("../../bin/hush", import.meta.url));
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };

async function fixture(t, answer) {
  const dir = await realpath(await mkdtemp("/tmp/hush-cli-subagent-"));
  const repo = join(dir, "repo"), cwd = join(repo, "src"), target = join(dir, "retry"), untrusted = join(dir, "untrusted");
  const agentDir = join(dir, ".hush"), dataDir = join(dir, "db"), socket = join(dataDir, "hush.sock");
  const env = { HOME: dir, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1" };
  const clients = new Set();
  let backend;
  t.after(async () => {
    for (const client of clients) client.close();
    await backend?.close();
    await rm(dir, { force: true, recursive: true });
  });
  await mkdir(cwd, { recursive: true }); await mkdir(agentDir, { mode: 0o700 });
  const git = (...args) => execFileSync("git", args, { cwd: repo, env, stdio: "pipe" });
  git("init", "-q"); git("config", "user.email", "fixture@example.test"); git("config", "user.name", "Fixture");
  await writeFile(join(cwd, "retry.txt"), "three attempts\n");
  await writeFile(join(repo, "retry.txt"), "original\n");
  git("add", "."); git("commit", "-qm", "base");
  git("worktree", "add", "-qb", "fix/retry", target);
  git("worktree", "add", "-qb", "untrusted", untrusted);
  await writeFile(join(target, "retry.txt"), "existing uncommitted work\n");
  const trust = new WorkspaceTrust(agentDir);
  for (const path of [cwd, target]) await trust.change(path, "grant", t.signal);
  const provider = await planProvider(t, agentDir, answer, { usage: { prompt_tokens: 100, completion_tokens: 20, total_tokens: 120 } });
  const config = { cwd, agentDir, dataDir, socket, model: provider.model, ephemeral: false, faux: false, idleMs: 60_000 };
  backend = await startBackend(config);
  const connect = async (capabilities = { subagents: [1] }) => {
    const peer = await dial(socket);
    clients.add(peer);
    const hello = ok(await peer.send("hello", { versions: [1], capabilities }));
    assert.equal(hello.capabilities.subagents, capabilities.subagents?.[0]);
    return peer;
  };
  const cli = (...args) => exec(binary, ["--socket", socket, ...args], { cwd, env, signal: t.signal, timeout: 8_000 });
  const thread = JSON.parse((await cli("thread", "new", "--json", "--cwd", cwd)).stdout);
  const submit = async (requestId, text) => {
    const { submissionId } = JSON.parse((await cli("--no-wait", "--json", "--thread", thread.id, "--request-id", requestId, text)).stdout);
    return { threadId: thread.id, submissionId };
  };
  const restart = async () => {
    for (const peer of clients) peer.close();
    clients.clear();
    await backend.close();
    backend = await startBackend(config);
  };
  return { backend, cwd, target, untrusted, repo, trust, connect, cli, thread, submit, restart };
}

async function until(peer, query, predicate, after) {
  for (let i = 0; i < 64; i++) {
    const snapshot = ok(await peer.send("ext.subagents.wait", { ...query, ...(after && { after }) }));
    if (predicate(snapshot)) return snapshot;
    assert.equal(snapshot.settled, false, JSON.stringify(snapshot));
    after = snapshot.cursor;
  }
  assert.fail("subagent stream did not reach expected state");
}

test("an ordinary CLI prompt streams backend subagents and uses their results without launching a TUI", { timeout: 18_000 }, async t => {
  const prompt = "Ask a read-only subagent to inspect the retry behavior, then have another update the retry text in the fix/retry worktree and summarize both results.";
  const inspect = "Inspect retry.txt without touching it.", fix = "Update retry.txt in the selected checkout.";
  const release = Promise.withResolvers(), beginText = Promise.withResolvers();
  t.after(() => { release.resolve(); beginText.resolve(); });
  const parentCalls = [];
  const f = await fixture(t, async turn => {
    if (turn.text === inspect) {
      assert.deepEqual(turn.tools, ["read"]);
      assert.equal(turn.messages.filter(msg => msg.role === "user").length, 1);
      assert.doesNotMatch(turn.prompt, /Use subagent for a focused task/);
      if (turn.results.length === 0) return [call("write", { path: "forbidden.txt", content: "This must not be written." })];
      if (turn.results.length === 1) {
        assert.match(turn.results[0].content, /not (offered|available)|unavailable/i);
        return [call("read", { path: "retry.txt" })];
      }
      assert.match(turn.results[1].content, /three attempts/);
      await beginText.promise;
      return (async function* () {
        yield "Read-only inspection: ";
        await release.promise;
        yield "the retry limit is three.";
      })();
    }
    if (turn.text === fix) {
      assert.deepEqual(turn.tools.toSorted(), ["bash", "edit", "read", "write"]);
      assert.equal(turn.messages.filter(msg => msg.role === "user").length, 1);
      if (turn.results.length === 0) return [call("read", { path: "retry.txt" })];
      if (turn.results.length === 1) {
        assert.match(turn.results[0].content, /existing uncommitted work/);
        return [call("write", { path: "retry.txt", content: "updated by write tool\n" })];
      }
      assert.match(turn.results[1].content, /wrote|written/i);
      return "Updated the selected worktree.";
    }
    assert.equal(turn.text, prompt);
    assert.ok(turn.tools.includes("subagent"));
    assert.match(turn.prompt, /Use subagent for a focused task/);
    parentCalls.push(turn.results.length);
    switch (turn.results.length) {
      case 0: return [call("subagent", { prompt: inspect, read_only: true })];
      case 1:
        assert.equal(turn.results[0].content, "Read-only inspection: the retry limit is three.");
        return [call("subagent", { prompt: fix, read_only: false, worktree: "fix/retry" })];
      case 2:
        assert.equal(turn.results[1].content, "Updated the selected worktree.");
        return [call("subagent", { prompt: "Never launch in an untrusted worktree.", read_only: false, worktree: "untrusted" })];
      default:
        assert.equal(turn.results.length, 3);
        assert.match(turn.results[2].content, /trusted worktree/);
        return "The retry limit was three; the fix is in fix/retry.";
    }
  });
  const q = await f.submit("subagent-e2e", prompt);
  const overview = await f.connect({ subagents: [2] });
  const watch = async (check, after) => {
    for (let i = 0; i < 50; i++) {
      const snap = ok(await overview.send("ext.subagents.overview", { threadId: f.thread.id, ...(after && { after }) }));
      if (check(snap)) return snap;
      after = snap.cursor;
    }
    assert.fail("overview did not reach expected state");
  };
  const waiting = await watch(s => s.usage.totalTokens === 240 && s.workers[0]?.status === "waiting");
  assert.equal(waiting.workers[0].text, "", "waiting is visible before the worker writes public text");
  beginText.resolve();
  const streaming = await watch(s => s.workers.some(w => w.status === "responding"), waiting.cursor);
  assert.equal(streaming.workers[0].text, "Read-only inspection: ");
  assert.equal(streaming.usage.totalTokens, 240, "only the child's two completed model calls count");
  assert.ok(streaming.usage.costUsd > 0);
  const initial = await f.connect();
  const partial = await until(initial, q, snap => snap.events.some(e => e.type === "text" && e.text === "Read-only inspection: "));
  const workerId = partial.events[0].workerId;
  assert.deepEqual(partial.events[0], { type: "started", workerId, cwd: f.cwd, read_only: true });
  assert.equal(partial.events.some(e => e.type === "finished"), false);
  assert.deepEqual(parentCalls, [0], "parent has not resumed before the worker finished");
  initial.close();
  const peer = await f.connect();
  assert.deepEqual(ok(await peer.send("ext.subagents.wait", q)).events, partial.events);
  release.resolve();
  const done = await until(peer, q, snap => snap.settled, partial.cursor);
  assert.deepEqual(done.events.map(e => e.type), ["started", "text", "finished", "started", "text", "finished"]);
  assert.deepEqual(done.events.filter(e => e.type === "finished").map(e => e.status), ["ok", "ok"]);
  assert.deepEqual(done.events[3], { type: "started", workerId: done.events[3].workerId, cwd: f.target, read_only: false });
  assert.equal(done.events[4].text, "Updated the selected worktree.", "raw read/write tool outputs are not in the stream");
  assert.deepEqual(parentCalls, [0, 1, 2, 3]);
  assert.equal(await readFile(join(f.target, "retry.txt"), "utf8"), "updated by write tool\n");
  assert.equal(await readFile(join(f.repo, "retry.txt"), "utf8"), "original\n");
  assert.equal(await readFile(join(f.untrusted, "retry.txt"), "utf8"), "original\n");
  await assert.rejects(readFile(join(f.cwd, "forbidden.txt")), { code: "ENOENT" });
  const children = await f.backend.harness.commit(async tx => (await tx.scanConversations({ ownerTaskId: Number(workerId) }, 2)).items, BACKGROUND_CONTEXT);
  assert.equal(children.length, 1);
  assert.equal(children[0].parent, undefined, "the subagent is a fresh backend thread, not a transcript fork");
  const childID = String(children[0].id);
  const privateRead = await peer.send("subscribe", { threadId: childID });
  assert.equal(privateRead.error?.code, "not_found");
  const catalog = await f.connect({ modelCatalog: [2] });
  assert.equal((await catalog.send("ext.modelCatalog.list", { threadId: childID })).error?.code, "not_found");
  assert.equal((await catalog.send("ext.modelCatalog.list", { threadId: childID, query: "chat" })).error?.code, "not_found");
  ok(await catalog.send("ext.modelCatalog.list", { threadId: q.threadId }));
  const legacy = await f.connect({});
  assert.equal((await legacy.send("ext.subagents.wait", q)).error?.code, "unsupported_capability");
  const answer = await f.cli("--print", "--thread", f.thread.id, "--request-id", "subagent-e2e", prompt);
  assert.equal(answer.stdout, "The retry limit was three; the fix is in fix/retry.\n");
  const { usage, workers } = await watch(s => s.workers.length === 2 && s.workers.every(w => w.status === "ok"));
  assert.deepEqual(workers.map(w => w.usage.totalTokens), [360, 360]);
  assert.equal(usage.totalTokens, 720);
  assert.ok(Math.abs(usage.costUsd - 0.00168) < 1e-10);
  const again = await f.connect({ subagents: [2] });
  assert.deepEqual(ok(await again.send("ext.subagents.overview", { threadId: f.thread.id })).usage, usage);
  const parent = ok(await again.send("subscribe", { threadId: f.thread.id })).state;
  assert.equal(parent.usage.totalTokens, 480, "core usage remains parent-only");
  assert.deepEqual(parentCalls, [0, 1, 2, 3], "reading the same receipt did not replay work");
});

test("the sixth call can't start a worker, and aborting stops running and queued workers", { timeout: 18_000 }, async t => {
  const countPrompt = "Try to start six little tasks, then report the limit.", stopPrompt = "Start a long subagent task.";
  const held = "Wait on the fixture.", release = Promise.withResolvers();
  t.after(() => release.resolve());
  const children = [];
  let writeStarts = 0;
  const f = await fixture(t, turn => {
    if (turn.text === countPrompt) {
      const n = turn.results.length;
      if (n < 6) return [call("subagent", { prompt: `Read task ${n + 1}`, read_only: true })];
      assert.deepEqual(turn.results.slice(0, 5).map(r => r.content), Array.from({ length: 5 }, (_, i) => `Finished task ${i + 1}.`));
      assert.match(turn.results[5].content, /five per user turn/);
      return "Five tasks finished; the sixth hit the limit.";
    }
    if (turn.text === stopPrompt) {
      assert.equal(turn.results.length, 0, "an aborted parent doesn't resume the turn");
      return [1, 2].map(() => call("subagent", { prompt: held, read_only: false }));
    }
    if (turn.text === held) {
      if (turn.results.length === 0) {
        writeStarts++;
        return [call("read", { path: "retry.txt" })];
      }
      return (async function* () {
        yield "Waiting for the parent to stop me.";
        await release.promise;
        yield "Unexpected continuation.";
      })();
    }
    const task = /^Read task ([1-5])$/.exec(turn.text);
    assert.ok(task, turn.text);
    assert.ok(!turn.tools.includes("subagent"));
    if (task[1] === "1" && !turn.results.length) return [call("subagent", { prompt: "Attempt to nest", read_only: true })];
    if (task[1] === "1") {
      assert.match(turn.results[0].content, /not (offered|available)|unavailable/i);
      if (turn.results.length > 1) assert.match(turn.results.at(-1).content, /three attempts/);
      // This worker must finish even after exceeding the old 16-round cap.
      if (turn.results.length <= 17) return [call("read", { path: "retry.txt" })];
    }
    children.push(task[1]);
    return `Finished task ${task[1]}.`;
  });
  const peer = await f.connect();
  const count = await until(peer, await f.submit("count", countPrompt), s => s.settled);
  assert.equal(count.events.filter(e => e.type === "started").length, 5);
  assert.deepEqual(count.events.filter(e => e.type === "finished").map(e => e.status), Array(5).fill("ok"));
  assert.deepEqual(children, ["1", "2", "3", "4", "5"]);
  const overview = await f.connect({ subagents: [2] });
  const prior = ok(await overview.send("ext.subagents.overview", { threadId: f.thread.id }));
  assert.equal(prior.usage.totalTokens, 23 * 120);
  const q = await f.submit("stop", stopPrompt);
  const running = await until(peer, q, s => s.events.filter(e => e.type === "started").length === 2 && s.events.some(e => e.type === "text"));
  const worker = await f.backend.harness.getTask(Number(running.events.find(e => e.type === "text").workerId), BACKGROUND_CONTEXT);
  assert.equal(worker.state.status, "sleeping");
  assert.equal(worker.state.until, undefined, "the worker waits for settlement without a deadline");
  ok(await peer.send("thread.abort", { threadId: f.thread.id }));
  const stopped = await until(peer, q, s => s.settled, running.cursor);
  assert.deepEqual(stopped.events.filter(e => e.type === "finished").map(e => e.status), ["cancelled", "cancelled"]);
  assert.equal(writeStarts, 1, "the queued worker never reached the model");
  assert.equal(stopped.events.find(e => e.type === "text")?.text, "Waiting for the parent to stop me.");
  const view = ok(await overview.send("ext.subagents.overview", { threadId: f.thread.id, after: prior.cursor }));
  assert.deepEqual(view.workers.map(w => [w.status, w.text, w.usage.totalTokens]), [
    ["cancelled", "Waiting for the parent to stop me.", 120], ["cancelled", "", 0],
  ]);
  assert.equal(view.usage.totalTokens, (19 + 4 + 1) * 120, "earlier turns and stopped work both count");
  const receipt = ok(await peer.send("submission.get", { threadId: q.threadId, submissionId: q.submissionId }));
  assert.equal(receipt.status, "unanswered");
  assert.equal(receipt.reason, "aborted");
});

test("an interrupted writable subagent is not replayed across a backend restart", { timeout: 18_000 }, async t => {
  const prompt = "Delegate the update in fix/retry.", task = "Write one file, then report.";
  const release = Promise.withResolvers();
  t.after(() => release.resolve());
  let writes = 0, resumes = 0;
  const f = await fixture(t, turn => {
    if (turn.text === task) {
      if (!turn.results.length) {
        writes++;
        return [call("write", { path: "retry.txt", content: "written before restart\n" })];
      }
      return (async function* () {
        yield "I wrote the file; preparing the summary.";
        await release.promise;
        yield "Unexpected continuation.";
      })();
    }
    assert.equal(turn.text, prompt);
    if (!turn.results.length) return [call("subagent", { prompt: task, read_only: false, worktree: "fix/retry" })];
    resumes++;
    assert.equal(turn.results.length, 1);
    assert.match(turn.results[0].content, /interrupted.*partially run/);
    return "The worker was interrupted; inspect the worktree for partial edits.";
  });
  const q = await f.submit("restart-write", prompt), first = await f.connect();
  const running = await until(first, q, s => s.events.some(e => e.type === "text"));
  assert.equal(await readFile(join(f.target, "retry.txt"), "utf8"), "written before restart\n");
  await f.restart();
  release.resolve();
  const recovering = await f.connect();
  const done = await until(recovering, q, s => s.settled);
  assert.deepEqual(done.events.filter(e => e.type === "finished").map(e => [e.workerId, e.status]), [[running.events[0].workerId, "failed"]]);
  assert.equal(writes, 1);
  assert.equal(resumes, 1);
  assert.equal(await readFile(join(f.target, "retry.txt"), "utf8"), "written before restart\n");
});

test("one model response runs read-only subagents together but serializes writable workers", { timeout: 18_000 }, async t => {
  const prompt = "Read the two fixtures in parallel, then delegate two updates.";
  const releaseReads = Promise.withResolvers(), releaseWrite = Promise.withResolvers();
  t.after(() => { releaseReads.resolve(); releaseWrite.resolve(); });
  const writes = [], parentRounds = [];
  const f = await fixture(t, async turn => {
    if (turn.text === prompt) {
      parentRounds.push(turn.results.length);
      if (turn.results.length === 0) {
        assert.match(turn.prompt, /issue the calls together/);
        return [1, 2].map(n => call("subagent", { prompt: `Read ${n}`, read_only: true }));
      }
      if (turn.results.length === 2) {
        assert.deepEqual(turn.results.map(r => r.content).sort(), ["Read 1 started.\nRead 1 done.", "Read 2 started.\nRead 2 done."]);
        return [1, 2].map(n => call("subagent", { prompt: `Write ${n}`, read_only: false }));
      }
      assert.equal(turn.results.length, 4);
      assert.deepEqual(turn.results.slice(2).map(r => r.content).sort(), ["Write 1 started.\nWrite 1 done.", "Write 2 started.\nWrite 2 done."]);
      return "Both rounds finished.";
    }
    if (/^Read [12]$/.test(turn.text)) return (async function* () {
      yield `${turn.text} started.\n`;
      await releaseReads.promise;
      yield `${turn.text} done.`;
    })();
    assert.match(turn.text, /^Write [12]$/);
    writes.push(turn.text);
    if (writes.length === 2) {
      const snapshot = ok(await check.send("ext.subagents.wait", q));
      const first = snapshot.events.find(e => e.type === "text" && e.text === `${writes[0]} started.\n${writes[0]} done.`);
      assert.ok(snapshot.events.some(e => e.type === "finished" && e.workerId === first?.workerId && e.status === "ok"),
        "the first writable worker must be terminal before the second reaches the provider");
    }
    return (async function* () {
      yield `${turn.text} started.\n`;
      await releaseWrite.promise;
      yield `${turn.text} done.`;
    })();
  });
  const peer = await f.connect(), check = await f.connect();
  const q = await f.submit("parallel-workers", prompt);
  const readers = await until(peer, q, s => s.events.filter(e => e.type === "text" && /^Read [12] started/.test(e.text)).length === 2);
  assert.equal(readers.events.some(e => e.type === "finished"), false, "both readers streamed before either could finish");
  assert.deepEqual(parentRounds, [0]);
  releaseReads.resolve();
  const firstWrite = await until(peer, q, s => s.events.filter(e => e.type === "started").length === 4 &&
    s.events.some(e => e.type === "text" && /^Write [12] started/.test(e.text)), readers.cursor);
  assert.equal(writes.length, 1);
  assert.deepEqual(parentRounds, [0, 2]);
  releaseWrite.resolve();
  const completed = await until(peer, q, s => s.settled, firstWrite.cursor);
  assert.deepEqual(completed.events.filter(e => e.type === "finished").map(e => e.status), Array(4).fill("ok"));
  assert.deepEqual(parentRounds, [0, 2, 4]);
});

test("a follow-up placed after a queued reset can delegate", { timeout: 15_000 }, async t => {
  const previous = "Prior turn held at the model.", next = "Delegate a fresh check.", brief = "Check retry text.";
  const entered = Promise.withResolvers(), release = Promise.withResolvers();
  t.after(() => release.resolve());
  const f = await fixture(t, async turn => {
    if (turn.text === previous) {
      entered.resolve();
      await release.promise;
      return "Ready for the reset.";
    }
    if (turn.text === brief) return "The check ran after reset.";
    assert.equal(turn.text, next);
    assert.ok(turn.tools.includes("subagent"));
    if (turn.results.length === 0) return [call("subagent", { prompt: brief, read_only: true })];
    assert.equal(turn.results[0].content, "The check ran after reset.");
    return "Follow-up finished.";
  });
  await f.submit("before-reset", previous);
  await entered.promise;
  const peer = await f.connect(), threadId = f.thread.id;
  ok(await peer.send("thread.reset", { threadId, handoff: "Resume with the new request." }));
  const { submissionId } = ok(await peer.send("submit", { threadId, requestId: "after-reset", text: next, whenBusy: "followUp" }));
  assert.equal(ok(await peer.send("submission.get", { threadId, submissionId })).status, "queued");
  release.resolve();
  const done = await until(peer, { threadId, submissionId }, s => s.settled);
  assert.deepEqual(done.events.filter(e => e.type === "finished").map(e => e.status), ["ok"]);
});

test("a subagent requires the original turn's grant, even if the workspace has been trusted again", { timeout: 15_000 }, async t => {
  const prompt = "Delegate a workspace check.", brief = "Read the retry fixture.";
  const entered = Promise.withResolvers(), release = Promise.withResolvers();
  t.after(() => release.resolve());
  let first = true;
  const f = await fixture(t, async turn => {
    if (turn.text === brief) return "Worker checked the fixture.";
    assert.equal(turn.text, prompt);
    if (!turn.results.length) {
      if (first) {
        entered.resolve();
        await release.promise;
      }
      return [call("subagent", { prompt: brief, read_only: true })];
    }
    if (first) {
      assert.match(turn.results[0].content, /workspace is not authorized/);
      first = false;
      return "The original grant is no longer valid.";
    }
    assert.equal(turn.results[0].content, "Worker checked the fixture.");
    return "The new turn can delegate.";
  });
  const peer = await f.connect(), original = await f.submit("original-grant", prompt);
  await entered.promise;
  await f.trust.change(f.cwd, "revoke", t.signal);
  await f.trust.change(f.cwd, "grant", t.signal);
  release.resolve();
  assert.deepEqual((await until(peer, original, s => s.settled)).events, []);
  const next = await f.submit("new-grant", prompt);
  assert.deepEqual((await until(peer, next, s => s.settled)).events.filter(e => e.type === "finished").map(e => e.status), ["ok"]);
});
