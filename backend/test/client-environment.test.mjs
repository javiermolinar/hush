import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { dial } from "../../server/test/client.mjs";
import { WorkspaceTrust } from "../../dist/backend/src/workspace-trust.js";

const extension = "ext.clientEnvironment.submit";
const bundle = fileURLToPath(new URL("../../dist/hush-backend.mjs", import.meta.url));
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const clientOne = { os: "macos", terminal: "ghostty", multiplexer: "rex", shell: "fish" };
const clientTwo = { os: "linux", terminal: "wezterm", multiplexer: "tmux", loginShellHint: "zsh" };
const firstText = " Fish input\nleave this text unchanged ";
const secondText = "Linux input queued by a different client";
const toolText = "Fixture asks for a bash call";
const steerText = "Steer placed after the tool round";

function respond(res, useTool) {
  const send = (delta, finish_reason) => res.write("data: " + JSON.stringify({ id: "fixture", object: "chat.completion.chunk",
    created: 1, model: "test", choices: [{ index: 0, delta, finish_reason }] }) + "\n\n");
  res.writeHead(200, { "content-type": "text/event-stream" });
  if (useTool) {
    send({ tool_calls: [{ index: 0, id: "fixture-tool", type: "function", function: {
      name: "bash", arguments: JSON.stringify({ command: 'printf "fixture-tool-%s\\n" "${BASH_VERSION:+bash}"' }),
    } }] }, null);
    send({}, "tool_calls");
  } else {
    send({ content: "Fixture reply" }, null);
    send({}, "stop");
  }
  res.end("data: [DONE]\n\n");
}

function checkPrompt(request, expected, absent = []) {
  const sys = request.messages.filter(m => m.role === "system" || m.role === "developer").map(m => m.content).join("\n");
  assert.equal(sys.split("## Environment").length, 2, sys);
  assert.match(sys, /Tool host OS: (macOS|Linux)/);
  assert.match(sys, /Tool execution shell: bash/);
  for (const field of expected) assert.ok(sys.includes(field), `missing ${field}: ${sys}`);
  for (const field of absent) assert.ok(!sys.includes(field), `stale ${field}: ${sys}`);
}

