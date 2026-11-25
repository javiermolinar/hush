import assert from 'node:assert/strict';
import { access, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { test } from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { FileListWorker } from '../../dist/backend/src/file-list-worker.js';

async function fixture(t, script) {
  const dir = await mkdtemp('/tmp/hush-file-worker-');
  const helper = join(dir, 'helper.mjs'), started = join(dir, 'started'), launches = join(dir, 'launches');
  const worker = new FileListWorker(helper);
  t.after(async () => { await worker.close(); await rm(dir, { recursive: true, force: true }); });
  await writeFile(helper, `#!${process.execPath}
import { appendFileSync, readFileSync, writeFileSync } from 'node:fs';
import { createInterface } from 'node:readline';
appendFileSync(${JSON.stringify(launches)}, process.pid + '\\n');
const started = ${JSON.stringify(started)};
const reply = (id, value) => console.log(JSON.stringify({ id, ...value }));
for await (const line of createInterface({ input: process.stdin })) {
  const msg = JSON.parse(line);
  ${script}
}
`, { mode: 0o700 });
  return { worker, async waitStarted() {
    const deadline = AbortSignal.timeout(2000);
    while (!await access(started).then(() => true, () => false)) await delay(5, undefined, { signal: deadline });
  }, pids: async () => (await readFile(launches, 'utf8')).trim().split('\n').map(Number) };
}

test('an abandoned search sends its ID, drains the cancel reply and keeps the helper warm', { timeout: 8000 }, async t => {
  const { worker, waitStarted, pids } = await fixture(t, `
    if (msg.query === 'cold') { writeFileSync(started, '' + msg.id); continue; }
    if (msg.cancel) {
      if (msg.id !== +readFileSync(started, 'utf8')) process.exit(2);
      reply(msg.id, { error: 'cancelled' });
      continue;
    }
    if (msg.query === 'skipped') process.exit(2);
    reply(msg.id, { pid: process.pid });
  `);
  const running = new AbortController(), queued = new AbortController();
  const abandoned = assert.rejects(worker.request({ query: 'cold' }, running.signal), { name: 'AbortError' });
  await waitStarted();
  const skipped = assert.rejects(worker.request({ query: 'skipped' }, queued.signal), { name: 'AbortError' });
  const next = worker.request({ query: 'next' }, AbortSignal.timeout(2500));
  queued.abort(); running.abort();
  await Promise.all([abandoned, skipped]);
  const { pid } = await next;
  assert.deepEqual(await pids(), [pid]);
});

test('a helper that does not acknowledge cancellation is replaced', { timeout: 8000 }, async t => {
  const { worker, waitStarted, pids } = await fixture(t, `
    if (msg.directory === 'slow') { writeFileSync(started, ''); continue; }
    if (!msg.cancel) reply(msg.id, { pid: process.pid });
  `);
  const running = new AbortController();
  const abandoned = assert.rejects(worker.request({ directory: 'slow' }, running.signal), { name: 'AbortError' });
  await waitStarted();
  const next = worker.request({ directory: '' }, AbortSignal.timeout(2500));
  running.abort();
  await abandoned;
  const reply = await next;
  const launched = await pids();
  assert.equal(launched.length, 2);
  assert.notEqual(launched[0], reply.pid);
  assert.equal(launched[1], reply.pid);
});

test('a mismatched reply ID cannot complete the active request', { timeout: 8000 }, async t => {
  const { worker } = await fixture(t, `
    reply(msg.wrong ? msg.id - 1 : msg.id, { page: true });
  `);
  await assert.rejects(worker.request({ wrong: true }, AbortSignal.timeout(2500)), { code: 'not_found' });
  assert.deepEqual(await worker.request({}, AbortSignal.timeout(2500)), { page: true });
});
