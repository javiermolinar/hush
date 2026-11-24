// Opt-in: uses the real OS keyring, but only unique temporary Hush scopes
// with synthetic tokens and a loopback provider. Never locks the user's keyring.
import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { once } from "node:events";
import { lstat, mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join, resolve } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { dial } from "../../server/test/client.mjs";

const binary = process.env.HUSH_TEST_KEYRING_BINARY;
const bundle = resolve(process.env.HUSH_TEST_KEYRING_BACKEND ?? "dist/hush-backend.mjs");
const absent = path => assert.rejects(lstat(path), { code: "ENOENT" });
const pty = String.raw`
import os, select, subprocess, sys, termios, time
master, slave = os.openpty()
child = subprocess.Popen([sys.argv[1], 'auth', 'login'], stdin=slave, stdout=slave, stderr=slave)
steps = [(b'[1] Browser', b'1\n'), (b'(hidden)', b'keyring-fixture-code\n')]
output = b''
try:
    limit = time.monotonic() + 20
    while time.monotonic() < limit:
        if select.select([master], [], [], .05)[0]:
            output += os.read(master, 8192)
            if steps and steps[0][0] in output:
                assert not termios.tcgetattr(slave)[3] & termios.ECHO
                os.write(master, steps.pop(0)[1])
        elif child.poll() is not None:
            break
    sys.stdout.buffer.write(output)
    assert not steps and b'keyring-fixture-code' not in output
    sys.exit(child.wait(timeout=1))
finally:
    if child.poll() is None:
        child.kill()
        child.wait()
    os.close(master)
    os.close(slave)
`;

function reply(res, callTool) {
  const send = (type, data) => res.write(`data: ${JSON.stringify({ type, ...data })}\n\n`);
  res.setHeader("Content-Type", "text/event-stream");
  send("response.created", { response: { id: "fixture", status: "in_progress", output: [] } });
  const position = { output_index: 0, item_id: "item" };
  if (callTool) {
    const item = { id: position.item_id, type: "function_call", call_id: "call-fixture", name: "bash", arguments: "" };
    const args = JSON.stringify({ command: 'test -z "${OPENAI_API_KEY+x}${DBUS_SESSION_BUS_ADDRESS+x}${HUSH_KEYRING_HELPER+x}" && printf safe-tool-environment' });
    send("response.output_item.added", { ...position, item: { ...item, status: "in_progress" } });
    send("response.function_call_arguments.delta", { ...position, delta: args });
    send("response.function_call_arguments.done", { ...position, arguments: args });
    send("response.output_item.done", { ...position, item: { ...item, arguments: args, status: "completed" } });
  } else {
    const item = { id: position.item_id, type: "message", role: "assistant" };
    const part = { type: "output_text", text: "Keyring fixture answer", annotations: [] };
    send("response.output_item.added", { ...position, item: { ...item, status: "in_progress" } });
    send("response.content_part.added", { ...position, content_index: 0, part: { ...part, text: "" } });
    send("response.output_text.delta", { ...position, content_index: 0, delta: part.text });
    send("response.output_item.done", { ...position, item: { ...item, content: [part], status: "completed" } });
  }
  send("response.completed", { response: { id: "fixture", status: "completed", usage: {
    input_tokens: 12, output_tokens: 4, total_tokens: 16,
    input_tokens_details: { cached_tokens: 0 }, output_tokens_details: { reasoning_tokens: 0 },
  } } });
  res.end("data: [DONE]\n\n");
}

