// Offline benchmark: after npm run backend:bundle, run node backend/test/bench-rewind.mjs.
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, realpath, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { monitorEventLoopDelay } from "node:perf_hooks";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { fauxAssistantMessage, fauxProvider } from "@earendil-works/pi-ai/providers/faux";
import { startBackend } from "../../dist/hush-backend.mjs";
import { dial } from "../../server/test/client.mjs";
import { checkTarget } from "../../server/test/perf.mjs";

const run = promisify(execFile);
const caps = { versions: [1], capabilities: { checkpoints: [1] } };
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const streamPatch = frame => frame.type === "patch" && frame.ops.some(op => op[0] !== "d" && op[1]?.[0] === "streaming");

test("preview and confirm a 100k-file rewind while another peer streams", { timeout: 90_000 }, async t => {
  const home = await realpath(await mkdtemp("/tmp/hush-rw-bench-"));
  t.after(() => rm(home, { recursive: true, force: true }));
  const cwd = join(home, "repo"), agentDir = join(home, ".hush"), dataDir = join(agentDir, "runtime");
  const socket = join(dataDir, "hush.sock");
  const env = { HOME: home, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" };
  const git = (...args) => run("git", args, { cwd, env });
  await mkdir(cwd);
  await git("init", "-q", "--template=");
  for (let d = 0; d < 100; d++) {
    const dir = join(cwd, `dir-${d}`);
    await mkdir(dir);
    for (let n = 0; n < 1000; n += 20) {
      await Promise.all(Array.from({ length: 20 }, (_, i) => writeFile(join(dir, `file-${n + i}`), "")));
    }
  }
  await git("add", ".");
  await git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "100k files");
  await run(process.execPath, [fileURLToPath(new URL("../../dist/hush-backend.mjs", import.meta.url)), "trust", "grant", "--cwd", cwd], { cwd, env });
  const app = await startBackend({ cwd, agentDir, dataDir, socket, profile: "hush", idleMs: 30_000, ephemeral: false, faux: true });
  let peer, streamer;
  const histogram = monitorEventLoopDelay({ resolution: 1 });
  try {
    peer = await dial(socket);
    assert.equal(ok(await peer.send("hello", caps)).capabilities.checkpoints, 1);
    const { id: threadId } = ok(await peer.send("thread.new", { cwd }));
    const cp = async (operation, params) => ok(await peer.send("ext.checkpoints." + operation, { threadId, ...params }));
    const { submissionId } = ok(await peer.send("submit", { threadId, text: "checkpoint here", requestId: "seed", whenBusy: "reject" }));
    assert.equal(ok(await peer.send("submission.wait", { threadId, submissionId })).status, "done");
    const checkpoint = await cp("record", { kind: "files" });

    const faux = fauxProvider({ provider: "rewind-fixture", tokensPerSecond: 250, tokenSize: { min: 12, max: 12 } });
    faux.setResponses([fauxAssistantMessage("Streaming fixture output. ".repeat(2000))]);
    app.runtime.registerNativeProvider(faux.provider);
    let patches = 0;
    streamer = await dial(socket, frame => { if (streamPatch(frame)) patches++; });
    ok(await streamer.send("hello", caps));
    const model = { provider: "rewind-fixture", modelId: faux.getModel().id };
    const { id: streamId } = ok(await streamer.send("thread.new", { cwd, model }));
    ok(await streamer.send("subscribe", { threadId: streamId }));
    ok(await streamer.send("submit", { threadId: streamId, text: "stream during rewind", requestId: "stream", whenBusy: "reject" }));
    await streamer.wait(streamPatch);

    const measure = async (phase, action) => {
      histogram.enable();
      await delay(5);
      histogram.reset();
      const previous = patches, start = performance.now();
      const result = await action();
      const ms = performance.now() - start, streamed = patches - previous;
      await delay(2); // record the final timer gap before reading the histogram
      histogram.disable();
      const p99 = histogram.percentile(99) / 1e6;
      t.diagnostic(`${phase}: ${ms.toFixed(1)} ms, ${histogram.count} samples, p99 ${p99.toFixed(2)} ms, ` +
        `max ${(histogram.max / 1e6).toFixed(2)} ms, ${streamed} streaming patches`);
      assert.ok(histogram.count > 0 && streamed > 0, `${phase} did not overlap streaming`);
      checkTarget(t, { name: `${phase} event-loop delay p99`, value: p99, limit: 50 });
      return result;
    };
    // Preview can finish within one Durable streaming publication interval.
    const preview = await measure("preview (8 calls)", async () => {
      let result;
      for (let i = 0; i < 8; i++) {
        result = await cp("preview", { checkpointId: checkpoint.id, mode: "files" });
        assert.equal(result.mode, "files", JSON.stringify(result));
      }
      return result;
    });
    await measure("confirm through ready", async () => {
      const input = { id: preview.id, mode: "files" };
      assert.equal((await cp("confirm", input)).state, "staging");
      for (const deadline = Date.now() + 45_000; Date.now() < deadline; await delay(25)) {
        const receipt = await cp("status", { id: preview.id });
        if (receipt.state === "staging") continue;
        assert.equal(receipt.state, "ready", JSON.stringify(receipt));
        assert.ok(receipt.forkThreadId);
        return;
      }
      assert.fail("rewind never reached ready");
    });
  } finally {
    histogram.disable();
    streamer?.close(); peer?.close();
    await app.close();
  }
});
