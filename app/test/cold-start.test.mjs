// Managed backend cold start: time from spawn to a compatible hello, the
// moment a TUI or CLI can attach. Each sample starts a new Node process on an
// existing store, as a restart does, with the arguments and helper
// environment backendstart uses; the first start also creates the store and is
// reported on its own. There is no target yet: cold-start sets its absolute
// target from this baseline. Run directly for more samples:
//   HUSH_BENCH_SAMPLES=50 node app/test/cold-start.test.mjs
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtemp, realpath, rm } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { createInterface } from 'node:readline';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { dial } from '../../server/test/client.mjs';
import { samples, summary } from '../../server/test/perf.mjs';

const bundle = resolve(process.env.HUSH_TEST_BACKEND ?? fileURLToPath(new URL('../../dist/hush-backend.mjs', import.meta.url)));
const exe = resolve(process.env.HUSH_TEST_CLI ?? fileURLToPath(new URL('../../bin/hush', import.meta.url)));
const count = samples(10, 5);

// The managed launch reports readiness on a FIFO (--startup-fd); this reads
// the same record from stdout, since Node's spawn pipes are socket pairs.
async function start(home) {
  const runtime = join(home, '.hush', 'runtime'), socket = join(runtime, 'hush.sock');
  const env = { HOME: home, LANG: 'C', PATH: dirname(process.execPath) + ':/usr/bin:/bin', NODE_NO_WARNINGS: '1',
    HUSH_KEYRING_HELPER: exe, HUSH_FILE_LIST_HELPER: exe };
  const begin = performance.now();
  const proc = spawn(process.execPath, [bundle, '--data-dir', runtime, '--profile', 'hush'], { env, stdio: ['ignore', 'pipe', 'inherit'] });
  const closed = once(proc, 'close');
  try {
    const lines = createInterface({ input: proc.stdout })[Symbol.asyncIterator]();
    const { value: line } = await lines.next();
    const ready = performance.now() - begin;
    assert.equal(JSON.parse(line).ready, true, line);
    const c = await dial(socket);
    try {
      const reply = await c.send('hello', { versions: [1], capabilities: { hushProfile: [1], storeIdentity: [1] } });
      const hello = performance.now() - begin;
      assert.equal(reply.ok, true, JSON.stringify(reply));
      assert.equal(reply.result.version, 1);
      assert.equal(reply.result.capabilities.hushProfile, 1);
      assert.equal(reply.result.capabilities.storeIdentity, 1);
      return { ready, hello };
    } finally { c.close(); }
  } finally {
    proc.kill('SIGTERM');
    await closed;
  }
}

test('managed backend spawn to compatible hello', { timeout: 120_000 }, async t => {
  const home = await realpath(await mkdtemp('/tmp/hush-cold-'));
  try {
    const first = await start(home);
    const runs = [];
    for (let n = 0; n < count; n++) runs.push(await start(home));
    const result = {
      node: process.version, platform: process.platform, arch: process.arch,
      first_start_ms: { ready: Number(first.ready.toFixed(1)), hello: Number(first.hello.toFixed(1)) },
      restart_to_ready: summary(runs.map(r => r.ready)),
      restart_to_hello: summary(runs.map(r => r.hello)),
    };
    t.diagnostic(JSON.stringify(result));
  } finally {
    await rm(home, { recursive: true, force: true });
  }
});
