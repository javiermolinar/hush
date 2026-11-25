import assert from 'node:assert/strict';
import { access, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { test } from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { FileListWorker } from '../../dist/backend/src/file-list-worker.js';

test('an abandoned listing releases the private worker for the next request', { timeout: 8000 }, async t => {
  const dir = await mkdtemp('/tmp/hush-file-worker-');
  const helper = join(dir, 'helper'), started = join(dir, 'started');
  const worker = new FileListWorker(helper);
  t.after(async () => { await worker.close(); await rm(dir, { recursive: true, force: true }); });
  await writeFile(helper, `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *slow*) : > '${started}'; IFS= read -r unused ;;
    *) printf '%s\\n' '{"page":true}' ;;
  esac
done
`, { mode: 0o700 });
  const signal = new AbortController();
  const abandoned = assert.rejects(worker.request({ directory: 'slow' }, signal.signal));
  const deadline = AbortSignal.timeout(2000);
  while (!await access(started).then(() => true, () => false)) await delay(10, undefined, { signal: deadline });
  const next = worker.request({ directory: '' }, AbortSignal.timeout(2000));
  signal.abort();
  await abandoned;
  assert.deepEqual(await next, { page: true });
});

test('abandoned searches release their callers and reuse the running helper', { timeout: 8000 }, async t => {
  const dir = await mkdtemp('/tmp/hush-file-worker-');
  const helper = join(dir, 'helper'), started = join(dir, 'started'), release = join(dir, 'release');
  const worker = new FileListWorker(helper);
  t.after(async () => { await worker.close(); await rm(dir, { recursive: true, force: true }); });
  await writeFile(helper, `#!/bin/sh
warm=false
while IFS= read -r line; do
  case "$line" in
    *cold*)
      : > '${started}'
      while [ ! -f '${release}' ]; do /bin/sleep 0.01; done
      warm=true ;;
    *skipped*) warm=false ;;
  esac
  printf '{"warm":%s}\\n' "$warm"
done
`, { mode: 0o700 });
  const running = new AbortController(), queued = new AbortController();
  const abandoned = assert.rejects(worker.request({ query: 'cold' }, running.signal, true), { name: 'AbortError' });
  const deadline = AbortSignal.timeout(2000);
  while (!await access(started).then(() => true, () => false)) await delay(10, undefined, { signal: deadline });
  const skipped = assert.rejects(worker.request({ query: 'skipped' }, queued.signal, true), { name: 'AbortError' });
  const next = worker.request({ query: 'next' }, AbortSignal.timeout(2000), true);
  running.abort(); queued.abort();
  await Promise.all([abandoned, skipped]);
  await writeFile(release, '');
  assert.deepEqual(await next, { warm: true });
});
