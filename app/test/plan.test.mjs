import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readdir, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { join } from "node:path";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { promisify } from "node:util";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { startBackend } from "../../dist/hush-backend.mjs";
import { dial } from "../../server/test/client.mjs";
import { call, createRequest, draftArgs, edits, editDraft, executionRequest, fileSetDraft, fileSetReference, fileSetRequest, planProvider, preparedFiles, reviewReport, scopedReply } from "../../backend/test/plan-provider.mjs";

const exec = promisify(execFile);
const repo = fileURLToPath(new URL("../../", import.meta.url));
const script = fileURLToPath(new URL("./plan-pty.py", import.meta.url));
const okay = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };

async function terminalPlan(t, mode) {
  const historical = mode === "historical";
  const faux = mode === "faux" || historical, scoped = mode === "scoped", batch = mode === "fileset", preview = mode === "preview";
  const startup = mode === "startup", chat = mode === "chat";
  const promptText = preview ? "Lets work on creating a new plan to support subagents, independent reviews etc."
    : batch ? fileSetRequest : scoped ? executionRequest : createRequest;
  const previewDraft = { baseRevision: 0, title: "Subagents and independent reviews", steps: [
    { id: "join", title: "Join worker results", description: "Wait only when results are needed.", checks: ["A failed worker does not hide another report"] },
  ] };
  const startingFiles = batch ? { ...preparedFiles, ...fileSetReference } : scoped ? preparedFiles : {};
  const home = await realpath(await mkdtemp("/tmp/hush-plan-pty-"));
  const agentDir = join(home, ".hush"), dataDir = join(home, "data"), cwd = join(home, "work");
  const socket = join(dataDir, "hush.sock");
  let host, peer, observer, threadId;
  const previousHelper = process.env.HUSH_FILE_LIST_HELPER;
  try {
    await Promise.all([mkdir(agentDir, { mode: 0o700 }), mkdir(cwd, { mode: 0o700 })]);
    const binary = process.env.HUSH_TEST_CLI || join(home, "hush");
    if (!process.env.HUSH_TEST_CLI) await exec("go", ["build", "-o", binary, "./app/cmd/hush"], { cwd: repo, timeout: 25_000 });
    await new WorkspaceTrust(agentDir).change(cwd, "grant", AbortSignal.timeout(5_000));
    if (scoped || batch) {
      process.env.HUSH_FILE_LIST_HELPER = binary;
      await Promise.all(Object.entries(startingFiles).map(([path, content]) => writeFile(join(cwd, path), content)));
    } else delete process.env.HUSH_FILE_LIST_HELPER;
    const provider = faux ? undefined : await planProvider(t, agentDir, async turn => {
      if (chat && turn.text.startsWith("Plan implementation\n")) {
        const assignment = JSON.parse(turn.text.split("\n")[1]);
        assert.deepEqual(assignment.selected, ["restore"]);
        assert.ok(turn.tools.includes("write") && turn.tools.includes("hush_plan_report"));
        assert.ok(!turn.messages.some(m => m.content === "keep this draft" || m.content === createRequest));
        return turn.results.length ? "Restored the preferences." : [call("hush_plan_report", {
          tasks: [{ stepId: "restore", state: "done", detail: "Checked the settings round-trip." }],
        })];
      }
      if (mode === "draft" && turn.text.startsWith("Plan review brief\n")) {
        assert.deepEqual(turn.tools, []);
        assert.equal(turn.messages.filter(m => m.role === "user").length, 1);
        const brief = JSON.parse(turn.text.slice("Plan review brief\n".length));
        assert.deepEqual(brief.requirements, [promptText]);
        assert.deepEqual(brief.plan.steps, draftArgs().steps);
        return JSON.stringify(reviewReport);
      }
      if (batch && turn.text.startsWith("Plan assignment\n")) {
        const { id, scope } = JSON.parse(turn.text.slice("Plan assignment\n".length));
        assert.equal(id, "profiles");
        assert.deepEqual(scope, fileSetDraft().steps[0].scope);
        assert.deepEqual(turn.tools.toSorted(), ["hush_plan_read_file", "hush_plan_stage_file"]);
        if (!turn.results.length) return [call("hush_plan_stage_file", { path: "save.txt", text: "saved=mocha\n" }),
          call("hush_plan_stage_file", { path: "restore.txt", text: "restored=mocha\n" })];
        return "Both file proposals are ready.";
      }
      if (scoped && typeof turn.text === "string" && turn.text.startsWith("Plan assignment\n")) {
        const { id } = JSON.parse(turn.text.slice("Plan assignment\n".length));
        assert.notEqual(id, "notes", "an unselected task must not get a turn");
        if (id === "restore" && !turn.results.length) {
          assert.equal(await readFile(join(cwd, edits.save.path), "utf8"), edits.save.newText + "\n");
          const { progress } = okay(await observer.send("ext.plan.wait", { threadId }));
          assert.equal(progress.find(p => p.stepId === "save")?.state, "verified");
        }
        return scopedReply(turn);
      }
      assert.equal(turn.text, promptText);
      assert.match(turn.prompt, /This request is for a draft only/);
      if (preview) {
        assert.deepEqual(turn.tools.toSorted(), ["hush_plan_current", "hush_plan_preview", "read"]);
        return turn.results.length ? "Preview shown, not saved." : [call("hush_plan_preview", previewDraft)];
      }
      assert.ok(turn.tools.includes("hush_plan_propose") && !turn.tools.includes("write"));
      assert.deepEqual((await readdir(cwd)).sort(), Object.keys(startingFiles).sort());
      if (scoped || batch) for (const [path, content] of Object.entries(startingFiles)) {
        assert.equal(await readFile(join(cwd, path), "utf8"), content, "drafting cannot change a file");
      }
      return turn.results.length ? turn.results[0].content : [call("hush_plan_propose", batch ? fileSetDraft() : scoped ? editDraft() : draftArgs())];
    });
    host = await startBackend({ faux, model: provider?.model, ephemeral: false, idleMs: 120_000, cwd, agentDir, dataDir, socket });
    peer = await dial(socket);
    okay(await peer.send("hello", { versions: [1], capabilities: { plan: [1], planWorkspace: [1], planReview: [1], planChat: [1] } }));
    threadId = okay(await peer.send("thread.new", {})).id;
    if (scoped) {
      observer = await dial(socket);
      okay(await observer.send("hello", { versions: [1], capabilities: { plan: [1] } }));
    }
    let original, historicalAnchor;
    if (faux) {
      const { submissionId } = okay(await peer.send("submit", { threadId, text: "!plan-fixture", requestId: "prepare-pty", whenBusy: "reject" }));
      const seeded = okay(await peer.send("submission.wait", { threadId, submissionId }));
      assert.equal(seeded.status, "done");
      if (historical) {
        historicalAnchor = seeded.answerEntryId;
        original = threadId;
        const next = okay(await peer.send("submit", { threadId, text: "!plan-fixture-revise", requestId: "prepare-new-head", whenBusy: "reject" }));
        assert.equal(okay(await peer.send("submission.wait", { threadId, submissionId: next.submissionId })).status, "done");
        threadId = okay(await peer.send("thread.fork", { threadId, atEntryId: historicalAnchor })).id;
      }
    }
    let source, threads;
    const list = async () => okay(await peer.send("thread.list", { limit: 32 })).threads;
    if (startup || chat) {
      const { submissionId } = okay(await peer.send("submit", { threadId, text: promptText, requestId: "prepare-live-plan", whenBusy: "reject" }));
      assert.equal(okay(await peer.send("submission.wait", { threadId, submissionId })).status, "done");
      source = okay(await peer.send("ext.plan.wait", { threadId })).plan.id;
      if (chat) threadId = okay(await peer.send("thread.new", {})).id;
      threads = await list();
    }
    await exec("python3", [script, binary, socket, threadId, mode, promptText], { cwd, timeout: 16_000,
      env: { HOME: home, PATH: process.env.PATH, TERM: "xterm-256color", LANG: "C.UTF-8" } });
    if (historical) {
      const live = okay(await peer.send("ext.plan.wait", { threadId: original }));
      const pin = okay(await peer.send("ext.plan.wait", { threadId }));
      assert.equal(live.plan.revision, 2);
      assert.equal(pin.plan.revision, 1);
      assert.equal(pin.runAvailability, "pinned");
      assert.equal(pin.run, null);
      assert.deepEqual(pin.progress, []);
      assert.deepEqual(okay(await peer.send("ext.planWorkspace.history", { threadId: original, planId: live.plan.id })).runs, []);
      const entries = okay(await peer.send("history", { threadId, limit: 32, range: { kind: "before", beforeEntryId: null } })).entries;
      assert.equal(entries.at(-1).id, historicalAnchor, "actions in a pinned view did not create an input");
      return;
    }
    if (startup || chat) {
      assert.deepEqual(await list(), threads, "browsing and running in an active thread don't create another thread");
      const view = okay(await peer.send("ext.planWorkspace.inspect", { threadId, planId: source }));
      if (startup) assert.equal(view.run, null, "startup browsing didn't admit work");
      else {
        assert.equal(view.run.state, "done");
        assert.equal(view.run.ownerThreadId, threadId);
        assert.deepEqual(view.run.stepIds, ["restore"]);
        assert.deepEqual(view.progress, []);
        assert.deepEqual(view.reports.map(r => [r.stepId, r.state]), [["restore", "done"]]);
        const { entries } = okay(await peer.send("history", { threadId, limit: 32, range: { kind: "before", beforeEntryId: null } }));
        const inputs = entries.filter(e => e.kind === "hush.plan.chat");
        assert.deepEqual(inputs.map(e => e.threadId), [threadId]);
        assert.equal(inputs[0].messages[0].blocks[0].text, "Plan run · r1 · Interface preferences · 1 step");
        assert.ok(!entries.some(e => e.kind === "pi.user"));
      }
      return;
    }
    const current = okay(await peer.send("ext.plan.wait", { threadId }));
    if (faux || scoped) {
      assert.equal(current.run.state, "verified");
      assert.deepEqual(current.run.stepIds, ["save", "restore"]);
      assert.deepEqual(current.progress.map(p => p.state), ["verified", "verified"]);
    } else if (batch) {
      assert.equal(current.run.state, "verified");
      assert.deepEqual(current.run.stepIds, ["profiles"]);
      assert.deepEqual(current.progress.map(p => p.evidence), [["Isolated acceptance passed: Both profiles match the reference (2 files)"]]);
    } else {
      assert.equal(current.runAvailability, "disabled");
      assert.equal(current.run, null);
      assert.deepEqual(current.progress, []);
      assert.deepEqual(current.plan.steps, preview ? previewDraft.steps : draftArgs().steps);
    }
    const history = async id => okay(await peer.send("history", { threadId: id, limit: 64, range: { kind: "before", beforeEntryId: null } }));
    const { entries } = await history(threadId);
    assert.equal(entries.filter(e => e.kind === "pi.user").length, 1, "Enter admits a typed plan turn, not a normal chat submission");
    if (faux) {
      const { runs } = okay(await peer.send("ext.planWorkspace.history", { threadId, planId: current.plan.id }));
      assert.equal(runs.length, 1);
      assert.equal(runs[0].id, current.run.id);
      assert.equal(runs[0].threadId, threadId, "the fixture was launched from the active plan thread");
    }
    if (preview) {
      const { plans } = okay(await peer.send("ext.planWorkspace.wait", { threadId, page: 0 }));
      assert.deepEqual(plans.map(p => [p.title, p.revision]), [[previewDraft.title, 1]]);
      assert.equal(entries.filter(e => e.kind === "hush.plan.preview-saved").length, 1);
      assert.equal(provider.requests.length, 2, "confirming the preview must not start another model turn");
    }
    if (mode === "draft") {
      assert.equal(provider.requests.filter(r => r.text.startsWith("Plan review brief\n")).length, 1);
      const view = okay(await peer.send("ext.planReview.wait", { threadId, planId: current.plan.id, revision: 1 }));
      assert.equal(view.review.state, "complete");
      assert.deepEqual(view.review.report.findings, reviewReport.findings);
      assert.ok(entries.some(e => e.kind === "hush.plan.review" && e.messages[0].blocks[0].text.includes(reviewReport.findings[0].message)));
    }
    if (scoped) {
      assert.equal(current.runAvailability, "scoped");
      const taskEntries = entries.filter(e => e.kind === "hush.plan.task");
      assert.deepEqual(taskEntries.map(e => e.id), current.progress.map(p => p.entryId));
      assert.ok(taskEntries.every(e => e.threadId === threadId));
      assert.ok(current.progress.every(p => p.workerThreadId === threadId));
      const receipt = entries.filter(e => e.kind === "hush.plan.result").map(e => e.messages[0].blocks[0].text);
      assert.deepEqual(receipt, ["Literal check passed: save.txt", "Literal check passed: restore.txt", "Plan run verified · 2/2 verified"]);
      const tools = entries.flatMap(e => e.messages).filter(m => m.role === "toolResult").map(m => m.name);
      assert.deepEqual(tools.slice(-4), ["hush_plan_read_file", "hush_plan_replace_text", "hush_plan_read_file", "hush_plan_replace_text"]);
      assert.deepEqual(current.progress.map(p => p.effectiveThinking), ["off", "off"]);
      assert.deepEqual(current.progress.map(p => p.evidence), [["Literal check passed: save.txt"], ["Literal check passed: restore.txt"]]);
      assert.deepEqual((await readdir(cwd)).sort(), Object.keys(preparedFiles).sort());
      for (const [id, { path, newText }] of Object.entries(edits)) {
        assert.equal(await readFile(join(cwd, path), "utf8"), id === "notes" ? preparedFiles[path] : newText + "\n");
      }
    } else if (batch) {
      assert.deepEqual((await readdir(cwd)).sort(), Object.keys(startingFiles).sort());
      for (const [path, text] of Object.entries(startingFiles)) {
        assert.equal(await readFile(join(cwd, path), "utf8"), /^(save|restore)\.txt$/.test(path) ? text.replace("latte", "mocha") : text);
      }
    } else assert.deepEqual(await readdir(cwd), [], "the workspace must remain untouched");
  } finally {
    observer?.close();
    peer?.close();
    if (previousHelper === undefined) delete process.env.HUSH_FILE_LIST_HELPER;
    else process.env.HUSH_FILE_LIST_HELPER = previousHelper;
    try { await host?.close(); } finally { await rm(home, { recursive: true, force: true }); }
  }
}

