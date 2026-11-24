// App-level fault checks for the interlock and gate. The release fault matrix
// also needs fixture task execution, client continuity and CI source evidence.
import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { once } from 'node:events';
import { lstat, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createConnection } from 'node:net';
import { dirname, join, resolve } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { test } from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

const sha = bytes => createHash('sha256').update(bytes).digest('hex');
const pinned = 'eec512d5bb9755f97f927ffcc2d8da77d59b834e1c5d3d32255ca82a1f17c21f';
const source = process.env.HUSH_CUTOVER_PREDECESSOR;
const target = process.env.HUSH_CUTOVER_TARGET;
const bundle = process.env.HUSH_TEST_BACKEND ?? fileURLToPath(new URL('../../dist/hush-backend.mjs', import.meta.url));

const preload = String.raw`
import fileOps from 'node:fs/promises';
import { appendFileSync } from 'node:fs';
import { basename } from 'node:path';
import { syncBuiltinESMExports } from 'node:module';
const { HUSH_FIXTURE_CRASH, HUSH_FIXTURE_RECORD, HUSH_FIXTURE_LAUNCHES } = process.env;
if (process.argv.includes('--startup-fd')) appendFileSync(HUSH_FIXTURE_LAUNCHES, process.pid + '\n');
const rename = fileOps.rename;
fileOps.rename = async (from, to) => {
  const name = basename(to);
  const crash = point => {
    if (HUSH_FIXTURE_CRASH === point + ':' + name) {
      appendFileSync(HUSH_FIXTURE_RECORD, point + ':' + name + '\n');
      process.kill(process.pid, 'SIGKILL');
    }
  };
  crash('before');
  await rename(from, to);
  crash('after');
};
syncBuiltinESMExports();
`;

const run = (exe, args, cfg) => new Promise((resolve, reject) => {
  execFile(exe, args, { ...cfg, timeout: 21_000 }, (err, stdout, stderr) => {
    if (err && typeof err.code !== 'number') reject(Object.assign(err, { stderr }));
    else resolve({ code: err?.code ?? 0, stdout, stderr });
  });
});
function value(result) {
  assert.equal(result.code, 0, result.stderr);
  return JSON.parse(result.stdout);
}
function refused(result, pattern) {
  assert.equal(result.code, 1, JSON.stringify(result));
  assert.equal(result.stdout, '');
  assert.match(result.stderr, pattern);
}
async function accepts(path) {
  const conn = createConnection(path);
  try { await once(conn, 'connect'); return true; }
  catch (err) { if (err.code === 'ENOENT' || err.code === 'ECONNREFUSED') return false; throw err; }
  finally { conn.destroy(); }
}
async function waitClosed(socket) {
  for (let i = 0; i < 120; i++) {
    if (!await accepts(socket)) return;
    await delay(25);
  }
  assert.fail('old listener is still reachable');
}
function dbContains(path, marker) {
  const db = new DatabaseSync(path, { readOnly: true });
  try {
    assert.equal(db.prepare('PRAGMA integrity_check').get().integrity_check, 'ok');
    assert.ok(db.prepare("SELECT 1 FROM document_revisions WHERE json_extract(content,'$.title')=?").get(marker));
  } finally { db.close(); }
}

