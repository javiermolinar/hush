import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { createHash } from 'node:crypto';
import { lstat, mkdir, mkdtemp, realpath, rename, rm, symlink, writeFile } from 'node:fs/promises';
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
  let client, older, listOnly, project;
  t.after(async () => {
    client?.close(); older?.close(); listOnly?.close(); project?.close();
    await stop();
    await rm(dir, { recursive: true, force: true });
  });
  for (const path of [home, workspace, nested, foreign]) await mkdir(path);
  const workflow = 'delete-old-tempo-releases.yaml', workflowPath = `.github/workflows/${workflow}`;
  await mkdir(join(workspace, '.github/workflows'), { recursive: true });
  await mkdir(join(home, '.github'));
  await writeFile(join(workspace, workflowPath), 'content must not be sent');
  await writeFile(join(workspace, '.github/workflows/.secret'), 'hidden');
  await writeFile(join(home, 'home notes.txt'), 'not in workspace');
  await writeFile(join(workspace, 'a file.txt'), 'content must not be sent');
  await writeFile(join(workspace, 'z.txt'), '');
  await writeFile(join(nested, 'an inner file.txt'), '');
  await writeFile(join(foreign, 'secret.txt'), '');
  await symlink(foreign, join(workspace, 'symlink'), 'dir');
  await symlink(join(foreign, 'secret.txt'), join(workspace, 'symlink file'));
  await cli('status');
  const connect = async () => {
    client = await dial(socket); // attach-only peer can have an unrelated client cwd
    const hello = ok(await client.send('hello', { versions: [1], capabilities: { workspaceFiles: [2, 1], workspaceSkills: [2, 1] } }));
    assert.deepEqual(hello.capabilities, { workspaceFiles: 2, workspaceSkills: 2 });
  };
  await connect();
  older = await dial(socket);
  assert.deepEqual(ok(await older.send('hello', { versions: [1] })).capabilities, {});
  listOnly = await dial(socket);
  assert.deepEqual(ok(await listOnly.send('hello', { versions: [1], capabilities: { workspaceFiles: [1] } })).capabilities, { workspaceFiles: 1 });
  project = await dial(socket);
  assert.deepEqual(ok(await project.send('hello', { versions: [1], capabilities: { workspaceFiles: [3, 2, 1] } })).capabilities, { workspaceFiles: 3 });
  const { id } = ok(await client.send('thread.new', { cwd: workspace }));
  const { id: same } = ok(await client.send('thread.new', { cwd: workspace }));
  const { id: outside } = ok(await client.send('thread.new', { cwd: foreign }));
  const scope = (threadId = id) => client.send('ext.workspaceFiles.scope', { threadId });
  const list = overrides => client.send('ext.workspaceFiles.list', { threadId: id, root: 'workspace', directory: '', limit: 1, ...overrides });
  const search = (overrides, peer = client) => peer.send('ext.workspaceFiles.search', { threadId: id, query: 'an inner file.txt', limit: 10, ...overrides });
  denied(await older.send('ext.workspaceFiles.scope', { threadId: id }), 'unsupported_capability');
  denied(await search({}, older), 'unsupported_capability');
  denied(await search({}, listOnly), 'unknown_method');
  const oldGrants = join(home, '.hush', 'workspace-browse-v1');
  const { dev, ino, birthtimeNs } = await lstat(workspace, { bigint: true });
  const oldBrowse = JSON.stringify(['browse', workspace, String(dev), String(ino), String(birthtimeNs)]) + '\n';
  await mkdir(oldGrants, { mode: 0o700 });
  await writeFile(join(oldGrants, createHash('sha256').update(oldBrowse).digest('hex')), oldBrowse, { mode: 0o600 });
  assert.deepEqual(ok(await scope()), { workspace: false, home: false });
  denied(await list(), 'not_found'); // old browse-only consent is not promoted
  denied(await search(), 'not_found');
  await cli('trust', 'grant', '--cwd', workspace);
  assert.deepEqual(ok(await scope()), { workspace: true, home: false });
  assert.deepEqual(ok(await scope(outside)), { workspace: false, home: false });
  assert.deepEqual(ok(await listOnly.send('ext.workspaceFiles.scope', { threadId: id })), { workspace: true, home: false });
  denied(await list({ root: 'home' }), 'not_found');
  denied(await search({ threadId: outside }), 'not_found');
  assert.deepEqual(ok(await search()), { matches: [{ path: 'nested folder/an inner file.txt', directory: false }], partial: false });
  assert.deepEqual(ok(await search({ query: 'secret' })), { matches: [], partial: false });
  const projectList = overrides => project.send('ext.workspaceFiles.list', {
    threadId: id, root: 'workspace', directory: '', limit: 1, ...overrides,
  });
  assert.deepEqual(ok(await search({ query: 'delete-old-tempo' }, project)), { matches: [{ path: workflowPath, directory: false }], partial: false });
  assert.deepEqual(ok(await search({ query: workflow }, project)), { matches: [{ path: workflowPath, directory: false }], partial: false });
  assert.deepEqual(ok(await search({ query: workflow })), { matches: [], partial: false });
  assert.deepEqual(ok(await search({ query: '.secret' }, project)), { matches: [], partial: false });
  const projectFirst = ok(await projectList());
  assert.deepEqual(projectFirst.entries, [{ name: '.github', directory: true }]);
  denied(await list({ cursor: projectFirst.nextCursor }), 'invalid_cursor');
  assert.deepEqual(ok(await projectList({ directory: '.github/workflows' })), { entries: [{ name: workflow, directory: false }] });
  denied(await list({ directory: '.github/workflows' }), 'invalid_request');
  for (const directory of ['.git', '.github/..', '.github/.secret', 'nested folder/.github']) {
    denied(await projectList({ directory }), 'invalid_request');
  }
  for (const overrides of [{ root: 'home' }, { cwd: foreign }, { query: '../foreign' }, { query: '\u00a0' }, { limit: 33 }]) {
    denied(await search(overrides), 'invalid_request');
  }

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

  // Before a thread exists, version 2 addresses the directory a new thread
  // would record. The same grant applies and nothing is created.
  const threads = async () => ok(await client.send('thread.list', { limit: 100 })).threads.map(row => row.id);
  const known = await threads();
  const draft = (method, params, peer = client) => peer.send(`ext.workspaceFiles.${method}`, params);
  const draftList = overrides => draft('list', { cwd: workspace, root: 'workspace', directory: '', limit: 1, ...overrides });
  assert.deepEqual(ok(await draft('scope', { cwd: workspace })), { workspace: true, home: false });
  assert.deepEqual(ok(await draft('scope', { cwd: foreign })), { workspace: false, home: false });
  assert.deepEqual(ok(await draft('scope', { cwd: join(workspace, 'symlink') })), { workspace: false, home: false }); // its untrusted target
  denied(await draft('scope', { cwd: join(dir, 'missing') }), 'not_found');
  for (const params of [{}, { cwd: 'workspace space' }, { cwd: workspace, threadId: id }, { cwd: `${workspace}\u0085` }]) {
    denied(await draft('scope', params), 'invalid_request');
  }
  denied(await draft('scope', { cwd: workspace }, listOnly), 'invalid_request'); // version 1 frames are unchanged
  const page = ok(await draftList());
  assert.deepEqual(page.entries, [{ name: 'nested folder', directory: true }]);
  assert.deepEqual(ok(await draftList({ cursor: page.nextCursor })).entries, [{ name: 'a file.txt', directory: false }]);
  denied(await list({ cursor: page.nextCursor }), 'invalid_cursor'); // a cursor stays with its target
  denied(await draftList({ cursor: first.nextCursor }), 'invalid_cursor');
  denied(await draftList({ cwd: foreign }), 'not_found');
  assert.deepEqual(ok(await draft('search', { cwd: workspace, query: 'an inner file.txt', limit: 10 })),
    { matches: [{ path: 'nested folder/an inner file.txt', directory: false }], partial: false });
  denied(await draft('search', { cwd: foreign, query: 'secret', limit: 10 }), 'not_found');
  assert.deepEqual(ok(await client.send('ext.workspaceSkills.list', { cwd: workspace })), { skills: [] });
  assert.deepEqual(await threads(), known);

  await cli('trust', 'grant', '--scope', 'home');
  assert.deepEqual(ok(await scope()), { workspace: true, home: true });
  assert.deepEqual(ok(await list({ root: 'home', limit: 100 })).entries, [{ name: 'home notes.txt', directory: false }]);
  assert.deepEqual(ok(await projectList({ root: 'home' })).entries, [{ name: 'home notes.txt', directory: false }]);
  denied(await projectList({ root: 'home', directory: '.github' }), 'invalid_request');
  denied(await list({ root: 'home', cursor: first.nextCursor }), 'invalid_cursor');
  await cli('trust', 'revoke', '--cwd', workspace);
  denied(await list({ cursor: first.nextCursor }), 'invalid_cursor');
  denied(await list(), 'not_found');
  denied(await search(), 'not_found'); // a home grant does not admit recursive search
  assert.deepEqual(ok(await scope()), { workspace: false, home: true });
  assert.deepEqual(ok(await draft('scope', { cwd: workspace })), { workspace: false, home: true });
  denied(await draftList(), 'not_found');
  await cli('trust', 'grant', '--cwd', workspace);
  denied(await list({ cursor: first.nextCursor }), 'invalid_cursor'); // a new grant does not restore old cursors
  const regranted = ok(await list());
  await rename(nested, join(workspace, 'old folder'));
  await symlink(foreign, nested, 'dir');
  denied(await list({ directory: 'nested folder' }), 'not_found');
  assert.deepEqual(ok(await search({ query: 'secret' })), { matches: [], partial: false });
  denied(await list({ cursor: regranted.nextCursor }), 'invalid_cursor');
  const beforeBoot = ok(await list());
  await stop(); client.close(); await cli('status'); await connect();
  denied(await list({ cursor: beforeBoot.nextCursor }), 'invalid_cursor');
  assert.ok(ok(await list({ limit: 2 })).entries.length);
});