test("real standalone keyring: OAuth, refresh, concurrent logout, and no plaintext", { skip: !binary, timeout: 85_000 }, async t => {
  const root = await realpath(await mkdtemp("/tmp/hkr-"));
  const dir = join(root, ".hush"), cwd = join(root, "project"), runtime = join(dir, "runtime"), socket = join(runtime, "hush.sock");
  const oldFile = join(dir, "auth.json"), events = join(root, "launches"), fixture = join(root, "loopback.mjs");
  const otherData = join(root, "other"), otherSocket = join(otherData, "hush.sock");
  await Promise.all([dir, cwd].map(path => mkdir(path, { mode: 0o700 })));
  await writeFile(join(dir, "credential-store"), "keyring\n", { mode: 0o600 });
  const scope = createHash("sha256").update(dir).digest("hex"), otherScope = createHash("sha256").update(root).digest("hex");
  const account = "fixture-account";
  const token = n => "synthetic." + Buffer.from(JSON.stringify({ "https://api.openai.com/auth": { chatgpt_account_id: account } })).toString("base64url") + "." + n;
  const refresh = n => "synthetic-refresh-" + n;
  const record = (n, expires = Date.now() + 3_600_000) => ({ type: "oauth", access: token(n), refresh: refresh(n), expires, accountId: account });
  const exposed = [];
  let logins = 0, refreshes = 0, current = 1, tools = 0, toolOutput = false;
  let held, heldLogin;
  const blockRefresh = () => held = { arrived: Promise.withResolvers(), release: Promise.withResolvers() };
  const provider = createServer(async (req, res) => {
    try {
      const body = [];
      for await (const chunk of req) body.push(chunk);
      const raw = Buffer.concat(body).toString();
      if (req.method === "POST" && req.url === "/oauth/token") {
        const form = new URLSearchParams(raw), grant = form.get("grant_type");
        if (grant === "authorization_code" && form.get("code") === "keyring-fixture-code") {
          logins++;
          heldLogin?.arrived.resolve();
          await heldLogin?.release.promise;
        } else if (grant === "refresh_token") {
          refreshes++;
          assert.equal(form.get("refresh_token"), refresh(current));
          const waiting = held;
          waiting?.arrived.resolve();
          await waiting?.release.promise;
          current++;
        } else throw Error("unknown fixture auth operation");
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify({ access_token: token(current), refresh_token: refresh(current), expires_in: 3600 }));
      } else if (req.method === "POST" && req.url === "/responses") {
        // During concurrent logout, a request whose auth has already resolved
        // may finish. No later admission is entitled to that token.
        assert.equal(req.headers.authorization, "Bearer " + token(current));
        assert.equal(req.headers["chatgpt-account-id"], account);
        const request = JSON.parse(raw);
        const call = raw.includes("test-tool-environment") && tools === 0;
        if (call) tools++;
        if (request.input.some(item => item.type === "function_call_output" && String(item.output).includes("safe-tool-environment"))) toolOutput = true;
        exposed.push(raw);
        reply(res, call);
      } else throw Error("outside fixture");
    } catch { res.writeHead(400).end(); }
  });
  provider.listen(0, "127.0.0.1");
  await once(provider, "listening");
  await writeFile(fixture, `
    import {appendFileSync} from 'node:fs';
    appendFileSync(${JSON.stringify(events)}, JSON.stringify({pid:process.pid,args:process.argv.slice(2)})+'\\n');
    const zlib=process.getBuiltinModule('node:zlib'); if(zlib) zlib.zstdCompressSync=undefined;
    globalThis.WebSocket=undefined;
    const original=globalThis.fetch;
    globalThis.fetch=(input,init)=>{
      const url=new URL(input instanceof Request?input.url:input);
      const auth=url.origin==='https://auth.openai.com'&&url.pathname==='/oauth/token';
      const codex=['https://chatgpt.com','https://api.openai.com'].includes(url.origin)&&url.pathname.endsWith('/responses');
      if(!auth&&!codex) throw Error('Outside keyring fixture');
      const local='http://127.0.0.1:${provider.address().port}'+(auth?'/oauth/token':'/responses');
      return original(input instanceof Request?new Request(local,input):local,{...init,redirect:'error'});
    };
  `);
  const env = { HOME: root, PATH: process.env.PATH, LANG: "C", TERM: "xterm-256color", NODE_NO_WARNINGS: "1",
    NODE_OPTIONS: `--import=${fixture}`, HUSH_BACKEND: bundle, OPENAI_API_KEY: "fixture-env-marker",
    ...(process.env.DBUS_SESSION_BUS_ADDRESS && { DBUS_SESSION_BUS_ADDRESS: process.env.DBUS_SESSION_BUS_ADDRESS }) };
  const invoke = (exe, args, { input, overrides } = {}) => new Promise((done, fail) => {
    const child = execFile(exe, args, { cwd, env: { ...env, ...overrides }, timeout: 24000 }, (error, stdout, stderr) => {
      if (error && typeof error.code !== "number") fail(error);
      else done({ code: error?.code ?? 0, stdout, stderr });
    });
    child.stdin.on("error", () => {});
    child.stdin.end(input);
  });
  const run = async (args, code = 0, overrides) => {
    const result = await invoke(binary, args, { overrides });
    exposed.push(result.stdout, result.stderr);
    assert.equal(result.code, code, [args[0], result.stderr].join(": "));
    return code ? result.stderr : result.stdout;
  };
  const helper = async (operation, target = scope, value) => {
    const r = await invoke(binary, ["--hush-internal-keyring"], {
      input: JSON.stringify({ operation, scope: target, record: value }), overrides: { HOME: undefined },
    });
    assert.equal(r.code, 0, "system keyring unavailable or a fixture item could not be accessed");
    assert.equal(r.stderr, "");
    if (r.stdout) {
      try { return JSON.parse(r.stdout); }
      catch { assert.fail("invalid private keyring fixture response"); }
    }
  };
  const launches = async () => (await readFile(events, "utf8")).trim().split("\n").map(JSON.parse);
  const until = async predicate => {
    for (let i = 0; i < 200; i++) { if (await predicate()) return; await delay(20); }
    assert.fail("fixture state did not settle");
  };
  const stopped = new Set(), cleanup = new Set();
  const stop = async () => {
    const daemon = (await launches()).findLast(row => row.args.includes(runtime) && row.args.includes("--startup-fd"));
    assert.ok(daemon);
    process.kill(daemon.pid, "SIGTERM");
    await until(async () => !(await lstat(socket).catch(() => undefined)));
    stopped.add(daemon.pid);
  };
  t.after(async () => {
    held?.release.resolve();
    heldLogin?.release.resolve();
    for (const row of await launches().catch(() => [])) {
      if (!stopped.has(row.pid) && row.args.includes(runtime) && row.args.includes("--startup-fd")) {
        try { process.kill(row.pid, "SIGTERM"); } catch { /* already exited */ }
      }
    }
    for (const close of cleanup) await close();
    provider.closeAllConnections();
    await new Promise(done => provider.close(done));
    try { for (const target of [scope, otherScope]) await helper("delete", target); }
    finally { await rm(root, { recursive: true, force: true }); }
  });
  assert.equal((await helper("get")) === undefined, true, "prototype fixture scope was not empty");
  const neighbor = record(0);
  await helper("set", otherScope, neighbor);
  await run(["status", "--json"]); // Go-managed daemon is already running before login.
  assert.match(await run(["--print", "before sign in"], 1), /hush auth login/);
  const login = async () => {
    const r = await invoke("/usr/bin/env", ["python3", "-c", pty, binary]);
    assert.equal(r.code, 0, r.stdout + r.stderr);
    exposed.push(r.stdout, r.stderr);
    assert.match(r.stdout, /signed in with OAuth/);
  };
  await login();
  assert.equal(logins, 1);
  await absent(oldFile);
  assert.match(await run(["auth", "status"]), /OAuth configured/);
  const saved = await helper("get");
  assert.equal(saved.access === token(1) && saved.refresh === refresh(1) && saved.accountId === account && saved.expires > Date.now(), true);
  assert.match(await run(["--no-session", "--print", "private fresh credential"]), /^Keyring fixture answer\n$/);
  assert.equal(JSON.stringify(await helper("get")) === JSON.stringify(saved), true);
  assert.equal(refreshes, 0);
  assert.match(await run(["--print", "test-tool-environment"]), /^Keyring fixture answer\n$/);
  assert.equal(tools, 1);
  assert.equal(toolOutput, true);
  await stop();

  await helper("set", scope, record(1, Date.now() - 1));
  assert.match(await run(["--no-session", "--print", "private expired credential"], 1), /do not refresh OAuth/);
  assert.equal(refreshes, 0);
  assert.equal((await helper("get")).expires < Date.now(), true);
  await run(["status", "--json"]);
  const second = spawn(process.execPath, [bundle, "--profile", "hush", "--data-dir", otherData, "--cwd", cwd], {
    cwd, env: { ...env, HUSH_KEYRING_HELPER: binary }, stdio: ["ignore", "pipe", "pipe"],
  });
  const secondExit = once(second, "close");
  cleanup.add(async () => { second.kill("SIGTERM"); await secondExit; });
  second.stderr.on("data", text => exposed.push(String(text)));
  const next = await createInterface({ input: second.stdout })[Symbol.asyncIterator]().next();
  assert.deepEqual(JSON.parse(next.value), { ready: true, socket: otherSocket });
  const gate = blockRefresh();
  const primary = run(["--print", "refresh after restart"]);
  await Promise.race([gate.arrived.promise, primary.then(() => assert.fail("no refresh before provider call"))]);
  // The sibling daemon has admitted its own generation while the first
  // process owns the credential refresh lock (separate history writers).
  const other = JSON.parse(await run(["--socket", otherSocket, "--no-wait", "--json", "concurrent refresh"]));
  const waiting = run(["--socket", otherSocket, "submission", "wait", "--thread", other.threadId, "--submission-id", other.submissionId, "--json"]);
  await delay(200);
  gate.release.resolve();
  assert.match(await primary, /^Keyring fixture answer\n$/);
  assert.equal(JSON.parse(await waiting).status, "done");
  assert.equal(refreshes, 1);
  assert.equal((await helper("get")).refresh === refresh(2), true);

  await helper("set", scope, record(2, Date.now() - 1));
  const race = blockRefresh();
  const during = run(["--print", "refresh races logout"]);
  await Promise.race([race.arrived.promise, during.then(() => assert.fail("no refresh in logout race"))]);
  const logout = run(["auth", "logout"]);
  await until(async () => (await launches()).some(row => row.args.join(" ") === "auth logout"));
  await delay(150);
  race.release.resolve();
  assert.match(await during, /^Keyring fixture answer\n$/);
  assert.match(await logout, /no Hush-managed credential/);
  assert.equal(refreshes, 2);
  assert.equal((await helper("get")) === undefined, true);
  assert.equal(JSON.stringify(await helper("get", otherScope)) === JSON.stringify(neighbor), true);
  assert.match(await run(["--print", "after logout"], 1), /hush auth login/);

  heldLogin = { arrived: Promise.withResolvers(), release: Promise.withResolvers() };
  const signIn = login();
  await Promise.race([heldLogin.arrived.promise, signIn.then(() => assert.fail("login exchange was not held"))]);
  const afterLogin = run(["auth", "logout"]);
  await until(async () => (await launches()).filter(row => row.args.join(" ") === "auth logout").length === 2);
  await delay(150);
  heldLogin.release.resolve();
  await signIn;
  assert.match(await afterLogin, /no Hush-managed credential/);
  assert.equal(logins, 2);
  assert.equal((await helper("get")) === undefined, true, "a concurrent login resurrected the credential after logout");
  assert.equal(JSON.stringify(await helper("get", otherScope)) === JSON.stringify(neighbor), true);
  await absent(oldFile);
  const peer = await dial(socket);
  try {
    const hello = await peer.send("hello", { versions: [1], capabilities: { modelCatalog: [1] } });
    assert.equal(hello.ok, true);
    const created = await peer.send("thread.new", {});
    await peer.send("ext.modelCatalog.list", { threadId: created.result.id });
    exposed.push(JSON.stringify(peer.frames));
  } finally { peer.close(); }
  exposed.push(await readFile(join(runtime, "hush.log"), "utf8"), await readFile(events, "utf8"));
  assert.equal(exposed.every(s => ![1, 2, 3].some(n => s.includes(token(n)) || s.includes(refresh(n)))), true, "credential escaped an allowed channel");
  await stop();
});