async function fixture(t) {
  const root = await mkdtemp('/tmp/hush-fence-');
  const agent = join(root, '.hush'), runtime = join(agent, 'runtime'), control = join(agent, 'store-cutover');
  const history = join(runtime, 'history.sqlite'), marker = join(runtime, 'store-profile'), state = join(control, 'state.json');
  const socket = join(runtime, 'hush.sock'), lock = join(runtime, 'owner.sqlite'), crashLog = join(root, 'crash');
  const script = join(root, 'preload.mjs'), launches = join(root, 'launches'), cwd = join(root, 'workspace');
  await mkdir(cwd);
  await writeFile(script, preload, { mode: 0o600 });
  const env = { HOME: root, PATH: dirname(process.execPath) + ':/usr/bin:/bin', LANG: 'C', NODE_NO_WARNINGS: '1',
    NODE_OPTIONS: `--import=${script}`, HUSH_FIXTURE_LAUNCHES: launches, HUSH_FIXTURE_RECORD: crashLog };
  const old = (...args) => run(resolve(source), args, { cwd, env });
  const next = (...args) => run(resolve(target), args, { cwd, env }); // target uses the same bytes, embedded
  const read = async path => JSON.parse(await readFile(path, 'utf8'));
  const pids = async () => (await readFile(launches, 'utf8').catch(err => err.code === 'ENOENT' ? '' : Promise.reject(err)))
    .trim().split('\n').filter(Boolean).map(Number);
  const stop = async signal => {
    process.kill((await pids()).at(-1), signal);
    await waitClosed(socket);
  };
  t.after(async () => {
    if (await accepts(socket)) await stop('SIGTERM');
    await rm(root, { recursive: true, force: true });
  });
  const invoke = action => action === 'prepare' ? ['store', action, '--from', resolve(source)] : ['store', action];
  const admin = async action => value(await next(...invoke(action)));
  const crash = async (action, point) => {
    const child = spawn(process.execPath, [resolve(bundle), ...invoke(action)], { cwd, env: { ...env, HUSH_FIXTURE_CRASH: point }, stdio: ['ignore', 'pipe', 'pipe'] });
    let failure = '';
    child.stderr.on('data', chunk => { failure += chunk; });
    const kill = setTimeout(() => child.kill('SIGKILL'), 10_000);
    try { assert.deepEqual(await once(child, 'close'), [null, 'SIGKILL'], failure); }
    finally { clearTimeout(kill); }
    assert.equal((await readFile(crashLog, 'utf8')).trim().split('\n').at(-1), point);
  };
  return { old, next, admin, crash, read, stop, history, marker, state, socket, runtime, control, lock };
}

