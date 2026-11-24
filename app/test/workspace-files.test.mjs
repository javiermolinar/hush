import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { mkdir, mkdtemp, realpath, rename, rm, symlink, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { dial } from '../../server/test/client.mjs';

const invoke = promisify(execFile);
const executable = resolve(process.env.HUSH_TEST_CLI ?? fileURLToPath(new URL('../../bin/hush', import.meta.url)));
const bundle = resolve(process.env.HUSH_TEST_BACKEND ?? fileURLToPath(new URL('../../dist/hush-backend.mjs', import.meta.url)));
const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const denied = (frame, code) => { assert.equal(frame.ok, false, JSON.stringify(frame)); assert.equal(frame.error.code, code); };

test('file browsing uses current host grants and a thread/root/directory/boot cursor', { timeout: 25_000 }, async t => {
  const dir = await realpath(await mkdtemp('/tmp/hs-files-'));
  const home = join(dir, 'home'), workspace = join(dir, 'workspace space'), foreign = join(dir, 'foreign');
  const nested = join(workspace, 'nested folder'), socket = join(home, '.hush', 'runtime', 'hush.sock');
  const env = { HOME: home, PATH: dirname(process.execPath) + ':/usr/bin:/bin', LANG: 'C' };
  const cli = (...args) => invoke(executable, ['--backend', bundle, ...args], { cwd: workspace, env, timeout: 12_000 });
  const stop = () => invoke(executable, ['stop'], { cwd: workspace, env, timeout: 12_000 });
  let client, older;
  t.after(async () => {
    client?.close(); older?.close();
    await stop();
    await rm(dir, { recursive: true, force: true });
  });
  for (const path of [home, workspace, nested, foreign]) await mkdir(path);
  await writeFile(join(home, 'home notes.txt'), 'not in workspace');
  await writeFile(join(workspace, 'a file.txt'), 'content must not be sent');
  await writeFile(join(workspace, 'z.txt'), '');
  await writeFile(join(nested, 'an inner file.txt'), '');
  await writeFile(join(foreign, 'secret.txt'), '');
  await symlink(foreign, join(workspace, 'symlink'), 'dir');
  await symlink(join(foreign, 'secret.txt'), join(workspace, 'symlink file'));
  await cli('trust', 'grant', '--scope', 'work', '--cwd', workspace);
  await cli('status');
  const connect = async () => {
    client = await dial(socket); // attach-only peer can have an unrelated client cwd
    assert.deepEqual(ok(await client.send('hello', { versions: [1], capabilities: { workspaceFiles: [1] } })).capabilities, { workspaceFiles: 1 });
  };
  await connect();
  older = await dial(socket);
  assert.deepEqual(ok(await older.send('hello', { versions: [1] })).capabilities, {});
  const { id } = ok(await client.send('thread.new', { cwd: workspace }));
  const { id: same } = ok(await client.send('thread.new', { cwd: workspace }));
  const { id: outside } = ok(await client.send('thread.new', { cwd: foreign }));
  const scope = (threadId = id) => client.send('ext.workspaceFiles.scope', { threadId });
  const list = overrides => client.send('ext.workspaceFiles.list', { threadId: id, root: 'workspace', directory: '', limit: 1, ...overrides });
  denied(await older.send('ext.workspaceFiles.scope', { threadId: id }), 'unsupported_capability');
  assert.deepEqual(ok(await scope()), { workspace: false, home: false });
  denied(await list(), 'not_found'); // tool trust was granted, browsing wasn't
  await cli('trust', 'grant', '--scope', 'browse', '--cwd', workspace);
  assert.deepEqual(ok(await scope()), { workspace: true, home: false });
  assert.deepEqual(ok(await scope(outside)), { workspace: false, home: false });
  denied(await list({ root: 'home' }), 'not_found');

  const first = ok(await list());
  assert.deepEqual(first.entries, [{ name: 'nested folder', directory: true }]);
  const second = ok(await list({ cursor: first.nextCursor }));
  assert.deepEqual(second.entries, [{ name: 'a file.txt', directory: false }]);
  assert.deepEqual(ok(await list({ cursor: second.nextCursor })), { entries: [{ name: 'z.txt', directory: false }] });
  assert.deepEqual(ok(await list({ directory: 'nested folder' })).entries, [{ name: 'an inner file.txt', directory: false }]);
  for (const directory of ['../foreign', '/tmp', '.hush', 'nested folder/../symlink']) denied(await list({ directory }), 'invalid_request');
  denied(await list({ directory: 'symlink' }), 'not_found');
  denied(await list({ path: foreign }), 'invalid_request');
  denied(await list({ threadId: same, cursor: first.nextCursor }), 'invalid_cursor');
  denied(await list({ directory: 'nested folder', cursor: first.nextCursor }), 'invalid_cursor');

  await cli('trust', 'grant', '--scope', 'home');
  assert.deepEqual(ok(await scope()), { workspace: true, home: true });
  assert.deepEqual(ok(await list({ root: 'home', limit: 100 })).entries, [{ name: 'home notes.txt', directory: false }]);
  denied(await list({ root: 'home', cursor: first.nextCursor }), 'invalid_cursor');
  await cli('trust', 'revoke', '--scope', 'browse', '--cwd', workspace);
  denied(await list({ cursor: first.nextCursor }), 'invalid_cursor');
  assert.deepEqual(ok(await scope()), { workspace: false, home: true });
  await cli('trust', 'grant', '--scope', 'browse', '--cwd', workspace);
  denied(await list({ cursor: first.nextCursor }), 'invalid_cursor'); // a new grant does not restore old cursors
  const regranted = ok(await list());
  await rename(nested, join(workspace, 'old folder'));
  await symlink(foreign, nested, 'dir');
  denied(await list({ directory: 'nested folder' }), 'not_found');
  denied(await list({ cursor: regranted.nextCursor }), 'invalid_cursor');
  const beforeBoot = ok(await list());
  await stop(); client.close(); await cli('status'); await connect();
  denied(await list({ cursor: beforeBoot.nextCursor }), 'invalid_cursor');
  assert.ok(ok(await list({ limit: 2 })).entries.length);
});
