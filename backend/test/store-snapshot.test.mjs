import assert from 'node:assert/strict';
import { lstat, mkdir, mkdtemp, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { test } from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { BACKGROUND_CONTEXT as context } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { createRegistry, defineExtension, defineTool, Harness } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import { initializeStore } from '../../dist/backend/src/profile.js';
import { makeSnapshot, verifySnapshot } from '../../dist/backend/src/store-snapshot.js';

// Synthetic app validation fixture only: tests the persisted pi.tool:v4
// representation with a sleeping substitute for read. This is not evidence
// that the format-1 candidate exposed approvals or parked coding tools.
async function parkedRead(t) {
  const dir = await mkdtemp('/tmp/hush-snapshot-');
  t.after(() => rm(dir, { recursive: true, force: true }));
  const runtime = join(dir, 'runtime'), out = join(dir, 'out');
  await mkdir(runtime, { mode: 0o700 });
  await mkdir(out, { mode: 0o700 });
  await initializeStore(runtime);
  const source = join(runtime, 'history.sqlite');
  const storage = await openNodeSqliteStorage(source);
  const fakeClock = Date.now() - 60_000, wake = fakeClock + 30_000;
  const faux = fauxProvider({ api: 'snapshot-faux', tokenSize: { min: 4, max: 4 } });
  faux.setResponses([fauxAssistantMessage(fauxToolCall('read', { path: 'does-not-exist.txt' }, { id: 'parked-read' }), { stopReason: 'toolUse' })]);
  const models = createModels();
  models.setProvider(faux.provider);
  let calls = 0;
  const registry = createRegistry();
  const coding = defineExtension({ name: 'coding-tools', tools: [defineTool({
    name: 'read', description: 'Synthetic parked read; performs no file access.',
    parameters: { type: 'object', properties: { path: { type: 'string' } }, required: ['path'], additionalProperties: false },
    replay: 'unsafe',
    async execute() { calls++; return { sleep: { until: wake } }; },
  })] });
  registry.install(coding);
  const h = await Harness.open(storage, { models, registry, now: () => fakeClock, settings: { compaction: { enabled: false } } }, context);
  try {
    const conv = await h.root(context, { agent: { cwd: dir, model: { provider: 'faux', modelId: 'faux-1' }, extensions: [coding] } });
    h.resume();
    await conv.submit({ type: 'input', requestId: 'parked-read', content: 'park synthetic read' }, context);
    for (let i = 0; i < 200 && h.wake().at !== wake; i++) await delay(10);
    assert.equal(h.wake().at, wake, 'park transition was not observed');
  } finally { await h.close(context); }
  assert.equal(calls, 1);
  return { source, runtime, out, snapshot: join(out, 'snapshot.sqlite'), wake };
}

const sleeping = db => db.prepare("SELECT record FROM tasks WHERE kind=json_quote('pi.tool') AND status='sleeping'").get();

test('store validation preserves a due parked tool without starting a Harness', async t => {
  const f = await parkedRead(t);
  const before = await readFile(f.source);
  assert.ok(f.wake < Date.now());
  const digest = await makeSnapshot(f.runtime, f.out, f.snapshot);
  await verifySnapshot(f.snapshot, digest);
  assert.deepEqual(await readFile(f.source), before);
  const src = new DatabaseSync(f.source, { readOnly: true }), dst = new DatabaseSync(f.snapshot, { readOnly: true });
  try {
    assert.ok(sleeping(src));
    assert.deepEqual(sleeping(dst), sleeping(src));
    assert.equal(JSON.parse(sleeping(dst).record).state.until, f.wake);
  } finally { dst.close(); src.close(); }
});

test('malformed wake or unrecognized task cannot produce a usable cutover snapshot', async t => {
  const f = await parkedRead(t);
  const edit = sql => {
    const db = new DatabaseSync(f.source);
    try { db.exec(sql); }
    finally { db.close(); }
  };
  edit(`UPDATE tasks SET record=json_set(record, '$.state.signals', json('[{"key":"fixture","signal":"unknown"}]')) WHERE status='sleeping'`);
  await assert.rejects(makeSnapshot(f.runtime, f.out, f.snapshot), /unsupported sleep\/wake/i);
  await assert.rejects(lstat(f.snapshot), { code: 'ENOENT' });
  edit(`UPDATE tasks SET kind=json_quote('fixture.unknown'),record=json_set(record,'$.kind','fixture.unknown') WHERE status='sleeping'`);
  await assert.rejects(makeSnapshot(f.runtime, f.out, f.snapshot), /unsupported task/i);
  await assert.rejects(lstat(f.snapshot), { code: 'ENOENT' });
});
