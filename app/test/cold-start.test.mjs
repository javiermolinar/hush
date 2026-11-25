// Measure the real Go launcher. A release run uses the installed binary and
// includes hashing, extracting both embedded files, starting Node, and hello.
// The development build uses its adjacent bundle and reports that separately.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtemp, realpath, rm } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { dial } from '../../server/test/client.mjs';
import { checkTarget, samples, summary } from '../../server/test/perf.mjs';

const install = process.env.HUSH_TEST_CLI;
const exe = resolve(install ?? fileURLToPath(new URL('../../bin/hush', import.meta.url)));
const count = samples(10, 5);

test('managed launch to compatible hello', { timeout: 120_000 }, async t => {
  const home = await realpath(await mkdtemp('/tmp/hush-cold-'));
  const socket = join(home, '.hush', 'runtime', 'hush.sock');
  // Do not select HUSH_TEST_BACKEND here: the release must exercise its embed.
  const env = { HOME: home, LANG: 'C', PATH: dirname(process.execPath) + ':/usr/bin:/bin', NODE_NO_WARNINGS: '1' };
  const invoke = args => {
    const result = spawnSync(exe, args, { env, cwd: home, encoding: 'utf8', timeout: 12_000 });
    assert.equal(result.status, 0, result.error?.message ?? result.stderr);
    return result.stdout;
  };
  const start = () => {
    const begin = performance.now();
    invoke(['restart']); // With no listener, returns only after a compatible hello.
    return performance.now() - begin;
  };
  const stop = () => invoke(['stop']);
  try {
    const first = start();
    const client = await dial(socket);
    try {
      const reply = await client.send('hello', { versions: [1], capabilities: { hushProfile: [1], storeIdentity: [1] } });
      assert.equal(reply.ok, true, JSON.stringify(reply));
      assert.equal(reply.result.version, 1);
      assert.deepEqual(reply.result.capabilities, { hushProfile: 1, storeIdentity: 1 });
    } finally { client.close(); }
    const runs = [];
    for (let n = 0; n < count; n++) {
      stop(); // Keep shutdown of the previous process outside the measurement.
      runs.push(start());
    }
    const result = {
      node: process.version, platform: process.platform, arch: process.arch,
      binary: install ? 'installed' : 'development',
      first_start_ms: Number(first.toFixed(1)),
      managed_start_to_hello: summary(runs),
    };
    t.diagnostic(JSON.stringify(result));
    checkTarget(t, { name: 'managed start to hello p50', value: result.managed_start_to_hello.p50_ms, limit: 315 });
    checkTarget(t, { name: 'managed start to hello p95', value: result.managed_start_to_hello.p95_ms, limit: 333 });
  } finally {
    try { stop(); }
    finally { await rm(home, { recursive: true, force: true }); }
  }
});
