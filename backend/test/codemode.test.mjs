import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import { copyFile, link, mkdir, mkdtemp, readdir, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { DatabaseSync } from "node:sqlite";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { pathToFileURL } from "node:url";
import { assertFrame } from "../../protocol/v1/replay.mjs";
import { dial } from "../../server/test/client.mjs";
import { WorkspaceWorkTrust } from "../../dist/backend/src/workspace-trust.js";

const bundle = new URL("../../dist/hush-backend.mjs", import.meta.url);
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const range = { kind: "before", beforeEntryId: null };
const submit = async (wire, threadId, text, requestId) => ok(await wire.send("submit", { threadId, text, requestId, whenBusy: "reject" }));
const settle = async (wire, threadId, submissionId) => ok(await wire.send("submission.wait", { threadId, submissionId }));
const inspectionTasks = db => db.prepare("select status from tasks where json_extract(record, '$.input.call.arguments.path') = ?");

async function setup(t) {
  const root = await realpath(await mkdtemp("/tmp/hcm-"));
  const home = join(root, "home"), cwd = join(root, "workspace"), tmp = join(root, "tmp");
  const agent = join(home, ".hush"), pi = join(home, ".pi", "agent");
  const entry = join(root, "standalone.mjs");
  const backends = [];
  t.after(async () => {
    try {
      await Promise.all(backends.map(async ({ child, exit }) => { child.kill("SIGKILL"); await exit; }));
    } finally { await rm(root, { recursive: true, force: true }); }
  });
  for (const dir of [cwd, tmp, agent, pi, join(cwd, ".hush"), join(cwd, ".pi")])
    await mkdir(dir, { mode: 0o700, recursive: true });
  await copyFile(bundle, entry);
  const trust = new WorkspaceWorkTrust(agent);
  await trust.change(cwd, "grant", AbortSignal.timeout(5000));
  const save = (path, data) => writeFile(path, data, { mode: 0o600 });
  async function settings(value, dir = agent) { await save(join(dir, "settings.json"), JSON.stringify({ codemode: value })); }
  const storage = join(agent, "runtime");
  async function boot({ privateRun = false, profile = "hush", preload } = {}) {
    const dataDir = profile === "hush" ? storage : join(pi, "hush");
    const socket = privateRun ? join(tmp, "p.sock") : join(dataDir, "hush.sock");
    const args = [...(preload ? ["--import", pathToFileURL(preload).href] : []),
      entry, "--profile", profile, "--faux", "--cwd", cwd, ...(privateRun ? ["--no-session", "--socket", socket] : [])];
    const child = spawn(process.execPath, args, { cwd, env: { HOME: home, PATH: "/usr/bin:/bin", TMPDIR: tmp, NODE_NO_WARNINGS: "1" },
      stdio: ["ignore", "pipe", "pipe"] });
    let stderr = "";
    child.stderr.on("data", chunk => { stderr += chunk; });
    const exit = once(child, "exit");
    backends.push({ child, exit });
    const stdout = createInterface({ input: child.stdout })[Symbol.asyncIterator]();
    const ready = await stdout.next();
    assert.deepEqual(JSON.parse(ready.value ?? "null"), { ready: true, socket }, stderr);
    const wire = await dial(socket, assertFrame);
    const hello = ok(await wire.send("hello", { versions: [1] }));
    assert.equal(hello.version, 1);
    async function stop(signal = "SIGTERM") {
      child.kill(signal);
      assert.deepEqual(await exit, signal === "SIGKILL" ? [null, "SIGKILL"] : [0, null], stderr);
      await wire.ended;
    }
    return { wire, hello, stop, stdout };
  }
  return { root, cwd, tmp, storage, boot, save, settings, pi, trust };
}

async function thread(wire, cwd) { return ok(await wire.send("thread.new", cwd ? { cwd } : {})).id; }
async function history(wire, threadId) { return ok(await wire.send("history", { threadId, range, limit: 20 })).entries; }
async function script(wire, threadId, code, requestId = "script") {
  const { submissionId } = await submit(wire, threadId, "!codemode\n" + code, requestId);
  assert.equal((await settle(wire, threadId, submissionId)).status, "done");
  const entries = await history(wire, threadId);
  const result = entries.slice(entries.findLastIndex(e => e.kind === "pi.user") + 1)
    .find(e => e.kind === "pi.tool-result")?.messages[0];
  assert.equal(result?.name, "codemode");
  return { ...result, text: result.blocks.map(b => b.text).join("\n") };
}

const batch = `const parts = await Promise.all(['one.txt', 'two.txt'].map(path => tools.inspect_file({path})));\n` +
  `return parts.map(part => part.split('\\n')[1]).join(' ');`;

test("host-only opt-in, private and Pi unchanged, and standalone workspace batching", { timeout: 20_000 }, async t => {
  const { cwd, boot, save, settings, pi } = await setup(t);
  await settings(true, join(cwd, ".hush"));
  await settings(true, join(cwd, ".pi"));
  await save(join(cwd, "one.txt"), "not selected\none\n");
  await save(join(cwd, "two.txt"), "not selected\ntwo\n");
  const off = await boot();
  const id = await thread(off.wire);
  const denied = await script(off.wire, id, batch);
  assert.equal(denied.isError, true);
  assert.match(denied.text, /not offered|not available/i);
  await off.stop();

  await settings(true);
  const privateRun = await boot({ privateRun: true });
  const privateId = ok(await privateRun.wire.send("thread.list", { limit: 1 })).threads[0].id;
  const submission = await submit(privateRun.wire, privateId, "!codemode\n" + batch, "private");
  assert.equal((await settle(privateRun.wire, privateId, submission.submissionId)).status, "done");
  assert.ok(!(await history(privateRun.wire, privateId)).some(e => e.kind === "pi.tool-result"));
  await privateRun.stop();

  await settings(true, pi);
  const compatible = await boot({ profile: "pi" });
  const deniedPi = await script(compatible.wire, await thread(compatible.wire), batch);
  assert.equal(deniedPi.isError, true);
  assert.match(deniedPi.text, /not offered|not available/i);
  await compatible.stop();

  const on = await boot();
  const result = await script(on.wire, id, batch, "enabled-on-existing-thread");
  assert.equal(result.isError, false, result.text);
  assert.match(result.text, /one two/);
  assert.doesNotMatch(result.text, /not selected/);
  await on.stop();
  await settings(false);
  const disabled = await boot();
  assert.equal((await script(disabled.wire, id, batch, "disabled-on-existing-thread")).isError, true);
  await disabled.stop();
});

test("Codemode only inspects bounded, regular workspace text files", { timeout: 15_000 }, async t => {
  const { root, cwd, boot, save, settings, trust } = await setup(t);
  const outside = join(root, "outside"), sentinel = "outside-not-for-codemode";
  await mkdir(outside);
  await Promise.all([save(join(outside, "secret"), sentinel), save(join(cwd, "valid"), "safe"),
    save(join(cwd, "too-big"), "a".repeat(32 * 1024 + 1)), save(join(cwd, "binary"), Buffer.from([0xff, 0, 0xcf]))]);
  await Promise.all([symlink(outside, join(cwd, "link-dir")), symlink(join(outside, "secret"), join(cwd, "link-file")),
    symlink(join(cwd, "valid"), join(cwd, "link-inside")), link(join(outside, "secret"), join(cwd, "hard-link"))]);
  execFileSync("mkfifo", [join(cwd, "fifo")]);
  await settings(true);
  const { wire } = await boot();
  const id = await thread(wire);
  const calls = ["bash", "write", "edit", "read", "codemode", "mcp__fixture__read", "hush_fixture_fast", "pi.codemode.classify"];
  const restricted = await script(wire, id, `
    return { inspect: typeof tools.inspect_file, denied: ${JSON.stringify(calls)}.every(name => !(name in tools)),
      models: typeof models, node: typeof process, network: typeof fetch, tools: ALL_TOOLS.map(tool => tool.name) };
  `);
  assert.equal(restricted.isError, false, restricted.text);
  assert.match(restricted.text, /{"inspect":"function","denied":true,"models":"undefined","node":"undefined","network":"undefined","tools":\["inspect_file"\]}/);
  const attempted = await script(wire, id, `return await tools.bash({command: 'printf unexpected'});`, "no-shell");
  assert.equal(attempted.isError, true);
  const paths = ["../outside/secret", join(outside, "secret"), "link-dir/secret", "link-file", "link-inside", "hard-link",
    "too-big", "binary", "fifo", ".", "missing"];
  // Keep the batch under the host's call ceiling, including the permitted file.
  const inspected = await script(wire, id, `
    const outcomes = await Promise.allSettled(${JSON.stringify([...paths, "valid"])}.map(path => tools.inspect_file({path})));
    return outcomes.map(outcome => outcome.status === 'fulfilled' ? outcome.value : 'denied').join(',');`, "bounded");
  assert.equal(inspected.isError, false, inspected.text);
  assert.match(inspected.text, new RegExp("denied,".repeat(paths.length) + "safe"));
  assert.doesNotMatch(inspected.text, new RegExp(sentinel));
  // The inspection root is the thread cwd, not the backend's startup cwd.
  await mkdir(join(cwd, "sub"));
  await trust.change(join(cwd, "sub"), "grant", AbortSignal.timeout(5000));
  await trust.change(outside, "grant", AbortSignal.timeout(5000));
  await save(join(cwd, "sub", "valid"), "thread-root");
  const other = await script(wire, await thread(wire, join(cwd, "sub")), `return await tools.inspect_file({path: 'valid'});`);
  assert.match(other.text, /thread-root/);
  const linkedWorkspace = await script(wire, await thread(wire, join(cwd, "link-dir")),
    `return await tools.inspect_file({path: 'secret'});`);
  assert.equal(linkedWorkspace.isError, true);
  assert.doesNotMatch(linkedWorkspace.text, new RegExp(sentinel));
});

test("Codemode caps output and wall time, cancels spinning code, and admits the next run", { timeout: 15_000 }, async t => {
  const { cwd, tmp, storage, boot, save, settings } = await setup(t);
  await settings(true);
  await save(join(cwd, "file.txt"), "inspection-done");
  await save(join(cwd, "before-spin.txt"), "the worker started");
  const { wire } = await boot();
  const id = await thread(wire);
  const excess = await script(wire, id, `// @options: {"max_output_tokens": 99999999}\nreturn 'ä'.repeat(100000);`, "output");
  assert.equal(excess.isError, false, excess.text);
  assert.match(excess.text, /truncated output/);
  assert.ok(Buffer.byteLength(excess.text) < 16 * 1024);
  assert.doesNotMatch(excess.text, /\[Full output:|pi-codemode-/);
  const image = await script(wire, id, `image('data:image/png;base64,iVBORw0KGgo='); return 'text only';`, "no-images");
  assert.equal(image.isError, false, image.text);
  assert.match(image.text, /text only/);
  assert.ok(image.blocks.every(block => block.type === "text"));
  assert.deepEqual(await readdir(tmp), []);
  const fanout = await script(wire, id, `await Promise.all(Array.from({length: 17}, () => tools.inspect_file({path: 'file.txt'})));`, "call-limit");
  assert.equal(fanout.isError, true);
  assert.match(fanout.text, /at most 16 calls/);
  const short = await script(wire, id, `// @options: {"timeout_ms": 500}\nwhile (true) {}`, "timeout");
  assert.equal(short.isError, true);
  assert.match(short.text, /timed out/);
  const { submissionId } = await submit(wire, id, "!codemode\nawait tools.inspect_file({path: 'before-spin.txt'}); while (true) {}", "abort");
  // Wait for a nested read before cancelling, not just a pending tool receipt.
  const db = new DatabaseSync(join(storage, "history.sqlite"), { readOnly: true });
  try {
    const task = inspectionTasks(db);
    for (let i = 0; task.get("before-spin.txt")?.status !== "terminal"; i++) {
      assert.ok(i < 250, "the worker did not commit its inspection");
      await delay(10);
    }
  } finally { db.close(); }
  ok(await wire.send("thread.abort", { threadId: id }));
  const terminal = await settle(wire, id, submissionId);
  assert.equal(terminal.status, "unanswered");
  assert.equal(terminal.reason, "aborted");
  const next = await script(wire, id, `return await tools.inspect_file({path: 'file.txt'});`, "after-abort");
  assert.equal(next.isError, false, next.text);
  assert.match(next.text, /inspection-done/);
});

async function codemodeRecovery(t, revokeTrust) {
  const { root, cwd, storage, boot, save, settings, trust } = await setup(t);
  await settings(true);
  const original = join(cwd, "record.txt"), second = join(cwd, "second.txt"), preload = join(root, "stall.mjs");
  await save(original, "before-restart-marker");
  await save(second, "after-replay-marker");
  // Stop the real second inspection just before open(), only in the first
  // process. Reaching it proves that the first result was delivered to the VM.
  await save(preload, `
    import fs from "node:fs/promises";
    import { syncBuiltinESMExports } from "node:module";
    const open = fs.open;
    fs.open = (path, ...args) => {
      if (path !== ${JSON.stringify(second)}) return open(path, ...args);
      process.stdout.write("second inspection is waiting\\n");
      return new Promise(() => {});
    };
    syncBuiltinESMExports();
  `);
  const first = await boot({ preload });
  const id = await thread(first.wire);
  const source = `// @options: {"timeout_ms": 60000}\n` +
    `const prior = await tools.inspect_file({path:'record.txt'});\n` +
    `return [prior, await tools.inspect_file({path:'second.txt'})];`;
  const { submissionId } = await submit(first.wire, id, "!codemode\n" + source, "kill-after-committed-inspection");
  assert.equal((await first.stdout.next()).value, "second inspection is waiting");
  const db = new DatabaseSync(join(storage, "history.sqlite"), { readOnly: true });
  try {
    const task = inspectionTasks(db);
    assert.deepEqual(task.all("record.txt").map(row => row.status), ["terminal"]);
    assert.deepEqual(task.all("second.txt").map(row => row.status), ["running"]);
    const start = db.prepare(`select r.content from documents d join document_revisions r on r.document_id = d.id
      where d.kind = json_quote('pi.codemode.run') and r.kind = 'base'`).get();
    const { deadline, startedAt } = JSON.parse(start.content).start;
    assert.equal(deadline - startedAt, 10_000, "a requested deadline cannot extend the host limit");
    await first.stop("SIGKILL");
  } finally { db.close(); }
  await save(original, "must-not-read-again");
  if (revokeTrust) await trust.change(cwd, "revoke", AbortSignal.timeout(5000));
  const after = await boot();
  assert.notEqual(after.hello.epoch, first.hello.epoch);
  assert.equal((await settle(after.wire, id, submissionId)).status, "done");
  const entries = await history(after.wire, id);
  assert.equal(entries.filter(e => e.kind === "pi.user").length, 1);
  const results = entries.filter(e => e.kind === "pi.tool-result");
  assert.equal(results.length, 1);
  const result = results[0].messages[0];
  assert.equal(result.isError, revokeTrust, JSON.stringify(result));
  if (revokeTrust) {
    assert.match(JSON.stringify(result), /not trusted for tool execution/);
    assert.doesNotMatch(JSON.stringify(result), /before-restart-marker|after-replay-marker/);
  } else {
    assert.match(JSON.stringify(result), /before-restart-marker.*after-replay-marker/);
  }
  assert.doesNotMatch(JSON.stringify(result), /must-not-read-again/);
}

test("SIGKILL replay reuses a committed nested inspection instead of reopening its file", { timeout: 20_000 },
  t => codemodeRecovery(t, false));

test("revoked workspace trust blocks Codemode replay even after execution intent", { timeout: 20_000 },
  t => codemodeRecovery(t, true));
