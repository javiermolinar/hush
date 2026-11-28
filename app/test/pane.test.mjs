import assert from "node:assert/strict";
import { execFile, execFileSync } from "node:child_process";
import { access, lstat, mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { call, planProvider } from "../../backend/test/plan-provider.mjs";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { startBackend } from "../../dist/hush-backend.mjs";
import { dial } from "../../server/test/client.mjs";

const exec = promisify(execFile);
const binary = process.env.HUSH_TEST_CLI || fileURLToPath(new URL("../../bin/hush", import.meta.url));
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const cap = op => `ext.paneControl.${op}`;
const paneTools = turn => turn.tools.filter(t => t.startsWith("pane_"));
const fixtureArguments = ["", "a b", "a'b\"c", "$HOME", "*;[]", "first\nsecond"];
const listFrames = peer => peer.frames.filter(f => f.type === "pane_call");
const launch = { mode: "argv", program: "/usr/bin/printf", args: fixtureArguments };
const handle = "hp_ABCDEFGHIJKLMNOPQRSTUVWXYZ";

async function fixture(t, answer) {
  const root = await realpath(await mkdtemp("/tmp/hpc-"));
  const cwd = join(root, "repo"), worktree = join(root, "worktree ' spaced"), agentDir = join(root, ".hush");
  const dataDir = join(root, "db"), socket = join(dataDir, "hush.sock");
  const env = { HOME: root, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1", XDG_STATE_HOME: join(root, "state") };
  let backend;
  const peers = [];
  t.after(async () => {
    for (const peer of peers) peer.close();
    await backend?.close();
    await rm(root, { recursive: true, force: true });
  });
  await mkdir(cwd); await mkdir(agentDir, { mode: 0o700 });
  const git = (...args) => execFileSync("git", args, { cwd, env, stdio: "pipe" });
  git("init", "-q"); git("config", "user.email", "pane@example.test"); git("config", "user.name", "Pane fixture");
  await writeFile(join(cwd, "file.txt"), "one\n");
  git("add", "."); git("commit", "-qm", "base"); git("worktree", "add", "-qb", "pane-probe", worktree);
  await writeFile(join(worktree, "file.txt"), "two\n");
  const trust = new WorkspaceTrust(agentDir);
  await trust.change(worktree, "grant", t.signal);
  const provider = await planProvider(t, agentDir, turn => answer(turn, root));
  const start = () => startBackend({ cwd, agentDir, dataDir, socket, model: provider.model, ephemeral: false, faux: false, idleMs: 60_000 });
  backend = await start();
  const connect = async capabilities => {
    const peer = await dial(socket);
    peers.push(peer);
    peer.hello = ok(await peer.send("hello", { versions: [1], capabilities }));
    return peer;
  };
  const wire = await connect({});
  const thread = ok(await wire.send("thread.new", { cwd: worktree }));
  const cli = (...args) => exec(binary, ["--socket", socket, ...args], { cwd, env, timeout: 8_000 });
  const restart = async () => {
    for (const peer of peers.splice(0)) peer.close();
    await backend.close();
    backend = await start();
  };
  return { root, cwd, worktree, socket, env, connect, thread, wire, provider, trust, cli, restart, get harness() { return backend.harness; } };
}

async function identity(path) {
  const s = await lstat(path, { bigint: true });
  return { device: String(s.dev), inode: String(s.ino), birth: String(s.birthtimeNs) };
}

async function settle(peer, threadId, receipt) {
  const result = ok(await peer.send("submission.wait", { threadId, submissionId: receipt.submissionId }));
  assert.equal(result.status, "done", JSON.stringify(result));
}

test("standalone pane tools serve plain prompts from the trusted effective worktree", { timeout: 18_000 }, async t => {
  const revoke = { arrived: Promise.withResolvers(), done: Promise.withResolvers() };
  t.after(() => revoke.done.resolve());
  const f = await fixture(t, async turn => {
    assert.equal(typeof turn.text, "string");
    if (turn.results.length) return "Pane fixture answered.";
    switch (turn.text) {
      case "Open a new side pane":
        assert.deepEqual(paneTools(turn).sort(), ["pane_focus", "pane_list", "pane_open"]);
        assert.match(turn.prompt, /Plain language is sufficient/);
        assert.match(turn.prompt, /Placements: right, below; default to right/);
        assert.match(turn.prompt, /use launch mode terminal/);
        assert.doesNotMatch(turn.prompt, /pane_read|\/pane/);
        return [call("pane_open", { placement: "right", launch: { mode: "terminal" } })];
      case "headless input":
        assert.deepEqual(paneTools(turn), []);
        assert.match(turn.prompt, /Pane control is unavailable for this turn/);
        return [call("pane_open", { placement: "right", launch })]; // knowing the name grants nothing
      case "Run the fixture in a pane on the right":
        assert.deepEqual(paneTools(turn).sort(), ["pane_focus", "pane_list", "pane_open"]);
        return [call("pane_open", { placement: "right", launch }), call("pane_open", { placement: "right", launch })];
      case "/pane try a tab":
        assert.ok(paneTools(turn).includes("pane_open"));
        assert.ok(!turn.tools.includes("bash"), "an explicit pane request must not give the provider a backend fallback");
        return [call("pane_open", { placement: "tab", launch }), call("bash", { command: "touch backend-fallback-ran" })];
      case "Open a pane after the grant is revoked":
        assert.ok(paneTools(turn).includes("pane_open"));
        revoke.arrived.resolve();
        await revoke.done.promise;
        return [call("pane_open", { placement: "right", launch })];
      default: throw Error("unexpected prompt: " + turn.text);
    }
  });
  const peer = await f.connect({ paneControl: [1] });
  assert.deepEqual(peer.hello.capabilities, { paneControl: 1 });
  const bound = ok(await peer.send(cap("bind"), { capabilities: { left: false, above: false, tabs: false, read: false, views: true } }));
  assert.deepEqual(bound.socket, await identity(f.socket));
  const directory = await identity(f.worktree), threadId = f.thread.id;
  assert.deepEqual(ok(await peer.send(cap("workspace"), { threadId })), { dir: f.worktree, directory });
  const input = (text, thread = threadId) => ({ threadId: thread, requestId: text.includes(" ") ? text.replaceAll(" ", "-") : text, text, whenBusy: "reject" });
  const submit = async text => ok(await peer.send(cap("submit"), { route: bound.route, input: input(text) }));

  await settle(f.wire, threadId, ok(await f.wire.send("submit", input("headless input"))));
  assert.equal(listFrames(peer).length, 0);
  const blank = await submit("Open a new side pane");
  const blankCall = await peer.wait(frame => frame.type === "pane_call");
  assert.deepEqual(blankCall.request, { op: "open", placement: "right", launch: { mode: "terminal" }, dir: f.worktree, directory });
  ok(await peer.send(cap("result"), { callId: blankCall.id, result: { outcome: "confirmed", pane: handle } }));
  await settle(peer, threadId, blank);

  const prompt = "Run the fixture in a pane on the right";
  const accepted = await submit(prompt);
  const request = await peer.wait(frame => frame.type === "pane_call" && frame.id !== blankCall.id);
  assert.deepEqual(request.request, { op: "open", placement: "right", launch, dir: f.worktree, directory });
  ok(await peer.send(cap("result"), { callId: request.id, result: { outcome: "confirmed", pane: handle } }));
  await settle(peer, threadId, accepted);
  assert.equal(listFrames(peer).length, 2, "one input cannot open two panes");
  const toolRound = f.provider.requests.find(turn => turn.text === prompt && turn.results.length === 2);
  assert.deepEqual(toolRound.results.map(r => JSON.parse(r.content)), [{ outcome: "confirmed", pane: handle }, { outcome: "rejected" }]);
  assert.ok(!paneTools(toolRound).includes("pane_open"));
  assert.doesNotMatch(JSON.stringify(toolRound.messages), new RegExp(bound.route + "|" + request.id + "|paneAdmission"));

  await settle(peer, threadId, await submit("/pane try a tab"));
  assert.equal(listFrames(peer).length, 2, "an unsupported placement reached the client");
  await assert.rejects(readFile(join(f.worktree, "backend-fallback-ran")), { code: "ENOENT" });
  const untrusted = ok(await f.wire.send("thread.new", { cwd: f.cwd }));
  const denied = await peer.send(cap("submit"), { route: bound.route, input: input("Open an untrusted pane", untrusted.id) });
  assert.equal(denied.error?.code, "invalid_request");
  assert.equal(listFrames(peer).length, 2);

  const revoked = await submit("Open a pane after the grant is revoked");
  await revoke.arrived.promise;
  await f.trust.change(f.worktree, "revoke", t.signal);
  revoke.done.resolve();
  await settle(peer, threadId, revoked);
  assert.equal(listFrames(peer).length, 2, "revoked workspace dispatched a native request");
});

test("a stopped pane steer cannot gain backend tools on repeated Continue", { timeout: 18_000 }, async t => {
  const ordinary = "Begin ordinary work", pane = "/pane run it beside this terminal", narrowed = "Continue with the same pane request";
  const holds = Array.from({ length: 4 }, () => ({ arrived: Promise.withResolvers(), release: Promise.withResolvers() }));
  t.after(() => holds.forEach(hold => hold.release.resolve()));
  let seen = 0;
  const f = await fixture(t, async turn => {
    const hold = async i => {
      holds[i].arrived.resolve();
      await holds[i].release.promise;
    };
    if (turn.text === ordinary) {
      assert.ok(turn.tools.includes("bash"));
      await hold(0);
      return [call("read", { path: "file.txt" })];
    }
    if (turn.text === "Fresh ordinary input") {
      assert.ok(turn.tools.includes("bash"));
      return "ordinary complete";
    }
    assert.ok(turn.text === pane || turn.text === narrowed, turn.text);
    const at = seen++;
    assert.ok(!turn.tools.includes("bash") && !turn.tools.includes("thread_start"), "pane-only work advertised backend tools");
    if (at === 0) {
      await hold(1);
      return [call("pane_list")]; // a post-tools boundary will place the next steer
    }
    assert.deepEqual(paneTools(turn), []);
    if (at === 1 || at === 2) {
      await hold(at + 1);
      return "interrupted";
    }
    if (!turn.results.length) return [call("bash", { command: "touch backend-fallback-ran" })];
    assert.ok(turn.results[0].content !== "", "unoffered command must yield an error result");
    return "Stopped pane-only work did not run a backend command.";
  });
  const caps = { left: false, above: false, tabs: false, read: false };
  let peer = await f.connect({ paneControl: [1] });
  const { route } = ok(await peer.send(cap("bind"), { capabilities: caps }));
  const threadId = f.thread.id;
  const submit = (text, requestId, whenBusy, routed = false) => {
    const input = { text, requestId, threadId, whenBusy };
    return peer.send(routed ? cap("submit") : "submit", routed ? { route, input } : input).then(ok);
  };
  const status = async receipt => ok(await peer.send("submission.get", { threadId, submissionId: receipt.submissionId })).status;
  const first = await submit(ordinary, "first", "reject", true);
  await holds[0].arrived.promise;
  const steer = await submit(pane, "pane-steer", "steer", true);
  holds[0].release.resolve();
  await holds[1].arrived.promise;
  const last = await submit(narrowed, "later-steer", "steer");
  // The pane_list attempt sees only the original route before the mixed
  // steer is placed. Confirm it so the next generation can observe the steer.
  holds[1].release.resolve();
  const list = await peer.wait(frame => frame.type === "pane_call");
  assert.equal(list.request.op, "list");
  ok(await peer.send(cap("result"), { callId: list.id, result: { outcome: "confirmed", panes: [] } }));
  await holds[2].arrived.promise;
  assert.equal(await status(last), "placed");
  ok(await peer.send("thread.abort", { threadId }));
  holds[2].release.resolve();
  assert.deepEqual(await Promise.all([first, steer, last].map(status)), ["unanswered", "unanswered", "unanswered"]);
  const resumed = ok(await peer.send("continue", { threadId }));
  await holds[3].arrived.promise;
  ok(await peer.send("thread.abort", { threadId }));
  holds[3].release.resolve();
  assert.equal(await status(resumed), "unanswered");
  assert.equal(listFrames(peer).length, 1);

  await f.restart();
  peer = await f.connect({});
  await settle(peer, threadId, ok(await peer.send("continue", { threadId })));
  assert.equal(seen, 5);
  await assert.rejects(readFile(join(f.worktree, "backend-fallback-ran")), { code: "ENOENT" });
  await settle(peer, threadId, await submit("Fresh ordinary input", "fresh", "reject"));
});

test("a fresh queue boundary or reset does not inherit a failed pane-only turn", { timeout: 18_000 }, async t => {
  for (const boundary of ["queued input", "reset handoff"]) {
    await t.test(boundary, async t => {
      const arrived = Promise.withResolvers(), release = Promise.withResolvers();
      const f = await fixture(t, async turn => {
        if (turn.text === "/pane hold here") {
          assert.ok(!turn.tools.includes("bash"));
          arrived.resolve();
          await release.promise;
          return "not reached";
        }
        assert.equal(turn.text, boundary);
        assert.deepEqual(paneTools(turn), []);
        assert.ok(turn.tools.includes("read"), "fresh context inherited the pane-only restriction");
        return "Fresh context answered.";
      });
      t.after(() => release.resolve());
      const peer = await f.connect({ paneControl: [1] });
      const { route } = ok(await peer.send(cap("bind"), { capabilities: { left: false, above: false, tabs: false, read: false } }));
      const threadId = f.thread.id;
      const pane = ok(await peer.send(cap("submit"), { route, input: { threadId, requestId: boundary, text: "/pane hold here", whenBusy: "reject" } }));
      await arrived.promise;
      if (boundary === "queued input") {
        const conv = await f.harness.conversation(Number(threadId), context);
        await conv.submit({ type: "input", content: boundary, whenBusy: "followUp", onAbort: "keep" }, context);
      } else {
        ok(await peer.send("thread.reset", { threadId, handoff: boundary }));
      }
      ok(await peer.send("thread.abort", { threadId }));
      release.resolve();
      assert.equal(ok(await peer.send("submission.get", { threadId, submissionId: pane.submissionId })).status, "unanswered");
      await settle(peer, threadId, ok(await peer.send("continue", { threadId })));
    });
  }
});

test("chat starts an independent durable thread, then opens just its view", { timeout: 18_000 }, async t => {
  const prompt = "Start a fresh thread to check the changes and open it beside me";
  const first = "/skill:literal-first-prompt";
  const childArrived = Promise.withResolvers(), releaseChild = Promise.withResolvers();
  t.after(() => releaseChild.resolve());
  let childId;
  const f = await fixture(t, async turn => {
    if (turn.text === first) {
      assert.ok(!turn.tools.includes("thread_start") && !turn.tools.includes("thread_open"));
      assert.ok(!turn.prompt.includes("Local pane tools available"));
      assert.ok(!JSON.stringify(turn.messages).includes(prompt));
      if (!turn.results.length) {
        childArrived.resolve();
        await releaseChild.promise;
        return [call("read", { path: "file.txt" })];
      }
      assert.match(turn.results[0].content, /two/, "the child read the parent worktree, not the main checkout");
      return "Child read the worktree.";
    }
    assert.equal(turn.text, prompt);
    if (!turn.results.length) {
      assert.ok(turn.tools.includes("thread_start"));
      assert.ok(turn.tools.includes("thread_open"));
      assert.match(turn.prompt, /first call thread_start, then pass the returned threadId/);
      return [call("thread_start", { prompt: first }), call("thread_start", { prompt: "duplicate must not start" })];
    }
    const receipt = JSON.parse(turn.results[0].content);
    assert.match(receipt.threadId, /^[1-9][0-9]*$/);
    assert.match(receipt.submissionId, /^[1-9][0-9]*$/);
    assert.equal(receipt.requestId, "hush.thread-start");
    assert.match(JSON.parse(turn.results[1].content).reason, /already started|ordinary placed/);
    assert.ok(!turn.tools.includes("thread_start"));
    assert.doesNotMatch(turn.prompt, /first call thread_start/);
    assert.deepEqual(JSON.parse(turn.results[1].content), { reason: "A thread was already started for this input.",
      state: "admitted", threadId: receipt.threadId, submissionId: receipt.submissionId });
    childId = receipt.threadId;
    if (turn.results.length === 2) return [call("thread_open", { threadId: childId, where: "split" })];
    assert.deepEqual(JSON.parse(turn.results[2].content), { outcome: "confirmed", threadId: childId, where: "split" });
    assert.ok(!turn.tools.includes("thread_open") && !turn.tools.includes("pane_open"));
    return "The thread is open.";
  });
  const caps = { left: false, above: false, tabs: false, read: false, views: true, threadTabs: true };
  const peer = await f.connect({ paneControl: [2, 1] });
  const other = await f.connect({ paneControl: [2] });
  assert.deepEqual(peer.hello.capabilities, { paneControl: 2 });
  ok(await other.send(cap("bind"), { capabilities: caps }));
  const { route } = ok(await peer.send(cap("bind"), { capabilities: caps }));
  const threadId = f.thread.id;
  const input = { threadId, requestId: "start-and-open", text: prompt, whenBusy: "reject" };
  const list = async () => ok(await f.wire.send("thread.list", { limit: 100 })).threads;
  const before = await list();
  const parentReceipt = ok(await peer.send(cap("submit"), { route, input }));
  const frame = await peer.wait(v => v.type === "pane_call");
  assert.deepEqual(frame.request, { op: "thread_open", threadId: childId, where: "split", dir: f.worktree, directory: await identity(f.worktree) });
  ok(await peer.send(cap("result"), { callId: frame.id, result: { outcome: "confirmed", threadId: childId, where: "split" } }));
  await settle(peer, threadId, parentReceipt);
  await childArrived.promise;
  assert.equal(listFrames(other).length, 0);
  const after = await list();
  const created = after.filter(row => !before.some(v => v.id === row.id));
  assert.deepEqual(created.map(({ id, owner, parent }) => ({ id, owner, parent })), [{ id: childId, owner: undefined, parent: undefined }]);
  const childState = ok(await f.wire.send("subscribe", { threadId: childId })).state;
  assert.equal(childState.agent.cwd, f.worktree);
  const childReceipt = ok(await f.wire.send("submission.get", { threadId: childId, requestId: "hush.thread-start" }));
  assert.equal(childReceipt.status, "placed", "the parent completed without waiting for the child");
  peer.close();
  releaseChild.resolve();
  await settle(f.wire, childId, { submissionId: childReceipt.submissionId });
});

test("chat moves this conversation into a new worktree and switches the TUI to it", { timeout: 20_000 }, async t => {
  const remember = "Remember the word mango.", move = "Move this conversation to a new worktree and continue there";
  const first = "Continue the task in the new worktree", checkRead = "Read file.txt in this checkout.";
  let receipt;
  const childDone = Promise.withResolvers();
  const f = await fixture(t, async turn => {
    if (turn.text === remember) return "Noted.";
    if (turn.text === first) {
      assert.ok(turn.messages.some(m => m.content === remember), "the moved conversation keeps its history");
      assert.ok(!turn.messages.some(m => m.content === move), "the move request itself stays in the source thread");
      assert.ok(!turn.tools.includes("thread_start"), "the generated first prompt cannot start another thread");
      if (!turn.results.length) return [call("read", { path: "file.txt" })];
      assert.match(turn.results[0].content, /one/, "the new checkout has committed HEAD, not uncommitted files");
      // Managed checkouts sit in the data directory; a read-only subagent can still read them.
      if (turn.results.length === 1) return [call("subagent", { prompt: checkRead })];
      assert.equal(turn.results[1].content, "The checkout file says one.");
      childDone.resolve();
      return "Continuing in the worktree.";
    }
    if (turn.text === checkRead) {
      if (!turn.results.length) return [call("read", { path: "file.txt" })];
      assert.equal(turn.results[0].content, "one\n");
      return "The checkout file says one.";
    }
    assert.equal(turn.text, move);
    if (!turn.results.length) {
      assert.match(turn.prompt, /do not cd in shell commands/);
      return [call("thread_start", { prompt: first, worktree: "new", history: true })];
    }
    receipt = JSON.parse(turn.results[0].content);
    return "Moved.";
  });
  const caps = { left: false, above: false, tabs: false, read: false, views: false, threadTabs: true, noPanes: true };
  const peer = await f.connect({ paneControl: [2] });
  const { route } = ok(await peer.send(cap("bind"), { capabilities: caps }));
  const threadId = f.thread.id;
  await settle(f.wire, threadId, ok(await f.wire.send("submit", { threadId, requestId: "remember", text: remember, whenBusy: "reject" })));
  const parentReceipt = ok(await peer.send(cap("submit"), { route, input: { threadId, requestId: "move", text: move, whenBusy: "reject" } }));
  const frame = await peer.wait(v => v.type === "pane_call");
  assert.equal(frame.request.op, "thread_open");
  assert.equal(frame.request.where, "focus", "a move opens and switches to the new thread");
  const childId = frame.request.threadId;
  ok(await peer.send(cap("result"), { callId: frame.id, result: { outcome: "confirmed", threadId: childId, where: "focus" } }));
  await settle(peer, threadId, parentReceipt);
  assert.equal(receipt.threadId, childId);
  assert.equal(receipt.history, true);
  assert.equal(receipt.opened, true);
  assert.equal(receipt.uncommittedNotCopied, 1, "the source's modified file.txt was not copied");
  assert.match(receipt.worktree.branch, /^hush\/continue-the-task-in-the-new-[0-9a-f-]{36}$/);
  assert.equal((await f.trust.review(receipt.worktree.cwd)).trusted, true, "the new checkout inherited the source's grant");
  const childState = ok(await f.wire.send("subscribe", { threadId: childId })).state;
  assert.equal(childState.agent.cwd, receipt.worktree.cwd);
  assert.equal(childState.thread.parent?.threadId, threadId, "the move is a history fork of the source thread");
  await childDone.promise;
  await settle(f.wire, childId, ok(await f.wire.send("submission.get", { threadId: childId, requestId: "hush.thread-start" })));
  const { entries } = ok(await f.wire.send("history", { threadId, limit: 32, range: { kind: "before", beforeEntryId: null } }));
  assert.ok(entries.some(e => e.kind === "hush.thread-start-notice" && /with this conversation's history/.test(e.messages[0].blocks[0].text)));
  assert.equal(await readFile(join(f.worktree, "file.txt"), "utf8"), "two\n", "the source checkout is unchanged");
});

test("thread start leaves read-only receipts if the parent stops before or after admission", { timeout: 18_000 }, async t => {
  for (const state of ["allocated", "admitted"]) {
    await t.test(state, async t => {
      const reported = Promise.withResolvers(), release = Promise.withResolvers();
      const first = "First child prompt";
      t.after(() => release.resolve());
      const f = await fixture(t, turn => {
        if (turn.text === first) return "The child ran independently.";
        assert.deepEqual(turn.results, [], "parent observed a tool reply after interruption");
        return [call("thread_start", { prompt: first })];
      });
      const threadId = f.thread.id, h = f.harness;
      const commit = h.commitWith.bind(h);
      let notice;
      const observer = h.observeCommits({ advance({ changes }) {
        for (const c of changes) {
          if (c.type !== "entry" || c.value.kind !== "hush.thread-start-notice" || String(c.value.conversationId) !== threadId) continue;
          if (new RegExp(`^Thread [1-9][0-9]* ${state}`).test(c.value.data.text)) notice = c.value;
        }
      }, end() {} });
      h.commitWith = async (change, ctx, scope, options) => {
        const before = notice, value = await commit(change, ctx, scope, options);
        // Pause the actual tool just after Durable publishes the selected
        // commit. The test can stop the parent without holding the storage line.
        if (notice !== before && String(scope?.conversationId) === threadId && scope.taskId !== undefined && ctx.abortSignal) {
          reported.resolve();
          const signal = ctx.abortSignal, { promise, resolve } = Promise.withResolvers();
          signal.addEventListener("abort", resolve, { once: true });
          try { if (!signal.aborted) await Promise.race([promise, release.promise]); }
          finally { signal.removeEventListener("abort", resolve); }
          signal.throwIfAborted();
        }
        return value;
      };
      try {
        const receipt = ok(await f.wire.send("submit", { threadId, requestId: state, text: "Start a fresh thread", whenBusy: "reject" }));
        await Promise.race([reported.promise, delay(5000, undefined, { ref: false, signal: t.signal })
          .then(() => assert.fail(`start notice did not reach the ${state} fence`))]);
        ok(await f.wire.send("thread.abort", { threadId }));
        assert.equal(ok(await f.wire.send("submission.get", { threadId, submissionId: receipt.submissionId })).status, "unanswered");
        const { entries } = ok(await f.wire.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 50 }));
        const messages = entries.filter(e => e.kind === "hush.thread-start-notice").map(e => e.messages[0].blocks[0].text);
        const childId = /^Thread ([1-9][0-9]*) allocated/.exec(messages[0])?.[1];
        assert.ok(childId);
        assert.equal(messages[0], `Thread ${childId} allocated for input ${receipt.submissionId}; first prompt not yet admitted.`);
        const child = await f.wire.send("submission.get", { threadId: childId, requestId: "hush.thread-start" });
        if (state === "allocated") {
          assert.equal(messages.length, 1);
          assert.equal(child.error?.code, "not_found");
        } else {
          const admitted = ok(child);
          assert.equal(messages.length, 2);
          assert.equal(messages[1], `Thread ${childId} admitted its first prompt (${admitted.submissionId}) for input ${receipt.submissionId}.`);
          await settle(f.wire, childId, admitted);
        }
      } finally {
        release.resolve();
        h.commitWith = commit;
        observer.detach();
      }
    });
  }
});

test("thread start rechecks trust on a headless turn before creating a thread", { timeout: 18_000 }, async t => {
  const seen = Promise.withResolvers(), release = Promise.withResolvers();
  t.after(() => release.resolve());
  const f = await fixture(t, async turn => {
    if (turn.results.length) {
      assert.match(turn.results[0].content, /not trusted|not authorized|turn changed/i);
      return "The thread was not started.";
    }
    assert.ok(turn.tools.includes("thread_start") && !turn.tools.includes("thread_open"));
    seen.resolve();
    await release.promise;
    return [call("thread_start", { prompt: "This must never run." })];
  });
  const list = async () => ok(await f.wire.send("thread.list", { limit: 100 })).threads.map(row => row.id);
  const before = await list();
  const threadId = f.thread.id;
  const receipt = ok(await f.wire.send("submit", { threadId, text: "Start a fresh thread", requestId: "headless-source", whenBusy: "reject" }));
  await seen.promise;
  await f.trust.change(f.worktree, "revoke", t.signal);
  release.resolve();
  await settle(f.wire, threadId, receipt);
  assert.deepEqual(await list(), before);
  assert.equal(f.provider.requests.length, 2);
});

test("headless prompt reopens by ID in a plain TUI tab and an attach-only child", { timeout: 18_000 }, async t => {
  const first = "Seed an attachable thread without a TUI";
  let target;
  const f = await fixture(t, turn => {
    if (turn.text === first) {
      assert.ok(!turn.tools.includes("thread_open"));
      return "EXACT-TARGET-TRANSCRIPT";
    }
    if (turn.text === "Seed the originating tab") return "READY-ORIGIN-THREAD";
    assert.equal(turn.text, `Open thread ${target} here as a tab`);
    if (!turn.results.length) {
      assert.deepEqual(paneTools(turn), []);
      assert.ok(turn.tools.includes("thread_open"));
      assert.match(turn.prompt, /a side split is unavailable/);
      return [call("thread_open", { threadId: target, where: "tab" })];
    }
    assert.deepEqual(JSON.parse(turn.results[0].content), { outcome: "confirmed", threadId: target, where: "tab" });
    return "TAB-REGISTERED-HERE";
  });
  const run = await exec(binary, ["--socket", f.socket, "-p", "--no-wait", "--json", first], { cwd: f.worktree, env: f.env, timeout: 5_000 });
  const receipt = JSON.parse(run.stdout);
  target = receipt.threadId;
  assert.notEqual(target, f.thread.id);
  await settle(f.wire, target, receipt);
  await settle(f.wire, f.thread.id, ok(await f.wire.send("submit", { threadId: f.thread.id, requestId: "seed-origin",
    text: "Seed the originating tab", whenBusy: "reject" })));
  const list = async () => ok(await f.wire.send("thread.list", { limit: 100 })).threads.map(row => row.id);
  const before = await list();
  const script = fileURLToPath(new URL("./thread-view-pty.py", import.meta.url));
  await exec("python3", [script, binary, f.socket, f.thread.id, target, f.wire.hello.epoch],
    { cwd: f.worktree, env: { ...f.env, TERM: "xterm-256color" }, timeout: 14_000, signal: t.signal });
  assert.deepEqual(await list(), before, "opening either view must not create a thread");
  assert.equal(f.provider.requests.filter(turn => turn.text === first).length, 1, "opening either view must not start a run");
});

test("closing and reopening an active tab preserves its run", { timeout: 20_000 }, async t => {
  const live = "Always keep this target active", active = Promise.withResolvers(), release = Promise.withResolvers();
  let target;
  const f = await fixture(t, async turn => {
    if (turn.text === live) {
      active.resolve();
      await release.promise;
      return "LATE-TARGET-ANSWER";
    }
    if (turn.text === "Seed the originating tab") return "READY-ORIGIN-THREAD";
    assert.ok([`Open thread ${target} here as a tab`, `Reopen thread ${target} here as a tab`].includes(turn.text), turn.text);
    if (!turn.results.length) return [call("thread_open", { threadId: target, where: "tab" })];
    assert.deepEqual(JSON.parse(turn.results[0].content), { outcome: "confirmed", threadId: target, where: "tab" });
    return turn.text.startsWith("Reopen") ? "TAB-REREGISTERED-HERE" : "TAB-REGISTERED-HERE";
  });
  t.after(() => release.resolve());
  const origin = f.thread.id;
  await settle(f.wire, origin, ok(await f.wire.send("submit", { threadId: origin, requestId: "origin", text: "Seed the originating tab", whenBusy: "reject" })));
  target = ok(await f.wire.send("thread.new", { cwd: f.worktree })).id;
  const receipt = ok(await f.wire.send("submit", { threadId: target, requestId: "target-work", text: live, whenBusy: "reject" }));
  await active.promise;
  const flag = join(f.root, "tab-reopened");
  const script = fileURLToPath(new URL("./thread-view-pty.py", import.meta.url));
  const pty = exec("python3", [script, binary, f.socket, origin, target, f.wire.hello.epoch, flag],
    { cwd: f.worktree, env: { ...f.env, TERM: "xterm-256color" }, timeout: 14_000, signal: t.signal });
  try {
    await Promise.race([pty, until("target was not reopened", () => access(flag).then(() => true, () => false))]);
    assert.equal(ok(await f.wire.send("submission.get", { threadId: target, submissionId: receipt.submissionId })).status, "placed");
    release.resolve();
    await pty;
    await settle(f.wire, target, receipt);
    assert.equal(f.provider.requests.filter(turn => turn.text === live).length, 1, "view navigation must not restart the target");
  } finally { release.resolve(); }
});

async function until(label, read, done = Boolean) {
  const end = Date.now() + 6500;
  let found;
  do {
    found = await read();
    if (done(found)) return found;
    await delay(100);
  } while (Date.now() < end);
  assert.fail(label + (typeof found === "string" ? ": " + found.slice(-2000) : ""));
}

const rex = process.env.HUSH_TEST_REX;
test("live Rex: plain chat opens a terminal, command and fresh Hush pane", { skip: !rex, timeout: 40_000 }, async t => {
  const blank = "Open a new side pane", fresh = "Open a fresh Hush thread in a side pane";
  const command = "Run the live fixture in a pane below", inspect = "Inspect the live command pane";
  let opened;
  const f = await fixture(t, (turn, root) => {
    const view = turn.text === blank ? { mode: "terminal" } : turn.text === fresh ? { mode: "hush" } : undefined;
    if (view) {
      if (!turn.results.length) {
        assert.ok(paneTools(turn).includes("pane_open"));
        assert.match(turn.prompt, /Plain language is sufficient/);
        return [call("pane_open", { placement: "right", launch: view })];
      }
      assert.equal(JSON.parse(turn.results[0].content).outcome, "confirmed");
      return `Opened ${view.mode} pane.`;
    }
    if (turn.text === command) {
      if (!turn.results.length) {
        assert.ok(paneTools(turn).includes("pane_open"));
        return [call("pane_open", { placement: "below", launch: { mode: "argv", program: process.execPath,
          args: ["-e", "const fs = require('node:fs'); fs.writeFileSync(process.argv[1], JSON.stringify([process.cwd(), ...process.argv.slice(2)])); console.log('REX_PANE_COMMAND_MARKER')",
            join(root, "command.json"), ...fixtureArguments] } })];
      }
      opened = JSON.parse(turn.results[0].content);
      assert.equal(opened.outcome, "confirmed");
      return "Command pane created.";
    }
    assert.equal(turn.text, inspect);
    assert.ok(paneTools(turn).includes("pane_read"));
    switch (turn.results.length) {
      case 0: return [call("pane_list")];
      case 1:
        assert.ok(JSON.parse(turn.results[0].content).panes.includes(opened.pane));
        return [call("pane_read", { pane: opened.pane, lines: 3 })];
      case 2:
        assert.match(JSON.parse(turn.results[1].content).text, /REX_PANE_COMMAND_MARKER/);
        return [call("pane_focus", { pane: opened.pane })];
      default:
        assert.deepEqual(JSON.parse(turn.results[2].content), { outcome: "confirmed", pane: opened.pane });
        return "Exact command pane inspected.";
    }
  });
  const invoke = async (...args) => (await exec(rex, ["--autostart=false", "--timeout=3s", ...args], { timeout: 5_000 })).stdout;
  const who = JSON.parse(await invoke("whoami", "--json"));
  assert.equal(who.principal.transport, "unix");
  const callRex = (...args) => invoke("-S", who.endpoint, ...args);
  let session;
  try {
    const origin = JSON.parse(await callRex("new", "hush-pane-workflow-smoke", "--cwd", f.cwd, "--keep-open", "--shell=none", "--json", "--",
      "/usr/bin/env", "-u", "TMUX", "-u", "ZELLIJ", "-u", "CMUX_SURFACE_ID", "TERM_PROGRAM=rex", `REX_BIN_DIR=${dirname(rex)}`,
      "SHELL=/bin/sh", `XDG_STATE_HOME=${f.env.XDG_STATE_HOME}`, binary, "--socket", f.socket, "--local", "--motion=false", "--thread", f.thread.id));
    session = origin.session_id;
    const window = origin.initial_windows[0].window_id, block = origin.initial_windows[0].block_ids[0];
    const args = ["-s", session, "-w", window];
    const capture = (id = block) => callRex("capture", ...args, "-b", id, "--format=text", "--trim");
    const enter = async (text, id = block) => {
      await callRex("send", ...args, "-b", id, "--", text);
      await callRex("send-key", ...args, "-b", id, "enter");
    };
    const list = async () => JSON.parse(await callRex("block", "ls", ...args, "--short=false", "--json")).blocks.map(b => b.block_id);
    const added = (baseline, name) => until(name, async () => (await list()).filter(id => !baseline.includes(id)), rows => rows.length === 1).then(rows => rows[0]);
    const inspectSession = async () => JSON.parse(await callRex("session", "inspect", session, "--json"));
    const closeBlock = id => callRex("block", "close", ...args, id);

    await until("origin did not attach", () => capture(), text => /chat|hush/i.test(text));
    await enter(blank);
    const blankBlock = await added([block], "plain chat did not open a split");
    await until("idle split not confirmed", () => capture(), text => text.includes("Opened terminal pane."));
    await enter("pwd -P", blankBlock);
    await until("idle shell did not use the thread's cwd", () => capture(blankBlock), text => text.includes(f.worktree));
    await closeBlock(blankBlock);
    await enter(command);
    const commandBlock = await added([block], "command did not open a split");
    await until("command output missing", () => capture(commandBlock), text => text.includes("REX_PANE_COMMAND_MARKER"));
    assert.deepEqual(JSON.parse(await readFile(join(f.root, "command.json"), "utf8")), [f.worktree, ...fixtureArguments]);
    await until("first turn did not settle", () => capture(), text => text.includes("Command pane created."));
    await enter(inspect);
    await until("pane inspections did not settle", () => capture(), text => text.includes("Exact command pane inspected."));
    assert.equal((await inspectSession()).windows[0].focused_block_id, commandBlock);
    await closeBlock(commandBlock);
    t.diagnostic("Rex command used the worktree and preserved argv; list, exact unfocused read, and exact focus passed.");

    const threadList = async () => ok(await f.wire.send("thread.list", { limit: 100 })).threads;
    const existing = new Set((await threadList()).map(th => th.id));
    await enter(fresh);
    const freshBlock = await added([block], "fresh Hush did not open a split");
    const [child] = await until("the same backend did not create one new thread",
      async () => (await threadList()).filter(th => !existing.has(th.id)), rows => rows.length === 1);
    assert.equal(child.parent, undefined); assert.equal(child.owner, undefined);
    const snapshot = ok(await f.wire.send("subscribe", { threadId: child.id }));
    assert.equal(snapshot.state.agent.cwd, f.worktree);
    assert.deepEqual(snapshot.state.window.entries, []);
    await until("fresh Hush did not attach", () => capture(freshBlock), text => /chat|hush/i.test(text));
    t.diagnostic("Rex fresh pane attached to the same backend and created a separate empty thread in the worktree.");
  } finally {
    if (session) await callRex("kill", session);
  }
});
