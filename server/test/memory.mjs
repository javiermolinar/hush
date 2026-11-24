// 140 workload conversations plus one root, same phases and line counts as the prototype.
import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { createModels, fauxProvider, fauxAssistantMessage, fauxText, fauxToolCall } from "@earendil-works/pi-ai";
import { createRegistry, Harness } from "@earendil-works/pi-durable";
import { CodingTools } from "@earendil-works/pi-durable/tools";
import { NodeExecutionEnv } from "@earendil-works/pi-durable/env/node";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { createCoreServer } from "../../dist/server/src/index.js";
import { dial } from "./client.mjs";

const exec = promisify(execFile);
const model = { provider: "faux", modelId: "faux-1" };

async function serve(dir) {
  const faux = fauxProvider({ tokensPerSecond: 30, tokenSize: { min: 3, max: 3 } });
  const models = createModels();
  const registry = createRegistry();
  registry.install(CodingTools);
  models.setProvider(faux.provider);
  const script = ({ messages }) => {
    const last = messages.at(-1);
    if (last?.role === "toolResult") {
      const text = last.content.filter(b => b.type === "text").map(b => b.text).join("\n");
      return fauxAssistantMessage("Finished. Last output:\n" + text.trim().slice(-400));
    }
    // The driver only selects one of two local, bounded workloads.
    const count = last?.content === "large" ? 20_000 : 3_000;
    return fauxAssistantMessage([fauxText("Running it now."),
      fauxToolCall("bash", { command: "seq 1 " + count }, { id: "bench-" + faux.state.callCount })],
    { stopReason: "toolUse" });
  };
  faux.setResponses(Array.from({ length: 500 }, () => script));
  const h = await Harness.open(await openNodeSqliteStorage(join(dir, "bench.sqlite")), {
    models, registry, env: () => new NodeExecutionEnv({ cwd: dir }),
  }, context);
  await h.root(context, { agent: { model, cwd: dir } });
  const server = createCoreServer(h);
  await server.listen(join(dir, "sock"));
  process.on("SIGTERM", async () => {
    await server.close(); await h.close(context);
    process.exit(0);
  });
  process.stdout.write("ready\n");
}

const ok = reply => { assert.equal(reply.ok, true, JSON.stringify(reply)); return reply.result; };
async function workload(dir) {
  const child = spawn(process.execPath, [fileURLToPath(import.meta.url), "--serve", dir], { stdio: ["ignore", "pipe", "inherit"] });
  const exit = once(child, "exit");
  const lines = createInterface({ input: child.stdout })[Symbol.asyncIterator]();
  const sock = join(dir, "sock");
  let sampling = Promise.resolve();
  let reading = false;
  let peakFp = 0, peakRSS = 0;
  async function sample() {
    const { stdout: rss } = await exec("ps", ["-o", "rss=", "-p", String(child.pid)]);
    peakRSS = Math.max(peakRSS, Number(rss) / 1024);
    if (process.platform === "darwin") {
      const { stdout } = await exec("footprint", ["--noCategories", "-f", "bytes", String(child.pid)]);
      const fp = /phys_footprint:\s+([\d,.]+)\s*(B|KB|MB|GB)?/.exec(stdout);
      assert.ok(fp, "footprint output missing");
      const exponent = ({ B: 0, KB: 1, MB: 2, GB: 3 })[fp[2] ?? "B"];
      peakFp = Math.max(peakFp, Number(fp[1].replaceAll(",", "")) * (1024 ** (exponent - 2)));
    }
  }
  let timer;
  async function phase(label, count, action) {
    peakFp = peakRSS = 0;
    await action();
    await sampling;
    await sample();
    console.log("%s conversations=%d peak_footprint_MiB=%s peak_rss_MiB=%s", label, count,
      process.platform === "darwin" ? peakFp.toFixed(1) : "n/a", peakRSS.toFixed(1));
  }
  async function conversation(turns, size) {
    const peer = await dial(sock, (frame) => assert.notEqual(frame.type, "state_end"), { retain: false });
    try {
      ok(await peer.send("hello", { versions: [1] }));
      const { id: threadId } = ok(await peer.send("thread.new", { model, cwd: dir }));
      ok(await peer.send("subscribe", { threadId }));
      for (let i = 0; i < turns; i++) {
        const { submissionId } = ok(await peer.send("submit", { threadId, requestId: "bench-" + i, text: size, whenBusy: "reject" }));
        const settled = ok(await peer.send("submission.wait", { threadId, submissionId }));
        assert.equal(settled.status, "done");
        ok(await peer.send("thread.wait", { threadId }));
      }
    } finally { peer.close(); }
  }
  const batch = (clients, turns, size) => Promise.all(Array.from({ length: clients }, () => conversation(turns, size)));
  try {
    assert.equal((await lines.next()).value, "ready");
    timer = setInterval(() => {
      if (reading) return;
      reading = true;
      sampling = sample().finally(() => { reading = false; });
      sampling.catch(() => {});
    }, 300);
    await phase("idle", 1, () => {});
    await phase("small", 21, () => batch(20, 1, "small"));
    await phase("fanout", 121, async () => { for (let i = 0; i < 4; i++) await batch(25, 1, "small"); });
    await phase("large", 141, () => batch(20, 5, "large"));
    const peer = await dial(sock);
    try {
      ok(await peer.send("hello", { versions: [1] }));
      const first = ok(await peer.send("thread.list", { limit: 100 }));
      const second = ok(await peer.send("thread.list", { limit: 100, cursor: first.nextCursor }));
      assert.equal(first.threads.length + second.threads.length, 141);
      assert.equal(second.nextCursor, null);
    } finally { peer.close(); }
  } finally {
    clearInterval(timer);
    if (child.exitCode === null) child.kill("SIGTERM");
    const [status] = await exit;
    assert.equal(status, 0);
  }
}

if (process.argv[2] === "--serve") await serve(process.argv[3]);
else {
  const dir = await mkdtemp(join(tmpdir(), "hsm-"));
  try { await workload(dir); }
  finally { await rm(dir, { recursive: true, force: true }); }
}