test("terminal UI browses plans at startup without creating a thread or submitting the draft", { timeout: 45_000 }, t => terminalPlan(t, "startup"));
test("terminal UI runs an unbound dependent task here without touching the composer", { timeout: 45_000 }, t => terminalPlan(t, "chat"));
test("terminal UI runs the scoped fixture selection", { timeout: 45_000 }, t => terminalPlan(t, "faux"));
test("terminal UI cannot run a fork's historical pin", { timeout: 45_000 }, t => terminalPlan(t, "historical"));
test("terminal UI opens plans from a different Git worktree at startup", { timeout: 45_000 }, async t => {
  const home = await realpath(await mkdtemp("/tmp/hush-plan-catalog-"));
  const cwd = join(home, "source-checkout"), worktree = join(home, "other-worktree");
  const agentDir = join(home, ".hush"), dataDir = join(home, "data"), socket = join(dataDir, "hush.sock");
  let host, peer;
  t.after(async () => { peer?.close(); await host?.close(); await rm(home, { recursive: true, force: true }); });
  await exec("git", ["init", "-q", cwd]);
  await exec("git", ["-c", "user.name=Hush Fixture", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false",
    "commit", "--allow-empty", "-qm", "fixture"], { cwd });
  await exec("git", ["worktree", "add", "--detach", worktree], { cwd });
  await mkdir(agentDir, { mode: 0o700 });
  const trust = new WorkspaceTrust(agentDir);
  await trust.change(cwd, "grant", t.signal);
  await trust.change(worktree, "grant", t.signal);
  const provider = await planProvider(t, agentDir, async turn => {
    assert.equal(turn.text, createRequest);
    return turn.results.length ? "Plan saved." : [call("hush_plan_propose", draftArgs())];
  });
  const binary = process.env.HUSH_TEST_CLI || join(home, "hush");
  if (!process.env.HUSH_TEST_CLI) await exec("go", ["build", "-o", binary, "./app/cmd/hush"], { cwd: repo, timeout: 25_000 });
  host = await startBackend({ model: provider.model, ephemeral: false, idleMs: 30_000, cwd, agentDir, dataDir, socket });
  peer = await dial(socket);
  const rpc = async (method, params) => okay(await peer.send(method, params));
  await rpc("hello", { versions: [1], capabilities: { plan: [1], planWorkspace: [1], planChat: [1], planCatalog: [1] } });
  const { id: threadId } = await rpc("thread.new", {});
  const { submissionId } = await rpc("submit", { threadId, text: createRequest, requestId: "prepare-pty", whenBusy: "reject" });
  assert.equal((await rpc("submission.wait", { threadId, submissionId })).status, "done");
  const before = await rpc("thread.list", { limit: 32 });
  const requests = provider.requests.length;
  await exec("python3", [script, binary, socket, threadId, "catalog", ""], { cwd: worktree, timeout: 16_000,
    env: { HOME: home, PATH: process.env.PATH, TERM: "xterm-256color", LANG: "C.UTF-8" } });
  const current = await rpc("ext.plan.wait", { threadId });
  assert.equal(current.run, null);
  assert.equal(current.closed, undefined);
  assert.deepEqual(await rpc("thread.list", { limit: 32 }), before, "browsing and opening a foreign plan do not create a thread");
  assert.equal(provider.requests.length, requests, "read-only keys neither run the plan nor send the draft");
});
test("terminal UI saves an unsaved plan preview with 'ok save it'", { timeout: 45_000 }, t => terminalPlan(t, "preview"));
test("terminal UI saves and independently reviews a plan without the scoped file helper", { timeout: 45_000 }, t => terminalPlan(t, "draft"));
test("explicit scoped Run verifies exactly two real edits in the initiating thread", { timeout: 45_000 }, t => terminalPlan(t, "scoped"));

