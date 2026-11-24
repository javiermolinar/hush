import assert from "node:assert/strict";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { ModelRuntime } from "@earendil-works/pi-coding-agent";
import { standaloneModelDefault } from "../../dist/backend/src/standalone-settings.js";

test("Pi-compatible Hush global model defaults, validation, and file privacy", async t => {
  const dir = await mkdtemp(join(tmpdir(), "hush-settings-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const runtime = await ModelRuntime.create({ authPath: join(dir, "auth.json"), modelsPath: join(dir, "models.json"),
    allowModelNetwork: false, refreshOnCreate: false });
  const path = join(dir, "settings.json");
  const fallback = await standaloneModelDefault(dir, runtime);
  assert.deepEqual(fallback, { model: { provider: "openai-codex", modelId: "gpt-6-sol" }, thinkingLevel: "off" });
  const save = value => writeFile(path, JSON.stringify(value), { mode: 0o600 });
  await save({ defaultProvider: "openai-codex", defaultModel: "gpt-6-sol", defaultThinkingLevel: "high" });
  assert.deepEqual(await standaloneModelDefault(dir, runtime), {
    model: { provider: "openai-codex", modelId: "gpt-6-sol" }, thinkingLevel: "high",
  });
  await save({ defaultThinkingLevel: "max" });
  assert.equal((await standaloneModelDefault(dir, runtime)).thinkingLevel, "max");
  for (const value of [
    { defaultProvider: "openai-codex" }, { defaultModel: "gpt-6-sol" },
    { defaultProvider: "openai-codex", defaultModel: "not-installed" },
    { defaultThinkingLevel: "maximum" },
    { defaultProvider: "\u001b", defaultModel: "gpt-6-sol" },
  ]) {
    await save(value);
    await assert.rejects(standaloneModelDefault(dir, runtime), { code: "configuration" });
  }
  await writeFile(path, "{", { mode: 0o600 });
  await assert.rejects(standaloneModelDefault(dir, runtime), { code: "configuration" });
  await rm(path);
  await writeFile(path, "{}", { mode: 0o600 });
  await chmod(path, 0o644);
  await assert.rejects(standaloneModelDefault(dir, runtime), { code: "configuration" });
});
