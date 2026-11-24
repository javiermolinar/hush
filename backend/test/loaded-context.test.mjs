import assert from 'node:assert/strict';
import { mkdir, mkdtemp, realpath, rm, symlink, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { getSystemMessageText } from '@earendil-works/pi-ai';
import { dial } from '../../server/test/client.mjs';
import { startBackend } from '../../dist/hush-backend.mjs';
import { WorkspaceTrust } from '../../dist/backend/src/workspace-trust.js';

const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };

test('loaded context is the ordered next-prompt sources, scoped and stripped of contents', { timeout: 15_000 }, async t => {
  const dir = await realpath(await mkdtemp('/tmp/hs-ctx-'));
  const agentDir = join(dir, '.hush'), dataDir = join(dir, 'run'), socket = join(dataDir, 'hush.sock');
  const repo = join(dir, 'workspace repository'), parent = join(repo, 'docs'), cwd = join(parent, 'chapter');
  const empty = join(dir, 'standalone-no-instructions');
  const locked = join(dir, 'not-approved');
  for (const path of [agentDir, join(repo, '.git'), cwd, empty, locked]) await mkdir(path, { recursive: true, mode: 0o700 });
  const personalFile = join(agentDir, 'APPEND_SYSTEM.md'), rootFile = join(repo, 'AGENTS.md'), cwdFile = join(cwd, 'AGENTS.md');
  for (const [path, value] of [[personalFile, 'SENTINEL personal'], [rootFile, 'SENTINEL parent'], [cwdFile, 'SENTINEL child'],
    [join(locked, 'AGENTS.md'), 'SENTINEL unauthorized']]) await writeFile(path, value, { mode: 0o600 });
  await symlink(join(locked, 'AGENTS.md'), join(parent, 'AGENTS.md'));
  const trust = new WorkspaceTrust(agentDir);
  for (const path of [cwd, empty]) await trust.change(path, 'grant', AbortSignal.timeout(5000));
  const host = await startBackend({ profile: 'hush', cwd, agentDir, dataDir, socket, idleMs: 5000, ephemeral: false, faux: true });
  const local = await dial(socket), remote = await dial(socket), older = await dial(socket);
  t.after(async () => { local.close(); remote.close(); older.close(); await host.close(); await rm(dir, { recursive: true, force: true }); });
  for (const client of [local, remote]) {
    const frame = await client.send('hello', { versions: [1], capabilities: { loadedContext: [1], workspaceFiles: [1], workspaceSkills: [1] } });
    assert.deepEqual(ok(frame).capabilities, { loadedContext: 1 }); // independent of files or skills
  }
  assert.deepEqual(ok(await older.send('hello', { versions: [1] })).capabilities, {});
  const create = async cwd => ok(await local.send('thread.new', { cwd })).id;
  const threadId = await create(cwd), emptyId = await create(empty), lockedId = await create(locked);
  const report = (p, threadId, extra) => p.send('ext.loadedContext.get', { threadId, ...extra });
  assert.equal((await report(older, threadId)).error.code, 'unsupported_capability');
  assert.equal((await report(remote, threadId, { cwd: locked })).error.code, 'invalid_request');
  const expected = [{ kind: 'personal', path: personalFile }, { kind: 'workspace', path: rootFile }, { kind: 'workspace', path: cwdFile }];
  assert.deepEqual(ok(await report(local, threadId)), { sources: expected });
  const reply = await report(remote, threadId);
  assert.deepEqual(ok(reply), { sources: expected });
  assert.doesNotMatch(JSON.stringify(reply), /SENTINEL/);
  assert.deepEqual(ok(await report(remote, lockedId)), { sources: [expected[0]] });
  assert.deepEqual(ok(await report(remote, emptyId)), { sources: [expected[0]] });
  const view = await host.harness.conversation(Number(threadId), BACKGROUND_CONTEXT);
  assert.equal((await view.entries({}, 100, undefined, BACKGROUND_CONTEXT)).items.length, 0);
  const { submissionId } = ok(await local.send('submit', { threadId, text: 'Check resolved instructions', requestId: 'capture-prompt', whenBusy: 'reject' }));
  assert.equal(ok(await local.send('submission.wait', { threadId, submissionId })).status, 'done');
  const entries = (await view.entries({}, 100, undefined, BACKGROUND_CONTEXT)).items;
  const prompt = entries.flatMap(e => e.model ?? []).filter(m => m.role === 'system').map(getSystemMessageText).join('\n');
  const offsets = ['personal', 'parent', 'child'].map(label => prompt.indexOf('SENTINEL ' + label));
  assert.ok(offsets[0] >= 0 && offsets[0] < offsets[1] && offsets[1] < offsets[2], prompt);
  for (const source of expected.slice(1)) assert.ok(prompt.includes(source.path));
  assert.doesNotMatch(prompt, /SENTINEL unauthorized/);
  await rm(personalFile);
  assert.deepEqual(ok(await report(remote, emptyId)), { sources: [] });
  await trust.change(cwd, 'revoke', AbortSignal.timeout(5000));
  assert.deepEqual(ok(await report(local, threadId)), { sources: [] });
});
