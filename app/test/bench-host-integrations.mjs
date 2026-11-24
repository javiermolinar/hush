// Offline, opt-in latency benchmark against the built Hush CLI and real Unix
// socket. No login service, provider, or existing profile is touched.
import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { mkdir, mkdtemp, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { dial } from '../../server/test/client.mjs';

const samples = Number(process.env.HUSH_BENCH_SAMPLES ?? 120);
if (!Number.isInteger(samples) || samples < 20 || samples > 1000) throw new Error('HUSH_BENCH_SAMPLES must be 20–1000');
const exec = promisify(execFile);
const exe = resolve(process.env.HUSH_TEST_CLI ?? fileURLToPath(new URL('../../bin/hush', import.meta.url)));
const bundle = resolve(process.env.HUSH_TEST_BACKEND ?? fileURLToPath(new URL('../../dist/hush-backend.mjs', import.meta.url)));
const home = await realpath(await mkdtemp('/tmp/hs-bench-'));
const agent = join(home, '.hush'), socket = join(agent, 'runtime', 'hush.sock'), entry = join(home, 'fixture.mjs');
const env = { HOME: home, LANG: 'C', PATH: dirname(process.execPath) + ':/usr/bin:/bin', NODE_NO_WARNINGS: '1' };
const invoke = (binary, args) => exec(binary, args, { env, timeout: 10_000 });
const cli = (...args) => invoke(exe, args);
const put = (name, data) => writeFile(name, data, { mode: 0o600 });
const dir = name => mkdir(name, { recursive: true, mode: 0o700 });
const capabilities = { workspaceFiles: [1], workspaceSkills: [1], loadedContext: [1] };
let runner;
const records = [];
const checked = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };

async function connect() {
  const c = await dial(socket);
  try {
    const hello = checked(await c.send('hello', { versions: [1], capabilities }));
    return [c, hello];
  } catch (error) { c.close(); throw error; }
}
async function request(method, params) {
  const [c] = await connect();
  try { return checked(await c.send(method, params)); }
  finally { c.close(); }
}

async function measure(name, count, body, after = () => {}, warmup = 8) {
  for (let n = 0; n < warmup; n++) await after(await body());
  const ms = [];
  for (let n = 0; n < count; n++) {
    const time = performance.now();
    const value = await body();
    ms.push(performance.now() - time);
    await after(value);
  }
  ms.sort((a, b) => a - b);
  const at = p => Number(ms[Math.ceil(p * ms.length) - 1].toFixed(2));
  const result = { name, samples: count, p50_ms: at(.5), p95_ms: at(.95), p99_ms: at(.99), max_ms: at(1) };
  records.push(result);
  process.stderr.write(`${name}: p50 ${result.p50_ms} ms · p95 ${result.p95_ms} ms · p99 ${result.p99_ms} ms\n`);
}

async function stop() {
  if (!runner) return;
  const active = runner;
  runner = undefined;
  active.proc.kill('SIGTERM');
  await active.closed;
}

async function boot(token) {
  const start = performance.now(), node = await realpath(process.execPath);
  const proc = spawn(exe, ['--hush-internal-service', token, node, entry, exe], { env, stdio: 'ignore' });
  const closed = new Promise(resolve => proc.once('close', (code, signal) => resolve({ code, signal })));
  runner = { proc, closed };
  while (performance.now() - start < 5000) {
    try {
      const [conn, hello] = await connect();
      conn.close();
      return hello;
    } catch (err) {
      if (!['ECONNREFUSED', 'ENOENT'].includes(err.code)) throw err;
      if (proc.exitCode !== null || proc.signalCode !== null) break;
      await delay(6);
    }
  }
  throw new Error('fixture supervisor failed to start');
}

