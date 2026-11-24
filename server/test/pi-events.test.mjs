import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT as ctx } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai";
import { createRegistry, Harness, MemoryStorage } from "@earendil-works/pi-durable";
import { createCoreServer } from "../../dist/server/src/index.js";
import { dial } from "./client.mjs";

const ok = f => { assert.equal(f.ok, true, JSON.stringify(f)); return f.result; };

test("piEvents feed: bounded overflow is terminal and never waits for a slow reader", { timeout: 10000 }, async t => {
  const dir = await mkdtemp("/tmp/hush-wire-pi-");
  const h = await Harness.open(new MemoryStorage(), { models: createModels(), registry: createRegistry() }, ctx);
  const root = await h.root(ctx);
  const released = Promise.withResolvers();
  let calls = 0, opened = 0;
  const piEvents = {
    async open({ cwd }, sink) {
      opened++;
      return {
        threadId: "99", services: [], header: {
          type: "session", version: 3, id: "6b663e90-d644-49ac-bb75-b6f6da462166", timestamp: "2026-11-22T12:00:00.000Z", cwd,
        },
        async command(command) {
          calls++;
          if (command.type === "flood") {
            for (let i = 0; i < 8; i++) sink.emit({ type: "queue_update", steering: [String(i)], followUp: [] });
          }
          return { response: { type: "response", command: command.type, id: command.id, success: true } };
        },
        close() { released.resolve(); },
      };
    },
    async catalog() { return { models: [] }; },
  };
  const server = createCoreServer(h, { pendingPiEvents: 3, capabilities: { piEvents } });
  const socket = join(dir, "hush.sock");
  await server.listen(socket);
  t.after(async () => { await server.close(); await h.close(ctx); await rm(dir, { recursive: true, force: true }); });
  const w = await dial(socket), steady = await dial(socket);
  t.after(() => { w.close(); steady.close(); });
  const hello = ok(await w.send("hello", { versions: [1], capabilities: { piEvents: [1] } }));
  assert.deepEqual(hello.capabilities, { piEvents: 1 });
  assert.deepEqual(ok(await steady.send("hello", { versions: [1] })).capabilities, {});
  const before = await w.send("ext.piEvents.command", { epoch: hello.epoch, command: { type: "flood" } });
  assert.equal(before.error?.code, "invalid_request");
  assert.equal(calls, 0);
  const ready = ok(await w.send("ext.piEvents.open", { cwd: dir }));
  assert.equal(opened, 1);
  const again = await w.send("ext.piEvents.open", { cwd: dir });
  assert.equal(again.error?.code, "invalid_request");
  assert.equal(opened, 1);
  const live = w.wait(f => f.type === "pi_end");
  w.socket.pause();
  void w.send("ext.piEvents.command", { epoch: ready.epoch, command: { type: "flood", id: "unanswered" } }).catch(() => {});
  const snapshot = ok(await steady.send("subscribe", { threadId: String(root.id) }));
  assert.equal(snapshot.state.thread.id, String(root.id));
  await released.promise; // Entire failing stream detached before this peer resumes reading.
  w.socket.resume();
  assert.deepEqual(await live, { type: "pi_end", threadId: ready.threadId, epoch: ready.epoch, reason: "overflow" });
  await w.ended;
  assert.equal(calls, 1);
  assert.ok(!w.frames.some(f => f.type === "pi_event" || f.result?.response?.id === "unanswered"));
});
