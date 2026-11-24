import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, realpath, rename, rm, symlink, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { dial } from '../../server/test/client.mjs';
import { WorkspaceTrust } from '../../dist/backend/src/workspace-trust.js';
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

test('standalone skills isolate workspaces and bind a staged input to the live source and grant', { timeout: 20_000 }, async t => {
  const dir = await realpath(await mkdtemp('/tmp/hs-skill-')), a = join(dir, 'first workspace'), b = join(dir, 'second workspace');
  const agentDir = join(dir, '.hush'), dataDir = join(dir, 'data'), socket = join(dataDir, 'hush.sock');
  for (const path of [a, b, agentDir]) await mkdir(path, { mode: 0o700 });
  const config = { profile: 'hush', cwd: a, agentDir, dataDir, socket, idleMs: 8000, ephemeral: false, faux: true };
  const host = await startBackend(config);
  let wire;
  t.after(async () => { wire?.close(); await host.close(); await rm(dir, { recursive: true, force: true }); });
  const connect = async () => {
    wire = await dial(socket);
    return ok(await wire.send('hello', { versions: [1], capabilities: { workspaceSkills: [1], intro: [2] } })).capabilities;
  };
  const trust = new WorkspaceTrust(agentDir), grant = (cwd, action = 'grant') => trust.change(cwd, action, AbortSignal.timeout(5000));
  await grant(a);
  await addSkill(join(a, '.pi', 'skills'), 'pi-only', 'Pi skill', 'PI body must not load');
  const first = await addSkill(join(a, '.agents/skills'), 'review', 'Review FIRST', 'FIRST body');
  const second = await addSkill(join(b, '.agents/skills'), 'review', 'Review SECOND', 'SECOND body');
  await addSkill(join(agentDir, 'skills'), 'personal', 'Personal text', 'Personal body');
  await addSkill(join(a, '.agents/skills'), 'extra', 'Workspace choice', 'Another body');
  const api = await connect();
  assert.equal(api.workspaceSkills, 1);
  denied(await wire.send('ext.workspaceSkills.list', { cwd: a })); // v1 accepts existing threads only
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
  const aSkills = ok(await list()).skills, aReview = aSkills.find(s => s.name === 'review');
  assert.deepEqual(aSkills.map(({ name, source }) => [name, source]), [['extra', 'workspace'], ['personal', 'personal'], ['review', 'workspace']]);
  assert.equal(aReview.description, 'Review FIRST');
  assert.deepEqual(ok(await list(threadB)).skills.map(s => s.name), ['personal']); // discovery did not grant trust
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

test('fresh prompts discover standard roots without configuration and keep preview pins in scope', { timeout: 15_000 }, async t => {
  const dir = await realpath(await mkdtemp('/tmp/hs-skill-draft-'));
  const home = join(dir, 'home'), agentDir = join(home, '.hush'), dataDir = join(dir, 'run');
  const parent = join(dir, 'outside repo'), repo = join(parent, 'repo');
  const cwd = join(repo, 'docs', 'nested folder'), other = join(dir, 'other workspace');
  for (const path of [home, agentDir, join(repo, '.git'), cwd, other]) await mkdir(path, { recursive: true, mode: 0o700 });
  const socket = join(dataDir, 'hush.sock');
  const host = await startBackend({ profile: 'hush', cwd, agentDir, dataDir, socket, idleMs: 8000, ephemeral: false, faux: true });
  const wire = await dial(socket);
  t.after(async () => { wire.close(); await host.close(); await rm(dir, { recursive: true, force: true }); });
  assert.equal(ok(await wire.send('hello', { versions: [1], capabilities: { workspaceSkills: [2, 1] } })).capabilities.workspaceSkills, 2);
  const previewFrame = (path = cwd) => wire.send('ext.workspaceSkills.list', { cwd: path });
  const preview = async path => ok(await previewFrame(path)).skills;
  const threadList = async () => ok(await wire.send('thread.list', { limit: 100 }));
  const initial = await threadList();
  assert.deepEqual(await preview(), []);
  denied(await previewFrame('relative/path'));
  denied(await wire.send('ext.workspaceSkills.list', { cwd, threadId: '1' }));
  assert.deepEqual(await threadList(), initial, 'preview must not allocate a thread');

  await addSkill(join(agentDir, 'skills'), 'common', 'Hush personal has priority', 'HUSH');
  await addSkill(join(home, '.agents/skills'), 'common', 'Shared personal duplicate', 'OTHER');
  await addSkill(join(home, '.agents/skills'), 'shared', 'Shared personal text', 'SHARED');
  const linked = await addSkill(join(dir, 'untrusted'), 'linked', 'Not a regular source', 'LINKED');
  await symlink(dirname(linked), join(home, '.agents/skills/linked'));
  await addSkill(join(cwd, '.agents/skills'), 'local', 'Nearest project has priority', 'NEAREST');
  await addSkill(join(repo, '.agents/skills'), 'local', 'Root duplicate', 'DUPLICATE');
  const repoSkill = await addSkill(join(repo, '.agents/skills'), 'at-root', 'Repo root instructions', 'ROOT');
  await addSkill(join(parent, '.agents/skills'), 'outside', 'Beyond the repository', 'OUTSIDE');
  await addSkill(join(repo, '.claude/skills'), 'claude', 'Not auto-discovered', 'CLAUDE');
  await addSkill(join(repo, '.pi/skills'), 'pi', 'Not auto-discovered', 'PI');
  assert.deepEqual((await preview()).map(s => s.name), ['common', 'shared']);
  assert.deepEqual((await preview(join(dir, 'no such checkout'))).map(s => s.name), ['common', 'shared']);
  const trust = new WorkspaceTrust(agentDir);
  const grant = (path, action = 'grant') => trust.change(path, action, AbortSignal.timeout(5000));
  await grant(repo);
  assert.deepEqual((await preview()).map(s => s.name), ['common', 'shared']); // the parent grant is not a grant for this cwd
  await grant(cwd);
  await grant(other);
  const list = await preview();
  assert.deepEqual(list.map(({ name, description }) => [name, description]), [
    ['at-root', 'Repo root instructions'], ['common', 'Hush personal has priority'],
    ['local', 'Nearest project has priority'], ['shared', 'Shared personal text'],
  ]);
  assert.deepEqual(await threadList(), initial);
  const alias = join(dir, 'workspace alias');
  await symlink(cwd, alias);
  assert.deepEqual(await preview(alias), list); // a trusted draft binds the physical checkout
  const pinned = Object.fromEntries(list.map(skill => [skill.name, skill.token]));
  const { id } = ok(await wire.send('thread.new', { cwd }));
  const { id: elsewhere } = ok(await wire.send('thread.new', { cwd: other }));
  const invoke = (threadId, requestId, name) => wire.send('ext.workspaceSkills.submit', {
    input: { threadId, requestId, text: '/skill:' + name, whenBusy: 'reject' }, token: pinned[name],
  });
  denied(await invoke(elsewhere, 'wrong-draft', 'common')); // exists in both scopes; still cannot reuse the token
  const receipt = ok(await invoke(id, 'first-skill', 'at-root'));
  assert.equal(ok(await wire.send('submission.wait', { threadId: id, submissionId: receipt.submissionId })).status, 'done');
  const saved = ok(await wire.send('history', { threadId: id, range: { kind: 'before', beforeEntryId: null }, limit: 10 }));
  const loaded = saved.entries.find(e => e.kind === 'pi.user').messages[0].blocks[0].text;
  assert.ok(loaded.includes('\n\nROOT\n</skill>'));
  const shadow = await addSkill(join(cwd, '.agents/skills'), 'at-root', 'A nearer source', 'SHADOW');
  denied(await invoke(id, 'shadowed-draft', 'at-root'));
  await rm(shadow);
  await rename(repoSkill, repoSkill + '.old');
  denied(await invoke(id, 'stale-draft', 'at-root'));
  await grant(cwd, 'revoke');
  assert.deepEqual((await preview()).map(s => s.name), ['common', 'shared']);

  await addSkill(join(other, 'custom texts'), 'custom', 'Explicit extra path', 'CUSTOM');
  await writeFile(join(agentDir, 'skills.json'), JSON.stringify({ personal: false, projectRoots: [{ workspace: other, directory: 'custom texts' }] }), { mode: 0o600 });
  assert.deepEqual((await preview(other)).map(s => s.name), ['custom']);
  assert.deepEqual(await preview(), []);
});
