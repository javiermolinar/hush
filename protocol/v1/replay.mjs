// Fixture oracle, not the SDK. It consumes the native core wire and has no socket or Harness dependency.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import Ajv2020 from "ajv/dist/2020.js";
import { applyImmutable } from "@earendil-works/chord/delta";

const schema = JSON.parse(readFileSync(new URL("./core.schema.json", import.meta.url), "utf8"));
const ajv = new Ajv2020({ allErrors: true, strict: true });
const validateFrame = ajv.compile(schema);
const validateState = ajv.compile({ $ref: `${schema.$id}#/$defs/state` });
const unkeyed = new Set(["continue", "thread.new", "thread.fork", "thread.configure", "thread.abort", "thread.compact", "thread.reset"]);

function scalarJSON(value) {
  if (typeof value === "string") return value.isWellFormed();
  if (typeof value === "number") return Number.isFinite(value);
  if (Array.isArray(value)) return value.every(scalarJSON);
  return value === null || typeof value !== "object" || Object.entries(value)
    .every(([k, v]) => k.isWellFormed() && scalarJSON(v));
}

export function assertFrame(frame) {
  assert.ok(scalarJSON(frame), "nonfinite number or unpaired surrogate");
  assert.ok(validateFrame(frame), ajv.errorsText(validateFrame.errors));
}

export function readFrames(url) {
  const contents = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(readFileSync(url));
  const lines = contents.split("\n");
  assert.equal(lines.pop(), "", "missing terminal LF");
  return lines.map((raw) => {
    const record = raw.endsWith("\r") ? raw.slice(0, -1) : raw;
    assert.ok(Buffer.byteLength(record) <= 8 << 20, "oversize record");
    return JSON.parse(record);
  });
}

const compare = (a, b) => Math.sign(Number(a) - Number(b));
const durableId = (id) => assert.ok(Number.isSafeInteger(Number(id)) && Number(id) > 0, `invalid Durable ID ${id}`);
function assertOrder(entries) {
  for (let i = 0; i < entries.length; i++) {
    durableId(entries[i].id);
    if (i) assert.ok(compare(entries[i - 1].id, entries[i].id) < 0, "entries out of order or duplicated");
  }
}

function assertState(state, threadId) {
  assert.ok(scalarJSON(state) && validateState(state), "invalid projected state");
  assert.equal(state.thread.id, threadId);
  const { entries, beforeEntryId, lastEntryId } = state.window;
  assertOrder(entries);
  assert.equal(lastEntryId, entries.at(-1)?.id ?? null);
  if (beforeEntryId !== null) {
    durableId(beforeEntryId);
    assert.ok(entries.length && compare(beforeEntryId, entries[0].id) < 0, "window predecessor is not older");
  }
}

export class Replica {
  epoch;
  active;
  pending = new Map();
  seen = new Set();
  history = new Map();
  pages = new Map();
  lost = [];
  results = [];

  disconnect() {
    for (const row of this.pending.values()) {
      if (row.method === "submit") this.lost.push({ request: row, recovery: "keyed" });
      if (unkeyed.has(row.method)) this.lost.push({ request: row, recovery: "indeterminate" });
    }
    this.pending.clear();
    this.seen.clear();
    this.pages.clear();
    this.epoch = undefined;
    this.active = undefined;
  }

  accept(frame) {
    try { assertFrame(frame); }
    catch (error) {
      if (frame?.type === "patch" && this.active?.subscriptionId === frame.subscriptionId) this.active.stale = true;
      throw error;
    }
    switch (frame.type) {
      case "request":
        assert.ok(!this.seen.has(frame.id), `request id already used: ${frame.id}`);
        this.seen.add(frame.id);
        this.pending.set(frame.id, frame);
        return "request";
      case "response": return this.response(frame);
      case "patch": return this.patch(frame);
      case "state_end":
        if (this.active?.subscriptionId !== frame.subscriptionId) return "ignored";
        this.active.stale = true;
        return "stale";
    }
  }