async function seedSkills(cwd, count) {
  for (let i = 0; i < count; i++) {
    const name = `review-${String(i).padStart(2, '0')}`, folder = join(cwd, '.agents/skills', name);
    await dir(folder);
    await put(join(folder, 'SKILL.md'), `---\nname: ${name}\ndescription: Read-only benchmark skill ${i}\n---\n${'Fixture text. '.repeat(150)}`);
  }
}
async function seedFiles(cwd, name, count) {
  const parent = join(cwd, name);
  await dir(parent);
  for (let i = 0; i < count; i += 48) {
    await Promise.all(Array.from({ length: Math.min(48, count - i) }, (_, n) => put(join(parent, `file ${String(i+n).padStart(5, '0')}.txt`), '')));
  }
}

try {
  const work = join(home, 'workspace'), heavy = join(home, 'large workspace');
  await dir(agent);
  await dir(work);
  await dir(join(heavy, '.git'));
  await put(join(agent, 'APPEND_SYSTEM.md'), 'Personal fixture instructions\n');
  await put(join(work, 'AGENTS.md'), 'Typical workspace instructions\n');
  const levels = 6;
  let sub = heavy;
  for (let n = 0; n < levels; n++) {
    await dir(sub);
    await put(join(sub, 'AGENTS.md'), 'Fixture instructions.\n'.repeat(1570));
    if (n+1 < levels) sub = join(sub, `nested folder ${n}`);
  }
  await Promise.all([seedSkills(work, 8), seedSkills(sub, 64), seedFiles(work, 'documents', 80), seedFiles(work, 'many files', 3800)]);
  await put(join(agent, 'skills.json'), JSON.stringify({ projectRoots: [work, sub].map(workspace => ({ workspace, directory: '.agents/skills' })) }));
  await put(entry, `process.env.NODE_NO_WARNINGS = '1';\nif (process.argv.includes('--supervisor')) process.argv.push('--faux');\n${await readFile(bundle, 'utf8')}`);
  const token = randomUUID();
  await invoke(process.execPath, [entry, '--hush-internal-service-claim', token]);
  for (const cwd of [work, sub]) await cli('trust', 'grant', '--cwd', cwd);
  await boot(token);
  const { id: small } = await request('thread.new', { cwd: work });
  const { id: large } = await request('thread.new', { cwd: sub });
  const files = (directory, cursor) => request('ext.workspaceFiles.list', { threadId: small, root: 'workspace', directory, limit: 100, ...(cursor && { cursor }) });
  const skills = threadId => request('ext.workspaceSkills.list', { threadId });
  const instructions = threadId => request('ext.loadedContext.get', { threadId });
  const cursor = (await files('many files')).nextCursor;
  const [{ name, token: skillToken }] = (await skills(small)).skills;
  assert.ok(cursor && skillToken);
  assert.equal((await skills(large)).skills.length, 64);
  assert.equal((await instructions(large)).sources.length, levels + 1);
  await measure('file-scope', samples, () => request('ext.workspaceFiles.scope', { threadId: small }));
  await measure('file-first-page-80', samples, () => files('documents'));
  await measure('file-first-page-3800', samples, () => files('many files'));
  await measure('file-next-page-3800', samples, () => files('many files', cursor));
  await measure('skills-list-8', samples, () => skills(small));
  await measure('skills-list-64', samples, () => skills(large));
  await measure('loaded-context-small', samples, () => instructions(small));
  await measure('loaded-context-200KiB', samples, () => instructions(large));
  await measure('skill-source-check-and-receipt', samples, () => request('ext.workspaceSkills.submit', {
    token: skillToken,
    input: { threadId: small, requestId: randomUUID(), text: '/skill:' + name, whenBusy: 'reject' },
  }), ({ submissionId }) => request('submission.wait', { threadId: small, submissionId }));
  await measure('supervised-cli-attach', samples, () => cli('status', '--json'));
  await stop();
  await measure('supervisor-dispatch-to-listener', Math.min(25, samples), () => boot(token), stop, 2);
  process.stdout.write(JSON.stringify({ node: process.version, platform: process.platform, arch: process.arch, results: records }, null, 2) + '\n');
} finally {
  await stop();
  await rm(home, { recursive: true, force: true });
}
