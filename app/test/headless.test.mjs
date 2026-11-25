import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { once } from "node:events";
import { createHash } from "node:crypto";
import { lstat, mkdir, mkdtemp, readFile, readdir, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { createServer as httpServer } from "node:http";
import { createConnection, createServer as unixServer } from "node:net";
import { dirname, join } from "node:path";
import { createInterface } from "node:readline";
import { after, before, test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { ProjectTrustStore, RpcClient } from "@earendil-works/pi-coding-agent";
import { assertFrame } from "../../protocol/v1/replay.mjs";
import { dial, request } from "../../server/test/client.mjs";

const repo = fileURLToPath(new URL("../../", import.meta.url));
const bundle = process.env.HUSH_TEST_BACKEND ?? join(repo, "dist", "hush-backend.mjs");
const installed = process.env.HUSH_TEST_CLI;
let binDir, binary = installed;
before(async () => {
  if (installed) return;
  binDir = await mkdtemp("/tmp/hush-cli-bin-");
  binary = join(binDir, "hush");
  await promisify(execFile)("go", ["build", "-o", binary, "./app/cmd/hush"], { cwd: repo, timeout: 60_000 });
});
after(async () => { if (binDir) await rm(binDir, { recursive: true, force: true }); });

async function layout(t) {
  const dir = await mkdtemp("/tmp/hush-cli-");
  t.after(() => rm(dir, { force: true, recursive: true }));
  const cwd = join(dir, "project"), agent = join(dir, "agent"), tmp = join(dir, "tmp");
  await Promise.all([cwd, agent, tmp].map(path => mkdir(path, { mode: 0o700 })));
  return { dir, cwd, agent, tmp, env: {
    HOME: dir, TMPDIR: tmp, HUSH_PROFILE: "pi", PI_CODING_AGENT_DIR: agent, NODE_NO_WARNINGS: "1", LANG: "C",
    PATH: dirname(process.execPath) + ":" + process.env.PATH,
  } };
}

function launch(t, program, args, { cwd, env }, input = "") {
  const child = spawn(program, args, { cwd, env, stdio: ["pipe", "pipe", "pipe"] });
  let stdout = "", stderr = "";
  child.stdout.setEncoding("utf8").on("data", s => { stdout += s; });
  child.stderr.setEncoding("utf8").on("data", s => { stderr += s; });
  const exit = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, signal) => resolve({ code, signal, stdout, stderr }));
  });
  exit.catch(() => {});
  child.stdin.on("error", () => {}); // An early parser rejection can close stdin.
  if (input !== null) child.stdin.end(input);
  t.after(async () => {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    await exit;
  });
  return { child, exit };
}

const cli = (t, cfg, args, input) => launch(t, binary, args, cfg, input);
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const success = (r, stdout) => assert.deepEqual(r, { code: 0, signal: null, stdout, stderr: "" });
function failed(r, stderr, stdout = "") {
  assert.equal(r.code, 1, JSON.stringify(r));
  assert.equal(r.signal, null);
  assert.equal(r.stdout, stdout);
  assert.match(r.stderr, stderr);
}
function value(r) {
  assert.equal(r.code, 0, r.stderr);
  assert.equal(r.signal, null);
  assert.equal(r.stderr, "");
  assert.match(r.stdout, /^[^\n]+\n$/);
  return JSON.parse(r.stdout);
}

async function nextLine(lines, exit) {
  // readline can see EOF before the child's close event. In either order,
  // wait for the exit status and captured stderr to diagnose a failed startup.
  const exited = exit.then(x => {
    throw new Error("process exited before the expected line: " + JSON.stringify(x));
  });
  const result = await Promise.race([lines.next(), exited]);
  return result.done ? await exited : result.value;
}

async function faux(t, pi = false, cfg) {
  cfg ??= await layout(t);
  const host = launch(t, process.execPath, [bundle, "--faux", ...(pi ? ["--pi-events"] : [])], cfg, null);
  const lines = createInterface({ input: host.child.stdout });
  const socket = join(cfg.agent, "hush", "hush.sock");
  assert.deepEqual(JSON.parse(await nextLine(lines[Symbol.asyncIterator](), host.exit)), { ready: true, socket });
  lines.close();
  t.after(async () => {
    const p = await host.exit;
    assert.doesNotMatch(p.stdout + p.stderr, /\x1B\]/);
  });
  const wire = await dial(socket, assertFrame);
  t.after(() => wire.close());
  const hello = ok(await wire.send("hello", { versions: [1], capabilities: { piEvents: [1] } }));
  assert.deepEqual(hello.capabilities, pi ? { piEvents: 1 } : {});
  return { cfg, wire, hello, host };
}

async function eventually(read, message, ms = 5000) {
  const deadline = performance.now() + ms;
  do {
    const value = await read();
    if (value) return value;
    await delay(15);
  } while (performance.now() < deadline);
  assert.fail(message);
}

test("redirected default print handles text, length, failed model, and an aborted run", { timeout: 20_000 }, async t => {
  const { cfg, wire } = await faux(t);
  success(await cli(t, cfg, ["hello", "from args"]).exit, "Faux: hello from args\n");
  success(await cli(t, cfg, [], "piped text").exit, "Faux: piped text\n");
  success(await cli(t, cfg, ["-p", "status"]).exit, "Faux: status\n");
  failed(await cli(t, cfg, ["!length"]).exit, /output token limit/, "partial faux answer\n");
  const error = await cli(t, cfg, ["!error"]).exit;
  failed(error, /request failed.*model_error/);
  assert.doesNotMatch(error.stderr, /synthetic failure/);

  const threadId = ok(await wire.send("thread.new", {})).id;
  const requestId = "abort-this-input";
  const pending = cli(t, cfg, ["!pause", "--thread", threadId, "--request-id", requestId]);
  await eventually(async () => {
    const f = await wire.send("submission.get", { threadId, requestId });
    return f.ok && f.result.status === "placed";
  }, "print did not place its input");
  ok(await wire.send("thread.abort", { threadId }));
  failed(await pending.exit, /request aborted/);
});

