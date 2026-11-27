import assert from "node:assert/strict";
import { once } from "node:events";
import { writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join } from "node:path";

export const createRequest = "Create a plan for exporting interface preferences";
export const draftArgs = (baseRevision = 0, title = "Interface preferences") => ({
  baseRevision, title, steps: [
    { id: "save", title: "Save preferences", description: "Serialize the current preferences.", checks: ["Saved settings match the selected theme"] },
    { id: "restore", title: "Restore preferences", description: "Load the saved preferences.",
      requires: ["save"], checks: ["Round-trip preserves the selected theme"] },
  ],
});
export const reviewReport = { summary: "The acceptance checks miss a default.",
  checks: { requirements: "addressed", acceptance: "concern", dependencies: "addressed" },
  findings: [{ severity: "blocking", area: "acceptance", stepId: "save", message: "Check how omitted values keep their defaults." }],
  limitations: ["No behavior evidence was supplied."] };
export const call = (name, args = {}) => ({ name, arguments: JSON.stringify(args) });
export const executionRequest = "Create a plan to switch the saved and restored preference themes to mocha";
export const preparedFiles = { "save.txt": "saved=latte\n", "restore.txt": "restored=latte\n", "notes.txt": "pending=latte\n" };
export const edits = {
  save: { path: "save.txt", oldText: "saved=latte", newText: "saved=mocha" },
  restore: { path: "restore.txt", oldText: "restored=latte", newText: "restored=mocha" },
  notes: { path: "notes.txt", oldText: "pending=latte", newText: "pending=mocha" },
};
export const fileSetRequest = "Create a plan to align both profile files with the shared theme reference";
export const fileSetReference = { "reference.json": '{"theme":"mocha"}\n' };
export const fileSetBehavior = `if (typeof process !== "undefined" || typeof fetch !== "undefined" || typeof require !== "undefined" || ALL_TOOLS.length !== 0) throw Error("unexpected host access");
if (Object.keys(files).length !== 3 || files["notes.txt"] !== undefined || !Object.isFrozen(files)) throw Error("unexpected file scope");
const theme = JSON.parse(files["reference.json"]).theme;
return files["save.txt"].trim() === "saved=" + theme && files["restore.txt"].trim() === "restored=" + theme;`;
export function fileSetDraft(script = fileSetBehavior) {
  return { baseRevision: 0, title: "Align saved profile themes", steps: [
    { id: "profiles", title: "Align both profiles", description: "Use the theme declared in the reference without changing it.",
      checks: ["Both profile files have the declared theme"], scope: { read: ["reference.json"], write: ["save.txt", "restore.txt"],
        acceptance: { label: "Both profiles match the reference", script } } },
    { id: "notes", title: "Update notes", description: "Separately update the notes.", checks: ["Notes say mocha"],
      edit: { path: "notes.txt", contains: "pending=mocha" } },
  ] };
}

export function editDraft() {
  const args = draftArgs(0, "Switch preference themes");
  args.steps.push({ id: "notes", title: "Update notes", description: "Update the theme in the notes.", checks: [] });
  for (const step of args.steps) {
    const { path, newText } = edits[step.id];
    step.edit = { path, contains: newText };
    step.checks = [`${path} contains ${newText}`];
  }
  args.steps[0].thinking = "high";
  return args;
}

export function scopedReply({ text, tools, results }) {
  assert.ok(text.startsWith("Plan assignment\n"), text);
  assert.deepEqual(tools.toSorted(), ["hush_plan_read_file", "hush_plan_replace_text"]);
  const { id } = JSON.parse(text.slice("Plan assignment\n".length));
  const { oldText, newText } = edits[id];
  if (results.length === 0) return [call("hush_plan_read_file")];
  if (results.length === 1) {
    assert.match(results[0].content, new RegExp(oldText));
    return [call("hush_plan_replace_text", { oldText, newText })];
  }
  assert.match(results.at(-1).content, /Replacement recorded/);
  return "The requested replacement is ready.";
}

// Use the non-faux runtime, prompt builder and tool dispatcher. Only the
// provider's responses are scripted; the ordinary user text goes through Hush.
export async function planProvider(t, agentDir, answer, { alternate = false, usage } = {}) {
  const requests = [], errors = [];
  const provider = createServer(async (req, res) => {
    try {
      assert.equal(req.method, "POST");
      assert.equal(req.url, "/v1/chat/completions");
      const parts = [];
      for await (const chunk of req) parts.push(chunk);
      const body = JSON.parse(Buffer.concat(parts).toString());
      const user = body.messages.findLastIndex(msg => msg.role === "user");
      const results = body.messages.slice(user + 1).filter(msg => msg.role === "tool");
      const turn = { model: body.model, text: body.messages[user]?.content, results, messages: body.messages,
        tools: (body.tools ?? []).map(tool => tool.function.name),
        prompt: body.messages.filter(msg => msg.role === "system" || msg.role === "developer").map(msg => msg.content).join("\n") };
      requests.push(turn);
      const response = await answer(turn);
      res.writeHead(200, { "content-type": "text/event-stream" });
      const send = (delta, finish_reason) => res.write("data: " + JSON.stringify({ id: "plan-fixture-provider", object: "chat.completion.chunk",
        created: 1, model: body.model, choices: [{ index: 0, delta, finish_reason }] }) + "\n\n");
      if (Array.isArray(response)) {
        send({ tool_calls: response.map((fn, index) => ({ index, id: `plan-call-${requests.indexOf(turn)}-${index}`, type: "function", function: fn })) }, null);
        send({}, "tool_calls");
      } else {
        if (typeof response === "string") send({ content: response }, null);
        else for await (const content of response) {
          assert.equal(typeof content, "string");
          send({ content }, null);
        }
        send({}, "stop");
      }
      if (usage) res.write("data: " + JSON.stringify({ choices: [], usage }) + "\n\n");
      res.end("data: [DONE]\n\n");
    } catch (error) {
      errors.push(error);
      if (res.headersSent) res.destroy();
      else res.writeHead(400).end();
    }
  }).listen(0, "127.0.0.1");
  t.after(async () => {
    provider.closeAllConnections();
    await new Promise(resolve => provider.close(resolve));
    assert.deepEqual(errors, []);
  });
  await once(provider, "listening");
  const model = { provider: "plan-test", modelId: "chat" }, staged = { provider: "staged-plan-test", modelId: "staged-chat" };
  const entries = alternate ? [model, staged] : [model];
  const auth = Object.fromEntries(entries.map(ref => [ref.provider, { type: "api_key", key: "fixture-not-a-secret" }]));
  const providers = Object.fromEntries(entries.map(ref => [ref.provider, {
    baseUrl: `http://127.0.0.1:${provider.address().port}/v1`, api: "openai-completions",
    models: [{ id: ref.modelId, name: "Plan fixture " + ref.modelId, reasoning: ref === staged, contextWindow: 100_000, maxTokens: 1_000,
      ...(usage && { cost: { input: 2, output: 4, cacheRead: 0, cacheWrite: 0 } }) }],
  }]));
  for (const [file, data] of Object.entries({ "auth.json": auth, "models.json": { providers } })) {
    await writeFile(join(agentDir, file), JSON.stringify(data), { mode: 0o600 });
  }
  return { requests, model, ...(alternate && { staged }) };
}
