// The public Pi executable, not only its print/RPC functions. No operator profile, tools, or external provider.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, readdir, realpath, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { SessionManager } from "@earendil-works/pi-coding-agent";

const cli = fileURLToPath(new URL("../../../node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js", import.meta.url));

async function fixture(t) {
  const cwd = await realpath(await mkdtemp(join(tmpdir(), "hush-pi-cli-")));
  const agent = join(cwd, "agent"), sessions = join(cwd, "isolated-sessions");
  await mkdir(agent);
  const requests = [];
  const provider = createServer(async (req, res) => {
    try {
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const body = JSON.parse(Buffer.concat(chunks));
      requests.push(body);
      const chunk = choice => "data: " + JSON.stringify({
        id: "chatcmpl-pi-cli-fixture", object: "chat.completion.chunk", created: 1, model: body.model,
        choices: [{ index: 0, ...choice }],
      }) + "\n\n";
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.end(chunk({ delta: { content: `fixture answer ${requests.length}` }, finish_reason: null }) +
        chunk({ delta: {}, finish_reason: "stop" }) + "data: [DONE]\n\n");
    } catch (err) {
      res.destroy(err);
    }
  });
  provider.listen(0, "127.0.0.1");
  await once(provider, "listening");
  t.after(async () => {
    provider.closeAllConnections();
    await new Promise(resolve => provider.close(resolve));
    await rm(cwd, { recursive: true, force: true });
  });
  await Promise.all([
    writeFile(join(cwd, "input.txt"), "fixture file payload"),
    writeFile(join(agent, "settings.json"), JSON.stringify({ defaultProvider: "hush-reference", defaultModel: "fixture-1",
      retry: { enabled: false }, compaction: { enabled: false } })),
    writeFile(join(agent, "models.json"), JSON.stringify({ providers: { "hush-reference": {
      baseUrl: `http://127.0.0.1:${provider.address().port}/v1`, api: "openai-completions", apiKey: "synthetic-not-a-secret",
      models: ["fixture-1", "fixture-2"].map(id => ({ id, name: id, reasoning: false, contextWindow: 20000, maxTokens: 500,
        input: ["text", "image"] })),
    } } })),
  ]);
  const run = async (args, input = "") => {
    const child = spawn(process.execPath, [cli, "--no-tools", "--no-extensions", "--no-skills", "--no-context-files",
      "--session-dir", sessions, ...args], {
      cwd, stdio: ["pipe", "pipe", "pipe"], env: { HOME: cwd, PI_CODING_AGENT_DIR: agent, PI_OFFLINE: "1",
        PATH: dirname(process.execPath) + ":" + process.env.PATH, NODE_NO_WARNINGS: "1" },
    });
    const closed = once(child, "close");
    const timer = setTimeout(() => child.kill("SIGKILL"), 8000);
    let stdout = "", stderr = "";
    child.stdout.setEncoding("utf8").on("data", chunk => { stdout += chunk; });
    child.stderr.setEncoding("utf8").on("data", chunk => { stderr += chunk; });
    child.stdin.on("error", () => {});
    child.stdin.end(input);
    try {
      const [code, signal] = await closed;
      return { code, signal, stdout, stderr };
    } finally { clearTimeout(timer); }
  };
  const files = async () => (await readdir(sessions).catch(error => {
    if (error.code !== "ENOENT") throw error;
    return [];
  })).filter(name => name.endsWith(".jsonl"));
  return { cwd, sessions, requests, run, files };
}

function userText(request) {
  return request.messages.filter(m => m.role === "user").map(m => typeof m.content === "string" ? m.content
    : m.content.filter(b => b.type === "text").map(b => b.text).join(""));
}

const succeeds = (out, stdout) => assert.deepEqual(out, { code: 0, signal: null, stdout, stderr: "" });