test('app cutover fences the pinned predecessor at operator crash boundaries', {
  skip: !source || !target ? 'provide HUSH_CUTOVER_PREDECESSOR and HUSH_CUTOVER_TARGET for the pinned Linux arm64 test' : false,
  timeout: 190_000,
}, async t => {
  assert.equal(process.versions.node, '22.19.0');
  assert.equal(`${process.platform}/${process.arch}`, 'linux/arm64');
  assert.equal(sha(await readFile(source)), pinned);
  const cases = [
    { name: 'before legacy seal', action: 'prepare', boundary: 'before:store-profile', phase: 'not-prepared' },
    { name: 'legacy seal, no outside record', action: 'prepare', boundary: 'after:store-profile', phase: 'unknown' },
    { name: 'outside prepared record', action: 'prepare', boundary: 'after:state.json', phase: 'prepared' },
    { name: 'database replaced but binding not published', action: 'activate', boundary: 'after:history.sqlite', phase: 'prepared' },
    { name: 'binding replaced before the gate', action: 'activate', boundary: 'after:history.identity', phase: 'prepared' },
    { name: 'committed gate, no target manifest', action: 'activate', boundary: 'after:state.json', phase: 'committed' },
    { name: 'target manifest published', action: 'activate', boundary: 'after:store-profile', phase: 'committed' },
    { name: 'restored gate, no legacy manifest', action: 'rollback', boundary: 'after:state.json', phase: 'restored' },
  ];
  for (const [i, check] of cases.entries()) await t.test(check.name, async t => {
    const f = await fixture(t), title = `committed WAL fixture ${i}`;
    const a = value(await f.old('status', '--json'));
    const thread = value(await f.old('thread', 'new', '--title', title, '--json'));
    const marker = await f.read(f.marker), lock = (await lstat(f.lock)).ino;
    assert.equal(marker.format, 1);
    assert.ok(!(await readFile(f.history)).includes(title), 'fixture requires a commit not yet in the main file');
    assert.ok((await readFile(f.history + '-wal')).includes(title));
    refused(await f.next('store', 'prepare', '--from', resolve(source)), /not quiescent/);
    await f.stop('SIGKILL'); // no clean-close checkpoint of the WAL commit

    if (check.action !== 'prepare') assert.equal((await f.admin('prepare')).phase, 'prepared');
    await f.crash(check.action, check.boundary);
    assert.equal((await lstat(f.lock)).ino, lock);
    assert.equal(await accepts(f.socket), false);
    const inspection = await f.admin('inspect');
    assert.equal(inspection.phase, check.phase);
    assert.equal(inspection.rollbackEligible, check.phase === 'prepared');
    if (inspection.transaction) dbContains(join(f.control, inspection.transaction, 'snapshot.sqlite'), title);
    if (check.phase === 'not-prepared') {
      refused(await f.next('status'), /store_profile/);
      assert.notEqual(value(await f.old('status', '--json')).epoch, a.epoch);
      assert.ok(value(await f.old('thread', 'list', '--json')).threads.some(row => row.id === thread.id && row.title === title));
      assert.deepEqual(await f.read(f.marker), marker);
      return;
    }
    // An old managed command always uses its own embedded bundle. A current
    // launch races it on the real name without replacing the owner inode.
    const [legacy, current] = await Promise.all([f.old('status', '--json'), f.next('status', '--json')]);
    if (check.name === 'target manifest published') {
      // After the gate an old *client* may attach to the new writer. It must
      // never open the store itself, and only one new listener can appear.
      const after = value(current);
      assert.notEqual(after.epoch, a.epoch);
      if (legacy.code === 0) assert.equal(value(legacy).epoch, after.epoch);
      else refused(legacy, /store_profile|wait for persistent backend/);
      assert.equal((await readFile(join(f.runtime, 'hush.log'), 'utf8')).split('\n').filter(line => line.endsWith(' listening')).length, 2);
      assert.equal(value(await f.next('stop', '--json')).stopped, true);
    } else {
      refused(legacy, /store_profile|wait for persistent backend/);
      refused(current, /store_profile|store_cutover|wait for persistent backend/);
      assert.equal(await accepts(f.socket), false);
    }
    if (check.phase === 'unknown') {
      refused(await f.next('store', 'rollback'), /unknown or past/);
      refused(await f.old('status'), /store_profile/);
      return;
    }
    if (check.phase === 'prepared' || check.phase === 'restored') {
      const status = await f.admin('rollback');
      assert.equal(status.phase, 'rolled-back');
      assert.equal(status.target.storeId, marker.storeId);
      assert.notEqual(status.target.generation, marker.generation);
      assert.ok(value(await f.old('thread', 'list', '--json')).threads.some(row => row.id === thread.id && row.title === title));
      assert.equal((await f.read(f.marker)).generation, status.target.generation);
      return;
    }
    refused(await f.next('store', 'rollback'), /unknown or past/);
    await f.admin('activate'); // finish only a committed, unpublished marker
    assert.ok(value(await f.next('thread', 'list', '--json')).threads.some(row => row.id === thread.id && row.title === title));
    assert.equal((await f.read(f.marker)).format, 2);
    assert.equal(value(await f.next('stop', '--json')).stopped, true);
    refused(await f.old('status'), /store_profile/);
    // Missing or unreadable gate is not permission to start or restore.
    await writeFile(f.state, '{unreadable', { mode: 0o600 });
    assert.equal((await f.admin('inspect')).phase, 'unknown');
    refused(await f.next('status'), /store_cutover/);
    refused(await f.next('store', 'rollback'), /refused|unknown/);
    await rm(f.state);
    assert.equal((await f.admin('inspect')).phase, 'unknown');
    refused(await f.next('status'), /store_cutover/);
    refused(await f.next('store', 'rollback'), /unknown or past/);
    assert.equal((await lstat(f.lock)).ino, lock);
  });
});
