import assert from "node:assert/strict";
import { mkdir, mkdtemp, realpath, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { startBackend } from "../../dist/hush-backend.mjs";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";
import { dial } from "../../server/test/client.mjs";

const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const range = { kind: "before", beforeEntryId: null };
const denied = { code: "invalid_request",
  message: "Workspace is not trusted for tool execution. Run 'hush trust grant --cwd PATH' on the backend host." };

async function fixture(t) {
  const home = await realpath(await mkdtemp("/tmp/hush-work-"));
  const cwd = join(home, "workspace"), agentDir = join(home, ".hush"), dataDir = join(agentDir, "runtime");
  const socket = join(dataDir, "hush.sock");
  let app, peer;
  t.after(async () => { peer?.close(); await app?.close(); await rm(home, { recursive: true, force: true }); });
  await mkdir(cwd);
  app = await startBackend({ profile: "hush", cwd, agentDir, dataDir, socket, idleMs: 30_000, ephemeral: false, faux: true });
  peer = await dial(socket);
  const hello = ok(await peer.send("hello", { versions: [1], capabilities: { threadDirectory: [2], images: [1], workspaceWorkTrust: [1] } }));
  assert.equal(hello.capabilities.workspaceWorkTrust, 1);
  const trust = new WorkspaceTrust(agentDir);
  const call = async (method, params) => ok(await peer.send(method, params));
  const newThread = async path => (await call("thread.new", { cwd: path })).id;
  const history = async threadId => (await call("history", { threadId, range, limit: 30 })).entries;
  const work = (action, path = cwd) => trust.change(path, action, AbortSignal.timeout(5000));
  const wait = (threadId, submissionId) => call("submission.wait", { threadId, submissionId });
  return { cwd, peer, call, work, newThread, history, wait };
}

test("work admission checks the stored exact cwd and never grants permission on the socket", { timeout: 12_000 }, async t => {
  const { cwd, peer, call, work, newThread, history, wait } = await fixture(t);
  const sub = join(cwd, "child");
  await mkdir(sub);
  await work("grant");
  const threadId = await newThread(sub);
  const input = { threadId, text: "first prompt", requestId: "first", whenBusy: "reject" };
  assert.equal((await peer.send("ext.workspaceTrust.grant", { cwd: sub })).error.code, "unsupported_capability");
  assert.deepEqual((await peer.send("submit", input)).error, denied);
  assert.deepEqual((await peer.send("continue", { threadId })).error, denied);
  assert.deepEqual((await peer.send("ext.images.submit", { ...input, images: [{ mimeType: "image/png", data: "YQ==" }] })).error, denied);
  assert.equal((await peer.send("submission.get", { threadId, requestId: "first" })).error.code, "not_found");
  assert.deepEqual(await history(threadId), []);
  await work("grant", sub);
  const receipt = await call("submit", input);
  assert.equal((await wait(threadId, receipt.submissionId)).status, "done");
  await work("revoke", sub);
  assert.deepEqual(await call("submit", input), receipt, "a receipt lookup must not restart accepted work");
  assert.deepEqual((await peer.send("submit", { ...input, requestId: "another" })).error, denied);
  assert.ok((await history(threadId)).some(e => e.kind === "pi.user"));
});

test("revocation stops both a parked tool and a file read admitted in the queue", { timeout: 12_000 }, async t => {
  const { cwd, call, work, newThread, history, wait } = await fixture(t);
  const marker = "revoked workspace must not read this";
  await writeFile(join(cwd, "workspace-marker.txt"), marker);
  await work("grant");
  const threadId = await newThread(cwd);
  const input = (text, requestId, whenBusy) => call("submit", { threadId, text, requestId, whenBusy });
  const parked = await input("!sleep 1500", "parked", "reject");
  let sleeping = false;
  for (const deadline = Date.now() + 1000; !sleeping && Date.now() < deadline; await delay(15)) {
    const page = await call("ext.threadDirectory.list", { limit: 10 });
    sleeping = page.threads.some(row => row.id === threadId && row.status === "parked");
  }
  assert.ok(sleeping, "fixture should enter a committed tool sleep");
  const queued = await input("!workspace-read", "queued", "followUp");
  await work("revoke");
  await wait(threadId, parked.submissionId);
  await wait(threadId, queued.submissionId);
  const entries = await history(threadId);
  const results = entries.filter(e => e.kind === "pi.tool-result").flatMap(e => e.messages);
  assert.deepEqual(results.map(row => row.name), ["hush_fixture_wait", "read"]);
  for (const result of results) {
    assert.equal(result.isError, true, JSON.stringify(result));
    assert.match(JSON.stringify(result), /not trusted for tool execution/);
  }
  assert.doesNotMatch(JSON.stringify(entries), new RegExp(marker));
});
