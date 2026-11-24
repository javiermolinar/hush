import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, realpath, rename, rm, symlink, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { test } from 'node:test';
import { dial } from '../../server/test/client.mjs';
import { WorkspaceWorkTrust } from '../../dist/backend/src/workspace-trust.js';
import { startBackend } from '../../dist/hush-backend.mjs';

const ok = frame => { assert.equal(frame.ok, true, JSON.stringify(frame)); return frame.result; };
const denied = (frame, code = 'invalid_request') => { assert.equal(frame.ok, false, JSON.stringify(frame)); assert.equal(frame.error.code, code); };
async function addSkill(root, name, description, body) {
  const dir = join(root, name);
  await mkdir(dir, { recursive: true });
  const path = join(dir, 'SKILL.md');
  await writeFile(path, `---\nname: ${name}\ndescription: ${description}\n---\n${body}`, { mode: 0o600 });
  return path;
}

test('standalone skills opt in, isolate workspaces, and bind a staged input to the live source and grant', { timeout: 20_000 }, async t => {
  const dir = await realpath(await mkdtemp('/tmp/hs-skill-')), a = join(dir, 'first workspace'), b = join(dir, 'second workspace');
  const agentDir = join(dir, '.hush'), dataDir = join(dir, 'data'), socket = join(dataDir, 'hush.sock');
  for (const path of [a, b, agentDir]) await mkdir(path, { mode: 0o700 });
  const config = { profile: 'hush', cwd: a, agentDir, dataDir, socket, idleMs: 8000, ephemeral: false, faux: true };
  let host = await startBackend(config), wire;
  t.after(async () => { wire?.close(); await host.close(); await rm(dir, { recursive: true, force: true }); });
  const connect = async () => {
    wire = await dial(socket);
    return ok(await wire.send('hello', { versions: [1], capabilities: { workspaceSkills: [1], intro: [2] } })).capabilities;
  };
  const trust = new WorkspaceWorkTrust(agentDir), grant = (cwd, action = 'grant') => trust.change(cwd, action, AbortSignal.timeout(5000));
  await grant(a);
  await addSkill(join(a, '.pi', 'skills'), 'pi-only', 'Pi skill', 'PI body must not load');
  const first = await addSkill(join(a, '.agents/skills'), 'review', 'Review FIRST', 'FIRST body');
  const second = await addSkill(join(b, '.agents/skills'), 'review', 'Review SECOND', 'SECOND body');
  await addSkill(join(agentDir, 'skills'), 'personal', 'Personal text', 'Personal body');
  await addSkill(join(a, '.agents/skills'), 'extra', 'Workspace choice', 'Another body');
  const api = await connect();
  assert.equal(api.workspaceSkills, undefined);
  const { id: threadA } = ok(await wire.send('thread.new', { cwd: a }));
  const { id: threadB } = ok(await wire.send('thread.new', { cwd: b }));
  const list = (threadId = threadA) => wire.send('ext.workspaceSkills.list', { threadId });
  const submit = (threadId, requestId, text, token) => {
    const input = { threadId, requestId, text, whenBusy: 'reject' };
    return token ? wire.send('ext.workspaceSkills.submit', { input, token }) : wire.send('submit', input);
  };
  const settle = async (threadId, frame) => {
    const { submissionId } = ok(await frame);
    assert.equal(ok(await wire.send('submission.wait', { threadId, submissionId })).status, 'done');
    return submissionId;
  };
  denied(await list(), 'unsupported_capability');
  denied(await submit(threadA, 'default-empty', '/skill:review'));
  await writeFile(join(agentDir, 'skills.json'), JSON.stringify({ personal: true, projectRoots: [a, b].map(workspace => ({ workspace, directory: '.agents/skills' })) }), { mode: 0o600 });
  wire.close(); await host.close(); host = await startBackend(config);
  assert.equal((await connect()).workspaceSkills, 1);
  const aSkills = ok(await list()).skills, aReview = aSkills.find(s => s.name === 'review');
  assert.deepEqual(aSkills.map(({ name, source }) => [name, source]), [['extra', 'workspace'], ['personal', 'personal'], ['review', 'workspace']]);
  assert.equal(aReview.description, 'Review FIRST');
  assert.deepEqual(ok(await list(threadB)).skills.map(s => s.name), ['personal']); // config did not grant trust
  denied(await submit(threadB, 'without-work-grant', '/skill:review'));
  await grant(b);
  const bReview = ok(await list(threadB)).skills.find(s => s.name === 'review');
  assert.equal(bReview.description, 'Review SECOND');
  assert.notEqual(aReview.token, bReview.token);
  denied(await submit(threadB, 'wrong-scope', '/skill:review', aReview.token));
  await settle(threadA, submit(threadA, 'typed-first', '/skill:review my instructions'));
  const used = await settle(threadB, submit(threadB, 'staged-second', '/skill:review', bReview.token));
  const history = async (threadId, expected) => {
    const saved = ok(await wire.send('history', { threadId, range: { kind: 'before', beforeEntryId: null }, limit: 20 }));
    assert.match(JSON.stringify(saved), expected);
  };
  await history(threadA, /FIRST body/); await history(threadB, /SECOND body/);
  const previous = second + '.old';
  await rename(second, previous);
  await writeFile(second, await readFile(previous), { mode: 0o600 });
  denied(await submit(threadB, 'stale-replaced', '/skill:review', bReview.token)); // identical bytes, different source
  // A lost, already-admitted reply still returns the original receipt.
  assert.equal(ok(await submit(threadB, 'staged-second', '/skill:review', bReview.token)).submissionId, used);
  const fresh = ok(await list(threadB)).skills.find(s => s.name === 'review');
  await grant(b, 'revoke');
  denied(await submit(threadB, 'revoked', '/skill:review', fresh.token));
  await grant(b);
  denied(await submit(threadB, 'regranted', '/skill:review', fresh.token));
  await rename(first, first + '.old');
  await symlink(second, first);
  denied(await submit(threadA, 'symlink', '/skill:review'));
  denied(await submit(threadA, 'rename', '/skill:review', aReview.token));
  assert.ok(!ok(await list()).skills.some(s => s.name === 'review'));
});
