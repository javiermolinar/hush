import assert from "node:assert/strict";
import { execFile, execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, realpath, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { BACKGROUND_CONTEXT as ctx } from "@earendil-works/chord/context";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { call, planProvider } from "../../backend/test/plan-provider.mjs";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { startBackend } from "../../dist/hush-backend.mjs";
import { dial } from "../../server/test/client.mjs";

const exec = promisify(execFile);
const binary = process.env.HUSH_TEST_CLI || fileURLToPath(new URL("../../bin/hush", import.meta.url));
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };

test("hush review discovers, launches and inspects an explicit native reservation", { timeout: 12_000 }, async t => {
  const dir = await realpath(await mkdtemp("/tmp/hush-cli-review-"));
  let backend, wire;
  t.after(async () => { wire?.close(); await backend?.close(); await rm(dir, { force: true, recursive: true }); });
  const cwd = join(dir, "repo"), agentDir = join(dir, ".hush"), dataDir = join(dir, "db"), socket = join(dataDir, "hush.sock");
  await mkdir(cwd); await mkdir(agentDir, { mode: 0o700 });
  const env = { HOME: dir, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1" };
  const git = (...args) => execFileSync("git", args, { cwd, env });
  git("init", "-q"); git("config", "user.email", "fixture@example.test"); git("config", "user.name", "Fixture");
  await writeFile(join(cwd, "main.txt"), "base\n"); git("add", "."); git("commit", "-qm", "base");
  await writeFile(join(cwd, "main.txt"), "dirty\n");
  await new WorkspaceTrust(agentDir).change(cwd, "grant", AbortSignal.timeout(4_000));
  const provider = await planProvider(t, agentDir, turn => {
    assert.ok(turn.text.startsWith("Code review assignment\n"));
    assert.deepEqual(turn.tools.toSorted(), ["hush_review_read", "hush_review_report"]);
    return turn.results.length ? "Finished." : [call("hush_review_report", { report: "CLI_ADVISORY_REPORT" })];
  });
  backend = await startBackend({ cwd, agentDir, dataDir, socket, model: provider.model, ephemeral: false, faux: false, idleMs: 60_000 });
  wire = await dial(socket);
  ok(await wire.send("hello", { versions: [1], capabilities: { delegation: [1] } }));
  const { id } = ok(await wire.send("thread.new", {}));
  ok(await wire.send("subscribe", { threadId: id }));
  assert.match((await exec(binary, ["--help"], { cwd, env })).stdout, /hush review start/);
  const cli = async (op, ...args) => JSON.parse((await exec(binary, ["review", op, "--socket", socket, "--thread", id, "--json", ...args], { cwd, env })).stdout);
  const q = ["--request-id", "cli-review", "--tracked", "main.txt", "--brief", "Inspect the working change"];
  const receipt = await cli("start", ...q);
  const handle = receipt.workers[0].handle;
  await wire.wait(f => f.type === "patch" && JSON.stringify(f.ops).includes("CLI_ADVISORY_REPORT"));
  assert.deepEqual(await cli("start", ...q), receipt);
  const view = await cli("status", "--request-id", receipt.requestId);
  assert.equal(view.workers[0].state, "succeeded");
  assert.equal(view.target, "current");
  assert.deepEqual(await cli("stop", handle), view.workers[0]);
  const transcript = await cli("transcript", handle);
  assert.equal(transcript.worker.report, view.workers[0].report);
  assert.ok(transcript.transcript.entries.some(e => e.kind === "reviewer" && e.text === "Finished."));
  assert.equal(provider.requests.length, 2, "no exact retry, inspection or late Stop started another request");
});

test("Hush inspects overlapping pinned reviews, reattaches during join and synthesizes with one timed-out worker", { timeout: 35_000 }, async t => {
  const dir = await realpath(await mkdtemp("/tmp/hush-review-pty-"));
  const cwd = join(dir, "repo"), agentDir = join(dir, ".hush"), dataDir = join(dir, "db"), socket = join(dataDir, "hush.sock");
  const env = { HOME: dir, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1", TERM: "xterm-256color", LANG: "C.UTF-8" };
  let backend, pty, result;
  const finish = Promise.withResolvers(), slow = Promise.withResolvers();
  try {
    await mkdir(cwd); await mkdir(agentDir, { mode: 0o700 });
    await writeFile(join(agentDir, "settings.json"), '{"codemode":true}', { mode: 0o600 });
    const git = (...args) => execFileSync("git", args, { cwd, env }).toString();
    const source = join(cwd, "settings.txt");
    git("init", "-q"); git("config", "user.email", "fixture@example.test"); git("config", "user.name", "Fixture");
    await writeFile(source, "default=base\n"); git("add", "."); git("commit", "-qm", "base");
    await writeFile(source, "default=staged\n"); git("add", "."); await writeFile(source, "default=working\n");
    const checkoutState = () => ({ workingDiff: git("diff"), stagedDiff: git("diff", "--cached"), untracked: git("ls-files", "--others", "-z") });
    const original = checkoutState();
    await new WorkspaceTrust(agentDir).change(cwd, "grant", t.signal);
    const names = ["Rules reader", "Slow reader"];
    const prompt = "Launch the reserved reviewers; join to summarize the pinned changes.";
    const synthesis = "Synthesis: default changed; slow review timed out.";
    const readPrefix = "Code review assignment\n";
    const readSnapshots = new Map();
    const parentRequests = [];
    let joined;
    const provider = await planProvider(t, agentDir, async turn => {
      if (turn.text.startsWith(readPrefix)) {
        const input = JSON.parse(turn.text.slice(readPrefix.length));
        assert.ok(names.includes(input.name));
        assert.deepEqual(turn.tools.toSorted(), ["hush_review_read", "hush_review_report"]);
        assert.equal(turn.model, "staged-chat");
        assert.equal(turn.messages.filter(m => m.role === "user").length, 1);
        assert.doesNotMatch(JSON.stringify(turn.messages), /unsent parent draft|Launch the reserved reviewers/);
        assert.equal(input.assessment.evidence[0].label, "Fixture test excerpt");
        if (turn.results.length === 0) return [call("hush_review_read", { path: "settings.txt" })];
        if (turn.results.length === 1) {
          const { base, staged, working } = JSON.parse(turn.results[0].content);
          assert.deepEqual([base.text, staged.text, working.text], ["default=base\n", "default=staged\n", "default=working\n"]);
          readSnapshots.set(input.name, input.snapshotId);
          if (input.name === names[1]) { await slow.promise; return "Too late."; }
          await finish.promise;
          return [call("hush_review_report", { summary: "Recorded default finding", findings: [{ priority: "P1", path: "settings.txt",
            version: "working", line: 1, message: "The working default differs from the staged value." }],
            checks: [{ id: "default", basis: "inspected", source: "settings.txt", note: "Compared each pinned version." },
              { id: "tests", basis: "supplied", source: "local", note: "Read the excerpt only." }],
            limitations: ["No live environment or executable tests."] })];
        }
        assert.match(turn.results.at(-1).content, /Advisory report recorded/);
        return "Read the three versions.";
      }
      parentRequests.push(turn);
      assert.equal(turn.text, prompt);
      assert.equal(turn.model, "chat");
      assert.equal(turn.messages.filter(m => m.role === "user").length, 1);
      assert.ok(turn.tools.includes("hush_spawn_reviews") && turn.tools.includes("hush_join_reviews") && turn.tools.includes("codemode"));
      switch (turn.results.length) {
        case 0: return [call("codemode", { code: `
          const source = await tools.inspect_file({path: 'settings.txt'});
          const [spawn, join] = await Promise.allSettled([
            (async () => tools.hush_spawn_reviews({}))(), (async () => tools.hush_join_reviews({handles: []}))(),
          ]);
          return [source.trim(), spawn.status, join.status];` })];
        case 1:
          assert.match(turn.results[0].content, /default=working.*rejected.*rejected/s);
          assert.equal(readSnapshots.size, 0, "nested inspection could not release the reservation");
          return [call("hush_spawn_reviews")];
        case 2: return [call("hush_join_reviews", { handles: JSON.parse(turn.results[1].content).workers.map(w => w.handle), timeoutMs: 20000 })];
        default:
          joined = JSON.parse(turn.results.at(-1).content);
          assert.equal(joined.wait, "settled");
          assert.deepEqual(joined.outcomes.map(w => w.state), ["succeeded", "timed_out"]);
          assert.match(joined.outcomes[0].report, /P1 · "settings.txt":1 \(working\)/);
          assert.match(joined.outcomes[0].report, /Unverified \(not assessed\)/);
          assert.equal(joined.outcomes[1].report, undefined);
          return synthesis;
      }
    }, { alternate: true });
    backend = await startBackend({ cwd, agentDir, dataDir, socket, model: provider.model, ephemeral: false, faux: false, idleMs: 60_000 });
    const cli = async (...args) => JSON.parse((await exec(binary, ["--socket", socket, "--json", ...args], { cwd, env, timeout: 5_000 })).stdout);
    const { id } = await cli("thread", "new");
    const receipt = await cli("review", "start", "--thread", id, "--request-id", "pty-review", "--parallel",
      "--tracked", "settings.txt", "--reviewer", "Rules reader=Review default branch", "--reviewer", "Slow reader=Inspect the change independently",
      "--check", "default=Compare defaults", "--check", "tests=Assess reported tests", "--check", "live=Verify live environment",
      "--evidence", "local:Fixture test excerpt=3 checks passed", "--prompt", prompt,
      "--model", "staged-plan-test/staged-chat", "--thinking", "high", "--worker-timeout-ms", "9000");
    const script = fileURLToPath(new URL("./review-pty.py", import.meta.url));
    pty = spawn("python3", [script, binary, socket, id], { cwd, env, signal: t.signal });
    let stderr = "";
    pty.stderr.setEncoding("utf8").on("data", text => { stderr += text; });
    result = once(pty, "close");
    const lines = createInterface({ input: pty.stdout })[Symbol.asyncIterator]();
    const next = async expected => assert.equal((await lines.next()).value, expected, stderr);
    await next("inspected");
    await next("reattached");
    assert.deepEqual([...readSnapshots.entries()].sort(), names.map(name => [name, receipt.snapshotId]));
    assert.equal(parentRequests.length, 3, "the parent has not called its provider again while parked");
    const park = await backend.harness.commit(tx => tx.scanTasks({ conversationId: Number(id), kind: "pi.tool", status: "sleeping" }, 10), ctx);
    assert.equal(park.items.length, 1);
    assert.equal(park.items[0].state.checkpoint.phase, "parked");
    finish.resolve();
    await next("settled");
    assert.deepEqual(await result, [0, null], stderr);
    const records = await backend.harness.commit(async tx => ({
      submission: await tx.submission(Number(receipt.submissionId)),
      entries: (await tx.scanEntries({ conversationId: Number(id), order: "ascending" }, 64)).items,
      nested: (await tx.scanTasks({ conversationId: Number(id), kind: "pi.tool", status: "terminal" }, 32)).items
        .filter(task => task.input.kind === "nested"),
    }), ctx);
    assert.equal(records.submission.status, "done");
    assert.equal(parentRequests.length, 4);
    const results = records.entries.flatMap(e => e.model ?? []).filter(m => m.role === "toolResult");
    assert.deepEqual(results.map(m => m.toolName), ["codemode", "hush_spawn_reviews", "hush_join_reviews"]);
    assert.deepEqual(records.nested.map(t => [t.input.call.name, t.state.outcome.status]), [["inspect_file", "completed"]]);
    assert.equal(records.entries.filter(e => e.kind === "hush.review.request").length, 1);
    const notices = records.entries.filter(e => e.kind === "hush.worker.notice");
    assert.deepEqual(notices.map(e => [e.data.handle, e.data.state]), [[receipt.workers[0].handle, "succeeded"], [receipt.workers[1].handle, "timed_out"]]);
    assert.ok(notices.every(e => e.model === undefined));
    assert.deepEqual(joined.outcomes.map(w => w.noticeId), notices.map(e => String(e.id)));
    assert.deepEqual(checkoutState(), original, "the reviews did not alter tracked diffs or the untracked inventory");
  } finally {
    finish.resolve(); slow.resolve();
    if (pty && pty.exitCode === null && pty.signalCode === null) pty.kill();
    try { await result; } finally { await backend?.close(); await rm(dir, { force: true, recursive: true }); }
  }
});