test("cli:redirected-flags-and-input", { timeout: 25000 }, async t => {
  const f = await fixture(t);
  succeeds(await f.run(["--no-session", "--provider", "hush-reference", "--model", "fixture-1",
    "@input.txt", "--", "-dash first", "second"], "stdin text\n"), "fixture answer 2\n");
  const [first, second] = f.requests.map(r => userText(r).at(-1));
  assert.match(first, /^stdin text<file name="[^\n]+\/input\.txt">\nfixture file payload\n<\/file>\n-dash first$/);
  assert.equal(second, "second");
  assert.deepEqual(f.requests.map(r => r.model), ["fixture-1", "fixture-1"]);
  assert.deepEqual(await f.files(), []);

  succeeds(await f.run(["-p", "--mode", "text", "--no-session", "--models", "hush-reference/fixture-2",
    "--thinking", "high", "scoped prompt"]), "fixture answer 3\n");
  assert.equal(f.requests[2].model, "fixture-2");
  const rejected = await f.run(["--mode", "rpc", "--no-session", "@input.txt"]);
  assert.equal(rejected.code, 1);
  assert.equal(rejected.signal, null);
  assert.equal(rejected.stdout, "");
  assert.match(rejected.stderr, /@file arguments are not supported in RPC mode/);
  assert.equal(f.requests.length, 3);

  const json = await f.run(["--mode", "json", "--no-session", "json prompt"]);
  assert.equal(json.code, 0, json.stderr);
  assert.equal(json.stderr, "");
  assert.ok(json.stdout.endsWith("\n"));
  const records = json.stdout.trimEnd().split("\n").map(row => JSON.parse(row));
  assert.equal(records[0].type, "session");
  assert.equal(records[0].version, 3);
  assert.equal(records[0].cwd, f.cwd);
  assert.equal(records.at(-1).type, "agent_settled");
  assert.deepEqual(await f.files(), []);
});

test("cli:at-file-image-with-an-image-capable-model", { timeout: 15000 }, async t => {
  const f = await fixture(t);
  const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAAAAAA6fptVAAAACklEQVR4nGNgAAAAAgABSK+kcQAAAABJRU5ErkJggg==", "base64");
  await writeFile(join(f.cwd, "pixel.png"), png);
  succeeds(await f.run(["--no-session", "@pixel.png", "describe fixture"]), "fixture answer 1\n");
  const user = f.requests[0].messages.findLast(message => message.role === "user");
  assert.ok(user.content.some(part => part.type === "text" && part.text.includes("describe fixture")));
  const image = user.content.find(part => part.type === "image_url");
  assert.match(image?.image_url.url ?? "", /^data:image\/(png|jpeg);base64,/);
  assert.deepEqual(await f.files(), []);
});

test("cli:session-flag-continuation-and-ephemeral-difference", { timeout: 25000 }, async t => {
  const f = await fixture(t);
  succeeds(await f.run(["--print", "--name", "fixture name", "saved input marker"]), "fixture answer 1\n");
  const saved = await f.files();
  assert.equal(saved.length, 1);
  const path = join(f.sessions, saved[0]);
  assert.equal(SessionManager.open(path).getSessionName(), "fixture name");
  succeeds(await f.run(["--continue", "continued input"]), "fixture answer 2\n");
  assert.deepEqual(userText(f.requests[1]), ["saved input marker", "continued input"]);
  succeeds(await f.run(["--session", path, "explicit session input"]), "fixture answer 3\n");
  assert.deepEqual(userText(f.requests[2]), ["saved input marker", "continued input", "explicit session input"]);
  // Stock Pi accepts this combination, but chooses a fresh in-memory session. Hush deliberately
  // rejects a durable-continuation flag with --no-session rather than silently ignoring it.
  succeeds(await f.run(["--no-session", "--continue", "private input"]), "fixture answer 4\n");
  assert.deepEqual(userText(f.requests[3]), ["private input"]);
  assert.deepEqual(await f.files(), saved);
  assert.ok(!JSON.stringify(SessionManager.open(path).getEntries()).includes("private input"));
});
