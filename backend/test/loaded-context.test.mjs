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
  const shared = join(dir, '.agents'), rootAgents = join(repo, '.agents'), cwdAgents = join(cwd, '.agents'), lockedAgents = join(locked, '.agents');
  for (const path of [agentDir, shared, join(repo, '.git'), rootAgents, cwdAgents, empty, lockedAgents]) {
    await mkdir(path, { recursive: true, mode: 0o700 });
  }
  const append = join(agentDir, 'APPEND_SYSTEM.md'), personalHush = join(agentDir, 'AGENTS.md'), personalShared = join(shared, 'AGENTS.md');
  const rootFile = join(repo, 'AGENTS.md'), rootHidden = join(rootAgents, 'AGENTS.md');
  const cwdFile = join(cwd, 'AGENTS.md'), cwdHidden = join(cwdAgents, 'AGENTS.md');
  const expected = [
    [append, 'personal', 'append'], [personalHush, 'personal', 'hush'], [personalShared, 'personal', 'shared'],
    [rootFile, 'workspace', 'parent'], [rootHidden, 'workspace', 'parent-hidden'],
    [cwdFile, 'workspace', 'child'], [cwdHidden, 'workspace', 'child-hidden'],
  ].map(([path, kind, tag]) => ({ path, kind, text: `SENTINEL ${tag}\n` }));
  for (const { path, text } of expected) await writeFile(path, text, { mode: 0o600 });
  for (const folder of [locked, lockedAgents]) await writeFile(join(folder, 'AGENTS.md'), 'SENTINEL unauthorized', { mode: 0o600 });
  await symlink(join(locked, 'AGENTS.md'), join(parent, 'AGENTS.md'));
  await symlink(lockedAgents, join(parent, '.agents'));
  const trust = new WorkspaceTrust(agentDir);
  for (const path of [cwd, empty, dir]) await trust.change(path, 'grant', AbortSignal.timeout(5000));
  const host = await startBackend({ cwd, agentDir, dataDir, socket, idleMs: 5000, ephemeral: false, faux: true });
  const local = await dial(socket), remote = await dial(socket), legacy = await dial(socket), older = await dial(socket);
  t.after(async () => { for (const client of [local, remote, legacy, older]) client.close(); await host.close(); await rm(dir, { recursive: true, force: true }); });
  for (const client of [local, remote]) {
    const frame = await client.send('hello', { versions: [1], capabilities: { loadedContext: [1, 2], workspaceFiles: [1], workspaceSkills: [99] } });
    assert.deepEqual(ok(frame).capabilities, { loadedContext: 2 }); // independent of files or skills
  }
  assert.deepEqual(ok(await legacy.send('hello', { versions: [1], capabilities: { loadedContext: [1] } })).capabilities, { loadedContext: 1 });
  assert.deepEqual(ok(await older.send('hello', { versions: [1] })).capabilities, {});
  const create = async cwd => ok(await local.send('thread.new', { cwd })).id;
  const threadId = await create(cwd), emptyId = await create(empty), lockedId = await create(locked), homeId = await create(dir);
  const report = (p, threadId, extra) => p.send('ext.loadedContext.get', { threadId, ...extra });
  assert.equal((await report(older, threadId)).error.code, 'unsupported_capability');
  assert.equal((await report(remote, threadId, { cwd: locked })).error.code, 'invalid_request');
  const sources = expected.map(({ kind, path }) => ({ kind, path }));
  const personal = sources.filter(source => source.kind === 'personal');
  assert.deepEqual(ok(await report(local, threadId)), { sources });
  const reply = await report(remote, threadId);
  assert.deepEqual(ok(reply), { sources });
  assert.doesNotMatch(JSON.stringify(reply), /SENTINEL/);
  assert.deepEqual(ok(await report(legacy, threadId)), { sources: sources.filter(source => [append, rootFile, cwdFile].includes(source.path)) });
  for (const id of [lockedId, emptyId, homeId]) {
    assert.deepEqual(ok(await report(remote, id)), { sources: personal });
  }
  const view = await host.harness.conversation(Number(threadId), BACKGROUND_CONTEXT);
  assert.equal((await view.entries({}, 100, undefined, BACKGROUND_CONTEXT)).items.length, 0);
  const { submissionId } = ok(await local.send('submit', { threadId, text: 'Check resolved instructions', requestId: 'capture-prompt', whenBusy: 'reject' }));
  assert.equal(ok(await local.send('submission.wait', { threadId, submissionId })).status, 'done');
  const entries = (await view.entries({}, 100, undefined, BACKGROUND_CONTEXT)).items;
  const prompt = entries.flatMap(e => e.model ?? []).filter(m => m.role === 'system').map(getSystemMessageText).join('\n');
  let offset = 0;
  for (const { path, text } of expected) {
    const marker = text.trim();
    if (path !== append) assert.ok(prompt.includes(path));
    const at = prompt.indexOf(marker, offset);
    assert.ok(at >= offset, `missing or out of order: ${marker}\n${prompt}`);
    offset = at + marker.length;
  }
  assert.doesNotMatch(prompt, /SENTINEL unauthorized/);
  await trust.change(cwd, 'revoke', AbortSignal.timeout(5000));
  assert.deepEqual(ok(await report(local, threadId)), { sources: personal });
  await Promise.all(personal.map(({ path }) => rm(path)));
  assert.deepEqual(ok(await report(remote, emptyId)), { sources: [] });
  assert.deepEqual(ok(await report(local, threadId)), { sources: [] });
});
