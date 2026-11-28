import assert from "node:assert/strict";
import { once } from "node:events";
import { writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join } from "node:path";

export const createRequest = "Create a plan for exporting interface preferences";
// Three steps: an inline step, a dependent step, and a read-only review a subagent does.
export const draftArgs = (baseRevision = 0, title = "Interface preferences", planId) => ({
  ...(planId && { planId }), baseRevision, title, steps: [
    { id: "save", title: "Save preferences", prompt: "Serialize the current preferences. Check that saved settings match the selected theme." },
    { id: "restore", title: "Restore preferences", prompt: "Load the saved preferences. Check that a round trip preserves the theme.",
      requires: ["save"] },
    { id: "review", title: "Review the round trip", prompt: "Review the save and restore code for lost settings.",
      requires: ["restore"], delegate: { read_only: true } },
  ],
});
export const call = (name, args = {}) => ({ name, arguments: JSON.stringify(args) });
// The plan ID from a plan_save result.
export const savedPlanId = text => /as plan ([0-9a-f-]{36}),/.exec(text)?.[1];

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
