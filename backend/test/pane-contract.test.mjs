import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { compile } from "@earendil-works/pi-schema";
import { paneToolParameters } from "../../dist/backend/src/pane-contract.js";
import { PaneRoutes, paneLaunchInput } from "../../dist/server/src/pane-control.js";

const argv = { mode: "argv", program: "example", args: ["", "space and $literal"] };
const fixtures = JSON.parse(await readFile(new URL("../../protocol/v1/fixtures/pane-control.json", import.meta.url), "utf8"));

test("pane intent matches the shared Go and TypeScript grammar fixtures", () => {
  for (const [text, explicit] of fixtures.intent) assert.equal(paneLaunchInput(text), explicit, JSON.stringify(text));
});

test("tool schemas and wire dispatch use the shared capability matrix", async () => {
  const directory = { dir: "/fixture", directory: { device: "1", inode: "2", birth: "3" } };
  const launches = [argv, { mode: "shell", text: "printf value" }, { mode: "terminal" }, { mode: "hush" }];
  const handle = "hp_ABCDEFGHIJKLMNOPQRSTUVWXYZ";
  for (const row of fixtures.capabilityCases) {
    const schemas = paneToolParameters(row.caps);
    const checks = Object.fromEntries(Object.entries(schemas).map(([name, schema]) => [name, compile(schema).check]));
    const dispatch = async (request, expected) => {
      const routes = new PaneRoutes(), frames = [];
      const route = routes.bind(row.caps, frame => { frames.push(frame); return true; }, new AbortController().signal);
      const pending = route.dispatch("one", route.admission().until, request, undefined, "1");
      assert.equal(frames.length, Number(expected), JSON.stringify({ caps: row.caps, request }));
      route.close();
      await pending;
    };
    for (const placement of ["right", "below", "left", "above", "tab"]) {
      for (const launch of launches) {
        const expected = row.placements.includes(placement) && row.launches.includes(launch.mode);
        assert.equal(!!checks.pane_open?.({ placement, launch }), expected);
        await dispatch({ op: "open", placement, launch, ...directory }, expected);
      }
    }
    for (const where of ["tab", "split"]) {
      const expected = row.threadViews.includes(where);
      assert.equal(!!checks.thread_open?.({ where, threadId: "7" }), expected);
      await dispatch({ op: "thread_open", where, threadId: "7", ...(where === "split" && directory) }, expected);
    }
    for (const [request, expected] of [[{ op: "list" }, row.native], [{ op: "focus", pane: handle }, row.native],
      [{ op: "read", pane: handle, lines: 1 }, row.read]]) {
      assert.equal(!!checks[`pane_${request.op}`], expected);
      await dispatch(request, expected);
    }
  }
});

test("pane_open advertises and accepts only the adapter's placements", () => {
  for (const [caps, extras] of [
    [{}, []],
    [{ left: true }, ["left"]],
    [{ above: true }, ["above"]],
    [{ tabs: true }, ["tab"]],
    [{ left: true, above: true, tabs: true }, ["left", "above", "tab"]],
  ]) {
    const schema = paneToolParameters(caps).pane_open;
    const placements = ["right", "below", ...extras];
    assert.deepEqual(schema.properties.placement.enum, placements);
    const { check } = compile(schema);
    for (const placement of ["right", "below", "left", "above", "tab", "down", ""]) {
      assert.equal(check({ placement, launch: argv }), placements.includes(placement), placement);
    }
  }
});

test("open offers typed views only when the client and backend support them", () => {
  const terminal = { mode: "terminal" }, hush = { mode: "hush" };
  for (const views of [false, true]) {
    const { check } = compile(paneToolParameters({ views }).pane_open);
    const open = launch => check({ placement: "right", launch });
    assert.equal(open(terminal), views);
    assert.equal(open(hush), views);
    assert.equal(open({ mode: "review", handle: "a".repeat(32) }), false, "the review mode was removed");
    assert.equal(open({ ...terminal, program: "/bin/echo" }), false);
    assert.equal(open({ ...hush, args: ["--socket", "/tmp/not-the-origin"] }), false);
  }
});

test("pane_read is optional and requires an exact handle and bounded line count", () => {
  assert.equal(Object.hasOwn(paneToolParameters({}), "pane_read"), false);
  assert.equal(Object.hasOwn(paneToolParameters({ read: false }), "pane_read"), false);
  const { check } = compile(paneToolParameters({ read: true }).pane_read);
  const pane = "opaque-handle";
  assert.ok(check({ pane, lines: 1 }));
  assert.ok(check({ pane, lines: 200 }));
  for (const lines of [0, 201, 1.5, "5", undefined]) {
    assert.equal(check({ pane, lines }), false, `line count ${lines}`);
  }
  assert.equal(check({ lines: 1 }), false);
  assert.equal(check({ pane: "", lines: 1 }), false);
  assert.equal(check({ pane, lines: 1, workspace: "other" }), false);
});

test("pane tools accept typed operations without origin or cwd overrides", () => {
  const params = paneToolParameters({});
  const { check: open } = compile(params.pane_open);
  const { check: list } = compile(params.pane_list);
  const { check: focus } = compile(params.pane_focus);
  const req = { placement: "right", launch: argv };
  assert.ok(open(req));
  assert.ok(open({ ...req, launch: { mode: "shell", text: "printf x | cat" } }));
  assert.equal(open({ ...req, cwd: "/other" }), false);
  assert.equal(open({ ...req, anchor: "native-pane" }), false);
  for (const launch of ["example", { ...argv, text: "example" }, { ...argv, program: "" }, { ...argv, args: ["bad\0arg"] }]) {
    assert.equal(open({ ...req, launch }), false);
  }
  assert.ok(list({}));
  assert.equal(list({ instance: "other" }), false);
  assert.ok(focus({ pane: "opaque-handle" }));
  assert.equal(focus({ pane: "" }), false);
  assert.equal(focus({ pane: "opaque-handle", instance: "other" }), false);
});

test("thread_open advertises tabs without a multiplexer and only offers supported splits", () => {
  assert.equal(paneToolParameters({ views: true }).thread_open, undefined, "v1 clients do not support thread views");
  const tabOnly = paneToolParameters({ threadTabs: true, noPanes: true });
  assert.deepEqual(Object.keys(tabOnly), ["thread_open"]);
  const { check } = compile(tabOnly.thread_open);
  assert.ok(check({ threadId: "10002", where: "tab" }));
  assert.ok(!check({ threadId: "10002", where: "split" }));
  assert.ok(!check({ threadId: "10002", where: "tab", prompt: "not a view" }));
  assert.ok(!check({ threadId: "0007", where: "tab" }));
  const split = compile(paneToolParameters({ views: true, threadTabs: true }).thread_open).check;
  assert.ok(split({ threadId: "10002", where: "split" }));
  assert.ok(!split({ threadId: "10002", where: "split", dir: "/not-the-thread" }));
});
