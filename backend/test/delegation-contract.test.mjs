import assert from "node:assert/strict";
import { fork } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const fixture = fileURLToPath(new URL("./delegation-contract-fixture.mjs", import.meta.url));

test("Durable parks the parent turn and resumes it once after child settlement and SIGKILL", { timeout: 15_000 }, async t => {
  const directory = await mkdtemp("/tmp/hush-join-contract-");
  let active;
  t.after(async () => {
    if (active) { active.child.kill("SIGKILL"); await active.exit; }
    await rm(directory, { recursive: true, force: true });
  });
  async function run(phase, boundaries, kill = false) {
    const child = fork(fixture, [join(directory, "history.sqlite"), phase], { stdio: ["ignore", "ignore", "pipe", "ipc"] });
    const exit = once(child, "close");
    active = { child, exit };
    let errors = "";
    child.stderr.on("data", chunk => { errors += chunk; });
    const received = new Map();
    const ready = new Promise(resolve => child.on("message", message => {
      assert.ok(!received.has(message.boundary), JSON.stringify(message));
      received.set(message.boundary, message);
      if (boundaries.every(key => received.has(key))) resolve();
    }));
    await Promise.race([ready, exit.then(([code, signal]) => {
      assert.ok(boundaries.every(key => received.has(key)), `${phase} exited (${code ?? signal}) before ${boundaries}: ${errors}`);
    })]);
    if (kill) assert.ok(child.kill("SIGKILL"));
    assert.deepEqual(await exit, kill ? [null, "SIGKILL"] : [0, null], errors);
    active = undefined;
    return received.get(boundaries[0]);
  }
  const { parentEntry, submissionId, row } = await run("park", ["parked"], true);
  const { noticeId } = await run("settle", ["settled", "join-result-not-committed"], true);
  const resumed = await run("resume", ["finished"]);
  assert.equal(resumed.noticeId, noticeId);
  assert.equal(resumed.parentEntry, parentEntry);
  assert.equal(resumed.submissionId, submissionId);
  assert.deepEqual(resumed.row, { ...row, answer: "child evidence" });
  assert.deepEqual(await run("check", ["finished"]), resumed);
});