test("standalone environment follows durable placement, not the socket or conversation", { timeout: 25_000 }, async t => {
  const root = await mkdtemp("/tmp/hse-");
  const profile = join(root, ".hush"), cwd = join(root, "project");
  const socket = join(profile, "runtime", "hush.sock");
  const requests = new Map();
  const held = new Map([firstText, toolText].map(text => [text, { arrived: Promise.withResolvers(), reply: Promise.withResolvers() }]));
  const provider = createServer(async (req, res) => {
    try {
      if (req.method !== "POST" || req.url !== "/v1/chat/completions") throw Error("unexpected URL");
      const parts = [];
      for await (const chunk of req) parts.push(chunk);
      const request = JSON.parse(Buffer.concat(parts).toString());
      const input = request.messages.findLast(m => m.role === "user")?.content;
      if (typeof input !== "string" || requests.has(input)) throw Error("unexpected input or repeated provider call");
      requests.set(input, request);
      const gate = held.get(input);
      if (gate) { gate.arrived.resolve(); await gate.reply.promise; }
      respond(res, input === toolText);
    } catch { res.writeHead(400).end(); }
  });
  provider.listen(0, "127.0.0.1");
  await once(provider, "listening");
  const children = new Set(), sockets = new Set();
  t.after(async () => {
    for (const gate of held.values()) gate.reply.resolve();
    for (const peer of sockets) peer.close();
    for (const { child, exited } of children) { child.kill("SIGKILL"); await exited; }
    provider.closeAllConnections();
    await new Promise(resolve => provider.close(resolve));
    await rm(root, { recursive: true, force: true });
  });
  await Promise.all([mkdir(profile, { mode: 0o700 }), mkdir(cwd, { mode: 0o700 })]);
  await new WorkspaceTrust(profile).change(cwd, "grant", AbortSignal.timeout(5000));
  await writeFile(join(profile, "auth.json"), JSON.stringify({ fixture: { type: "api_key", key: "not-a-real-secret" } }), { mode: 0o600 });
  await writeFile(join(profile, "models.json"), JSON.stringify({ providers: { fixture: {
    baseUrl: `http://127.0.0.1:${provider.address().port}/v1`, api: "openai-completions",
    models: [{ id: "test", name: "Fixture", contextWindow: 100000, maxTokens: 500, reasoning: false }],
  } } }), { mode: 0o600 });

  const boot = async (pi = false) => {
    const child = spawn(process.execPath, [bundle, "--profile", pi ? "pi" : "hush", "--model", "fixture/test", "--cwd", cwd], {
      cwd, env: { HOME: root, PATH: "/usr/bin:/bin", LANG: "C", SHELL: "/pretend/daemon-shell",
        NODE_NO_WARNINGS: "1", PI_CODING_AGENT_DIR: join(root, "pi") }, stdio: ["ignore", "pipe", "pipe"],
    });
    const exited = once(child, "close");
    const proc = { child, exited };
    children.add(proc);
    let diagnostics = "";
    child.stderr.on("data", chunk => { diagnostics += chunk; });
    const row = await createInterface({ input: child.stdout })[Symbol.asyncIterator]().next();
    assert.ok(!row.done, diagnostics);
    const ready = JSON.parse(row.value);
    assert.deepEqual(ready, { ready: true, socket: pi ? join(root, "pi", "hush", "hush.sock") : socket });
    return { socket: ready.socket, async stop() {
      child.kill("SIGTERM");
      assert.deepEqual(await exited, [0, null], diagnostics);
      children.delete(proc);
    } };
  };
  const connect = async (capable = true, path = socket, expected = capable) => {
    const peer = await dial(path);
    sockets.add(peer);
    const hello = ok(await peer.send("hello", { versions: [1], capabilities: capable ? { clientEnvironment: [1] } : {} }));
    assert.deepEqual(hello.capabilities, expected ? { clientEnvironment: 1 } : {});
    return peer;
  };
  let host = await boot();
  const a = await connect(), b = await connect();
  const { id: threadId } = ok(await a.send("thread.new", {}));
  const input = (text, requestId, whenBusy = "reject") => ({ threadId, text, requestId, whenBusy });
  const submit = async (client, draft, environment) => ok(await client.send(extension, { input: draft, environment })).submissionId;
  const settle = async (client, submissionId) => assert.equal(ok(await client.send("submission.wait", { threadId, submissionId })).status, "done");

  const first = await submit(a, input(firstText, "first"), clientOne);
  await held.get(firstText).arrived.promise;
  checkPrompt(requests.get(firstText), ["Client shell (invoking process): fish", "Client OS: macOS", "Client multiplexer: rex", "Client terminal: ghostty"], ["daemon-shell", "Client login-shell hint"]);
  const draft = input(secondText, "second", "followUp");
  const second = await submit(b, draft, clientTwo);
  assert.equal(ok(await b.send("submission.get", { threadId, submissionId: second })).status, "queued");
  a.close();
  b.close();
  const reattached = await connect();
  assert.equal(await submit(reattached, draft, clientOne), second); // first admission owns the metadata as well
  held.get(firstText).reply.resolve();
  await settle(reattached, first);
  await settle(reattached, second);
  checkPrompt(requests.get(secondText), ["Client login-shell hint (active shell unknown): zsh", "Client OS: Linux", "Client multiplexer: tmux", "Client terminal: wezterm"], ["fish", "Client OS: macOS", "Client multiplexer: rex"]);

  const call = await submit(reattached, input(toolText, "call"), clientOne);
  await held.get(toolText).arrived.promise;
  const other = await connect();
  const steer = await submit(other, input(steerText, "steer", "steer"), clientTwo);
  assert.equal(ok(await other.send("submission.get", { threadId, submissionId: steer })).status, "queued");
  held.get(toolText).reply.resolve();
  await settle(reattached, call);
  await settle(other, steer);
  const mixed = requests.get(steerText);
  checkPrompt(mixed, [], ["Client OS:", "Client shell (", "Client terminal:", "Client multiplexer:", "Client login-shell hint ("]);
  assert.ok(mixed.messages.some(m => m.role === "tool" && String(m.content).includes("fixture-tool-bash")), "tool did not execute in Bash");

  await host.stop();
  host = await boot();
  const optOut = await connect(false);
  const plain = "Plain input after daemon restart";
  const denied = await optOut.send(extension, { input: input(plain, "plain"), environment: clientOne });
  assert.equal(denied.error?.code, "unsupported_capability");
  await settle(optOut, ok(await optOut.send("submit", input(plain, "plain"))).submissionId);
  const invalid = "Input with no recognized facts";
  const after = await connect();
  await settle(after, await submit(after, input(invalid, "invalid"), {
    os: "unallowlisted-os", multiplexer: "/no/raw/socket", terminal: { nested: "unknown" },
    shell: "/no/raw/shell", loginShellHint: "fish; unallowlisted", hostname: "private-hostname",
  }));
  for (const text of [plain, invalid]) {
    checkPrompt(requests.get(text), [], ["Client OS:", "Client shell (", "Client terminal:", "Client multiplexer:", "Client login-shell hint (", "unallowlisted", "/no/raw", "private-hostname"]);
  }
  const { entries } = ok(await after.send("history", { threadId, range: { kind: "before", beforeEntryId: null }, limit: 100 }));
  assert.deepEqual(entries.filter(e => e.messages.some(m => m.role === "user")).map(e => [e.kind, e.messages[0].blocks[0].text]),
    [firstText, secondText, toolText, steerText, plain, invalid].map(text => ["pi.user", text]));
  assert.doesNotMatch(JSON.stringify(entries), /clientInput|clientEnvironment|private-hostname|unallowlisted/);
  await host.stop();
  const optional = await boot(true);
  await connect(true, optional.socket, false);
  await optional.stop();
});
