import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { dial } from "../../server/test/client.mjs";

const bundle = fileURLToPath(new URL("../../dist/hush-backend.mjs", import.meta.url));

test("keyring unavailable fails closed; a live host does not switch stores", { timeout: 60_000 }, async t => {
  const root = await mkdtemp("/tmp/hkr-missing-");
  const profile = join(root, ".hush"), dir = join(profile, "runtime");
  const choice = join(profile, "credential-store"), auth = join(profile, "auth.json");
  const fileSecret = "synthetic-file-secret";
  const old = JSON.stringify({ "openai-codex": { type: "oauth", access: fileSecret,
    refresh: "synthetic-file-refresh", expires: Date.now() + 3_600_000, accountId: "fixture-account" } });
  await mkdir(profile, { mode: 0o700 });
  const preload = join(root, "no-network.mjs");
  await writeFile(preload, "globalThis.WebSocket=undefined; globalThis.fetch=()=>{throw Error('No provider in this fixture')};\n");
  const env = { HOME: root, PATH: "/usr/bin:/bin", NODE_NO_WARNINGS: "1", LANG: "C", NODE_OPTIONS: `--import=${preload}`,
    HUSH_KEYRING_HELPER: join(root, "missing-helper") };
  await Promise.all([writeFile(choice, "keyring\n", { mode: 0o600 }), writeFile(auth, old, { mode: 0o600 })]);
  const authCommand = action => new Promise((resolve, reject) => {
    // Bound the whole fixture process, including cold bundle startup on CI.
    // A timeout or signal is not the expected fail-closed status (exit 1).
    execFile(process.execPath, [bundle, "auth", action], { cwd: root, env, timeout: 15_000, killSignal: "SIGKILL" }, (err, stdout, stderr) => {
      if (err && (err.killed || err.signal || typeof err.code !== "number")) {
        reject(new Error(`auth ${action} did not exit normally (${err.signal ?? err.code ?? "unknown"})`, { cause: err }));
      } else resolve({ code: err?.code ?? 0, stdout, stderr });
    });
  });
  const stop = [];
  t.after(async () => { for (const fn of stop.reverse()) await fn(); await rm(root, { recursive: true, force: true }); });
  for (const action of ["status", "logout"]) {
    const { code, stdout, stderr } = await authCommand(action);
    assert.equal(code, 1);
    assert.equal(stdout, "");
    assert.match(stderr, /^Hush keyring helper is unavailable\./);
    assert.doesNotMatch(stderr, /synthetic-file|\[1\] Browser|password:/);
  }
  assert.equal(await readFile(auth, "utf8"), old);
  const child = spawn(process.execPath, [bundle, "--cwd", root], { cwd: root, env, stdio: ["ignore", "pipe", "pipe"] });
  const exit = once(child, "exit");
  stop.push(async () => { child.kill("SIGTERM"); await exit; });
  const ready = await createInterface({ input: child.stdout })[Symbol.asyncIterator]().next();
  assert.deepEqual(JSON.parse(ready.value), { ready: true, socket: join(dir, "hush.sock") });
  const peer = await dial(join(dir, "hush.sock"));
  stop.push(async () => peer.close());
  assert.equal((await peer.send("hello", { versions: [1] })).ok, true);
  const thread = await peer.send("thread.new", {});
  assert.equal(thread.ok, true);
  const threadId = thread.result.id;
  const attempt = async requestId => {
    const rejected = await peer.send("submit", { threadId, requestId, text: "must not reach a provider", whenBusy: "reject" });
    assert.equal(rejected.error?.code, "invalid_request");
    assert.equal((await peer.send("submission.get", { threadId, requestId })).error?.code, "not_found");
    return rejected.error.message;
  };
  assert.match(await attempt("unavailable"), /^Hush keyring helper is unavailable\./);
  await writeFile(choice, "file\n");
  assert.match(await attempt("different-store"), /credential store selection changed/);
  assert.match((await authCommand("status")).stdout, /OAuth configured/); // explicitly selected file CLI is separate
  assert.doesNotMatch(JSON.stringify(peer.frames), /synthetic-file/);
  assert.equal(await readFile(auth, "utf8"), old);
});
