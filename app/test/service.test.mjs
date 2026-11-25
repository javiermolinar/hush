import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import { mkdir, mkdtemp, readFile, readdir, realpath, rename, rm, stat, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { createConnection } from 'node:net';
import { setTimeout as delay } from 'node:timers/promises';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

const binary = resolve(process.env.HUSH_TEST_CLI ?? fileURLToPath(new URL('../../bin/hush', import.meta.url)));
const bundle = resolve(process.env.HUSH_TEST_BACKEND ?? fileURLToPath(new URL('../../dist/hush-backend.mjs', import.meta.url)));
const invoke = (exe, args, env, timeout = 20_000) => new Promise(resolve => {
  execFile(exe, args, { env, timeout }, (err, stdout, stderr) => resolve({ code: err?.code ?? (err ? -1 : 0), stdout, stderr }));
});
const ok = result => { assert.equal(result.code, 0, JSON.stringify(result)); return result.stdout; };
const json = result => JSON.parse(ok(result));

async function profile(t, native = false) {
  // systemd --user needs the dedicated test login's real HOME for enablement.
  // Never run the Linux native test in a profile that already has Hush data.
  const realHome = native && process.platform === 'linux';
  const home = await realpath(realHome ? process.env.HOME : await mkdtemp('/tmp/hush-svc-'));
  const agent = join(home, '.hush'), socket = join(agent, 'runtime', 'hush.sock');
  if (realHome) await assert.rejects(stat(agent), { code: 'ENOENT' });
  const env = { HOME: home, PATH: dirname(process.execPath) + ':/usr/bin:/bin', LANG: 'C' };
  const cli = (...args) => invoke(binary, args, env, 50_000);
  t.after(async () => {
    if (native && await stat(join(agent, 'supervisor-v1.json')).catch(() => null)) {
      ok(await cli('service', 'uninstall'));
    }
    if (!realHome) await rm(home, { recursive: true, force: true });
  });
  const archive = async label => { if (realHome) await rename(agent, join(await mkdtemp(join(home, `.hush-service-${label}-`)), 'profile')); };
  return { home, agent, socket, cli, env, archive, node: realpath(process.execPath) };
}

async function eventually(check, limit = 6000) {
  const until = Date.now() + limit;
  let failure;
  do {
    try { return await check(); }
    catch (error) { failure = error; }
    await delay(50);
  } while (Date.now() < until);
  throw failure;
}

async function fixtureBundle(home) {
  const entry = join(home, 'fixture bundle.mjs');
  // Use the same backend with the test-only parked Durable tool. This does
  // not use product scheduling, a provider API, or real credentials.
  const source = await readFile(bundle, 'utf8');
  await writeFile(entry, `if (process.argv.includes('--supervisor')) process.argv.push('--faux', '--idle-ms', '50');\n${source}`, { mode: 0o600 });
  return entry;
}

async function exerciseDeadline(f) {
  ok(await f.cli('trust', 'grant', '--scope', 'work', '--cwd', f.home));
  const { id } = json(await f.cli('thread', 'new', '--cwd', f.home, '--json'));
  const receipt = json(await f.cli('--no-wait', '--thread', id, '--json', '!sleep 1600'));
  assert.ok(receipt.submissionId);
  // No client is attached when the parked tool's deadline becomes due.
  await delay(1900);
  assert.equal(json(await f.cli('submission', 'get', '--thread', id, '--submission-id', receipt.submissionId, '--json')).status, 'done');
}

test('supervisor owns managed start, ignores idle, and wakes a parked Durable tool without a client', { timeout: 25_000 }, async t => {
  const f = await profile(t), token = randomUUID(), entry = await fixtureBundle(f.home);
  const node = await f.node, argv = ['--hush-internal-service', token, node, entry, binary];
  ok(await invoke(node, [entry, '--hush-internal-service-claim', token], f.env));
  const manual = await invoke(node, [entry, '--profile', 'hush'], f.env);
  assert.equal(manual.code, 1);
  assert.match(manual.stderr, /login supervisor owns/);
  assert.equal((await f.cli('status', '--socket', join(f.home, 'other.sock'))).code, 1);
  await assert.rejects(stat(join(f.agent, 'runtime')), { code: 'ENOENT' });
  const proc = spawn(binary, argv, { env: f.env, stdio: 'ignore' });
  const close = new Promise(resolve => proc.once('close', (code, signal) => resolve({ code, signal })));
  t.after(async () => { if (proc.exitCode === null) proc.kill('SIGTERM'); await close; });
  const connected = await Promise.all(Array.from({ length: 6 }, () => f.cli('status', '--json')));
  const epoch = json(connected[0]).epoch;
  for (const result of connected) assert.equal(json(result).epoch, epoch);
  const restart = await f.cli('restart');
  assert.equal(restart.code, 1);
  assert.match(restart.stderr, /service owns this backend.*hush service restart/);
  assert.equal(json(await f.cli('status', '--json')).epoch, epoch);
  await exerciseDeadline(f);
  await delay(250);
  assert.equal(json(await f.cli('status', '--json')).epoch, epoch);
  assert.match(ok(await f.cli('service', 'status')), /active, .* need attention/);
  proc.kill('SIGTERM');
  assert.deepEqual(await close, { code: 0, signal: null });
  await writeFile(join(f.agent, 'supervisor-hold-v1'), 'paused\n', { mode: 0o600 });
  ok(await invoke(binary, argv, f.env));
  assert.match((await f.cli('status')).stderr, /service is paused/);
  assert.match((await f.cli('restart')).stderr, /service owns this backend/);
  assert.match((await f.cli('status')).stderr, /service is paused/);
  await rm(join(f.agent, 'supervisor-hold-v1'));
  // An unknown persistent cutover record must not be opened by restart.
  await writeFile(join(f.agent, 'store-cutover', 'state.json'), '{}', { mode: 0o600 });
  ok(await invoke(binary, argv, f.env));
  assert.match(await readFile(join(f.agent, 'supervisor.log'), 'utf8'), /store is fenced/);
  assert.equal((await f.cli('status', '--socket', f.socket)).code, 1);
});

test('native user supervisor bootstrap, reattach, restart, pause, resume and uninstall on unchanged format 1', {
  skip: !process.env.HUSH_TEST_LOGIN_SUPERVISOR && 'opt in on a disposable login with HUSH_TEST_LOGIN_SUPERVISOR=1',
  timeout: 60_000,
}, async t => {
  const f = await profile(t, true), entry = await fixtureBundle(f.home);
  const seed = spawn(process.execPath, [entry, '--profile', 'hush', '--faux', '--idle-ms', '15000'], { env: f.env, stdio: 'ignore' });
  const stopped = new Promise(resolve => seed.once('close', resolve));
  t.after(async () => { if (seed.exitCode === null && seed.signalCode === null) seed.kill('SIGTERM'); await stopped; });
  const ready = () => f.cli('status', '--socket', f.socket, '--json').then(json);
  await eventually(ready);
  // Install must decline a living writer, not stop or replace it.
  const concurrent = await f.cli('service', 'install', '--backend', entry);
  assert.notEqual(concurrent.code, 0);
  await ready();
  ok(await f.cli('stop'));
  assert.equal(await stopped, 0);
  const marker = join(f.agent, 'runtime', 'store-profile');
  const { format, registry, cutover, ...identity } = JSON.parse(await readFile(marker, 'utf8'));
  assert.equal(format, 2);
  const legacy = JSON.stringify({ ...identity, format: 1 }) + '\n';
  await writeFile(marker, legacy);
  await writeFile(join(f.agent, 'settings.json'), '{"allowLegacyStore":true}\n', { mode: 0o600 });
  ok(await f.cli('service', 'install', '--backend', entry));
  const connected = await Promise.all(Array.from({ length: 6 }, () => f.cli('status', '--json')));
  const first = json(connected[0]);
  for (const result of connected) assert.equal(json(result).epoch, first.epoch);
  await exerciseDeadline(f);
  ok(await f.cli('service', 'restart'));
  const next = json(await f.cli('status', '--json'));
  assert.notEqual(next.epoch, first.epoch);
  ok(await f.cli('service', 'pause'));
  assert.match(ok(await f.cli('service', 'status')), /paused/);
  assert.match((await f.cli('status')).stderr, /service is paused/);
  assert.equal((await f.cli('status', '--socket', f.socket)).code, 1);
  ok(await f.cli('service', 'resume'));
  assert.notEqual(json(await f.cli('status', '--json')).epoch, next.epoch);
  ok(await f.cli('service', 'uninstall'));
  assert.match(ok(await f.cli('service', 'status')), /not installed/);
  await stat(join(f.agent, 'runtime', 'history.sqlite'));
  assert.equal(await readFile(marker, 'utf8'), legacy);
  assert.equal((await f.cli('status', '--socket', f.socket)).code, 1);
  await f.archive('format1-tested');
});

test('native service preserves a pinned cutover hold and restarts only after activation', {
  skip: !process.env.HUSH_TEST_LOGIN_SUPERVISOR || !process.env.HUSH_CUTOVER_PREDECESSOR || process.platform !== 'linux' || process.arch !== 'arm64' || process.versions.node !== '22.19.0',
  timeout: 65_000,
}, async t => {
  const f = await profile(t, true), old = process.env.HUSH_CUTOVER_PREDECESSOR;
  const expectedDigest = 'eec512d5bb9755f97f927ffcc2d8da77d59b834e1c5d3d32255ca82a1f17c21f';
  assert.equal(createHash('sha256').update(await readFile(old)).digest('hex'), expectedDigest);
  // The pinned format-1 CLI predates runtime stop. Capture its single test
  // child's PID through a scratch preload, as in the cutover fault matrix.
  const preload = join(f.home, 'record-predecessor.mjs'), pidFile = join(f.home, 'predecessor.pid');
  await writeFile(preload, `import {writeFileSync} from 'node:fs';\nif (process.argv.includes('--startup-fd')) writeFileSync(${JSON.stringify(pidFile)}, String(process.pid), {mode: 0o600});\n`);
  const predecessor = (...args) => invoke(old, args, { ...f.env, NODE_OPTIONS: `--import=${preload}` });
  assert.equal(json(await predecessor('status', '--json')).version, 1);
  const pid = Number(await readFile(pidFile, 'utf8'));
  assert.ok(Number.isSafeInteger(pid) && pid > 1);
  process.kill(pid, 'SIGTERM');
  await eventually(async () => {
    const socket = createConnection(f.socket);
    try {
      assert.match(await new Promise(resolve => {
        socket.once('connect', () => resolve('connected'));
        socket.once('error', e => resolve(e.code));
      }), /^E(?:CONNREFUSED|NOENT)$/);
    } finally { socket.destroy(); }
  });
  await writeFile(join(f.agent, 'settings.json'), '{"allowLegacyStore":true}\n', { mode: 0o600 });
  ok(await f.cli('service', 'install', '--backend', bundle));
  const oldEpoch = json(await f.cli('status', '--json')).epoch;
  const store = (...args) => f.cli('store', ...args, '--backend', bundle, '--json');
  const activate = await store('activate');
  assert.notEqual(activate.code, 0);
  ok(await f.cli('service', 'pause'));
  assert.equal(json(await store('prepare', '--from', old)).phase, 'prepared');
  assert.match((await f.cli('status')).stderr, /service is paused/);
  assert.notEqual((await predecessor('status')).code, 0);
  // Even a manual supervisor retry cannot bypass the hold or create a listener.
  const name = (await readdir(f.agent)).find(p => p.endsWith('.service'));
  const runtime = `/run/user/${process.getuid()}`;
  ok(await invoke('/usr/bin/systemctl', ['--user', 'start', name], { ...f.env,
    XDG_RUNTIME_DIR: runtime, DBUS_SESSION_BUS_ADDRESS: `unix:path=${runtime}/bus` }));
  assert.notEqual((await f.cli('status', '--socket', f.socket)).code, 0);
  assert.equal(json(await store('activate')).phase, 'committed');
  ok(await f.cli('service', 'resume'));
  assert.notEqual(json(await f.cli('status', '--json')).epoch, oldEpoch);
  assert.equal(json(await store('inspect')).format, 2);
  ok(await f.cli('service', 'uninstall'));
  await f.archive('cutover-tested');
});

test('unavailable service keyring never prompts, falls back, or restarts', { timeout: 20_000 }, async t => {
  const f = await profile(t), token = randomUUID(), helper = join(f.home, 'locked-keyring');
  await mkdir(f.agent, { mode: 0o700 });
  await writeFile(join(f.agent, 'credential-store'), 'keyring\n', { mode: 0o600 });
  await writeFile(helper, '#!/bin/sh\nexit 3\n', { mode: 0o700 });
  const node = await f.node;
  ok(await invoke(node, [bundle, '--hush-internal-service-claim', token], f.env));
  const proc = spawn(binary, ['--hush-internal-service', token, node, bundle, helper], { env: f.env, stdio: 'ignore' });
  const close = new Promise(resolve => proc.once('close', (code, signal) => resolve({ code, signal })));
  t.after(async () => { if (proc.exitCode === null) proc.kill('SIGTERM'); await close; });
  const first = json(await f.cli('status', '--json'));
  await eventually(async () => assert.match(await readFile(join(f.agent, 'supervisor.log'), 'utf8'), /keyring is locked or unavailable/i));
  assert.equal(json(await f.cli('status', '--json')).epoch, first.epoch);
  await assert.rejects(stat(join(f.agent, 'auth.json')), { code: 'ENOENT' });
  proc.kill('SIGTERM');
  assert.deepEqual(await close, { code: 0, signal: null });
});
