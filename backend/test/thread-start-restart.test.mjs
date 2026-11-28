import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, realpath, rm, stat } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from "@earendil-works/pi-ai";
import { createRegistry, Harness } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { LocalEnvironment } from "../../dist/backend/src/resources.js";
import { threadStart } from "../../dist/backend/src/thread-start.js";
import { ToolPolicy } from "../../dist/backend/src/tool-policy.js";
import { ThreadWorktrees } from "../../dist/backend/src/worktrees.js";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";

const model = { provider: "faux", modelId: "faux-1" };
const crashCode = 38;

async function open(dir, ctx, cut) {
  const repo = join(dir, "repo"), trust = new WorkspaceTrust(join(dir, ".hush"));
  const realCheckout = new ThreadWorktrees(dir, trust);
  let h;
  const policy = new ToolPolicy(() => h, trust, cwd => cwd ?? repo, []);
  const ext = threadStart({ harness: () => h, policy, trust, defaults: async cwd => ({ cwd, model }), requireModel: () => undefined,
    worktrees: () => ({ async create(cwd, signal, plan) {
      const result = await realCheckout.create(cwd, signal, { ...plan, async reserve(allocation) {
        await plan.reserve(allocation);
        if (cut === "reserved") process.exit(crashCode);
      } });
      if (cut === "created") process.exit(crashCode);
      return result;
    } }) });
  const faux = fauxProvider(), models = createModels(), registry = createRegistry();
  models.setProvider(faux.provider);
  let calls = 0;
  faux.setResponses(Array(12).fill(({ messages }) => {
    if (messages.findLast(m => m.role !== "system").role === "toolResult") return fauxAssistantMessage("Recovered.");
    ++calls;
    return fauxAssistantMessage(fauxToolCall("thread_start", { prompt: "Continue there", worktree: "new" }), { stopReason: "toolUse" });
  }));
  registry.install(ext);
  h = await Harness.open(await openNodeSqliteStorage(join(dir, "history.sqlite")), { models, registry,
    env: ({ cwd }) => new LocalEnvironment(cwd ?? repo), settings: { extensions: [ext], compaction: { enabled: false }, retry: { enabled: false } } }, ctx);
  const parent = await h.root(ctx, { agent: { cwd: repo, model } });
  return { h, parent, repo, trust, calls: () => calls };
}

if (process.env.HUSH_TEST_CRASH_AT) {
  const ctx = withAbortSignal(AbortSignal.timeout(8_000), BACKGROUND_CONTEXT);
  const f = await open(process.argv[2], ctx, process.env.HUSH_TEST_CRASH_AT);
  await f.trust.change(f.repo, "grant", ctx.abortSignal);
  await (await f.parent.submit({ type: "input", content: "go", requestId: "go" }, ctx)).wait(ctx);
  assert.fail("tool did not hit the crash fence");
} else for (const cut of ["reserved", "created"]) test(`worktree thread interrupted at ${cut} restarts without replay`, { timeout: 12_000 }, async t => {
  const ctx = withAbortSignal(t.signal, BACKGROUND_CONTEXT);
  const dir = await realpath(await mkdtemp("/tmp/hush-restart-fence-"));
  let reopened;
  t.after(async () => { await reopened?.h.close(BACKGROUND_CONTEXT); await rm(dir, { force: true, recursive: true }); });
  const repo = join(dir, "repo");
  await mkdir(repo);
  const git = (...args) => execFileSync("git", args, { cwd: repo, env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" }, stdio: "pipe" }).toString();
  git("init", "-q");
  git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-qm", "base");
  const child = spawn(process.execPath, [import.meta.filename, dir], { env: { ...process.env, HUSH_TEST_CRASH_AT: cut }, signal: t.signal, stdio: "inherit" });
  assert.deepEqual(await once(child, "exit"), [crashCode, null]);
  const beforehand = git("worktree", "list", "--porcelain");
  assert.equal(beforehand.match(/^worktree /gm).length, cut === "reserved" ? 1 : 2);
  const f = reopened = await open(dir, ctx);
  const parentRow = await f.h.commit(tx => tx.submissionByRequest(f.parent.id, "go"), ctx);
  f.h.resume();
  const original = await f.h.submission(parentRow.id, ctx);
  await original.wait(ctx);
  assert.equal(f.calls(), 0, "model did not get a fresh tool call");
  assert.equal(git("worktree", "list", "--porcelain"), beforehand, "Git was not replayed");
  const { items } = await f.parent.entries({ order: "descending" }, 16, undefined, ctx);
  const [notice, ...extra] = items.filter(e => e.kind === "hush.thread-start-notice");
  assert.equal(extra.length, 0);
  const allocated = Number(/^Thread (\d+) allocated/.exec(notice?.data.text)?.[1]);
  assert.ok(allocated);
  const [record, submission] = await f.h.commit(async tx => [await tx.conversation(allocated), await tx.submissionByRequest(allocated, "hush.thread-start")], ctx);
  assert.ok(record && !record.owner);
  assert.equal(submission, undefined);
});
