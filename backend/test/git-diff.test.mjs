import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, realpath, rm, stat, symlink, writeFile } from "node:fs/promises";
import { delimiter, join } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { test } from "node:test";
import { setTimeout as sleep } from "node:timers/promises";
import { dial } from "../../server/test/client.mjs";
import { startBackend } from "../../dist/hush-backend.mjs";

const run = promisify(execFile), bundle = fileURLToPath(new URL("../../dist/hush-backend.mjs", import.meta.url));
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const needsTrust = { code: "invalid_request",
  message: "Workspace is not trusted by Hush. Run 'hush trust grant --cwd PATH' on the backend host." };

test("read-only Git review is negotiated, checkout-scoped, and can compare a file checkpoint", { timeout: 25_000 }, async () => {
  const home = await realpath(await mkdtemp("/tmp/hush-diff-"));
  let app, peer, legacy;
  try {
    const cwd = join(home, "project"), agentDir = join(home, ".hush"), dataDir = join(agentDir, "runtime");
    const socket = join(dataDir, "hush.sock"), sub = join(cwd, "src");
    await mkdir(sub, { recursive: true });
    const env = { HOME: home, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" };
    const git = async (...args) => (await run("git", args, { cwd, env })).stdout.trim();
    const commit = msg => git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", msg);
    const trust = (op, workspace = sub) =>
      run(process.execPath, [bundle, "trust", op, "--cwd", workspace], { cwd, env });
    await git("init", "-q", "-b", "main");
    await writeFile(join(cwd, "main"), "base\n");
    await writeFile(join(sub, "nested"), "neighbor\n");
    await writeFile(join(cwd, ".gitignore"), "ignored\n");
    await git("add", ".");
    await commit("seed");
    const initial = await git("rev-parse", "HEAD");
    app = await startBackend({ cwd, agentDir, dataDir, socket, idleMs: 30_000, ephemeral: false, faux: true });
    peer = await dial(socket);
    const call = async (method, params) => ok(await peer.send(method, params));
    assert.equal((await call("hello", { versions: [1], capabilities: { gitDiff: [1], checkpoints: [1], threadDirectory: [2] } })).capabilities.gitDiff, 1);
    const { id: threadId } = await call("thread.new", { cwd: sub });
    const summary = (range = "head") => call("ext.gitDiff.summary", { threadId, range });
    legacy = await dial(socket);
    assert.equal(ok(await legacy.send("hello", { versions: [1] })).capabilities.gitDiff, undefined);
    assert.equal((await legacy.send("ext.gitDiff.summary", { threadId, range: "head" })).error.code, "unsupported_capability");
    const deny = async () => assert.deepEqual((await peer.send("ext.gitDiff.summary", { threadId, range: "head" })).error, needsTrust);
    await deny();
    await trust("grant"); // The subdirectory grant enables Git review of this checkout and agent work.
    const clean = await summary();
    assert.deepEqual(clean.files, []);
    assert.equal(clean.branch, "main");
    await git("switch", "-qc", "feature/review");
    assert.equal((await summary()).branch, "feature/review");
    await git("switch", "-q", "--detach");
    assert.equal((await summary()).branch, undefined);
    await git("switch", "-q", "main");
    assert.match((await peer.send("ext.gitDiff.summary", { threadId, range: "checkpoint" })).error.message, /No file checkpoint/);
    const { submissionId } = await call("submit", { threadId, text: "seed", requestId: "diff-seed", whenBusy: "reject" });
    assert.equal((await call("submission.wait", { threadId, submissionId })).status, "done");
    await call("ext.checkpoints.record", { threadId, kind: "files" });
    await call("ext.checkpoints.record", { threadId, kind: "bookmark" }); // not file evidence
    await writeFile(join(cwd, "main"), "later commit\n");
    await git("add", "main");
    await commit("later");
    const latest = await git("rev-parse", "HEAD");
    await writeFile(join(cwd, "main"), "staged\n");
    await git("add", "main");
    await writeFile(join(cwd, "main"), "working\n\x1b[0mline\n");
    const literal = ":(glob)added*.txt";
    await writeFile(join(sub, literal), "new text\n");
    await writeFile(join(sub, "blob"), Buffer.from([0, 1, 2]));
    await writeFile(join(sub, "ignored"), "ignored\n");
    const outside = join(home, "not-in-worktree");
    await writeFile(outside, "outside content\n");
    await symlink(outside, join(sub, "link"));
    const index = await readFile(join(cwd, ".git", "index"));
    const worktree = await summary();
    assert.equal(worktree.commit, latest);
    assert.equal(worktree.checkpoint, true);
    assert.deepEqual(worktree.files.map(f => f.path), ["main", `src/${literal}`, "src/blob", "src/link"]);
    assert.deepEqual(worktree.files[0], { path: "main", status: "M", staged: true, text: true, added: 2, removed: 1 });
    assert.deepEqual(worktree.files[1], { path: `src/${literal}`, status: "?", staged: false, text: false });
    const file = (s, index) => call("ext.gitDiff.file", { threadId, revision: s.revision, file: index });
    const fromHead = (await file(worktree, 0)).lines;
    assert.deepEqual(fromHead.filter(l => l.kind === "remove"), [{ kind: "remove", oldLine: 1, text: "later commit" }]);
    assert.deepEqual(fromHead.filter(l => l.kind === "add").map(l => l.text), ["working", "\x1b[0mline"]);
    assert.equal((await file(worktree, 1)).lines.at(-1).text, "new text");
    assert.match((await file(worktree, 2)).lines[0].text, /no preview/);
    assert.match((await file(worktree, 3)).lines[0].text, /no preview/);
    const checkpoint = await summary("checkpoint");
    assert.equal(checkpoint.commit, initial);
    assert.equal((await file(checkpoint, 0)).lines.find(l => l.kind === "remove").text, "base");
    assert.deepEqual(await readFile(join(cwd, ".git", "index")), index);
    assert.equal(await git("rev-parse", "HEAD"), latest);
    assert.equal(await readFile(join(cwd, "main"), "utf8"), "working\n\x1b[0mline\n");
    const isolated = await call("ext.threadDirectory.new", { cwd: sub, worktree: true });
    assert.notEqual(isolated.checkout.cwd, cwd);
    const isolatedRequest = { threadId: isolated.id, range: "head" };
    // The new checkout keeps the source's subdirectory and inherits its grant there.
    assert.equal(isolated.checkout.cwd.endsWith("/src"), true, isolated.checkout.cwd);
    const isolatedSummary = await call("ext.gitDiff.summary", isolatedRequest);
    assert.deepEqual(isolatedSummary.files, []);
    assert.equal(isolatedSummary.branch, isolated.checkout.branch);
    assert.equal((await peer.send("ext.gitDiff.file", { threadId: isolated.id, revision: worktree.revision, file: 0 })).error.code, "invalid_cursor");
    assert.equal((await peer.send("ext.gitDiff.file", { threadId, revision: worktree.revision, file: 0, path: outside })).error.code, "invalid_request");
    const nested = join(cwd, "other-repo");
    await git("init", "-q", nested);
    await git("-C", nested, "config", "core.worktree", cwd);
    await trust("grant", nested);
    const { id: nestedId } = await call("thread.new", { cwd: nested });
    assert.match((await peer.send("ext.gitDiff.summary", { threadId: nestedId, range: "head" })).error.message, /Invalid workspace/);
    await trust("revoke");
    assert.deepEqual((await peer.send("ext.gitDiff.file", { threadId, revision: worktree.revision, file: 0 })).error, needsTrust);
  } finally {
    legacy?.close(); peer?.close();
    await app?.close();
    await rm(home, { recursive: true, force: true });
  }
});

test("Git watches share one linked-worktree scan and observe tool writes, index and HEAD", { timeout: 15_000 }, async t => {
  const home = await realpath(await mkdtemp("/tmp/hush-git-watch-"));
  const agentDir = join(home, ".hush"), dataDir = join(agentDir, "runtime"), socket = join(dataDir, "hush.sock");
  const primary = join(home, "project"), cwd = join(home, "linked"), sub = join(cwd, "sub");
  const bin = join(home, "bin"), trace = join(home, "git-spawns");
  const originalPath = process.env.PATH;
  const env = { HOME: home, PATH: originalPath, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" };
  const git = async (dir, ...args) => (await run("git", ["-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", ...args],
    { cwd: dir, env })).stdout.trim();
  const trust = (op, dir) => run(process.execPath, [bundle, "trust", op, "--cwd", dir], { cwd, env });
  let app, a, b, legacy;
  t.after(async () => {
    if (originalPath === undefined) delete process.env.PATH;
    else process.env.PATH = originalPath;
    a?.close(); b?.close(); legacy?.close();
    await app?.close();
    await rm(home, { recursive: true, force: true });
  });

  await mkdir(primary);
  await mkdir(bin);
  await git(primary, "init", "-q", "-b", "main");
  await mkdir(join(primary, "sub"));
  await writeFile(join(primary, "sub", "tracked"), "base\n");
  await git(primary, "add", ".");
  await git(primary, "commit", "-qm", "seed");
  await git(primary, "worktree", "add", "-qb", "review", cwd);
  const realGit = (await run("which", ["git"], { env })).stdout.trim();
  const quote = str => "'" + str.replaceAll("'", "'\\''") + "'";
  await writeFile(join(bin, "git"), `#!/bin/sh\nprintf '%s\\n' "$*" >> ${quote(trace)}\nexec ${quote(realGit)} "$@"\n`, { mode: 0o700 });
  const spawns = async () => (await readFile(trace, "utf8")).trimEnd().split("\n");
  const scans = rows => rows.filter(row => row.includes(" --raw --numstat "));
  await trust("grant", cwd);
  await trust("grant", sub);
  process.env.PATH = bin + delimiter + originalPath;
  app = await startBackend({ cwd, agentDir, dataDir, socket, idleMs: 30_000, ephemeral: false, faux: true });

  a = await dial(socket);
  b = await dial(socket);
  legacy = await dial(socket);
  const method = "ext.gitDiffWatch.wait";
  for (const peer of [a, b]) {
    assert.deepEqual(ok(await peer.send("hello", { versions: [1], capabilities: { gitDiff: [1], gitDiffWatch: [1] } })).capabilities,
      { gitDiff: 1, gitDiffWatch: 1 });
  }
  assert.deepEqual(ok(await legacy.send("hello", { versions: [1], capabilities: { gitDiff: [1] } })).capabilities, { gitDiff: 1 });
  const { id: threadId } = ok(await a.send("thread.new", { cwd }));
  const { id: sameTree } = ok(await a.send("thread.new", { cwd: sub }));
  assert.equal((await legacy.send(method, { threadId })).error.capability, "gitDiffWatch");
  assert.equal((await a.send(method, { threadId, after: "" })).error.code, "invalid_request");
  let [left, right] = (await Promise.all([a.send(method, { threadId }), b.send(method, { threadId: sameTree })])).map(ok);
  assert.deepEqual(left.summary.files, []);
  assert.equal(left.summary.branch, "review");
  assert.deepEqual(right.summary.files, []);
  assert.notEqual(left.summary.revision, right.summary.revision);
  const attach = await spawns();
  assert.equal(scans(attach).length, 1, attach.join("\n"));
  t.diagnostic(`two clients/threads on one linked worktree: ${scans(attach).length} stats scan, ${attach.length} Git spawns including both authority checks`);

  const arm = () => {
    const aWait = a.send(method, { threadId, after: left.revision });
    const bWait = b.send(method, { threadId: sameTree, after: right.revision });
    // Attaching independent rejection handlers keeps fixture failures local.
    return Promise.all([aWait, bWait]).then(result => result.map(ok));
  };
  let change = arm();
  await sleep(180);
  assert.equal((await a.send(method, { threadId })).error.code, "limit");
  const priorWrite = (await spawns()).length;
  const started = performance.now();
  const { submissionId } = ok(await a.send("submit", { threadId, text: "!workspace-write", requestId: "git-watch-write", whenBusy: "reject" }));
  [left, right] = await change;
  const elapsed = performance.now() - started;
  assert.ok(elapsed < 1_500, `tool update took ${elapsed} ms`);
  assert.deepEqual(left.summary.files, [{ path: "workspace-marker.txt", status: "?", staged: false, text: false }]);
  assert.deepEqual(right.summary.files, left.summary.files);
  assert.equal(ok(await a.send("submission.wait", { threadId, submissionId })).status, "done");
  const writes = (await spawns()).slice(priorWrite);
  assert.equal(scans(writes).length, 1, writes.join("\n"));
  t.diagnostic(`tool write through both replies: ${elapsed.toFixed(0)} ms, ${writes.length} Git spawns total`);
  assert.equal((await b.send("ext.gitDiff.file", { threadId: sameTree, revision: left.summary.revision, file: 0 })).error.code, "invalid_cursor");
  assert.ok(ok(await a.send("ext.gitDiff.file", { threadId, revision: left.summary.revision, file: 0 })).lines
    .some(line => line.kind === "add" && line.text === "Written by the faux tool."));

  change = arm();
  await git(cwd, "add", "workspace-marker.txt");
  [left, right] = await change;
  assert.ok(left.summary.files[0].staged);
  assert.deepEqual(left.summary.files, right.summary.files);
  change = arm();
  const commit = await git(cwd, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "move only current ref");
  await git(cwd, "update-ref", "refs/heads/review", commit);
  [left, right] = await change;
  assert.equal(left.summary.commit, commit);
  assert.equal(right.summary.commit, commit);
  const revoke = b.send(method, { threadId: sameTree, after: right.revision });
  await trust("revoke", sub);
  const denied = ok(await revoke);
  // Sharing the same checkout must not let a different cwd borrow trust.
  assert.deepEqual(denied.error, needsTrust);
  assert.equal(ok(await a.send(method, { threadId })).summary.commit, commit);
  const regrant = b.send(method, { threadId: sameTree, after: denied.revision });
  await trust("grant", sub);
  assert.equal(ok(await regrant).summary.commit, commit);
});

test("a native tool write in checkout A wakes the Git watch in checkout B", { timeout: 15_000 }, async t => {
  const home = await realpath(await mkdtemp("/tmp/hush-git-cross-"));
  const agentDir = join(home, ".hush"), dataDir = join(agentDir, "runtime"), socket = join(dataDir, "hush.sock");
  const a = join(home, "a"), b = join(home, "b"), target = join(b, "tracked.txt");
  const env = { HOME: home, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" };
  const git = async (cwd, ...args) => (await run("git", ["-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", ...args],
    { cwd, env })).stdout.trim();
  let app, peer;
  t.after(async () => {
    peer?.close();
    await app?.close();
    await rm(home, { recursive: true, force: true });
  });

  for (const cwd of [a, b]) {
    await mkdir(cwd);
    await git(cwd, "init", "-q", "-b", "main");
    await writeFile(join(cwd, "tracked.txt"), "base\n");
    await git(cwd, "add", "tracked.txt");
    await git(cwd, "commit", "-qm", "seed");
    await run(process.execPath, [bundle, "trust", "grant", "--cwd", cwd], { cwd, env });
  }
  // The faux provider calls the native write tool with a path relative to A.
  await symlink(target, join(a, "workspace-marker.txt"));
  const head = await git(b, "rev-parse", "HEAD");
  const metadata = () => Promise.all(["HEAD", "index"].map(async name => {
    const path = join(b, ".git", name);
    const { ino, mtimeNs, ctimeNs } = await stat(path, { bigint: true });
    return { ino, mtimeNs, ctimeNs, content: await readFile(path) };
  }));
  const before = await metadata();

  app = await startBackend({ cwd: a, agentDir, dataDir, socket, idleMs: 30_000, ephemeral: false, faux: true });
  peer = await dial(socket);
  const call = async (method, params) => ok(await peer.send(method, params));
  const method = "ext.gitDiffWatch.wait";
  assert.equal((await call("hello", { versions: [1], capabilities: { gitDiff: [1], gitDiffWatch: [1] } })).capabilities.gitDiffWatch, 1);
  const { id: writer } = await call("thread.new", { cwd: a });
  const { id: watcher } = await call("thread.new", { cwd: b });
  const started = performance.now(); // Before the first watch can start the host's 20 s fallback timer.
  const clean = await call(method, { threadId: watcher });
  assert.deepEqual(clean.summary.files, []);
  assert.equal(clean.summary.commit, head);
  const change = peer.send(method, { threadId: watcher, after: clean.revision })
    .then(frame => ({ frame, elapsed: performance.now() - started }));
  void change.catch(() => {}); // Await below, after independently confirming the tool write.
  assert.equal((await peer.send(method, { threadId: watcher })).error.code, "limit");

  const { submissionId } = await call("submit", { threadId: writer, text: "!workspace-write", requestId: "git-watch-cross-write", whenBusy: "reject" });
  assert.equal((await call("submission.wait", { threadId: writer, submissionId })).status, "done");
  assert.equal(await readFile(target, "utf8"), "Written by the faux tool.\n");
  assert.equal(await git(b, "rev-parse", "HEAD"), head);
  assert.deepEqual(await metadata(), before);
  const { frame, elapsed } = await change;
  assert.ok(elapsed < 10_000, `cross-checkout update took ${elapsed} ms; expected it before host fallback`);
  t.diagnostic(`initial watch through cross-checkout update: ${elapsed.toFixed(0)} ms`);
  const updated = ok(frame);
  assert.notEqual(updated.revision, clean.revision);
  assert.equal(updated.summary.commit, head);
  assert.deepEqual(updated.summary.files, [{ path: "tracked.txt", status: "M", staged: false, text: true, added: 1, removed: 1 }]);
});

test("Git review version 2 reads a trusted launch directory before any thread exists", { timeout: 20_000 }, async () => {
  const home = await realpath(await mkdtemp("/tmp/hush-diff-launch-"));
  let app, peer, legacy;
  try {
    const cwd = join(home, "project"), agentDir = join(home, ".hush"), dataDir = join(agentDir, "runtime");
    const socket = join(dataDir, "hush.sock");
    await mkdir(cwd, { recursive: true });
    const env = { HOME: home, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" };
    const git = async (...args) => (await run("git", args, { cwd, env })).stdout.trim();
    await git("init", "-q", "-b", "main");
    await writeFile(join(cwd, "tracked"), "base\n");
    await git("add", ".");
    await git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "seed");
    app = await startBackend({ cwd, agentDir, dataDir, socket, idleMs: 30_000, ephemeral: false, faux: true });
    peer = await dial(socket);
    legacy = await dial(socket);
    assert.deepEqual(ok(await peer.send("hello", { versions: [1], capabilities: { gitDiff: [2, 1], gitDiffWatch: [1] } })).capabilities,
      { gitDiff: 2, gitDiffWatch: 1 });
    assert.equal(ok(await legacy.send("hello", { versions: [1], capabilities: { gitDiff: [1] } })).capabilities.gitDiff, 1);
    const threads = async () => ok(await peer.send("thread.list", { limit: 32 })).threads.map(t => t.id);
    const before = await threads();
    assert.equal((await legacy.send("ext.gitDiff.summary", { cwd, range: "head" })).error.code, "invalid_request", "version 1 needs a thread");
    assert.equal((await peer.send("ext.gitDiff.summary", { cwd: "project", range: "head" })).error.code, "invalid_request");
    assert.deepEqual((await peer.send("ext.gitDiff.summary", { cwd, range: "head" })).error, needsTrust);
    await run(process.execPath, [bundle, "trust", "grant", "--cwd", cwd], { cwd, env });
    await writeFile(join(cwd, "tracked"), "changed\n");
    const summary = ok(await peer.send("ext.gitDiff.summary", { cwd, range: "head" }));
    assert.equal(summary.branch, "main");
    assert.equal(summary.checkpoint, false);
    assert.deepEqual(summary.files.map(f => [f.path, f.status]), [["tracked", "M"]]);
    const patch = ok(await peer.send("ext.gitDiff.file", { cwd, revision: summary.revision, file: 0 }));
    assert.ok(patch.lines.some(line => line.kind === "add" && line.text === "changed"));
    assert.match((await peer.send("ext.gitDiff.summary", { cwd, range: "checkpoint" })).error.message, /needs a thread/);
    const watched = ok(await peer.send("ext.gitDiffWatch.wait", { cwd }));
    assert.deepEqual(watched.summary.files.map(f => f.path), ["tracked"]);
    const { id: threadId } = ok(await peer.send("thread.new", { cwd }));
    assert.equal((await peer.send("ext.gitDiff.file", { threadId, revision: summary.revision, file: 0 })).error.code, "invalid_cursor",
      "a launch-directory locator does not address a thread");
    assert.equal((await peer.send("ext.gitDiff.summary", { threadId, cwd, range: "head" })).error.code, "invalid_request");
    assert.deepEqual((await threads()).filter(id => !before.includes(id)), [threadId], "reading a launch directory creates no thread");
  } finally {
    peer?.close();
    legacy?.close();
    await app?.close();
    await rm(home, { recursive: true, force: true });
  }
});
