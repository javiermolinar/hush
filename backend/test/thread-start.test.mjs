import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdir, mkdtemp, realpath, rm } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from "@earendil-works/pi-ai";
import { createRegistry, Harness, MemoryStorage, ToolResultEntry } from "@earendil-works/pi-durable";
import { LocalEnvironment } from "../../dist/backend/src/resources.js";
import { threadStart } from "../../dist/backend/src/thread-start.js";
import { ToolPolicy } from "../../dist/backend/src/tool-policy.js";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";

const model = { provider: "faux", modelId: "faux-1" };
const text = message => typeof message.content === "string" ? message.content : message.content.map(b => b.text ?? "").join("");

// A real Harness, ToolPolicy and trust store with a scripted model. `start`
// is the thread_start call the parent makes for the "go" input; hooks let a
// test act at the commit boundaries a user or another process could reach.
async function fixture(t, { start, worktrees, routing, policyHook } = {}) {
  const ctx = withAbortSignal(t.signal, BACKGROUND_CONTEXT);
  const dir = await realpath(await mkdtemp("/tmp/hush-thread-start-"));
  const repo = join(dir, "repo"), agentDir = join(dir, ".hush");
  let h;
  t.after(async () => { await h?.close(BACKGROUND_CONTEXT); await rm(dir, { force: true, recursive: true }); });
  await mkdir(repo);
  const env = { HOME: dir, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" };
  const git = (cwd, ...args) => execFileSync("git", args, { cwd, env, stdio: "pipe" }).toString();
  git(repo, "init", "-q");
  git(repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-qm", "base");
  const trust = new WorkspaceTrust(agentDir);
  await trust.change(repo, "grant", t.signal);
  const policy = new ToolPolicy(() => h, trust, cwd => cwd ?? repo, [agentDir]);
  // ToolPolicy has private state, so wrap methods on the instance itself.
  if (policyHook) for (const [name, wrap] of Object.entries(policyHook)) policy[name] = wrap(policy[name].bind(policy));
  const ext = threadStart({ harness: () => h, policy, trust, defaults: async cwd => ({ cwd, model }), requireModel: () => undefined,
    worktrees: () => worktrees?.(h, ctx), routing });
  const faux = fauxProvider(), models = createModels(), registry = createRegistry();
  models.setProvider(faux.provider);
  faux.setResponses(Array(16).fill(({ messages }) => {
    const last = messages.findLast(m => m.role !== "system");
    if (last.role === "toolResult") return fauxAssistantMessage("Parent done.");
    const said = text(last);
    if (said === "go") return fauxAssistantMessage(fauxToolCall("thread_start", start), { stopReason: "toolUse" });
    return fauxAssistantMessage(`ack: ${said}`);
  }));
  registry.install(ext);
  const forks = [];
  h = await Harness.open(new MemoryStorage(), { models, registry, env: ({ cwd }) => new LocalEnvironment(cwd ?? repo),
    conversationCreated: (tx, c) => { if (c.parent) forks.push(c.id); },
    settings: { extensions: [ext], compaction: { enabled: false }, retry: { enabled: false } } }, ctx);
  const parent = await h.root(ctx, { agent: { cwd: repo, model } });
  const say = async content => assert.equal((await (await parent.submit({ type: "input", content }, ctx)).wait(ctx)).status, "done");
  const receipt = async () => {
    const { items } = await parent.entries({ order: "descending" }, 16, undefined, ctx);
    const row = items.find(e => ToolResultEntry.is(e)).model[0];
    if (!row.content[0].text.startsWith("{")) assert.fail(row.content[0].text);
    return { ...JSON.parse(row.content[0].text), isError: !!row.isError };
  };
  return { ctx, dir, repo, git, trust, parent, say, receipt, forks, get h() { return h; } };
}

test("a moved thread used before admission does not get the generated first prompt", { timeout: 10_000 }, async t => {
  let used;
  const f = await fixture(t, {
    start: { prompt: "Continue there", worktree: "new", history: true },
    // Reserve the thread as the real checkout service does, then use it as a
    // person would before the first prompt is admitted.
    worktrees: (h, ctx) => ({ async create(_cwd, _signal, plan) {
      const cwd = join(f.dir, "checkout");
      await mkdir(cwd);
      await f.trust.change(cwd, "grant", ctx.abortSignal);
      await plan.reserve({ cwd, root: cwd, branch: "hush/x", head: "0".repeat(40), repository: f.repo });
      used = f.forks.at(-1);
      const child = await h.conversation(used, ctx);
      assert.equal((await (await child.submit({ type: "input", content: "I took this thread" }, ctx)).wait(ctx)).status, "done");
      return { cwd, branch: "hush/x", repository: f.repo };
    } }),
  });
  await f.say("remember mango");
  await f.say("go");
  const receipt = await f.receipt();
  assert.equal(receipt.isError, true);
  assert.match(receipt.reason, /first prompt was not submitted/);
  assert.equal(receipt.threadId, String(used));
  assert.equal(await f.h.commit(tx => tx.submissionByRequest(used, "hush.thread-start"), f.ctx), undefined);
});

test("an existing worktree must still be on the selected branch when the first prompt is admitted", { timeout: 10_000 }, async t => {
  let switched = false;
  const f = await fixture(t, {
    start: { prompt: "Work on the branch", worktree: "feature" },
    // Allocation is the last commit before admission. Move the checkout to
    // another branch there.
    policyHook: { confirmDefaultWorkspace: original => (...args) => {
      if (!switched) { switched = true; f.git(join(f.dir, "feature"), "switch", "-qc", "other"); }
      return original(...args);
    } },
  });
  const target = join(f.dir, "feature");
  f.git(f.repo, "worktree", "add", "-qb", "feature", target);
  await f.trust.change(target, "grant", t.signal);
  await f.say("go");
  const receipt = await f.receipt();
  assert.ok(switched);
  assert.equal(receipt.isError, true);
  assert.match(receipt.reason, /no longer on branch feature/);
  assert.equal(await f.h.commit(tx => tx.submissionByRequest(Number(receipt.threadId), "hush.thread-start"), f.ctx), undefined);
});

for (const [outcome, opened] of [["confirmed", true], ["rejected", false], ["unknown", "unknown"]]) {
  test(`a move reports a ${outcome} focus as opened: ${opened}`, { timeout: 10_000 }, async t => {
    const requests = [];
    const f = await fixture(t, { start: { prompt: "Continue here", history: true },
      routing: { dispatchRouted: async request => { requests.push(request); return { outcome }; } } });
    await f.say("remember mango");
    await f.say("go");
    const receipt = await f.receipt();
    assert.equal(receipt.opened, opened, JSON.stringify(receipt));
    assert.deepEqual(requests, [{ op: "thread_open", threadId: receipt.threadId, where: "focus" }]);
  });
}

const { ThreadWorktrees } = await import("../../dist/backend/src/worktrees.js");
const { stat, readdir } = await import("node:fs/promises");

test("worktree preflight distinguishes a Git error from a changed turn", { timeout: 8_000 }, async t => {
  const f = await fixture(t, { start: { prompt: "Continue there", worktree: "new" },
    worktrees: () => new ThreadWorktrees(f.dir, f.trust) });
  await rm(join(f.repo, ".git"), { recursive: true });
  await f.say("go");
  const receipt = await f.receipt();
  assert.equal(receipt.isError, true);
  assert.equal(receipt.threadId, undefined);
  assert.match(receipt.reason, /git|repository|worktree/i);
});

for (const abortBeforeCommit of [true, false]) test(`Stop ${abortBeforeCommit ? "before" : "after"} the worktree reservation commit`, { timeout: 8_000 }, async t => {
  const paused = Promise.withResolvers(), abortObserved = Promise.withResolvers(), release = Promise.withResolvers(), effected = Promise.withResolvers();
  t.after(() => release.resolve());
  let planned;
  const f = await fixture(t, { start: { prompt: "Continue there", worktree: "new" }, worktrees: () => ({
    async create(cwd, signal, options) {
      signal.addEventListener("abort", abortObserved.resolve, { once: true });
      try {
        return await new ThreadWorktrees(f.dir, f.trust).create(cwd, signal, { ...options, async reserve(allocation) {
          planned = allocation;
          if (!abortBeforeCommit) await options.reserve(allocation);
          paused.resolve();
          await release.promise;
          if (abortBeforeCommit) await options.reserve(allocation);
        } });
      } finally { effected.resolve(); }
    },
  }) });
  const source = await f.parent.submit({ type: "input", content: "go" }, f.ctx);
  await paused.promise;
  const onDisk = () => stat(planned.cwd).then(() => true, e => { assert.equal(e.code, "ENOENT"); return false; });
  assert.equal(await onDisk(), false);
  const stopped = f.parent.abort(f.ctx);
  await abortObserved.promise;
  release.resolve();
  await stopped;
  assert.equal((await source.wait(f.ctx)).status, "unanswered");
  await effected.promise;
  const { items } = await f.parent.entries({ order: "descending" }, 16, undefined, f.ctx);
  const notices = items.filter(item => item.kind === "hush.thread-start-notice");
  assert.equal(notices.length, abortBeforeCommit ? 0 : 1);
  assert.equal(await onDisk(), !abortBeforeCommit);
  if (notices.length) {
    const child = Number(/^Thread (\d+)/.exec(notices[0].data.text)?.[1]);
    assert.ok(child);
    assert.equal(await f.h.commit(tx => tx.submissionByRequest(child, "hush.thread-start"), f.ctx), undefined);
  } else assert.deepEqual(await readdir(join(f.dir, "worktrees")), []);
});

test("native checkout remains discoverable if grant publication fails", { timeout: 8_000 }, async t => {
  const f = await fixture(t);
  const { ThreadDirectory } = await import("../../dist/backend/src/thread-directory.js");
  const realChange = f.trust.change.bind(f.trust);
  f.trust.change = (path, ...args) => path.startsWith(join(f.dir, "worktrees") + "/") ? Promise.reject(new Error("fixture grant write failed")) : realChange(path, ...args);
  const directory = new ThreadDirectory(f.h, f.repo, async cwd => ({ cwd, model }), new ThreadWorktrees(f.dir, f.trust));
  try {
    const ids = () => f.h.commit(async tx => (await tx.scanConversations({}, 10)).items.map(row => row.id), f.ctx);
    const before = await ids();
    const result = await directory.management.create({ cwd: f.repo, worktree: true }, t.signal).catch(e => e);
    const registered = f.git(f.repo, "worktree", "list", "--porcelain");
    const leftover = registered.split("\n").filter(line => line.startsWith("worktree ")).length > 1;
    if (leftover) assert.notDeepEqual(await ids(), before, `Git checkout exists but Hush recorded no thread (${result.code ?? result})`);
  } finally { await directory.close(); }
});

test("an unrouted history move omits opened", { timeout: 8_000 }, async t => {
  const { PaneRoutes } = await import("../../dist/server/src/pane-control.js");
  const { PaneRouting } = await import("../../dist/backend/src/pane-routing.js");
  const f = await fixture(t, { start: { prompt: "Continue here", history: true }, routing: new PaneRouting(new PaneRoutes()) });
  await f.say("remember mango");
  await f.say("go");
  const receipt = await f.receipt();
  assert.equal(receipt.history, true);
  assert.equal(receipt.opened, undefined, JSON.stringify(receipt));
});

test("a stopped but completed checkout is listed in its workspace", { timeout: 8_000 }, async t => {
  const paused = Promise.withResolvers(), abortObserved = Promise.withResolvers(), release = Promise.withResolvers(), created = Promise.withResolvers();
  t.after(() => release.resolve());
  const f = await fixture(t, { start: { prompt: "Continue there", worktree: "new" }, worktrees: () => ({
    async create(cwd, signal, plan) {
      signal.addEventListener("abort", abortObserved.resolve, { once: true });
      const result = await new ThreadWorktrees(f.dir, f.trust).create(cwd, signal, { ...plan, async reserve(allocation) {
        await plan.reserve(allocation);
        paused.resolve();
        await release.promise;
      } });
      created.resolve(result);
      return result;
    },
  }) });
  const { ThreadDirectory } = await import("../../dist/backend/src/thread-directory.js");
  const directory = new ThreadDirectory(f.h, f.repo, async cwd => ({ cwd, model }), new ThreadWorktrees(f.dir, f.trust));
  try {
    const q = { limit: 10, workspace: f.repo };
    await f.parent.submit({ type: "input", content: "go" }, f.ctx);
    await paused.promise;
    const list = opts => directory.list(opts, t.signal, 2, 2);
    const all = await list({ ...q, browseAll: true });
    const i = all.threads.findIndex(row => row.id !== String(f.parent.id));
    assert.ok(i >= 0, "fixture must index the reservation before Git starts");
    assert.equal(all.workspaceMatches[i], "unknown");
    const stopped = f.parent.abort(f.ctx);
    await abortObserved.promise;
    release.resolve();
    await stopped;
    assert.ok((await stat((await created.promise).cwd)).isDirectory());
    const scoped = await list(q);
    assert.ok(scoped.threads.some(row => row.id === all.threads[i].id), "completed checkout is still absent from its workspace's thread picker");
  } finally { await directory.close(); }
});

const { rename } = await import("node:fs/promises");
const { bindExecutionWorkspace } = await import("../../dist/backend/src/execution-workspace.js");

test("inherited trust is granted only to the verified checkout, not a directory swapped into its path", { timeout: 8_000 }, async t => {
  const f = await fixture(t);
  const realChange = f.trust.change.bind(f.trust);
  let swapped;
  f.trust.change = async (path, action, signal, expected) => {
    if (path.startsWith(join(f.dir, "worktrees") + "/")) {
      await rename(path, path + ".moved");
      await mkdir(path);
      swapped = path;
    }
    return realChange(path, action, signal, expected);
  };
  const reservation = { name: async () => ({ fallback: "" }), reserve: async () => {} };
  await assert.rejects(new ThreadWorktrees(f.dir, f.trust).create(f.repo, t.signal, reservation));
  assert.ok(swapped);
  assert.equal((await f.trust.review(swapped)).trusted, false, "the replacement directory was not granted");
});

test("a source grant replaced after reservation stops the checkout before Git writes it", { timeout: 8_000 }, async t => {
  const f = await fixture(t);
  const { grant } = await bindExecutionWorkspace(f.trust, f.repo, t.signal);
  const reservation = { name: async () => ({ fallback: "" }), grant, reserve: async () => {
    await f.trust.change(f.repo, "revoke", t.signal);
    await f.trust.change(f.repo, "grant", t.signal);
  } };
  await assert.rejects(new ThreadWorktrees(f.dir, f.trust).create(f.repo, t.signal, reservation));
  assert.equal(f.git(f.repo, "worktree", "list", "--porcelain").match(/^worktree /gm).length, 1, "no checkout was written");
});
