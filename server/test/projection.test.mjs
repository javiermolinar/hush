import assert from "node:assert/strict";
import { test } from "node:test";
import { setImmediate } from "node:timers/promises";
import { diffRevisions } from "@earendil-works/chord/delta";
import { projectView } from "../../dist/server/src/durable.js";
import { cleanJSON, encode, patchShape, stateShape } from "../../dist/server/src/wire.js";
import { projectState } from "../../protocol/v1/native-probe.mjs";
import { readFrames, Replica } from "../../protocol/v1/replay.mjs";

const view = (entries, docs = {}, before) => ({ conversation: { id: 1 }, entries, docs, before });
const entry = (id, content = "fixture") => Object.freeze({
  id, conversationId: 1, kind: "pi.user", model: [{ role: "user", timestamp: 100, content }],
});

// The fixture is a wire transcript, not a Durable observation. Reconstruct
// its valid views, sharing the immutable records through appends and trims.
function fixtureView(state, records) {
  const { agent, tools, run, streaming, window } = state;
  const entries = window.entries.map(e => {
    if (!records.has(e.id)) records.set(e.id, Object.freeze({
      id: Number(e.id), conversationId: Number(e.threadId), kind: e.kind,
      model: e.messages.map(({ timestampMs, blocks, ...rest }) => ({ ...rest, timestamp: timestampMs, content: blocks })),
    }));
    return records.get(e.id);
  });
  return view(entries, {
    "pi.agent": agent,
    "pi.live": {
      tools: tools.map(({ taskId, resultEntryId, ...tool }) => ({ ...tool,
        taskId: taskId && Number(taskId), entry: resultEntryId && Number(resultEntryId),
      })),
      run: run && { taskId: Number(run.taskId), inputs: run.submissionIds.map(Number) },
      generation: streaming && { attempt: streaming.attempt, message: { content: streaming.blocks } },
    },
  }, window.beforeEntryId === null ? undefined : Number(window.beforeEntryId));
}

function patchBytes(frame, before, next) {
  const ops = diffRevisions(before, next);
  const patch = { ...frame, ops };
  return encode(cleanJSON(ops) && patchShape(patch) ? patch : { ...patch, ops: [["r", next]] });
}

test("projection preserves patch bytes across the replication fixture", () => {
  const frames = readFrames(new URL("../../protocol/v1/fixtures/replication.jsonl", import.meta.url));
  const rep = new Replica(), records = new Map();
  let before, refBefore, compared = 0;
  for (const frame of frames) {
    const outcome = rep.accept(frame);
    if (outcome !== "applied" && !(frame.type === "response" && frame.method === "subscribe" && frame.ok)) continue;
    const input = fixtureView(rep.active.state, records);
    const ref = projectState(input), projected = projectView(input);
    assert.deepEqual(ref, rep.active.state);
    assert.ok(cleanJSON(ref) && stateShape(ref));
    assert.equal(encode(projected), encode(ref));
    if (outcome === "applied") {
      assert.equal(patchBytes(frame, before, projected), patchBytes(frame, refBefore, ref));
      compared++;
    }
    before = projected;
    refBefore = ref;
  }
  assert.ok(compared >= 4, "expected streaming, append and trim transitions");
});

test("window projections memoize only identical validated records", () => {
  const record = entry(2);
  const first = projectView(view([record]));
  const warm = projectView(view([record, entry(3)]));
  assert.equal(warm.window.entries[0], first.window.entries[0]);
  const other = projectView(view([entry(2, "different record")])).window.entries[0];
  assert.notEqual(other, first.window.entries[0]);
  assert.equal(other.messages[0].blocks[0].text, "different record");

  const malformed = entry(3, "\ud800");
  const call = arguments_ => entry(3, [{ type: "toolCall", name: "fixture", id: "call", arguments: arguments_ }]);
  let deep = {};
  for (let i = 0; i < 249; i++) deep = { nested: deep };
  for (const input of [view([record, entry(-1)]), view([record, malformed]), view([record, call({ n: Infinity })]),
    view([record, call(deep)]), view(new Array(1)), view(Array.from({ length: 101 }, (_, i) => entry(i + 1))),
    view([record], {}, -1), view([record], { "pi.live": { generation: { attempt: 0 } } }),
    view([record], { "pi.agent": { cwd: "\ud800" } })]) {
    assert.throws(() => projectView(input), { code: "internal" });
  }
});

test("the projection memo does not retain evicted entries", { skip: !global.gc }, async () => {
  function weak() {
    const raw = entry(1);
    const projected = projectView(view([raw])).window.entries[0];
    return [new WeakRef(raw), new WeakRef(projected)];
  }
  const refs = weak();
  for (let i = 0; i < 20; i++) {
    await setImmediate();
    global.gc();
    if (refs.every(ref => ref.deref() === undefined)) return;
  }
  assert.fail("evicted record or memoized projection is still retained");
});
