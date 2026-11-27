import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { access, link, mkdir, mkdtemp, realpath, rename, rm, symlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT as ctx } from "@earendil-works/chord/context";
import { configure, defineDoc, defineTask, ToolResultEntry } from "@earendil-works/pi-durable";
import { dial } from "../../server/test/client.mjs";
import { admitToolProfile } from "../../dist/backend/src/tool-policy.js";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { startBackend } from "../../dist/hush-backend.mjs";
import { call, planProvider } from "./plan-provider.mjs";

const sha = text => createHash("sha256").update(text).digest("hex");
const ok = value => { assert.equal(value.ok, true, JSON.stringify(value)); return value.result; };

async function setup(t) {
  const dir = await realpath(await mkdtemp("/tmp/hush-code-review-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const project = join(dir, "repo"), cwd = join(project, "module"), agentDir = join(dir, ".hush"), dataDir = join(dir, "data");
  await mkdir(cwd, { recursive: true });
  await mkdir(agentDir, { mode: 0o700 });
  const git = (...args) => execFileSync("git", args, { cwd: project, encoding: "utf8",
    env: { PATH: process.env.PATH, HOME: dir, LC_ALL: "C", GIT_CONFIG_NOSYSTEM: "1" } }).trimEnd();
  git("init", "-q");
  git("config", "user.name", "Fixture");
  git("config", "user.email", "fixture@example.test");
  const put = (path, content) => writeFile(join(cwd, path), content);
  await put("entry.txt", "\uFEFFbase\r\n");
  await put("context.txt", "context\n");
  await put("unrelated.txt", "UNRELATED_EDIT\n");
  await put("removed.txt", "before deletion\n");
  await put(".gitignore", "ignored.txt\n");
  await put("ignored.txt", "IGNORED_TOKEN\n");
  await writeFile(join(project, "outside.txt"), "OUTSIDE_SUBDIR\n");
  await writeFile(join(dir, "secret.txt"), "OUTSIDE_CHECKOUT\n");
  git("add", "--", ".");
  git("commit", "-qm", "base");
  await put("entry.txt", "\uFEFFstaged\r\n");
  git("add", "--", "module/entry.txt");
  await put("entry.txt", "\uFEFFworking\r\n");
  await put("new.txt", "explicit untracked\n");
  await put("not-selected.txt", "UNRELATED_UNTRACKED\n");
  await put("unrelated.txt", "LATEST_UNRELATED_EDIT\n");
  await rm(join(cwd, "removed.txt"));
  await symlink(join(dir, "secret.txt"), join(cwd, "link.txt"));
  const trust = new WorkspaceTrust(agentDir);
  const grant = (path = cwd, verb = "grant") => trust.change(path, verb, AbortSignal.timeout(5_000));
  await grant();
  const config = { faux: false, ephemeral: false, idleMs: 60_000, cwd, agentDir, dataDir, socket: join(dataDir, "hush.sock") };
  return { config, project, cwd, git, grant, put, secret: join(dir, "secret.txt") };
}

// The inert owner is used only to prove that a profile without native admission grants no reads.
const anchor = defineTask({ name: "fixture.snapshot-owner", version: 1, initial: () => ({ phase: "hold" }), phases: {} });
const ledger = defineDoc({ kind: "hush.delegated-reviews", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({ cohorts: [] }), checkpointWhen: () => true });

const scope = { tracked: ["entry.txt", "context.txt", "removed.txt"], untracked: ["new.txt"] };

test("a persisted code review sees the exact dirty target, not its parent or the moving tree", { timeout: 18_000 }, async t => {
  const { config, cwd, git, grant, put } = await setup(t);
  const normal = "Read the persisted snapshot", revoked = "Read after a grant change";
  const gates = new Map([normal, revoked].map(brief => [brief, { placed: Promise.withResolvers(), read: Promise.withResolvers() }]));
  const provider = await planProvider(t, config.agentDir, async ({ text, tools, results, messages, prompt }) => {
    if (text === "unrelated parent transcript") return "parent response";
    const prefix = "Code review assignment\n";
    const native = text.startsWith(prefix);
    if (!native) assert.equal(text, "Code review fixture brief");
    assert.deepEqual(tools.toSorted(), native ? ["hush_review_read", "hush_review_report"] : ["hush_review_read"]);
    assert.doesNotMatch(prompt, /Current working directory|Saving a plan/);
    assert.doesNotMatch(JSON.stringify(messages), /UNRELATED|IGNORED_TOKEN|OUTSIDE_|LATEST_WORKING|parent transcript|parent response|git\/config/);
    if (!results.length) {
      if (native) {
        const gate = gates.get(JSON.parse(text.slice(prefix.length)).brief);
        assert.ok(gate);
        gate.placed.resolve();
        await gate.read.promise;
      }
      return [call("hush_review_read"), call("hush_review_read", { path: "entry.txt" }),
        call("hush_review_read", { path: "new.txt" }), call("hush_review_read", { path: "unrelated.txt" }),
        call("hush_review_read", { path: "../outside.txt" }), call("hush_review_read", { path: "ignored.txt" }),
        call("read", { path: "not-selected.txt" }), call("hush_plan_propose"), call("codemode", { code: 'return "no"' }),
        call("bash", { command: "echo should-not-run" })];
    }
    assert.equal(results.length, 10);
    return "advisory only";
  });
  let backend, wire;
  const start = async () => { backend = await startBackend({ ...config, model: provider.model }); };
  const stop = async () => { wire?.close(); await backend?.close(); backend = undefined; };
  t.after(async () => { for (const gate of gates.values()) gate.read.resolve(); await stop(); });
  await start();
  const { harness: h, root } = backend;
  assert.equal((await (await root.submit({ type: "input", content: "unrelated parent transcript" }, ctx)).wait(ctx)).status, "done");
  const before = await backend.reviewSnapshots.capture(root.id, scope, ctx);
  assert.equal(before.base, git("rev-parse", "HEAD"));
  assert.equal(before.subdirectory, "module");
  assert.deepEqual(before.files.map(({ path, stagedChange, unstagedChange }) => [path, stagedChange, unstagedChange]), [
    ["context.txt", false, false], ["entry.txt", true, true], ["removed.txt", false, true], ["new.txt", false, false],
  ]);
  assert.deepEqual(await backend.reviewSnapshots.freshness(root.id, before.id, scope, ctx), { stale: false });
  await put("not-selected.txt", "unrelated changed again\n");
  assert.deepEqual(await backend.reviewSnapshots.freshness(root.id, before.id, scope, ctx), { stale: false });
  assert.deepEqual(await backend.reviewSnapshots.freshness(root.id, before.id, { ...scope, untracked: [] }, ctx),
    { stale: true, reason: "scope_changed" });
  await stop();
  await start();
  wire = await dial(config.socket);
  ok(await wire.send("hello", { versions: [1], capabilities: { delegation: [1] } }));
  const running = backend.harness, owner = backend.root.id;
  assert.deepEqual(await backend.reviewSnapshots.freshness(owner, before.id, scope, ctx), { stale: false });
  const toolResults = async id => {
    const child = await running.conversation(id, ctx);
    return (await child.entries({}, 64, undefined, ctx)).items.filter(entry => ToolResultEntry.is(entry)).map(entry => entry.model[0]);
  };
  const launch = async (requestId, brief) => {
    const receipt = ok(await wire.send("ext.delegation.start", { threadId: String(owner), requestId, scope, assignments: [{ name: "Review", brief }] }));
    await gates.get(brief).placed.promise;
    const { cohorts } = await running.snapshot(ledger, owner, ctx);
    const [{ child, task }] = cohorts.find(c => c.pin.receipt.requestId === requestId).rows;
    return { receipt, child, task };
  };
  const unadmittedReview = async (owner, key) => {
    const id = await running.commit(async tx => {
      const taskId = await tx.createTask(anchor, null, { ownership: { kind: "conversation" }, conversationId: owner, background: true });
      const child = await tx.createConversation({ ownership: { kind: "task", taskId } });
      await admitToolProfile(tx, child, { kind: "reviewer", target: { kind: "code", snapshotId: before.id } });
      await configure(tx, child.id, { extensions: [{ name: "hush-code-review-snapshot" }], tools: [{ name: "hush_review_read" }], modelTools: null,
        cwd, model: provider.model, instructions: "Only review the supplied target." });
      return child.id;
    }, ctx);
    const child = await running.conversation(id, ctx);
    const submitted = await child.submit({ type: "input", content: "Code review fixture brief", requestId: key }, ctx);
    assert.equal((await submitted.wait(ctx)).status, "done");
    return toolResults(id);
  };
  const admitted = await launch("original", normal), id = admitted.child;
  assert.equal(admitted.receipt.snapshotId, before.id);
  await put("entry.txt", "LATEST_WORKING\n");
  assert.deepEqual(await backend.reviewSnapshots.freshness(owner, before.id, scope, ctx), { stale: true, reason: "source_changed" });
  assert.notEqual((await backend.reviewSnapshots.capture(owner, scope, ctx)).id, before.id);
  gates.get(normal).read.resolve();
  await running.waitForTask(admitted.task, ctx);
  const results = await toolResults(id);
  const input = index => {
    const m = results.find(r => r.toolCallId.endsWith(`-${index}`));
    assert.ok(m && !m.isError, JSON.stringify(results));
    return JSON.parse(m.content[0].text);
  };
  assert.deepEqual(input(0), before);
  assert.deepEqual(input(1), { path: "entry.txt", ...Object.fromEntries(
    [["base", "\uFEFFbase\r\n"], ["staged", "\uFEFFstaged\r\n"], ["working", "\uFEFFworking\r\n"]].map(([kind, text]) => [kind, { mode: "100644", hash: sha(text), text }])) });
  assert.equal(input(2).working.text, "explicit untracked\n");
  assert.equal(results.filter(r => r.isError).length, 7);
  const childEntries = (await (await running.conversation(id, ctx)).entries({}, 64, undefined, ctx)).items;
  const assignment = childEntries.find(e => e.kind === "hush.review.assignment");
  assert.ok(assignment, "the native worker received its own keyed assignment");
  for (const method of ["submit", "thread.fork", "thread.configure"]) {
    const threadId = String(id);
    const params = method === "submit" ? { threadId, text: "another task", requestId: "no", whenBusy: "reject" }
      : method === "thread.configure" ? { threadId, change: { thinkingLevel: "off" } }
      : { threadId, atEntryId: String(assignment.id) };
    assert.equal((await wire.send(method, params)).error.code, "not_found");
  }
  const otherParent = await running.createConversation({ ownership: { kind: "ownerless" }, agent: { cwd, model: provider.model } }, ctx);
  for (const [parent, key] of [[owner, "unadmitted"], [otherParent.id, "wrong-owner"]]) {
    const denied = await unadmittedReview(parent, key);
    assert.equal(denied.length, 10);
    assert.ok(denied.every(result => result.isError), "a profile without a receipt must not grant reads");
  }
  const next = await launch("revoked-regrant", revoked);
  await grant(cwd, "revoke");
  await grant();
  gates.get(revoked).read.resolve();
  await running.waitForTask(next.task, ctx);
  const denied = await toolResults(next.child);
  assert.equal(denied.length, 10);
  assert.ok(denied.every(result => result.isError), "a new grant does not authorize a captured snapshot");
});

test("snapshot capture rejects intent-to-add without rejecting genuinely staged empty files", { timeout: 18_000 }, async t => {
  const { config, git, put } = await setup(t);
  const backend = await startBackend(config);
  t.after(() => backend.close());
  const { reviewSnapshots, root } = backend;
  const capture = (tracked, untracked = []) => reviewSnapshots.capture(root.id, { tracked, untracked }, ctx);

  await put("empty.txt", "");
  git("add", "--", "module/empty.txt");
  const empty = await capture(["empty.txt"]);
  assert.deepEqual(empty.files[0], { path: "empty.txt", base: null,
    staged: { mode: "100644", hash: sha("") }, working: { mode: "100644", hash: sha("") }, stagedChange: true, unstagedChange: false });

  await put("intent.txt", "working bytes only\n");
  git("add", "-N", "--", "module/intent.txt");
  await assert.rejects(capture(["empty.txt", "intent.txt"]), /unavailable/);
  await assert.rejects(capture([], ["intent.txt"]), /unavailable/);
  assert.equal((await capture(["empty.txt"])).id, empty.id, "intent outside the scope does not affect the pin");
  git("add", "--", "module/intent.txt");
  assert.equal((await capture(["intent.txt"])).files[0].staged.hash, sha("working bytes only\n"));

  // An empty worktree can leave the same ordinary index listing after its flag changes.
  git("rm", "--cached", "--", "module/empty.txt");
  git("add", "-N", "--", "module/empty.txt");
  assert.deepEqual(await reviewSnapshots.freshness(root.id, empty.id, { tracked: ["empty.txt"], untracked: [] }, ctx),
    { stale: true, reason: "unavailable" });
  git("rm", "--cached", "--", "module/context.txt");
  git("add", "-N", "--", "module/context.txt");
  await assert.rejects(capture(["context.txt"]), /unavailable/, "a path already in HEAD can have an intent placeholder too");
});

test("snapshot capture rejects unadmitted, ignored, linked, missing, and unsupported inputs", { timeout: 18_000 }, async t => {
  const { config, cwd, git, put, secret } = await setup(t);
  const backend = await startBackend(config);
  t.after(() => backend.close());
  const { reviewSnapshots, root } = backend;
  const capture = (tracked = [], untracked = []) => reviewSnapshots.capture(root.id, { tracked, untracked }, ctx);
  for (const path of ["../outside.txt", "ignored.txt", ".git/config", "link.txt", "gone.txt", secret]) {
    await assert.rejects(capture([], [path]), /unavailable/);
  }
  await assert.rejects(capture(["not-selected.txt"]), /unavailable/);
  await assert.rejects(capture([], ["context.txt"]), /unavailable/);
  await assert.rejects(capture(["entry.txt"], ["entry.txt"]), /unavailable/);
  await link(secret, join(cwd, "alias.txt"));
  await assert.rejects(capture([], ["alias.txt"]));
  await put("huge.txt", "x".repeat(33 * 1024));
  await assert.rejects(capture([], ["huge.txt"]));
  await put("binary.txt", "x\0y");
  await assert.rejects(capture([], ["binary.txt"]));
  git("add", "-f", "--", "module/ignored.txt");
  await assert.rejects(capture(["ignored.txt"]), /unavailable/);
  // Git index flags would make normal diff omit working tree input.
  git("update-index", "--assume-unchanged", "--", "module/entry.txt");
  await assert.rejects(capture(["entry.txt"]), /unavailable/);
  git("update-index", "--no-assume-unchanged", "--", "module/entry.txt");
  await rename(join(cwd, "entry.txt"), join(cwd, "ENTRY.txt"));
  if (await access(join(cwd, "entry.txt")).then(() => true, () => false)) await assert.rejects(capture(["entry.txt"]));
  await rm(join(cwd, "ENTRY.txt"));
  await symlink(secret, join(cwd, "entry.txt"));
  await assert.rejects(capture(["entry.txt"]), /unavailable/);
  await rm(join(cwd, "entry.txt"));
  git("rm", "-f", "--", "module/entry.txt");
  await put("entry.txt", "untracked replacement\n");
  await assert.rejects(capture(["entry.txt"]), /unavailable/);
  const { files: [replacement] } = await capture([], ["entry.txt"]);
  assert.equal(replacement.staged, null);
  assert.deepEqual([replacement.stagedChange, replacement.working.hash], [true, sha("untracked replacement\n")]);
});
