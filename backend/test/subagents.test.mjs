import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdir, mkdtemp, realpath, rm } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from "@earendil-works/pi-ai";
import { createRegistry, defineTask, Harness, MemoryStorage, ToolResultEntry } from "@earendil-works/pi-durable";
import { LocalEnvironment } from "../../dist/backend/src/resources.js";
import { Subagents } from "../../dist/backend/src/subagents.js";
import { ToolPolicy } from "../../dist/backend/src/tool-policy.js";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";

const branch = "fixture-worker";
const unavailable = "The workspace or worktree is no longer trusted or available.";

for (const [name, change, failure] of [
  ["unchanged pins", () => {}, undefined],
  ["renewed source grant", f => f.regrant(f.repo), unavailable],
  ["renewed selected grant", f => f.regrant(f.target), unavailable],
  ["replaced selected directory", async ({ git, repo, target, other, grant }) => {
    git(repo, "worktree", "move", target, other);
    git(other, "switch", "-qc", "other");
    git(repo, "worktree", "add", "-q", target, branch);
    await grant(target);
  }, unavailable],
  ["branch moved to another trusted checkout", async ({ git, repo, target, other, grant }) => {
    git(target, "switch", "-qc", "other");
    git(repo, "worktree", "add", "-q", other, branch);
    await grant(other);
  }, "The selected worktree changed."],
]) test(`subagent checks workspace at prompt admission: ${name}`, { timeout: 8_000 }, async t => {
  const ctx = withAbortSignal(t.signal, BACKGROUND_CONTEXT);
  const dir = await realpath(await mkdtemp("/tmp/hush-subagent-admission-"));
  const repo = join(dir, "repo"), target = join(dir, "target"), other = join(dir, "other"), agentDir = join(dir, ".hush");
  let h;
  t.after(async () => { await h?.close(BACKGROUND_CONTEXT); await rm(dir, { force: true, recursive: true }); });
  await mkdir(repo);
  const git = (cwd, ...args) => execFileSync("git", ["-c", "core.hooksPath=/dev/null", ...args], { cwd, stdio: "pipe",
    env: { HOME: dir, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" } });
  git(repo, "init", "-q");
  git(repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-qm", "fixture");
  git(repo, "worktree", "add", "-qb", branch, target);
  const trust = new WorkspaceTrust(agentDir), grant = path => trust.change(path, "grant", t.signal);
  const regrant = async path => { await trust.change(path, "revoke", t.signal); await grant(path); };
  await grant(repo);
  await grant(target);

  const policy = new ToolPolicy(() => h, trust, cwd => cwd ?? repo, [agentDir], false);
  const promptExtension = { name: "fixture-subagent-prompt" }, coding = { name: "fixture-subagent-tools" };
  const service = new Subagents(() => h, trust, policy, promptExtension, coding);
  const { definition } = service.extension.tasks[0];
  let child;
  const worker = defineTask({ ...definition, phases: { ...definition.phases, submit(task, runtime, ctx) {
    child = task.state.checkpoint.child;
    return definition.phases.submit(task, { ...runtime, async commit(admit, context, options) {
      // The child already exists. Change the fixture at the commit boundary
      // so a check made only before queuing the admission cannot pass.
      await change({ git, repo, target, other, grant, regrant });
      return runtime.commit(admit, context, options);
    } }, ctx);
  } } });
  const extension = { ...service.extension, tasks: [worker] };
  const registry = createRegistry();
  for (const ext of [extension, promptExtension, coding]) registry.install(ext);

  const faux = fauxProvider(), models = createModels();
  models.setProvider(faux.provider);
  const prompt = "Delegate a workspace check.", brief = "Check the selected checkout.";
  const childRequests = [];
  faux.setResponses(Array(3).fill(({ messages }) => {
    const last = messages.findLast(m => m.role !== "system");
    if (last.role === "toolResult") return fauxAssistantMessage("Parent finished.");
    const text = typeof last.content === "string" ? last.content : last.content[0].text;
    if (text === prompt) return fauxAssistantMessage(fauxToolCall("subagent", { prompt: brief, read_only: true, worktree: branch }),
      { stopReason: "toolUse" });
    childRequests.push(text);
    return fauxAssistantMessage("Worker finished.");
  }));
  const storage = new MemoryStorage();
  h = await Harness.open(storage, { models, registry, env: ({ cwd }) => new LocalEnvironment(cwd ?? repo),
    settings: { extensions: [extension], compaction: { enabled: false }, retry: { enabled: false } } }, ctx);
  const parent = await h.root(ctx, { agent: { cwd: repo, model: { provider: "faux", modelId: "faux-1" } } });
  const input = await parent.submit({ type: "input", content: prompt }, ctx);
  assert.equal((await input.wait(ctx)).status, "done");
  const { items } = await parent.entries({}, 10, undefined, ctx);
  const result = items.find(e => ToolResultEntry.is(e))?.model[0];
  assert.ok(child, `the worker must reach the submission phase: ${JSON.stringify(result?.content)}`);
  assert.ok(result);
  assert.equal(result.content[0].text, failure ?? "Worker finished.");
  assert.equal(!!result.isError, !!failure);
  const { items: submissions } = await storage.scanSubmissions({ conversationId: child }, 2, undefined, ctx);
  assert.equal(submissions.length, failure ? 0 : 1);
  assert.deepEqual(childRequests, failure ? [] : [brief]);
});
