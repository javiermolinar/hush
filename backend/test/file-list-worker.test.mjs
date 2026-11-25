import assert from 'node:assert/strict';
import { access, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { test } from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { FileListWorker } from '../../dist/backend/src/file-list-worker.js';

test('an abandoned search releases the private worker for the next request', { timeout: 8000 }, async t => {
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
  const abandoned = assert.rejects(worker.request({ query: 'slow' }, signal.signal));
  const deadline = AbortSignal.timeout(2000);
  while (!await access(started).then(() => true, () => false)) await delay(10, undefined, { signal: deadline });
  const next = worker.request({ directory: '' }, AbortSignal.timeout(2000));
  signal.abort();
  await abandoned;
  assert.deepEqual(await next, { page: true });
});
