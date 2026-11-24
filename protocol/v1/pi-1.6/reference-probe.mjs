import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, realpath, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";

const reference = fileURLToPath(new URL("./reference-child.mjs", import.meta.url));

export async function pi(t, mode, scenario, { persist = false } = {}) {
  const dir = await realpath(await mkdtemp(join(tmpdir(), "hush-pi-fixture-")));
  const child = spawn(process.execPath, [reference, mode, scenario], {
    cwd: dir,
    env: {
      HOME: dir,
      PATH: process.env.PATH,
      PI_CODING_AGENT_DIR: join(dir, "agent"),
      ...(persist ? { HUSH_FIXTURE_PERSIST: "1" } : {}),
    },
    stdio: ["pipe", "pipe", "pipe"],
  });
  let stdout = "", stderr = "", tail = "";
  const records = [];
  let parseError, exit;
  child.stdout.setEncoding("utf8");
  child.stdout.on("data", (chunk) => {
    stdout += chunk;
    if (mode === "text") return;
    // U+2028/U+2029 are not JSONL delimiters. StringDecoder preserves split UTF-8.
    const lines = (tail + chunk).split("\n");
    tail = lines.pop();
    try { records.push(...lines.map((line) => JSON.parse(line.replace(/\r$/, "")))); }
    catch (error) { parseError ??= error; }
  });
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  child.stdin.on("error", () => {}); // A failed startup is reported with its stderr below.
  child.once("error", (error) => { parseError ??= error; });
  const closed = new Promise((resolve) => child.once("close", (code, signal) => {
    exit = { code, signal };
    resolve();
  }));
  t.after(async () => {
    if (!exit) child.kill("SIGKILL");
    await closed;
    await rm(dir, { recursive: true, force: true });
  });
  const until = async (predicate, after = 0) => {
    const deadline = performance.now() + 5000;
    while (performance.now() < deadline) {
      if (parseError) throw parseError;
      const record = records.slice(after).find(predicate);
      if (record) return record;
      if (exit) break;
      await delay(4);
    }
    assert.fail(`Pi fixture record not observed; exit=${JSON.stringify(exit)} stderr=${stderr} records=${records.map((r) => r.type + (r.command ? `:${r.command}` : ""))}`);
  };
  let nextID = 0;
  const send = async (type, fields = {}, expectedSuccess = true) => {
    const id = `r${++nextID}`;
    child.stdin.write(JSON.stringify({ type, id, ...fields }) + "\n");
    const reply = await until((r) => r.type === "response" && r.id === id);
    assert.equal(reply.command, type);
    assert.equal(reply.success, expectedSuccess, JSON.stringify(reply));
    if (!expectedSuccess) assert.equal(typeof reply.error, "string");
    return reply.data;
  };
  const finish = async () => {
    child.stdin.end();
    await closed;
    if (parseError) throw parseError;
    assert.equal(tail, "", "unterminated JSONL record");
    return { ...exit, stdout, stderr, records };
  };
  return { dir, records, send, until, finish, raw: (line) => child.stdin.write(line) };
}

export function ordered(records, predicates) {
  let i = 0;
  for (const predicate of predicates) {
    const j = records.findIndex((record, index) => index >= i &&
      (typeof predicate === "string" ? record.type === predicate : predicate(record)));
    assert.ok(j >= i, `missing ordered event ${predicate} after ${i}: ${records.map((r) => r.type)}`);
    i = j + 1;
  }
}

export const textOf = (value) => typeof value === "string" ? value : value
  .filter((part) => part.type === "text").map((part) => part.text).join("");
