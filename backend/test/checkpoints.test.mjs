import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { once } from "node:events";
import { chmod, lstat, mkdir, mkdtemp, readFile, realpath, readdir, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { dial } from "../../server/test/client.mjs";
import { startBackend } from "../../dist/hush-backend.mjs";

const run = promisify(execFile);
const bundle = fileURLToPath(new URL("../../dist/hush-backend.mjs", import.meta.url));
const caps = { versions: [1], capabilities: { checkpoints: [1], threadDirectory: [2] } };
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };

async function fixture(t) {
  const home = await realpath(await mkdtemp("/tmp/hush-rw-"));
  t.after(() => rm(home, { recursive: true, force: true }));
  const agentDir = join(home, ".hush"), dataDir = join(agentDir, "runtime"), cwd = join(home, "project");
  await mkdir(cwd, { mode: 0o700 });
  const env = { HOME: home, PATH: process.env.PATH, NODE_NO_WARNINGS: "1", LC_ALL: "C", GIT_OPTIONAL_LOCKS: "0",
    GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" };
  const git = async (dir, ...args) => (await run("git", args, { cwd: dir, env })).stdout.trim();
  const trust = (op, path = cwd) => run(process.execPath, [bundle, "trust", op, "--cwd", path, "--json"], { cwd, env });
  const commit = msg => git(cwd, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", msg);
  await git(cwd, "init", "-q");
  await writeFile(join(cwd, "main"), "at checkpoint\n");
  await writeFile(join(cwd, ".gitignore"), "private\n");
  await git(cwd, "add", "main", ".gitignore");
  await commit("seed");
  return { home, agentDir, dataDir, cwd, git, trust, commit, env, socket: join(dataDir, "hush.sock") };
}

async function view(f) {
  const peer = await dial(f.socket);
  assert.equal(ok(await peer.send("hello", caps)).capabilities.checkpoints, 1);
  const call = async (method, p) => ok(await peer.send(method, p));
  const cp = (op, p) => call("ext.checkpoints." + op, p);
  const newThread = async cwd => (await call("thread.new", { cwd })).id;
  const seed = async (threadId, key) => {
    const { submissionId } = await call("submit", { threadId, text: "test input", requestId: key, whenBusy: "reject" });
    const result = await call("submission.wait", { threadId, submissionId });
    assert.equal(result.status, "done");
    return result.answerEntryId;
  };
  const list = async () => (await call("thread.list", { limit: 100 })).threads;
  const wait = async (threadId, id) => {
    for (const end = Date.now() + 5_000; Date.now() < end; await delay(20)) {
      const state = await cp("status", { threadId, id });
      if (state.state !== "staging") return state;
    }
    assert.fail("rewind is still staging: " + JSON.stringify(await cp("status", { threadId, id })));
  };
  return { peer, call, cp, newThread, seed, list, wait };
}

const files = async (f, cwd) => Promise.all([readFile(join(cwd, "main")), readFile(join(cwd, "untracked")),
  readFile(join(cwd, "private")), readFile(join(cwd, ".git", "index")), f.git(cwd, "rev-parse", "HEAD"),
  f.git(cwd, "symbolic-ref", "HEAD")]);

test("checkpoint restore forks two shared dirty threads into an isolated committed snapshot; bookmark fallbacks are explicit",
  { timeout: 25_000 }, async t => {
  const f = await fixture(t);
  const app = await startBackend({ ...f, idleMs: 30_000, ephemeral: false, faux: true });
  t.after(() => app.close());
  const w = await view(f);
  try {
    const threadId = await w.newThread(f.cwd), shared = await w.newThread(f.cwd);
    await f.trust("grant");
    const atEntryId = await w.seed(threadId, "before");
    const snapshotCommit = await f.git(f.cwd, "rev-parse", "HEAD");
    const record = { threadId, kind: "files", atEntryId, label: "Before changes" };
    const checkpoint = await w.cp("record", record);
    await w.cp("effect", { threadId, text: "Published fixture release outside Git" });
    await writeFile(join(f.cwd, "main"), "later commit\n");
    await f.git(f.cwd, "add", "main");
    await f.commit("later");
    await writeFile(join(f.cwd, "main"), "staged edits\n");
    await f.git(f.cwd, "add", "main");
    await writeFile(join(f.cwd, "main"), "unstaged edits\n");
    await writeFile(join(f.cwd, "untracked"), "private untracked bytes\0\r\n");
    await writeFile(join(f.cwd, "private"), "ignored bytes");
    const original = await files(f, f.cwd);
    const count = (await w.list()).length;
    const preview = () => w.cp("preview", { threadId, checkpointId: checkpoint.id, mode: "files" });
    const before = await preview();
    assert.equal(before.mode, "files", JSON.stringify(before));
    assert.equal(before.files.commit, snapshotCommit);
    assert.match(before.files.branch, /^hush\/rewind\//);
    assert.deepEqual(before.effects.map(e => e.text), ["Published fixture release outside Git"]);
    assert.equal((await w.list()).length, count);
    await assert.rejects(lstat(before.files.cwd), { code: "ENOENT" });
    await w.cp("effect", { threadId, text: "Second recorded effect" });
    assert.match((await w.peer.send("ext.checkpoints.confirm", { threadId, id: before.id, mode: "files" })).error.message, /changed/);
    const p = await preview();
    const start = await w.cp("confirm", { threadId, id: p.id, mode: "files" });
    assert.equal(start.state, "staging");
    assert.equal(start.forkThreadId, undefined);
    const ready = await w.wait(threadId, p.id);
    assert.equal(ready.state, "ready", JSON.stringify(ready));
    assert.deepEqual(await w.cp("confirm", { threadId, id: p.id, mode: "files" }), ready);
    const fork = (await w.list()).find(t => t.id === ready.forkThreadId);
    assert.deepEqual(fork.parent, { threadId, atEntryId });
    assert.equal((await w.list()).length, count + 1);
    const directory = await w.call("ext.threadDirectory.list", { limit: 100 });
    assert.deepEqual(directory.threads.find(t => t.id === fork.id).checkout, { kind: "managed", cwd: p.files.cwd, branch: p.files.branch });
    for (const originalId of [threadId, shared]) {
      const { state } = await w.call("subscribe", { threadId: originalId });
      assert.equal(state.agent.cwd, f.cwd);
    }
    const { state } = await w.call("subscribe", { threadId: fork.id });
    assert.equal(state.agent.cwd, p.files.cwd);
    assert.equal(await f.git(p.files.cwd, "rev-parse", "HEAD"), snapshotCommit);
    assert.equal(await readFile(join(p.files.cwd, "main"), "utf8"), "at checkpoint\n");
    for (const name of ["untracked", "private"]) await assert.rejects(lstat(join(p.files.cwd, name)), { code: "ENOENT" });
    assert.deepEqual(await files(f, f.cwd), original);

    // A managed checkout needs its own grant, then can be checkpointed and rewound without changing it.
    const isolatedRecord = { threadId: fork.id, kind: "files" };
    assert.match((await w.peer.send("ext.checkpoints.record", isolatedRecord)).error.message, /not trusted/);
    await f.trust("grant", p.files.cwd);
    const isolated = await w.cp("record", isolatedRecord);
    const isolatedP = await w.cp("preview", { threadId: fork.id, checkpointId: isolated.id, mode: "files" });
    assert.equal(isolatedP.mode, "files", JSON.stringify(isolatedP));
    await w.cp("confirm", { threadId: fork.id, id: isolatedP.id, mode: "files" });
    const ready2 = await w.wait(fork.id, isolatedP.id);
    assert.equal(ready2.state, "ready", JSON.stringify(ready2));
    assert.equal(await f.git(isolatedP.files.cwd, "rev-parse", "HEAD"), snapshotCommit);
    assert.equal(await f.git(p.files.cwd, "branch", "--show-current"), p.files.branch);

    const beforeRevoke = await preview();
    await f.trust("revoke");
    const countBeforeRevoke = (await w.list()).length;
    const accepted = await w.cp("confirm", { threadId, id: beforeRevoke.id, mode: "files" });
    assert.equal(accepted.state, "staging");
    const refused = await w.wait(threadId, accepted.id);
    assert.equal(refused.state, "failed");
    assert.match(refused.reason, /not trusted/);
    assert.equal((await w.list()).length, countBeforeRevoke);
    await assert.rejects(lstat(beforeRevoke.files.cwd), { code: "ENOENT" });
    const denied = await preview();
    assert.equal(denied.mode, "transcript");
    assert.match(denied.reason, /not trusted/);
    const incorrectMode = await w.peer.send("ext.checkpoints.confirm", { threadId, id: denied.id, mode: "files" });
    assert.equal(incorrectMode.ok, false);
    const indexBefore = await files(f, f.cwd);
    const transcript = await w.cp("confirm", { threadId, id: denied.id, mode: "transcript" });
    assert.equal(transcript.state, "ready");
    assert.deepEqual(await files(f, f.cwd), indexBefore);
    await f.trust("grant");
    await f.git(f.cwd, "update-ref", "-d", p.files.ref);
    const missing = await preview();
    assert.equal(missing.mode, "transcript");
    assert.match(missing.reason, /snapshot is unavailable/);

    // An imported label with a plausible snapshot payload is still only a bookmark.
    const conv = await app.harness.conversation(Number(threadId), BACKGROUND_CONTEXT);
    const label = await conv.commit(tx => tx.appendEntry(conv.id, { kind: "pi.label",
      data: { label: "Imported Pi label", snapshot: { commit: snapshotCommit, ref: isolatedP.files.ref } } }), BACKGROUND_CONTEXT);
    assert.ok((await w.cp("list", { threadId })).items.some(v => v.kind === "imported_label" && v.atEntryId === String(label.id)));
    const legacy = await w.cp("preview", { threadId, atEntryId: String(label.id), mode: "files" });
    assert.equal(legacy.mode, "transcript");
    assert.equal(legacy.files, undefined);

    const plain = join(f.home, "plain");
    await mkdir(plain);
    const nonGit = await w.newThread(plain);
    await f.trust("grant", plain);
    const plainEntry = await w.seed(nonGit, "non-git");
    const plainRecord = await w.peer.send("ext.checkpoints.record", { threadId: nonGit, kind: "files", atEntryId: plainEntry });
    assert.match(plainRecord.error.message, /requires a Git working tree/);
    const bookmark = await w.cp("record", { threadId: nonGit, kind: "bookmark", atEntryId: plainEntry });
    assert.equal((await w.cp("preview", { threadId: nonGit, checkpointId: bookmark.id, mode: "files" })).mode, "transcript");
    assert.deepEqual(await readdir(plain), []);
  } finally { w.peer.close(); }
});

test("crash after Git staging recovers exactly one fork; dirty interrupted staging is never published or reset",
  { timeout: 30_000 }, async t => {
  const f = await fixture(t);
  const realGit = (await run("/bin/sh", ["-c", "command -v git"], { env: f.env })).stdout.trim();
  const tools = join(f.home, "tools");
  await mkdir(tools);
  const mark = join(f.home, "staged");
  const guard = join(f.home, "release-stage");
  const script = join(tools, "git");
  // Pause only after actual Git has populated a new isolated checkout, before
  // the backend has verified it or published any conversation.
  await writeFile(script, `#!/bin/sh\n${JSON.stringify(realGit)} "$@" || exit $?\ncase " $* " in\n*' worktree add '*'/rewind-worktrees-v1/'*)\n  echo $$ > ${JSON.stringify(mark)}\n  i=0\n  while [ ! -e ${JSON.stringify(guard)} ] && [ "$i" -lt 100 ]; do /bin/sleep .1; i=$((i+1)); done;;\nesac\n`);
  await chmod(script, 0o700);
  await f.trust("grant");
  let host, exit, wire;
  const boot = async pause => {
    host = spawn(process.execPath, [bundle, "--faux"], { cwd: f.cwd,
      env: { ...f.env, PATH: pause ? tools + ":" + f.env.PATH : f.env.PATH }, stdio: ["ignore", "pipe", "pipe"] });
    let stderr = "";
    host.stderr.setEncoding("utf8").on("data", s => { stderr += s; });
    exit = once(host, "exit"); exit.catch(() => {});
    const lines = createInterface({ input: host.stdout });
    await Promise.race([once(lines, "line"), exit.then(() => { throw Error(stderr); })]);
    lines.close();
    return wire = await view(f);
  };
  const stop = async (signal = "SIGTERM") => {
    wire?.peer.close();
    host.kill(signal);
    assert.deepEqual(await exit, signal === "SIGTERM" ? [0, null] : [null, signal]);
    host = undefined;
  };
  try {
    let w = await boot(true);
    const threadId = await w.newThread(f.cwd);
    await w.seed(threadId, "before-kill");
    const checkpoint = await w.cp("record", { threadId, kind: "files" });
    const unchanged = await readFile(join(f.cwd, "main"));
    for (const dirty of [false, true]) {
      const before = (await w.list()).length;
      const p = await w.cp("preview", { threadId, checkpointId: checkpoint.id, mode: "files" });
      assert.equal(p.mode, "files", JSON.stringify(p));
      assert.equal((await w.cp("confirm", { threadId, id: p.id, mode: "files" })).state, "staging");
      for (const deadline = Date.now() + 5_000; !(await lstat(mark).catch(() => null)); await delay(20)) {
        assert.ok(Date.now() < deadline, JSON.stringify(await w.cp("status", { threadId, id: p.id })));
      }
      assert.equal((await w.list()).length, before);
      await stop("SIGKILL");
      await writeFile(guard, "release");
      if (dirty) await writeFile(join(p.files.cwd, "kept"), "do not delete this");
      w = await boot(false);
      const result = await w.wait(threadId, p.id);
      assert.equal(result.state, dirty ? "failed" : "ready", JSON.stringify(result));
      assert.equal((await w.list()).length, before + (dirty ? 0 : 1));
      assert.deepEqual(await readFile(join(f.cwd, "main")), unchanged);
      if (dirty) {
        assert.equal(result.forkThreadId, undefined);
        assert.equal(await readFile(join(p.files.cwd, "kept"), "utf8"), "do not delete this");
        assert.match(result.reason, /transcript-only/i);
      } else {
        assert.deepEqual(await w.cp("confirm", { threadId, id: p.id, mode: "files" }), result);
        assert.equal(await f.git(p.files.cwd, "rev-parse", "HEAD"), p.files.commit);
        await stop();
        await rm(guard); await rm(mark);
        w = await boot(true);
      }
    }
    await stop();
  } finally {
    if (host) await stop().catch(() => {});
    await writeFile(guard, "release").catch(() => {});
  }
});