test("terminal UI authorizes one checked file set and leaves other files untouched", { timeout: 45_000 }, t => terminalPlan(t, "fileset"));


test("terminal UI distinguishes Run here, leaving a checkout idle, and starting after backend trust", { timeout: 35_000 }, async t => {
  const home = await realpath(await mkdtemp("/tmp/hush-plan-pty-wt-"));
  const cwd = join(home, "project"), agentDir = join(home, ".hush");
  const dataDir = join(home, "worktree state path with spaces"), socket = join(dataDir, "hush.sock");
  const grant = join(home, "granted");
  let peer, host, naming = 0;
  t.after(async () => { peer?.close(); await host?.close(); await rm(home, { recursive: true, force: true }); });
  await Promise.all([agentDir, cwd].map(path => mkdir(path, { mode: 0o700 })));
  const git = (...args) => exec("git", args, { cwd });
  await git("init", "-q");
  await git("-c", "user.name=Hush Fixture", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "fixture");
  const trust = new WorkspaceTrust(agentDir);
  await trust.change(cwd, "grant", t.signal);
  const provider = await planProvider(t, agentDir, async turn => {
    if (turn.text === createRequest) return turn.results.length ? "Saved." : [call("hush_plan_propose", draftArgs())];
    if (turn.text.startsWith("Plan worktree name\n")) return ++naming === 1 ? "Idle checkout" : "Preference round trip";
    assert.match(turn.text, /^Plan implementation\n/);
    assert.deepEqual(JSON.parse(turn.text.split("\n")[1]).selected, ["restore"]);
    assert.equal(turn.messages.some(m => m.content === createRequest), naming === 0, "only Run here sees the source transcript");
    return turn.results.length ? "Done." : [call("hush_plan_report", {
      tasks: [{ stepId: "restore", state: "done", detail: "Checked the round-trip." }],
    })];
  });
  const binary = process.env.HUSH_TEST_CLI || join(home, "hush");
  if (!process.env.HUSH_TEST_CLI) await exec("go", ["build", "-o", binary, "./app/cmd/hush"], { cwd: repo, timeout: 25_000 });
  host = await startBackend({ faux: false, model: provider.model, ephemeral: false, idleMs: 30_000, cwd, agentDir, dataDir, socket });
  peer = await dial(socket);
  const rpc = async (method, args) => okay(await peer.send(method, args));
  await rpc("hello", { versions: [1], capabilities: { plan: [1], planChat: [1], planWorkspace: [1], planWorktree: [1], threadDirectory: [2] } });
  const { id: threadId } = await rpc("thread.new", {});
  const { submissionId } = await rpc("submit", { threadId, text: createRequest, requestId: "draft", whenBusy: "reject" });
  assert.equal((await rpc("submission.wait", { threadId, submissionId })).status, "done");
  const { plan } = await rpc("ext.plan.wait", { threadId });
  const history = () => rpc("ext.planWorkspace.history", { threadId, planId: plan.id });
  const operator = async () => {
    for (let i = 0; i < 500; i++) {
      if (await readFile(grant + ".reviewed").then(() => true, () => false)) {
        const { threads } = await rpc("ext.threadDirectory.list", { limit: 10 });
        const idle = threads.find(row => row.title === "Idle checkout");
        const work = threads.find(row => row.title === "Preference round trip");
        if (idle?.checkout?.kind === "managed" && work?.checkout?.kind === "managed") {
          assert.equal(idle.status, "idle");
          assert.equal((await trust.review(idle.checkout.cwd)).trusted, false);
          assert.equal((await trust.review(work.checkout.cwd)).trusted, false);
          assert.deepEqual((await history()).runs.map(r => r.threadId), [threadId], "neither preparation nor a denied start admits work");
          await trust.change(work.checkout.cwd, "grant", t.signal);
          await writeFile(grant, "granted");
          return work.id;
        }
      }
      await delay(20, undefined, { signal: t.signal });
    }
    assert.fail("no prepared worktree in the thread directory");
  };
  const [worktree] = await Promise.all([operator(), exec("python3", [script, binary, socket, threadId, "worktree", grant], { cwd, timeout: 22_000,
    env: { HOME: home, PATH: process.env.PATH, TERM: "xterm-256color", LANG: "C.UTF-8" } })]);
  const { runs } = await history();
  assert.deepEqual(runs.map(run => [run.threadId, run.state, run.stepIds, run.worktree]),
    [[worktree, "done", ["restore"], true], [threadId, "done", ["restore"], undefined]]);
});