  response(frame) {
    const req = this.pending.get(frame.id);
    assert.ok(req, `reply without matching request: ${frame.id}`);
    assert.equal(frame.method, req.method);
    this.pending.delete(frame.id);
    this.results.push(frame);
    if (!frame.ok) return "error";
    const value = frame.result;
    if (frame.method === "hello") {
      assert.equal(this.epoch, undefined);
      assert.ok(req.params.versions.includes(value.version));
      assert.deepEqual(value.capabilities, {}, "native baseline does not negotiate extensions");
      this.epoch = value.epoch;
    } else {
      assert.ok(this.epoch, "successful reply before hello");
      if (frame.method === "subscribe") {
        assert.equal(value.epoch, this.epoch);
        assert.equal(value.rev, 0);
        assert.notEqual(value.subscriptionId, this.active?.subscriptionId, "subscription ID reused");
        assertState(value.state, req.params.threadId);
        this.merge(req.params.threadId, value.state.window.entries);
        this.active = { ...value, stale: false };
      } else if (frame.method === "history") {
        this.page(req, value);
      } else if (frame.method === "submission.get" || frame.method === "submission.wait") {
        assert.equal(value.threadId, req.params.threadId);
        if (req.params.submissionId) assert.equal(value.submissionId, req.params.submissionId);
        if (frame.method === "submission.wait") assert.ok(["done", "unanswered"].includes(value.status));
      }
    }
    return "response";
  }

  patch(frame) {
    const active = this.active;
    if (active?.subscriptionId !== frame.subscriptionId) return "ignored";
    if (active.stale || active.epoch !== frame.epoch || active.rev !== frame.baseRev || frame.nextRev <= frame.baseRev) {
      active.stale = true;
      return "stale";
    }
    try {
      assert.ok(frame.ops.every((op, i) => op[0] !== "r" || i === 0), "root replacement not first");
      const next = applyImmutable(active.state, frame.ops);
      const threadId = active.state.thread.id;
      assertState(next, threadId);
      this.merge(threadId, next.window.entries);
      active.state = next;
      active.rev = frame.nextRev;
      return "applied";
    } catch {
      active.stale = true;
      return "stale";
    }
  }

  page(req, value) {
    const { threadId, limit } = req.params;
    assert.equal(value.threadId, threadId);
    assert.ok(value.entries.length <= limit);
    assertOrder(value.entries);
    let range = req.params.range;
    if (range.kind === "cursor") {
      const prior = this.pages.get(range.cursor);
      assert.equal(prior?.threadId, threadId, "page cursor doesn't belong to this range");
      range = prior.range;
    }
    if (range.kind === "after") {
      if (range.throughEntryId === null) assert.deepEqual(value.entries, []);
      for (const e of value.entries) {
        assert.ok(range.afterEntryId === null || compare(e.id, range.afterEntryId) > 0);
        assert.ok(compare(e.id, range.throughEntryId) <= 0);
      }
    } else {
      for (const e of value.entries) assert.ok(range.beforeEntryId === null || compare(e.id, range.beforeEntryId) < 0);
    }
    if (value.nextCursor !== null) {
      assert.ok(value.entries.length, "empty page has a cursor");
      const next = range.kind === "after" ? { ...range, afterEntryId: value.entries.at(-1).id }
        : { ...range, beforeEntryId: value.entries[0].id };
      this.pages.set(value.nextCursor, { threadId, range: next });
    }
    this.merge(threadId, value.entries);
  }

  merge(threadId, entries) {
    const old = this.history.get(threadId) ?? new Map();
    for (const entry of entries) {
      if (old.has(entry.id)) assert.deepEqual(entry, old.get(entry.id), `immutable entry changed: ${entry.id}`);
    }
    for (const entry of entries) old.set(entry.id, entry);
    this.history.set(threadId, old);
  }

  ordered(threadId) {
    return [...(this.history.get(threadId)?.values() ?? [])].sort((a, b) => compare(a.id, b.id));
  }
}