test("native thread CLI persists checkout ownership, titles, done status and archive without touching files", { timeout: 20_000 }, async t => {
  const { cfg, wire, host, hello } = await faux(t);
  const cwd = await realpath(cfg.cwd);
  const git = async (path, ...args) => (await promisify(execFile)("git", args, { cwd: path,
    env: { ...cfg.env, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" } })).stdout.trim();
  await git(cwd, "init", "-q");
  await writeFile(join(cwd, "notes.txt"), "committed training notes\n");
  await writeFile(join(cwd, ".gitignore"), "ignored.txt\n");
  await git(cwd, "add", "notes.txt", ".gitignore");
  await git(cwd, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "seed");
  const head = await git(cwd, "rev-parse", "HEAD");
  await writeFile(join(cwd, "notes.txt"), "unsaved work in main\n");
  await writeFile(join(cwd, "main-untracked.txt"), "keep this\n");
  const dirty = await git(cwd, "status", "--porcelain");
  const command = async (...args) => value(await cli(t, cfg, ["thread", ...args, "--json"]).exit);
  const shared = await command("new", "--title", " Shared threads ");
  assert.deepEqual(shared.checkout, { kind: "shared", cwd });
  assert.equal(shared.title, "Shared threads");

  const worktree = ["thread", "new", "--worktree", "--title", "  Isolated   review  ", "--json"];
  new ProjectTrustStore(cfg.agent).set(cwd, false);
  failed(await cli(t, cfg, worktree).exit, /Invalid request/);
  assert.equal((await git(cwd, "worktree", "list", "--porcelain")).match(/^worktree /gm).length, 1);
  new ProjectTrustStore(cfg.agent).set(cwd, true);
  const created = value(await cli(t, cfg, worktree).exit);
  const { id, checkout } = created;
  assert.equal(created.title, "Isolated review");
  assert.equal(created.status, "idle");
  assert.equal(checkout.kind, "managed");
  assert.match(checkout.branch, /^hush\/[0-9a-f-]+$/);
  assert.equal(await git(checkout.cwd, "branch", "--show-current"), checkout.branch);
  assert.equal(await git(checkout.cwd, "rev-parse", "HEAD"), head);
  assert.equal(await readFile(join(checkout.cwd, "notes.txt"), "utf8"), "committed training notes\n");
  const snap = ok(await wire.send("subscribe", { threadId: id }));
  assert.equal(snap.state.agent.cwd, checkout.cwd);
  assert.equal(await git(cwd, "status", "--porcelain"), dirty);
  assert.equal(await git(cwd, "rev-parse", "HEAD"), head);

  success(await cli(t, cfg, ["--thread", id, "Review the checkout"]).exit, "Faux: Review the checkout\n");
  const page = await command("list", "--limit", "1");
  assert.equal(page.indexing, false);
  assert.equal(page.threads[0].id, id);
  assert.equal(page.threads[0].status, "done");
  assert.deepEqual(page.threads[0].checkout, checkout);
  assert.ok(page.nextCursor);
  failed(await cli(t, cfg, ["thread", "list", "--all", "--cursor", page.nextCursor, "--json"]).exit, /Invalid or expired cursor/);
  assert.ok(!(await command("list", "--cursor", page.nextCursor)).threads.some(row => row.id === id));

  await writeFile(join(checkout.cwd, "ignored.txt"), "ignored but valuable\n");
  await writeFile(join(checkout.cwd, "notes.txt"), "unsaved work in isolated checkout\n");
  const saved = await command("archive", "--thread", id);
  assert.deepEqual(saved, { id, archived: true });
  assert.ok(!(await command("list")).threads.some(row => row.id === id));
  const archived = (await command("list", "--all")).threads.find(row => row.id === id);
  assert.deepEqual(archived, { ...page.threads[0], archived: true });
  assert.deepEqual(await command("unarchive", "--thread", id), { id, archived: false });

  const history = ok(await wire.send("history", { threadId: id, limit: 1, range: { kind: "before", beforeEntryId: null } }));
  const atEntryId = history.entries[0].id;
  const fork = await command("fork", "--thread", id, "--at-entry", atEntryId);
  assert.deepEqual(fork.parent, { threadId: id, atEntryId });
  const list = (await command("list")).threads;
  const forkRow = list.find(row => row.id === fork.id);
  assert.equal(forkRow.status, "done");
  assert.deepEqual(forkRow.checkout, { kind: "shared", cwd: checkout.cwd });
  assert.deepEqual(list.find(row => row.id === id).checkout, forkRow.checkout, "both threads reference that checkout");
  await command("archive", "--thread", fork.id);
  await command("archive", "--thread", shared.id);
  assert.equal(await readFile(join(checkout.cwd, "ignored.txt"), "utf8"), "ignored but valuable\n");
  assert.equal(await readFile(join(checkout.cwd, "notes.txt"), "utf8"), "unsaved work in isolated checkout\n");
  assert.equal(await git(cwd, "status", "--porcelain"), dirty);

  wire.close();
  host.child.kill("SIGTERM");
  assert.equal((await host.exit).code, 0);
  const restarted = launch(t, process.execPath, [bundle, "--faux"], cfg, null);
  const lines = createInterface({ input: restarted.child.stdout });
  assert.equal(JSON.parse(await nextLine(lines[Symbol.asyncIterator](), restarted.exit)).ready, true);
  lines.close();
  const reopened = (await command("list", "--all")).threads;
  assert.deepEqual(reopened.find(row => row.id === fork.id), { ...forkRow, archived: true });
  assert.equal(reopened.find(row => row.id === id).title, created.title);
  assert.equal(reopened.find(row => row.id === id).status, "done");
  const current = await command("list");
  assert.ok(current.threads.some(row => row.id === id));
  assert.ok(!current.threads.some(row => row.id === fork.id || row.id === shared.id));
  assert.equal(await git(cwd, "rev-parse", "HEAD"), head);
  assert.equal(await git(cwd, "status", "--porcelain"), dirty);
  assert.equal(await git(checkout.cwd, "branch", "--show-current"), checkout.branch);
});

test("standalone workspace trust uses one exact-directory grant across operations and restart", { timeout: 35_000 }, async t => {
  const cfg = await layout(t);
  delete cfg.env.PI_CODING_AGENT_DIR;
  cfg.env.HUSH_PROFILE = "hush";
  const git = async (cwd, ...args) => (await promisify(execFile)("git", args, { cwd,
    env: { ...cfg.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1" } })).stdout.trim();
  const cwd = await realpath(cfg.cwd);
  await git(cwd, "init", "-q");
  await writeFile(join(cwd, "file"), "committed\n");
  await git(cwd, "add", "file");
  await git(cwd, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "seed");
  const head = await git(cwd, "rev-parse", "HEAD");
  await writeFile(join(cwd, "file"), "dirty\n");
  await writeFile(join(cwd, "untracked"), "keep\n");
  const index = await readFile(join(cwd, ".git", "index"));
  const snapshot = () => Promise.all([git(cwd, "worktree", "list", "--porcelain"), git(cwd, "show-ref"), git(cwd, "status", "--porcelain")]);
  const before = await snapshot();
  const boot = async () => {
    const host = launch(t, process.execPath, [bundle, "--faux"], cfg, null);
    const lines = createInterface({ input: host.child.stdout });
    const { socket } = JSON.parse(await nextLine(lines[Symbol.asyncIterator](), host.exit));
    lines.close();
    return { ...host, socket };
  };
  const trust = async (op, path = cwd) => value(await cli(t, cfg,
    ["--backend", bundle, "trust", op, "--cwd", path, "--json"]).exit);
  assert.deepEqual(await trust("status"), { kind: "directory", path: cwd, trusted: false, dismissed: false });
  assert.deepEqual(await readdir(cfg.dir), ["agent", "project", "tmp"]); // No daemon or profile creates on status.
  let h = await boot();
  t.after(async () => { if (h.child.exitCode === null) h.child.kill("SIGTERM"); await h.exit; });
  const wt = (path = cwd) => ["--socket", h.socket, "thread", "new", "--worktree", "--cwd", path, "--json"];
  const deny = async path => failed(await cli(t, cfg, wt(path)).exit, /not trusted by Hush/);
  await deny();
  assert.deepEqual(await snapshot(), before);
  assert.deepEqual(await readFile(join(cwd, ".git", "index")), index);

  // Pi and project records purporting to grant trust are ignored even when present.
  const pi = join(cfg.dir, ".pi", "agent"), project = join(cwd, ".pi");
  await mkdir(pi, { recursive: true });
  await mkdir(project);
  new ProjectTrustStore(pi).set(cwd, true);
  await writeFile(join(project, "settings.json"), JSON.stringify({ defaultProjectTrust: "always" }));
  // A valid grant from the old Git-only store must not become a workspace grant.
  const common = join(cwd, ".git"), gitGrantDir = join(cfg.dir, ".hush", "workspace-trust-v1");
  const { dev, ino, birthtimeNs } = await lstat(common, { bigint: true });
  const oldGrant = JSON.stringify(["git", common, String(dev), String(ino), String(birthtimeNs)]) + "\n";
  await mkdir(gitGrantDir, { mode: 0o700 });
  await writeFile(join(gitGrantDir, createHash("sha256").update(oldGrant).digest("hex")), oldGrant, { mode: 0o600 });
  for (const scope of ["git", "browse"]) {
    failed(await cli(t, cfg, ["trust", "grant", "--scope", scope, "--cwd", cwd]).exit, /use --cwd PATH for workspace trust/);
  }
  await deny();
  assert.equal((await trust("status")).trusted, false);
  const piConfigState = await readFile(join(project, "settings.json"));
  failed(await cli(t, cfg, ["--socket", h.socket, "trust", "grant", "--cwd", cwd]).exit, /local to the Hush backend host/);
  failed(await cli(t, { ...cfg, env: { ...cfg.env, HUSH_SOCKET: h.socket } }, ["trust", "grant"]).exit, /local to the Hush backend host/);
  failed(await cli(t, cfg, ["--profile", "pi", "trust", "grant"]).exit, /local to the Hush backend host/);
  const wire = await dial(h.socket);
  try {
    ok(await wire.send("hello", { versions: [1], capabilities: { workspaceTrust: [1] } }));
    assert.equal((await wire.send("ext.workspaceTrust.grant", { cwd })).error.code, "unsupported_capability");
  } finally { wire.close(); }

  const plain = await realpath(join(cfg.dir, "agent"));
  assert.deepEqual(await trust("grant", plain), { kind: "directory", path: plain, trusted: true, dismissed: false });
  failed(await cli(t, cfg, wt(plain)).exit, /requires a Git working tree/);
  await deny();
  const alias = join(cfg.dir, "alias");
  await symlink(cwd, alias);
  assert.deepEqual(await trust("grant", alias), { kind: "directory", path: cwd, trusted: true, dismissed: false });
  const grantDir = join(cfg.dir, ".hush", "workspace-work-v1");
  assert.equal((await lstat(grantDir)).mode & 0o777, 0o700);
  for (const name of await readdir(grantDir))
    assert.equal((await lstat(join(grantDir, name))).mode & 0o777, 0o600);
  const created = value(await cli(t, cfg, wt(alias)).exit);
  assert.equal(created.checkout.kind, "managed");
  assert.equal(await git(created.checkout.cwd, "rev-parse", "HEAD"), head);
  assert.equal(await readFile(join(created.checkout.cwd, "file"), "utf8"), "committed\n");
  await writeFile(join(created.checkout.cwd, "file"), "keep isolated edits\n");
  assert.equal((await trust("status", created.checkout.cwd)).trusted, false);
  const prompt = (id, text) => cli(t, cfg, ["--socket", h.socket, "--thread", id, text]).exit;
  const source = value(await cli(t, cfg, ["--socket", h.socket, "thread", "new", "--cwd", cwd, "--json"]).exit);
  success(await prompt(source.id, "one grant"), "Faux: one grant\n");
  failed(await prompt(created.id, "does not inherit the source grant"), /not trusted for tool execution/);
  await deny(created.checkout.cwd);
  assert.deepEqual(await trust("grant", created.checkout.cwd, "work"), {
    kind: "directory", path: await realpath(created.checkout.cwd), trusted: true, dismissed: false,
  });
  assert.equal((await trust("revoke", alias)).trusted, false);
  await deny(alias);
  failed(await prompt(source.id, "revoked"), /not trusted for tool execution/);
  const revoked = await snapshot();
  h.child.kill("SIGTERM");
  assert.equal((await h.exit).code, 0);
  h = await boot();
  await deny();
  assert.equal((await trust("status", alias)).trusted, false);
  success(await prompt(created.id, "still usable"), "Faux: still usable\n");
  assert.equal((await trust("revoke", created.checkout.cwd)).trusted, false);
  failed(await prompt(created.id, "revoked"), /not trusted for tool execution/);
  await deny(created.checkout.cwd);
  value(await cli(t, cfg, ["--socket", h.socket, "thread", "archive", "--thread", created.id, "--json"]).exit);
  assert.deepEqual(await snapshot(), revoked);
  assert.deepEqual(await readFile(join(cwd, ".git", "index")), index);
  assert.equal(await readFile(join(cwd, "file"), "utf8"), "dirty\n");
  assert.equal(await readFile(join(cwd, "untracked"), "utf8"), "keep\n");
  assert.deepEqual(await readFile(join(project, "settings.json")), piConfigState);
  assert.equal(await readFile(join(created.checkout.cwd, "file"), "utf8"), "keep isolated edits\n");
});

test("schedule CLI shares durable state, preserves time and never prompts", { timeout: 20_000 }, async t => {
  const cfg = await layout(t);
  delete cfg.env.PI_CODING_AGENT_DIR;
  cfg.env.HUSH_PROFILE = "hush";
  assert.equal(value(await cli(t, cfg, ["--backend", bundle, "trust", "grant", "--scope", "work", "--json"]).exit).trusted, true);
  const host = launch(t, process.execPath, [bundle, "--faux", "--schedule-late-ms", "400"], cfg, null);
  const lines = createInterface({ input: host.child.stdout });
  const { socket } = JSON.parse(await nextLine(lines[Symbol.asyncIterator](), host.exit));
  lines.close();
  t.after(async () => { if (host.child.exitCode === null) host.child.kill("SIGTERM"); await host.exit; });
  const run = (args, input) => cli(t, cfg, ["--socket", socket, ...args], input).exit;
  const source = value(await run(["thread", "new", "--json"])).id;
  const target = value(await run(["thread", "new", "--json"])).id;
  const exact = value(await run(["--no-wait", "--json", "--thread", target, "prerequisite"]));
  value(await run(["submission", "wait", "--thread", target, "--submission-id", exact.submissionId, "--json"]));
  await delay(500);
  const common = ["--thread", source, "--json"];
  const scheduled = await run(["schedule", "add", ...common, "--target", target, "--at", "2020-11-05T09:30:00+01:30",
    "--after", target + "/" + exact.submissionId, "--text", "scheduled receipt"], null);
  const item = value(scheduled);
  assert.equal(item.zone, "+01:30");
  assert.equal(item.dueMs, Date.parse("2020-11-05T09:30:00+01:30"));
  assert.deepEqual(item.prerequisite, { threadId: target, submissionId: exact.submissionId });
  const list = async () => value(await run(["schedule", "list", ...common])).items[0];
  await eventually(async () => (await list()).state === "needs_attention", "expected late attention");
  const moved = value(await run(["schedule", "reschedule", item.id, ...common, "--at", "2030-02-03T04:05:06Z"]));
  assert.equal(moved.state, "pending");
  assert.equal(moved.zone, "UTC");
  assert.equal(moved.targetThreadId, target);
  value(await run(["schedule", "reschedule", item.id, ...common, "--at", "2020-11-05T08:00:00Z"]));
  await eventually(async () => (await list()).state === "needs_attention", "expected late attention again");
  assert.equal(value(await run(["schedule", "run", item.id, ...common])).id, item.id);
  await eventually(async () => (await list()).state === "done", "scheduled work did not settle");
  const cancelled = value(await run(["schedule", "cancel", item.id, ...common]));
  assert.equal(cancelled.state, "done");
  assert.equal(cancelled.submissionId, (await list()).submissionId);
  const text = await run(["schedule", "list", "--thread", source]);
  assert.match(text.stdout, /2020-11-05T08:00:00Z .*after #.* submission #/);
  failed(await run(["schedule", "add", ...common, "--target", target, "--at", "2030-01-02T03:04:05",
    "--text", "wrong zone"]), /explicit zone/);
});

test("checkpoint CLI previews exact scope and known effects before a separate confirmation", { timeout: 15_000 }, async t => {
  const cfg = await layout(t);
  delete cfg.env.PI_CODING_AGENT_DIR;
  cfg.env.HUSH_PROFILE = "hush";
  assert.equal(value(await cli(t, cfg, ["--backend", bundle, "trust", "grant", "--scope", "work", "--json"]).exit).trusted, true);
  const host = launch(t, process.execPath, [bundle, "--faux"], cfg, null);
  const lines = createInterface({ input: host.child.stdout });
  const { socket } = JSON.parse(await nextLine(lines[Symbol.asyncIterator](), host.exit));
  lines.close();
  t.after(async () => { if (host.child.exitCode === null) host.child.kill("SIGTERM"); await host.exit; });
  const cliRun = args => cli(t, cfg, ["--socket", socket, ...args], null).exit;
  const threadId = value(await cliRun(["thread", "new", "--json"])).id;
  success(await cliRun(["--thread", threadId, "checkpoint anchor"]), "Faux: checkpoint anchor\n");
  const common = ["--thread", threadId, "--json"];
  const marker = value(await cliRun(["checkpoint", "bookmark", ...common, "--title", "Before release"]));
  assert.equal(marker.kind, "bookmark");
  assert.equal(marker.label, "Before release");
  assert.deepEqual(value(await cliRun(["checkpoint", "list", ...common])).items, [marker]);
  const { entryId } = value(await cliRun(["checkpoint", "effect", ...common, "--text", "Published fixture release"]));
  const p = value(await cliRun(["rewind", "preview", ...common, "--checkpoint", marker.id, "--restore", "files"]));
  assert.equal(p.mode, "transcript");
  assert.deepEqual(p.effects, [{ entryId, text: "Published fixture release" }]);
  assert.match(p.reason, /No verified file snapshot/);
  failed(await cliRun(["rewind", "confirm", p.id, ...common, "--restore", "files"]), /Preview again/);
  const r = value(await cliRun(["rewind", "confirm", p.id, ...common, "--restore", "transcript"]));
  assert.equal(r.state, "ready");
  assert.equal(r.atEntryId, marker.atEntryId);
  assert.notEqual(r.forkThreadId, threadId);
  assert.deepEqual(value(await cliRun(["rewind", "status", p.id, ...common])), r);
  assert.deepEqual(value(await cliRun(["rewind", "confirm", p.id, ...common, "--restore", "transcript"])), r);
});

test("no-wait prints admission; keyed submission wait is not idle wait", { timeout: 20_000 }, async t => {
  const { cfg, wire, hello } = await faux(t);
  assert.deepEqual(value(await cli(t, cfg, ["status", "--json"]).exit), hello);
  const first = value(await cli(t, cfg, ["--no-wait", "--json", "--request-id", "first", "first reply"]).exit);
  assert.deepEqual(Object.keys(first).sort(), ["requestId", "submissionId", "threadId"]);
  assert.equal(first.requestId, "first");
  const { threadId, submissionId } = first;
  const thread = ["--thread", threadId];
  const firstKey = ["--request-id", first.requestId];
  const firstResult = value(await cli(t, cfg, ["submission", "wait", ...thread, ...firstKey, "--json"]).exit);
  assert.equal(firstResult.status, "done");
  assert.equal(firstResult.submissionId, submissionId);
  assert.ok(firstResult.answerEntryId);

  const second = value(await cli(t, cfg, ["--no-wait", "--json", ...thread, "--request-id", "second", "!pause"]).exit);
  assert.equal(second.threadId, threadId);
  assert.notEqual(second.submissionId, submissionId);
  await eventually(async () => {
    const state = value(await cli(t, cfg, ["status", ...thread, "--json"]).exit);
    return state.run?.submissionIds.includes(second.submissionId);
  }, "later input did not become active");
  const later = value(await cli(t, cfg, ["submission", "get", ...thread, "--request-id", "second", "--json"]).exit);
  assert.equal(later.status, "placed");
  assert.equal(later.submissionId, second.submissionId);
  const idle = cli(t, cfg, ["wait", ...thread, "--json"]);
  assert.deepEqual(value(await cli(t, cfg, ["submission", "wait", ...thread, ...firstKey, "--json"]).exit), firstResult);
  assert.deepEqual(value(await cli(t, cfg, ["submission", "get", ...thread, "--submission-id", submissionId, "--json"]).exit), firstResult);
  success(await cli(t, cfg, ["first reply", ...thread, ...firstKey]).exit, "Faux: first reply\n");
  assert.equal(await Promise.race([idle.exit, delay(200, "still waiting")]), "still waiting");
  ok(await wire.send("thread.abort", { threadId }));
  assert.deepEqual(value(await idle.exit), { threadId, idle: true });
  const laterSettled = value(await cli(t, cfg, ["submission", "wait", ...thread, "--request-id", "second", "--json"]).exit);
  assert.equal(laterSettled.status, "unanswered");
  assert.equal(laterSettled.reason, "aborted");
});

test("raw native protocol offers no Pi events; Pi modes reject before submission", { timeout: 15_000 }, async t => {
  const { cfg, wire, hello } = await faux(t);
  const threadId = ok(await wire.send("thread.new", {})).id;
  failed(await cli(t, cfg, ["must not submit", "--thread", threadId, "--mode=json"]).exit, /piEvents/);
  failed(await cli(t, cfg, ["--mode=rpc", "--thread", threadId], '{"type":"prompt","id":"do-not-admit","message":"must not submit"}\n').exit, /piEvents/);
  const unchanged = ok(await wire.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 10 }));
  assert.deepEqual(unchanged.entries, []);

  const raw = cli(t, cfg, ["--mode", "protocol"], null);
  const lines = createInterface({ input: raw.child.stdout })[Symbol.asyncIterator]();
  const rows = [];
  async function send(req) {
    raw.child.stdin.write(JSON.stringify(req) + "\n");
    const row = await nextLine(lines, raw.exit);
    rows.push(row);
    return JSON.parse(row);
  }
  assert.deepEqual(await send(request("h", "hello", { versions: [1], capabilities: { piEvents: [1] } })),
    { type: "response", id: "h", method: "hello", ok: true, result: hello });
  const ext = await send(request("e", "ext.piEvents.subscribe", { threadId }));
  assert.equal(ext.type, "response");
  assert.equal(ext.id, "e");
  assert.equal(ext.error.code, "unsupported_capability");
  raw.child.stdin.end();
  success(await raw.exit, rows.join("\n") + "\n");

  const offered = JSON.stringify(request("pipe", "hello", { versions: [1] })) + "\r\n";
  const finite = value(await cli(t, cfg, ["--mode", "protocol"], offered).exit);
  assert.equal(finite.id, "pipe");
  assert.deepEqual(finite.result, hello);
});

test("lost settlement transport reports it, without resubmission or a fabricated answer", { timeout: 10_000 }, async t => {
  const cfg = await layout(t);
  const path = join(cfg.dir, "fixture.sock");
  const methods = [];
  const sockets = new Set();
  const peer = unixServer(socket => {
    sockets.add(socket);
    socket.once("close", () => sockets.delete(socket));
    socket.on("error", () => {});
    const lines = createInterface({ input: socket });
    lines.on("line", line => {
      const req = JSON.parse(line);
      methods.push(req.method);
      const result = req.method === "hello" ? { version: 1, epoch: "loss-fixture", capabilities: {} }
        : req.method === "submit" ? { submissionId: "88" } : undefined;
      if (!result) {
        socket.destroy(); // submission.wait reached the peer, but there is no usable reply
      } else {
        socket.write(JSON.stringify({ type: "response", id: req.id, method: req.method, ok: true, result }) + "\n");
      }
    });
  });
  t.after(() => {
    for (const socket of sockets) socket.destroy();
    return new Promise(resolve => peer.close(resolve));
  });
  peer.listen(path);
  await once(peer, "listening");
  const r = await cli(t, cfg, ["--socket", path, "--thread", "2", "--request-id", "loss", "prompt"]).exit;
  failed(r, /wait for thread 2 submission 88 \(request ID "loss"\).*transport/);
  assert.deepEqual(methods, ["hello", "submit", "hello", "submission.wait"]);
});

async function privateConfig(t) {
  const cfg = await layout(t);
  const seen = [];
  const blocked = Promise.withResolvers();
  const abandoned = Promise.withResolvers();
  const provider = httpServer(async (req, res) => {
    try {
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const call = JSON.parse(Buffer.concat(chunks));
      seen.push(call);
      const payload = JSON.stringify(call.messages);
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      if (payload.includes("headless-private-kill-marker")) {
        res.once("close", abandoned.resolve);
        res.flushHeaders();
        blocked.resolve();
        return;
      }
      const chunk = choice => "data: " + JSON.stringify({
        id: "chatcmpl-hush", object: "chat.completion.chunk", created: 1, model: "private-fixture",
        choices: [{ index: 0, ...choice }],
      }) + "\n\n";
      res.end(chunk({ delta: { content: "private final text" }, finish_reason: null }) +
        chunk({ delta: {}, finish_reason: "stop" }) + "data: [DONE]\n\n");
    } catch (err) {
      res.destroy(err);
    }
  });
  provider.listen(0, "127.0.0.1");
  await once(provider, "listening");
  t.after(() => {
    provider.closeAllConnections();
    return new Promise(resolve => provider.close(resolve));
  });
  const config = {
    "settings.json": { defaultProvider: "hush-private", defaultModel: "private-fixture",
      retry: { enabled: false }, compaction: { enabled: false } },
    "models.json": { providers: { "hush-private": {
      baseUrl: "http://127.0.0.1:" + provider.address().port + "/v1", api: "openai-completions",
      models: [{ id: "private-fixture", name: "Private fixture", reasoning: false, contextWindow: 20000, maxTokens: 500 }],
    } } },
    "auth.json": { "hush-private": { type: "api_key", key: "fixture-not-a-secret" } },
  };
  await Promise.all(Object.entries(config).map(([file, v]) =>
    writeFile(join(cfg.agent, file), JSON.stringify(v), { mode: 0o600 })));
  return { cfg, config, seen, blocked, abandoned };
}

async function accepting(path) {
  const peer = createConnection(path);
  try { await once(peer, "connect"); return true; }
  catch (err) {
    if (err.code === "ENOENT" || err.code === "ECONNREFUSED") return false;
    throw err;
  } finally { peer.destroy(); }
}

async function within(promise, ms, message) {
  const controller = new AbortController();
  try {
    return await Promise.race([promise, delay(ms, undefined, { signal: controller.signal }).then(() => assert.fail(message))]);
  } finally {
    controller.abort();
  }
}

test("private one-shot leaves no payload or detached work on exit or killed CLI", { timeout: 20_000 }, async t => {
  const { cfg, config, seen, blocked, abandoned } = await privateConfig(t);
  const flags = ["--no-session", "--backend", bundle];
  // Rejected requests cannot reach the provider or spawn a backend.
  for (const bad of [["--thread", "1"], ["--no-wait"], ["--socket", join(cfg.dir, "other.sock")], ["--mode", "json", "--continue"]]) {
    failed(await cli(t, cfg, ["private reject", ...bad, ...flags]).exit, /--no-session requires/);
  }
  assert.deepEqual(seen, []);
  assert.deepEqual(await readdir(cfg.tmp), []);

  success(await cli(t, cfg, ["headless-private-normal-marker", ...flags]).exit, "private final text\n");
  assert.equal(seen.length, 1);
  assert.match(JSON.stringify(seen[0].messages), /headless-private-normal-marker/);
  assert.deepEqual(seen[0].tools ?? [], []);
  assert.deepEqual(await readdir(cfg.tmp), []);
  assert.deepEqual((await readdir(cfg.agent)).sort(), Object.keys(config).sort());

  const running = cli(t, cfg, [...flags, "headless-private-kill-marker"]);
  await within(Promise.race([blocked.promise, running.exit.then(x => {
    throw new Error("private CLI exited before provider call: " + JSON.stringify(x));
  })]), 7000, "private provider request never arrived");
  assert.equal(seen.length, 2);
  const dirs = await readdir(cfg.tmp);
  assert.equal(dirs.length, 1);
  const privateDir = join(cfg.tmp, dirs[0]);
  assert.equal((await lstat(privateDir)).mode & 0o777, 0o700);
  const socket = join(privateDir, "hush.sock");
  assert.equal((await lstat(socket)).mode & 0o777, 0o600);
  running.child.kill("SIGKILL");
  assert.deepEqual(await running.exit, { code: null, signal: "SIGKILL", stdout: "", stderr: "" });
  await within(abandoned.promise, 5000, "provider connection outlived the parent");
  await eventually(async () => !(await accepting(socket)), "private backend still accepting after parent died");
  for (const file of await readdir(privateDir)) {
    assert.equal(file, "hush.sock");
    assert.ok((await lstat(join(privateDir, file))).isSocket());
  }
  assert.deepEqual((await readdir(cfg.agent)).sort(), Object.keys(config).sort());
  assert.deepEqual(await readdir(cfg.cwd), []);
});

test("private parent's EOF shuts down before any SDK client connects", { timeout: 12_000 }, async t => {
  const cfg = await layout(t);
  const socket = join(cfg.tmp, "hush.sock");
  const rejected = launch(t, process.execPath, [bundle, "--faux", "--parent-stdin"], cfg);
  failed(await rejected.exit, /hush-backend: configuration/);
  assert.deepEqual(await readdir(cfg.agent), []);
  for (const early of [true, false]) {
    const host = launch(t, process.execPath, [bundle, "--faux", "--no-session", "--socket", socket, "--parent-stdin"], cfg, null);
    if (!early) {
      const rl = createInterface({ input: host.child.stdout });
      assert.deepEqual(JSON.parse(await nextLine(rl[Symbol.asyncIterator](), host.exit)), { ready: true, socket });
      rl.close();
    }
    host.child.stdin.end();
    const result = await within(host.exit, 3500, "backend ignored lost parent before first SDK client");
    assert.equal(result.code, 0, result.stderr);
    // Early loss permits a socket inode if killed during listen, but no host can still serve.
    assert.equal(await accepting(socket), false);
    await rm(socket, { force: true });
  }
  assert.deepEqual(await readdir(cfg.agent), []);
});

const rows = text => {
  assert.ok(text === "" || text.endsWith("\n"), "unterminated stdout record");
  return text.split("\n").slice(0, -1).map(JSON.parse);
};
const piText = message => typeof message.content === "string" ? message.content : message.content?.filter(b => b.type === "text").map(b => b.text).join("") ?? "";
function jsonEvents(result) {
  assert.equal(result.code, 0, JSON.stringify(result));
  assert.equal(result.stderr, "");
  const emitted = rows(result.stdout);
  assert.equal(emitted[0].type, "session");
  assert.equal(emitted[0].version, 3);
  assert.ok(!emitted.slice(1).some(row => row.type === "response" || row.type === "session" || row.type === "pi_event"));
  return emitted;
}

function watchRPC(process) {
  let text = "";
  process.child.stdout.setEncoding("utf8").on("data", s => { text += s; });
  const complete = () => rows(text.slice(0, text.lastIndexOf("\n") + 1));
  return {
    send(type, fields = {}) { process.child.stdin.write(JSON.stringify({ type, ...fields }) + "\n"); },
    wait(pred) { return eventually(() => complete().find(pred), "no matching record in: " + text); },
  };
}

test("Pi JSON/print: sequential argv, stdin and @file, status per mode, stable session selection", { timeout: 20000 }, async t => {
  const { cfg, wire } = await faux(t, true);
  await writeFile(join(cfg.cwd, "fixture.txt"), "fixture file payload");
  const out = jsonEvents(await cli(t, cfg, ["--mode=json", "--model", "faux-1", "--provider=faux", "--thinking=low", "--name", "fixture",
    "@fixture.txt", "--", "-dash first", "second"], "stdin text\n").exit);
  const header = out[0];
  assert.equal(header.cwd.replace(/^\/private/, ""), cfg.cwd);
  const delivered = out.filter(r => r.type === "message_start" && r.message.role === "user").map(r => piText(r.message));
  assert.match(delivered[0], /^stdin text<file name="[^\n]+\/fixture\.txt">\nfixture file payload\n<\/file>\n-dash first$/);
  assert.deepEqual(delivered.slice(1), ["second"]);
  assert.equal(out.filter(r => r.type === "agent_settled").length, 2);
  assert.equal(out.at(-1).type, "agent_settled");
  assert.ok(out.some(r => r.type === "message_update" && r.usage && !r.message && !r.assistantMessageEvent.partial));
  assert.equal(jsonEvents(await cli(t, cfg, ["--mode=json", "--continue", "continued"]).exit)[0].id, header.id);
  success(await cli(t, cfg, ["-p", "--session", header.id, "from existing"]).exit, "Faux: from existing\n");
  const fork = jsonEvents(await cli(t, cfg, ["--mode=json", "--fork", header.id, "forked"]).exit)[0];
  assert.notEqual(fork.id, header.id);
  assert.equal(fork.parentSession, header.id);
  const modelList = await cli(t, cfg, ["--list-models", "faux-1"]).exit;
  assert.equal(modelList.code, 0, modelList.stderr);
  assert.match(modelList.stdout, /^provider\s+model\s+context\s+max-out\s+thinking\s+images\n.*faux-1/m);
  for (const [input, reason] of [["!error", "error"], ["!length", "length"], ["!aborted", "aborted"]]) {
    const trace = jsonEvents(await cli(t, cfg, ["--mode=json", input]).exit);
    assert.ok(trace.some(r => r.type === "message_end" && r.message.role === "assistant" && r.message.stopReason === reason));
    assert.equal(trace.at(-1).type, "agent_settled");
  }
  failed(await cli(t, cfg, ["-p", "!error"]).exit, /^synthetic failure\n$/);
  failed(await cli(t, cfg, ["-p", "!aborted"]).exit, /^synthetic abort\n$/);
  failed(await cli(t, cfg, ["-p", "!length"]).exit, /^Response was truncated.*\n$/, "partial faux answer\n");
  success(await cli(t, cfg, ["--print", "first", "second"]).exit, "Faux: second\n");
  const before = ok(await wire.send("thread.list", { limit: 100 })).threads;
  await writeFile(join(cfg.cwd, "pixel.png"), Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAAAAAA6fptVAAAACklEQVR4nGNgAAAAAgABSK+kcQAAAABJRU5ErkJggg==", "base64"));
  failed(await cli(t, cfg, ["--mode=json", "@pixel.png", "do not admit"]).exit, /capability: images/);
  failed(await cli(t, cfg, ["--mode=json", "--session", "/missing/pi.jsonl", "do not import"]).exit, /piSessions/);
  failed(await cli(t, cfg, ["--mode=json", "--model", "missing-physical-model", "do not admit"]).exit, /open Pi session/);
  assert.deepEqual(ok(await wire.send("thread.list", { limit: 100 })).threads, before);
});

test("Pi RPC: stock RpcClient interoperates with tools, abort, queue and trusted prompt templates", { timeout: 20000 }, async t => {
  const cfg = await layout(t);
  await mkdir(join(cfg.agent, "prompts"));
  await writeFile(join(cfg.agent, "prompts", "fixture.md"), "---\ndescription: Fixture resource\n---\nExpanded: $1\n");
  // Seed startup resources before the host caches them. This test is not a
  // filesystem-watch test; creating the template after ready races getCommands.
  await faux(t, true, cfg);
  const wrapper = join(cfg.dir, "rpc-loader.mjs");
  // RpcClient starts `node cliPath`. Replace this fixture process with the
  // compiled binary so the same stdin/stdout and signals reach hush directly.
  await writeFile(wrapper, `process.execve(${JSON.stringify(binary)}, [${JSON.stringify(binary)}, ...process.argv.slice(2)], process.env);\n`);
  const rpc = new RpcClient({ cliPath: wrapper, cwd: cfg.cwd, env: cfg.env, provider: "faux", model: "faux-1" });
  try {
    await rpc.start();
    assert.equal((await rpc.getState()).model.id, "faux-1");
    const resource = (await rpc.getCommands()).find(r => r.name === "fixture");
    assert.ok(resource, "get_commands did not return the seeded user template");
    assert.equal(resource.source, "prompt");
    assert.equal(resource.sourceInfo.scope, "user");
    const template = await rpc.promptAndWait("/fixture needle", undefined, 5000);
    assert.match(piText(template.find(r => r.type === "message_end" && r.message.role === "user").message), /Expanded: needle/);
    const tools = await rpc.promptAndWait("!fast-tool", undefined, 5000);
    assert.deepEqual(tools.filter(e => e.type === "tool_execution_update").map(e => e.partialResult.content[0].text), ["first", " second"]);
    assert.equal(tools.at(-1).type, "agent_settled");
    assert.equal(await rpc.getLastAssistantText(), "fixture tool finished");
    const events = rpc.collectEvents(5000);
    void events.catch(() => {});
    const starting = Promise.withResolvers();
    const stop = rpc.onEvent(e => {
      if (e.type === "message_end" && e.message.role === "user") starting.resolve();
    });
    try {
      assert.equal(await rpc.prompt("!pause"), "started");
      await within(starting.promise, 5000, "did not observe the paused prompt");
      await assert.rejects(rpc.prompt("must not start"), /busy/);
      assert.equal(await rpc.steer("stay queued"), "queued");
      assert.equal(await rpc.prompt("queued follow-up", undefined, "followUp"), "queued");
      await rpc.abort();
    } finally { stop(); }
    const stopped = await events;
    assert.equal(stopped.at(-1).aborted, true);
    assert.ok(stopped.some(e => e.type === "message_end" && e.message.stopReason === "aborted"));
    assert.deepEqual(await rpc.clearQueue(), { steering: ["stay queued"], followUp: ["queued follow-up"] });
    assert.equal((await rpc.getState()).isStreaming, false);
    assert.equal(rpc.getStderr(), "");
  } finally { await rpc.stop(); }
});

test("Pi RPC stdio permits only LF framing, correlates once, and rejects an absent producer before opening", { timeout: 20000 }, async t => {
  const { cfg } = await faux(t, true);
  const r = await cli(t, cfg, ["--mode=rpc"], [
    "{invalid json}", '{"type":"steer","message":"a\u2028b\u2029c","id":"same"}\r',
    '{"type":"clear_queue","id":"same"}', '{"type":"get_state"}',
    '{"type":"get_tree","id":""}', '{"type":"get_state","id":"bad-field","extra":true}', "",
  ].join("\n")).exit;
  assert.equal(r.code, 0, r.stderr);
  assert.equal(r.stderr, "");
  const stdout = rows(r.stdout);
  assert.ok(!stdout.some(e => e.type === "session" || e.type === "agent_start" || e.type === "agent_settled"));
  const answers = stdout.filter(e => e.type === "response");
  assert.equal(answers.length, 6);
  assert.deepEqual(answers.filter(e => e.id === "same").map(e => [e.command, e.data]), [
    ["steer", { disposition: "queued" }], ["clear_queue", { steering: ["a\u2028b\u2029c"], followUp: [] }],
  ]);
  assert.ok(answers.some(e => e.command === "parse" && e.success === false && e.id === undefined));
  assert.ok(answers.some(e => e.command === "get_state" && e.success && e.id === undefined));
  assert.ok(answers.some(e => e.command === "get_state" && !e.success && e.id === "bad-field"));
  assert.ok(answers.some(e => e.command === "get_tree" && !e.success && e.id === ""));
  const missing = await faux(t);
  const before = ok(await missing.wire.send("thread.list", { limit: 100 }));
  failed(await cli(t, missing.cfg, ["--mode=json", "not admitted"]).exit, /piEvents/);
  failed(await cli(t, missing.cfg, ["--mode=rpc"], '{"type":"prompt","message":"not admitted"}\n').exit, /piEvents/);
  assert.deepEqual(ok(await missing.wire.send("thread.list", { limit: 100 })), before);
});

test("Pi JSON/RPC transport loss after backend SIGKILL leaves no fabricated completion", { timeout: 20000 }, async t => {
  const { cfg, host } = await faux(t, true);
  const json = cli(t, cfg, ["--mode=json", "!pause"]);
  const rpc = cli(t, cfg, ["--mode=rpc"], null);
  const one = watchRPC(json), two = watchRPC(rpc);
  two.send("prompt", { id: "admitted", message: "!pause" });
  await Promise.all([one.wait(r => r.type === "agent_start"), two.wait(r => r.type === "response" && r.id === "admitted")]);
  host.child.kill("SIGKILL");
  await host.exit;
  for (const proc of [json, rpc]) {
    const result = await proc.exit;
    assert.equal(result.code, 1, JSON.stringify(result));
    assert.match(result.stderr, /transport/);
    assert.ok(!rows(result.stdout).some(e => e.type === "agent_settled" || e.type === "agent_end"));
  }
  const next = launch(t, process.execPath, [bundle, "--faux", "--pi-events"], cfg, null);
  const rl = createInterface({ input: next.child.stdout });
  const { socket } = JSON.parse(await nextLine(rl[Symbol.asyncIterator](), next.exit));
  rl.close();
  const peer = await dial(socket, assertFrame);
  t.after(() => peer.close());
  await peer.send("hello", { versions: [1] });
  const { threads } = ok(await peer.send("thread.list", { limit: 100 }));
  assert.equal(threads.length, 3, "recovery did not replay a profile open");
  for (const { id } of threads) {
    const dump = ok(await peer.send("history", { threadId: id, range: { kind: "before", beforeEntryId: null }, limit: 100 }));
    const inputs = dump.entries.flatMap(entry => entry.messages).filter(m => m.role === "user");
    assert.equal(inputs.length, id === threads[0].id ? 0 : 1, "recovery did not re-admit input");
  }
});

test("persistent headless joins concurrent cold starts, detaches, and reopens after a stale socket", { timeout: 25_000 }, async t => {
  const { cfg, seen } = await privateConfig(t);
  const tracking = join(cfg.dir, "backend-processes");
  const entry = join(cfg.dir, "current-backend.mjs");
  await writeFile(entry, Buffer.concat([
    Buffer.from(`import {appendFileSync as trackLauncherPID} from 'node:fs'; trackLauncherPID(${JSON.stringify(tracking)}, process.pid + '\\n');\n`),
    await readFile(bundle),
  ]));
  cfg.env.HUSH_BACKEND = entry;
  const dataDir = join(cfg.agent, "hush"), socket = join(dataDir, "hush.sock");
  const signalChildren = async signal => {
    const pids = await readFile(tracking, "utf8").catch(err => err.code === "ENOENT" ? "" : Promise.reject(err));
    for (const pid of pids.trim().split("\n").filter(Boolean).map(Number)) {
      try { process.kill(pid, signal); } catch (err) { if (err.code !== "ESRCH") throw err; }
    }
  };
  try {
    assert.equal(await accepting(socket), false);
    const prompts = cli(t, cfg, ["--mode=json", "cold-start prompt"]);
    const statuses = Array.from({ length: 4 }, () => cli(t, cfg, ["status", "--json"]));
    const [json, ...states] = await Promise.all([prompts.exit, ...statuses.map(x => x.exit)]);
    const trace = jsonEvents(json);
    assert.equal(trace.at(-1).type, "agent_settled");
    assert.deepEqual(states.map(result => value(result).epoch), Array(4).fill(value(states[0]).epoch));
    const original = value(states[0]);
    assert.deepEqual(original.capabilities, {}); // status is native; no unsolicited capabilities or ready records
    const countStarts = async () => (await readFile(join(dataDir, "hush.log"), "utf8")).split("\n").filter(line => line.endsWith(" listening")).length;
    assert.equal(await countStarts(), 1);
    assert.equal(seen.length, 1);
    const attach = { ...cfg, env: { ...cfg.env, PATH: "/nonexistent", HUSH_BACKEND: "/not/a/bundle" } };
    assert.deepEqual(value(await cli(t, attach, ["status", "--json"]).exit), original);
    const rpc = await cli(t, attach, ["--mode=rpc"], '{"id":"ready","type":"get_state"}\n').exit;
    assert.equal(rpc.code, 0, rpc.stderr);
    assert.equal(rpc.stderr, "");
    const [response] = rows(rpc.stdout);
    assert.equal(rows(rpc.stdout).length, 1);
    assert.equal(response.id, "ready");
    assert.equal(response.success, true);
    assert.equal(await countStarts(), 1, "clients attach without inspecting Node or a launch bundle");

    await signalChildren("SIGKILL");
    await eventually(async () => !(await accepting(socket)), "backend still accepting after fixture SIGKILL");
    assert.ok((await lstat(socket)).isSocket());
    const restarted = await cli(t, cfg, ["--backend", entry, "status", "--json"]).exit;
    assert.notEqual(value(restarted).epoch, original.epoch);
    assert.equal(await countStarts(), 2);
    const resumed = await cli(t, attach, ["--mode=rpc", "--session", trace[0].id], '{"id":"state","type":"get_state"}\n').exit;
    assert.equal(resumed.code, 0, resumed.stderr);
    assert.equal(rows(resumed.stdout)[0].data.sessionId, trace[0].id, "the persisted session survives backend restart");
  } finally {
    await signalChildren("SIGTERM");
    await eventually(async () => !(await accepting(socket)), "autostart backend did not close");
  }
});

test("persistent autostart never replaces an incompatible socket or executes a bundle from cwd", { timeout: 15_000 }, async t => {
  const cfg = await layout(t);
  const dataDir = join(cfg.agent, "hush"), socket = join(dataDir, "hush.sock");
  const launched = join(cfg.dir, "unexpected-start");
  const dist = join(cfg.cwd, "dist"), entry = join(dist, "hush-backend.mjs");
  await mkdir(dist);
  await writeFile(entry, `import {writeFileSync} from 'node:fs'; writeFileSync(${JSON.stringify(launched)}, 'bad');\n`);
  if (!installed) failed(await cli(t, cfg, ["status", "--json"]).exit, /no local backend bundle found/);
  const missing = join(cfg.dir, "external.sock");
  for (const attached of [
    cli(t, { ...cfg, env: { ...cfg.env, HUSH_BACKEND: entry } }, ["--socket", missing, "status", "--json"]),
    cli(t, { ...cfg, env: { ...cfg.env, HUSH_BACKEND: entry, HUSH_SOCKET: missing } }, ["--mode=rpc"]),
    cli(t, { ...cfg, env: { ...cfg.env, HUSH_BACKEND: entry } }, ["--mode=protocol"]),
  ]) failed(await attached.exit, /no such file/i);
  await assert.rejects(lstat(dataDir), { code: "ENOENT" });
  await assert.rejects(lstat(launched), { code: "ENOENT" });

  await mkdir(dataDir, { mode: 0o700 });
  const incompatible = unixServer(client => {
    client.on("error", () => {});
    createInterface({ input: client }).on("line", line => {
      const req = JSON.parse(line);
      client.end(JSON.stringify({ type: "response", id: req.id, method: req.method, ok: false,
        error: { code: "unsupported_version", message: "different core contract" } }) + "\n");
    });
  });
  incompatible.listen(socket);
  await once(incompatible, "listening");
  try {
    failed(await cli(t, cfg, ["--backend", entry, "status", "--json"]).exit, /unsupported_version/);
    assert.equal(await accepting(socket), true);
    await assert.rejects(lstat(launched), { code: "ENOENT" });
    await assert.rejects(lstat(join(dataDir, "history.sqlite")), { code: "ENOENT" });
  } finally { await new Promise(resolve => incompatible.close(resolve)); }
  await writeFile(socket, "ordinary file");
  failed(await cli(t, cfg, ["--backend", entry, "--mode=json", "no admission"]).exit, /not a Unix socket/);
  assert.equal(await readFile(socket, "utf8"), "ordinary file");
});

test("hush stop is attach-only and the next command starts the replacement backend", { timeout: 20_000 }, async t => {
  const cfg = await layout(t);
  delete cfg.env.HUSH_PROFILE;
  const entry = join(cfg.dir, "stop-fixture.mjs");
  await writeFile(entry, Buffer.concat([Buffer.from("process.argv.push('--faux', '--idle-ms', '1000');\n"), await readFile(bundle)]));
  cfg.env.HUSH_BACKEND = entry;
  const hush = join(cfg.dir, ".hush"), socket = join(hush, "runtime", "hush.sock");

  const legacyPath = join(cfg.dir, "legacy.sock"), legacyMethods = [];
  const legacy = unixServer(peer => {
    peer.on("error", () => {});
    createInterface({ input: peer }).on("line", line => {
      const req = JSON.parse(line);
      legacyMethods.push(req.method);
      peer.write(JSON.stringify({ type: "response", id: req.id, method: req.method, ok: true,
        result: { version: 1, epoch: "legacy", capabilities: {} } }) + "\n");
    });
  });
  legacy.listen(legacyPath);
  await once(legacy, "listening");
  failed(await cli(t, cfg, ["--socket", legacyPath, "stop"]).exit, /unsupported_capability \(runtime\)/);
  assert.deepEqual(legacyMethods, ["hello"]);
  assert.equal(await accepting(legacyPath), true);
  await new Promise(resolve => legacy.close(resolve));

  assert.deepEqual(value(await cli(t, cfg, ["stop", "--json"]).exit), { stopped: false });
  await assert.rejects(lstat(hush), { code: "ENOENT" });
  const first = value(await cli(t, cfg, ["status", "--json"]).exit);
  const threadId = value(await cli(t, cfg, ["thread", "new", "--json"]).exit).id;
  assert.deepEqual(value(await cli(t, cfg, ["stop", "--json"]).exit), { stopped: true });
  await eventually(async () => !(await accepting(socket)), "stop returned before the old listener disappeared");

  const second = value(await cli(t, cfg, ["status", "--json"]).exit);
  assert.notEqual(second.epoch, first.epoch);
  const threads = value(await cli(t, cfg, ["thread", "list", "--json"]).exit);
  assert.ok(threads.threads.some(row => row.id === threadId));
  success(await cli(t, cfg, ["stop"]).exit, "backend stopped\n");
  success(await cli(t, cfg, ["stop"]).exit, "backend is not running\n");
  value(await cli(t, cfg, ["status", "--json"]).exit);
  success(await cli(t, cfg, ["stop", "-q"]).exit, "");
  success(await cli(t, cfg, ["stop", "--quiet"]).exit, "");
});

test("managed work cannot fall back to a running host without tool trust", { timeout: 10_000 }, async t => {
  const cfg = await layout(t);
  delete cfg.env.HUSH_PROFILE;
  const runtime = join(cfg.dir, ".hush", "runtime");
  await mkdir(runtime, { recursive: true, mode: 0o700 });
  const seen = [];
  const legacy = unixServer(peer => {
    peer.on("error", () => {});
    createInterface({ input: peer }).on("line", line => {
      const req = JSON.parse(line);
      seen.push(req.method);
      const result = req.method === "hello" ? { version: 1, epoch: "legacy-hush", capabilities: { hushProfile: 1, storeIdentity: 1 } }
        : { storeId: "10000000-0000-4000-8000-000000000001", generation: "20000000-0000-4000-8000-000000000001" };
      peer.write(JSON.stringify({ type: "response", id: req.id, method: req.method, ok: true, result }) + "\n");
    });
  });
  legacy.listen(join(runtime, "hush.sock"));
  await once(legacy, "listening");
  t.after(() => new Promise(resolve => legacy.close(resolve)));
  failed(await cli(t, cfg, ["--print", "no fallback"]).exit, /unsupported_capability.*workspaceWorkTrust/);
  assert.deepEqual(seen, ["hello", "ext.storeIdentity.get"]);
});

test("the default standalone host ignores Pi profiles, shares its writer, and prints through native admission", { timeout: 20_000 }, async t => {
  for (const populated of [false, true]) {
    const cfg = await layout(t);
    delete cfg.env.HUSH_PROFILE;
    cfg.env.PI_PROVIDER = "unavailable-pi-provider";
    cfg.env.PI_MODEL = "unavailable-pi-model";
    const hush = join(cfg.dir, ".hush"), runtime = join(hush, "runtime"), socket = join(runtime, "hush.sock");
    const sentinels = new Map();
    if (populated) {
      for (const root of [cfg.agent, join(cfg.dir, ".pi", "agent"), join(cfg.cwd, ".pi"), join(cfg.cwd, ".hush")]) {
        await mkdir(root, { mode: 0o700, recursive: true });
        for (const [name, data] of Object.entries({
          "settings.json": '{"defaultProvider":"other","defaultModel":"other","defaultTools":[]}',
          "models.json": '{"providers":{"other":{"baseUrl":"http://127.0.0.1:1"}}}',
          "auth.json": '{"other":{"type":"api_key","key":"not-a-real-secret"}}',
          "SYSTEM.md": "pi-or-project-prompt-sentinel",
        })) {
          const path = join(root, name);
          await writeFile(path, data, { mode: 0o600 });
          sentinels.set(path, data);
        }
      }
    }
    const entry = join(cfg.dir, "standalone-fixture.mjs");
    // Bounded idle timeout also handles cleanup of a failed detached fixture.
    await writeFile(entry, Buffer.concat([Buffer.from("process.argv.push('--faux', '--idle-ms', '1000');\n"), await readFile(bundle)]));
    cfg.env.HUSH_BACKEND = entry;
    success(await cli(t, cfg, ["--no-session", "-p", "!fast-tool"]).exit, "Faux: !fast-tool\n");
    assert.deepEqual(await readdir(cfg.tmp), []);
    await assert.rejects(lstat(hush), { code: "ENOENT" });
    assert.equal(value(await cli(t, cfg, ["--backend", bundle, "trust", "grant", "--scope", "work", "--json"]).exit).trusted, true);

    const status = () => cli(t, cfg, ["status", "--json"]).exit;
    const [a, b, answer] = await Promise.all([status(), status(), cli(t, cfg, ["--print", "standalone", "answer"]).exit]);
    assert.deepEqual(value(a), value(b));
    assert.deepEqual(value(a).capabilities, { hushProfile: 1, storeIdentity: 1 });
    success(answer, "Faux: standalone answer\n");
    assert.equal((await lstat(hush)).mode & 0o777, 0o700);
    assert.equal((await lstat(runtime)).mode & 0o777, 0o700);
    assert.equal((await lstat(socket)).mode & 0o777, 0o600);
    assert.equal((await lstat(join(runtime, "history.sqlite"))).mode & 0o777, 0o600);
    const wire = await dial(socket, assertFrame);
    try {
      const hello = ok(await wire.send("hello", { versions: [1], capabilities: { hushProfile: [1], piEvents: [1] } }));
      assert.deepEqual(hello.capabilities, { hushProfile: 1 });
      const threadId = ok(await wire.send("thread.new", { cwd: cfg.cwd })).id;
      const snap = ok(await wire.send("subscribe", { threadId }));
      assert.equal(snap.state.agent.model.provider, "faux");
      assert.equal(snap.state.agent.model.modelId, "faux-1");
      assert.equal(snap.state.agent.cwd, cfg.cwd);
    } finally { wire.close(); }
    for (const [path, data] of sentinels) assert.equal(await readFile(path, "utf8"), data);
    await assert.rejects(lstat(join(cfg.agent, "hush")), { code: "ENOENT" });
    if (!populated) {
      assert.deepEqual(await readdir(cfg.cwd), []);
      await assert.rejects(lstat(join(cfg.dir, ".pi")), { code: "ENOENT" });
    }
    await eventually(async () => lstat(socket).then(() => false, err => {
      if (err.code === "ENOENT") return true;
      throw err;
    }), "fixture daemon did not stop on idle");
    const log = await readFile(join(runtime, "hush.log"), "utf8");
    assert.equal(log.split("\n").filter(row => row.endsWith(" listening")).length, 1);
  }
});

test("native print sends the bundled Hush prompt and refreshes only applicable supplements", { timeout: 20_000 }, async t => {
  const { cfg, config, seen } = await privateConfig(t);
  delete cfg.env.HUSH_PROFILE;
  cfg.env.TERM_PROGRAM = "rex";
  cfg.env.SHELL = "/pretend/login/fish";
  const workspace = cfg.cwd;
  const home = join(cfg.dir, ".hush");
  const middle = join(workspace, "src");
  const leaf = join(middle, "nested");
  const projectHush = join(leaf, ".hush");
  await Promise.all([home, projectHush, join(workspace, ".git"), leaf].map(path =>
    mkdir(path, { mode: 0o700, recursive: true })));
  cfg.cwd = await realpath(leaf);
  const agents = join(cfg.cwd, "AGENTS.md");
  const outside = join(cfg.dir, "AGENTS.md");
  const ignored = ["pi-system-sentinel", "pi-append-sentinel", "project-system-sentinel", "project-append-sentinel",
    "host-replacement-sentinel", "above-repository-sentinel", "symlink-agents-sentinel", "pi-agents-sentinel"];
  await Promise.all([
    ...["models.json", "auth.json"].map(file => writeFile(join(home, file), JSON.stringify(config[file]), { mode: 0o600 })),
    writeFile(join(cfg.agent, "SYSTEM.md"), ignored[0]),
    writeFile(join(cfg.agent, "APPEND_SYSTEM.md"), ignored[1]),
    writeFile(join(projectHush, "SYSTEM.md"), ignored[2]),
    writeFile(join(projectHush, "APPEND_SYSTEM.md"), ignored[3]),
    writeFile(join(home, "SYSTEM.md"), ignored[4]),
    writeFile(outside, ignored[5]),
    writeFile(join(cfg.dir, "unscoped.md"), ignored[6]),
    writeFile(join(cfg.agent, "AGENTS.md"), ignored[7]),
    symlink(join(cfg.dir, "unscoped.md"), join(middle, "AGENTS.md")),
    writeFile(join(workspace, "AGENTS.md"), "repo-agents-sentinel\n"),
    writeFile(agents, "leaf-agents-original\n"),
  ]);
  assert.equal(value(await cli(t, cfg, ["--backend", bundle, "trust", "grant", "--scope", "work", "--json"]).exit).trusted, true);
  const host = launch(t, process.execPath, [bundle, "--model", "hush-private/private-fixture"], cfg, null);
  const lines = createInterface({ input: host.child.stdout });
  assert.deepEqual(JSON.parse(await nextLine(lines[Symbol.asyncIterator](), host.exit)), {
    ready: true, socket: join(home, "runtime", "hush.sock"),
  });
  lines.close();
  const thread = value(await cli(t, cfg, ["thread", "new", "--json"]).exit).id;
  const bundled = (await readFile(join(repo, "backend/src/prompts/system.md"), "utf8")).trimEnd();
  const append = join(home, "APPEND_SYSTEM.md");
  await assert.rejects(lstat(append), { code: "ENOENT" });
  for (const [i, local] of ["leaf-agents-original", "leaf-agents-edited"].entries()) {
    if (i) {
      await writeFile(append, "personal-append-sentinel\n");
      await writeFile(agents, local + "\n");
      // An oversized regular file is ignored even when in scope.
      await rm(join(middle, "AGENTS.md"));
      await writeFile(join(middle, "AGENTS.md"), "oversized-agents-sentinel" + "x".repeat(64 * 1024));
    }
    success(await cli(t, cfg, ["--print", "--thread", thread, `native-prompt-fixture-${i}`]).exit, "private final text\n");
    assert.equal(seen.length, i + 1);
    const { messages, tools } = seen[i];
    const prompt = messages.filter(message => message.role === "system" || message.role === "developer")
      .map(message => message.content).join("\n\n");
    const expected = [bundled, `Working directory: ${JSON.stringify(cfg.cwd)}`,
      ...(i ? ["## Personal instructions", "personal-append-sentinel"] : []),
      "## Workspace context", "repo-agents-sentinel", local, "## Environment", "Tool host OS:"];
    assert.doesNotMatch(prompt, /\n- Client |\/pretend\/login\/fish/);
    let after = 0;
    for (const text of expected) {
      const at = prompt.indexOf(text, after);
      assert.ok(at >= after, `missing or out of order: ${text}\n${prompt}`);
      after = at + text.length;
      assert.equal(prompt.split(text).length, 2, `duplicated: ${text}`);
    }
    const declared = new Map(tools.map(({ function: tool }) => [tool.name, tool]));
    assert.deepEqual([...declared.keys()].sort(), ["bash", "edit", "read", "write"]);
    assert.doesNotMatch(prompt, /^- (bash|edit|read|write): /m);
    const edit = declared.get("edit");
    assert.equal(edit.description, "Replace exact text in one file. Keep replacements small.");
    const edits = edit.parameters.properties.edits;
    assert.match(edits.description, /matched against the original file/);
    assert.match(edits.items.properties.oldText.description, /unique.*not overlap/);
    for (const text of [...ignored, "oversized-agents-sentinel", ...(i ? ["leaf-agents-original"] : ["## Personal instructions", "personal-append-sentinel"])]) {
      assert.ok(!JSON.stringify(messages).includes(text), `unexpected: ${text}`);
    }
    for (const message of messages.filter(m => m.role !== "system" && m.role !== "developer")) {
      assert.ok(!JSON.stringify(message).includes("You are Hush"));
    }
  }
  // The scripted reply only drives the CLI to settlement; the requests are the prompt evidence.
  host.child.kill("SIGTERM");
  assert.equal((await host.exit).code, 0);
});

test("managed Hush refuses an opt-in Pi listener and store at its own path", { timeout: 15_000 }, async t => {
  const cfg = await layout(t);
  delete cfg.env.HUSH_PROFILE;
  const data = join(cfg.dir, ".hush", "runtime"), socket = join(data, "hush.sock");
  const host = launch(t, process.execPath, [bundle, "--data-dir", data, "--faux", "--pi-events"], cfg, null);
  const lines = createInterface({ input: host.child.stdout });
  assert.deepEqual(JSON.parse(await nextLine(lines[Symbol.asyncIterator](), host.exit)), { ready: true, socket });
  lines.close();
  // A Pi listener lacks both capabilities; the SDK need only report one.
  failed(await cli(t, cfg, ["--backend", bundle, "status", "--json"]).exit, /unsupported_capability \((?:hushProfile|storeIdentity)\)/);
  assert.deepEqual(value(await cli(t, cfg, ["--socket", socket, "status", "--json"]).exit).capabilities, {});
  assert.equal(await accepting(socket), true);
  host.child.kill("SIGTERM");
  assert.equal((await host.exit).code, 0);
  const history = join(data, "history.sqlite"), before = await readFile(history);
  failed(await cli(t, cfg, ["--backend", bundle, "status", "--json"]).exit, /store_profile/);
  assert.deepEqual(await readFile(history), before);
  await assert.rejects(lstat(join(data, "store-profile")), { code: "ENOENT" });
});

for (const mode of ["json", "rpc"]) {
  test(`Pi ${mode} --no-session keeps invocation data private on normal exit and SIGKILL`, { timeout: 20000 }, async t => {
    const { cfg, config, seen, blocked, abandoned } = await privateConfig(t);
    const args = ["--mode=" + mode, "--no-session", "--backend", bundle];
    const normalMarker = "private-normal-" + mode;
    await writeFile(join(cfg.cwd, "system.txt"), "invocation-private system prompt");
    const prefs = ["--system-prompt", "system.txt", "--append-system-prompt", "one extra line", "--append-system-prompt", "second extra line"];
    const run = (message, extra = []) => {
      if (mode === "json") return { proc: cli(t, cfg, [...args, ...extra, message]) };
      const proc = cli(t, cfg, [...args, ...extra], null);
      const rpc = watchRPC(proc);
      rpc.send("prompt", { id: "prompt", message });
      return { proc, rpc };
    };
    const normal = run(normalMarker, prefs);
    if (normal.rpc) {
      await normal.rpc.wait(r => r.type === "agent_settled");
      normal.proc.child.stdin.end();
    }
    const result = await normal.proc.exit;
    assert.equal(result.code, 0, result.stderr);
    assert.equal(result.stderr, "");
    assert.deepEqual(await readdir(cfg.tmp), []);
    assert.match(JSON.stringify(seen[0].messages), /invocation-private system prompt/);
    assert.match(JSON.stringify(seen[0].messages), /one extra line.*second extra line/);
    assert.match(JSON.stringify(seen[0].messages), new RegExp(normalMarker));
    assert.deepEqual(seen[0].tools ?? [], []);
    const killing = run("headless-private-kill-marker-" + mode);
    await within(blocked.promise, 7000, "provider did not see the held private input");
    const dirs = await readdir(cfg.tmp);
    assert.equal(dirs.length, 1);
    killing.proc.child.kill("SIGKILL");
    const killed = await killing.proc.exit;
    assert.equal(killed.signal, "SIGKILL");
    assert.ok(!rows(killed.stdout).some(r => r.type === "agent_settled"));
    await within(abandoned.promise, 5000, "private provider connection outlived its client");
    await eventually(async () => !(await accepting(join(cfg.tmp, dirs[0], "hush.sock"))), "private listener outlived its client");
    for (const file of await readdir(join(cfg.tmp, dirs[0]))) {
      assert.equal(file, "hush.sock");
      assert.ok((await lstat(join(cfg.tmp, dirs[0], file))).isSocket());
    }
    assert.deepEqual((await readdir(cfg.agent)).sort(), Object.keys(config).sort());
    assert.deepEqual(await readdir(cfg.cwd), ["system.txt"]);
  });
}
