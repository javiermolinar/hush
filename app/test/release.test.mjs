// Standalone fixture: only Node builtins, a candidate binary and Python 3 for PTY.
import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { once } from 'node:events';
import { chmod, copyFile, lstat, mkdir, mkdtemp, readFile, readdir, realpath, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { createConnection } from 'node:net';
import { dirname, join } from 'node:path';
import { createInterface } from 'node:readline';
import { test } from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';

const sha = data => createHash('sha256').update(data).digest('hex');
const absent = path => assert.rejects(lstat(path), { code: 'ENOENT' });
const invoke = (program, args, options) => new Promise((done, fail) => {
  execFile(program, args, { timeout: 20_000, ...options }, (err, stdout, stderr) => {
    if (err && typeof err.code !== 'number') fail(err);
    else done({ code: err?.code ?? 0, stdout, stderr });
  });
});
const pty = String.raw`
import os, select, subprocess, sys, termios, time
master, slave = os.openpty()
initial = termios.tcgetattr(slave)
steps = [(b'[1] Browser', b'1\n'), (b'(hidden)', b'fixture-code\n')]
child = subprocess.Popen([sys.argv[1], 'auth', 'login'], stdin=slave, stdout=slave, stderr=slave)
output = b''
try:
    until = time.monotonic() + 15
    while time.monotonic() < until:
        if select.select([master], [], [], .1)[0]:
            output += os.read(master, 4096)
            if steps and steps[0][0] in output:
                assert not termios.tcgetattr(slave)[3] & termios.ECHO
                os.write(master, steps.pop(0)[1])
        elif child.poll() is not None:
            break
    sys.stdout.buffer.write(output)
    assert not steps and b'fixture-code' not in output
    mask = termios.ECHO | termios.ICANON
    assert (termios.tcgetattr(slave)[3] & mask) == (initial[3] & mask)
    sys.exit(child.wait(timeout=1))
finally:
    if child.poll() is None:
        child.kill()
        child.wait()
    os.close(master)
    os.close(slave)
`;

const pickerPTY = String.raw`
import fcntl, os, re, select, signal, struct, subprocess, sys, termios, time
binary, thread, previous, action, transcript_path, *args = sys.argv[1:]
master, slave = os.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 35, 125, 0, 0))
start = time.monotonic()
child = subprocess.Popen([binary, '--thread', thread, '--motion=false', '--fills=off'], stdin=slave, stdout=slave, stderr=slave)
output = b''
transcript = bytearray()
ansi = re.compile(rb'\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]')
def read_chunk():
    chunk = os.read(master, 8192)
    transcript.extend(chunk)
    del transcript[:-262144]
    return chunk

def wait_for(text):
    global output
    until = time.monotonic() + 4
    while time.monotonic() < until and child.poll() is None:
        if select.select([master], [], [], .05)[0]:
            output += read_chunk()
            body = ansi.sub(b'', output)
            if text.encode() in body:
                output = b''
                return body
    raise AssertionError('Did not observe ' + repr(text) + ': ' + repr(ansi.sub(b'', output)[-1000:]))
try:
    wait_for(previous)
    print('pty_attach_ms=' + str(round((time.monotonic() - start) * 1000, 2)))
    os.write(master, ('/' + action).encode())
    wait_for(' › /' + action)
    os.write(master, b'\r')
    if action == 'login':
        wait_for('Sign in to a provider')
        os.write(master, b'\r')
        wait_for('[1] Browser')
        assert not termios.tcgetattr(slave)[3] & termios.ECHO
        os.write(master, b'1\n')
        wait_for('(hidden)')
        assert not termios.tcgetattr(slave)[3] & termios.ECHO
        os.write(master, b'fixture-code\n')
        assert b'fixture-code' not in wait_for('Signed in to OpenAI Codex')
    else:
        model, = args
        wait_for('Select model')
        os.write(master, b'\x1b[200~' + model.encode() + b'\x1b[201~')
        # The padded search field can contain the full ID before the host
        # answers. Wait for the selected catalog row, not the typed text.
        wait_for('› openai-codex/' + model + '  ')
        os.write(master, b'\r')
        wait_for('Thinking level')
        os.write(master, b'\r')
        wait_for('Model updated')
    # End the terminal client, not the detached host. A bare PTY does not
    # answer terminal-protocol queries or necessarily deliver Ctrl+D as a key.
    child.send_signal(signal.SIGTERM)
    until = time.monotonic() + 4
    while child.poll() is None and time.monotonic() < until:
        if select.select([master], [], [], .05)[0]:
            output += read_chunk()
    assert child.poll() in (0, 143), repr(ansi.sub(b'', output)[-1000:])
finally:
    if child.poll() is None:
        child.kill()
        child.wait()
    os.close(master)
    os.close(slave)
    clean = re.sub(rb'[\x00-\x08\x0b-\x1f\x7f]', b'', ansi.sub(b'', transcript))
    with open(transcript_path, 'wb') as capture:
        capture.write(clean.replace(b'fixture-code', b'[redacted fixture code]'))
`;

// Use the Responses SSE shape of the existing Go OAuth fixture.
function reply(res) {
  const part = { type: 'output_text', text: 'Loopback Codex reply', annotations: [] };
  const item = { id: 'message', type: 'message', role: 'assistant' };
  const position = { output_index: 0, item_id: item.id, content_index: 0 };
  const send = (type, data) => res.write(`data: ${JSON.stringify({ type, ...data })}\n\n`);
  res.setHeader('Content-Type', 'text/event-stream');
  send('response.created', { response: { id: 'fixture', status: 'in_progress', output: [] } });
  send('response.output_item.added', { output_index: 0, item: { ...item, status: 'in_progress', content: [] } });
  send('response.content_part.added', { ...position, part: { ...part, text: '' } });
  send('response.output_text.delta', { ...position, delta: part.text });
  send('response.output_text.done', { ...position, text: part.text });
  send('response.content_part.done', { ...position, part });
  send('response.output_item.done', { output_index: 0, item: { ...item, status: 'completed', content: [part] } });
  send('response.completed', { response: { id: 'fixture', status: 'completed', usage: {
    input_tokens: 10, output_tokens: 4, total_tokens: 14,
    input_tokens_details: { cached_tokens: 0 }, output_tokens_details: { reasoning_tokens: 0 },
  } } });
  res.end('data: [DONE]\n\n');
}

const batchPrompt = 'release-run-codemode-batch';
const batchSource = `const results = await Promise.all(['first.txt', 'second.txt'].map(path => tools.inspect_file({ path })));\n` +
  `return results.map(value => value.split('=')[1].trim()).join(', ');`;

// Newer Codex models receive tool declarations in transcript deltas, not at
// the top level. Keep the latest declaration by name in either format.
function requestTools(request) {
  const tools = new Map();
  for (const tool of [...(request.tools ?? []), ...request.input.flatMap(item => item.tools ?? [])]) tools.set(tool.name, tool);
  return [...tools.values()];
}

function replyBatch(res, tool) {
  assert.ok(tool, 'the installed backend did not offer Codemode');
  const raw = tool.type === 'custom';
  const item = { type: raw ? 'custom_tool_call' : 'function_call', id: raw ? 'ctc_batch' : 'fc_batch',
    call_id: 'call_release_batch', name: 'codemode', ...(raw ? { input: batchSource } : { arguments: JSON.stringify({ code: batchSource }) }) };
  const send = (type, data) => res.write(`data: ${JSON.stringify({ type, ...data })}\n\n`);
  res.setHeader('Content-Type', 'text/event-stream');
  send('response.created', { response: { id: 'fixture-batch', status: 'in_progress', output: [] } });
  send('response.output_item.added', { output_index: 0, item: { ...item, status: 'in_progress' } });
  send('response.output_item.done', { output_index: 0, item: { ...item, status: 'completed' } });
  send('response.completed', { response: { id: 'fixture-batch', status: 'completed', output: [item], usage: {
    input_tokens: 10, output_tokens: 12, total_tokens: 22,
  } } });
  res.end('data: [DONE]\n\n');
}

async function native(socket, action) {
  const conn = createConnection(socket);
  conn.setTimeout(5000, () => conn.destroy(new Error('native fixture timed out')));
  const lines = createInterface({ input: conn })[Symbol.asyncIterator]();
  let seq = 0;
  const call = async (method, params) => {
    const id = `f${++seq}`;
    conn.write(JSON.stringify({ type: 'request', id, method, params }) + '\n');
    const frame = JSON.parse((await lines.next()).value);
    assert.equal(frame.id, id);
    assert.equal(frame.ok, true, JSON.stringify(frame));
    return frame.result;
  };
  try {
    await once(conn, 'connect');
    return await action(call);
  } finally {
    conn.destroy();
  }
}

const [candidate, digest, promptFile, transcriptDir] = process.argv.slice(2);
const tuiWalkthrough = transcriptDir !== undefined;
test('packaged Hush clean install, OAuth, model selection, and resume' + (tuiWalkthrough ? ' with TUI walkthrough' : ''), {
  skip: !candidate, timeout: 90_000,
}, async t => {
  assert.equal(process.version, 'v22.19.0');
  assert.match(digest, /^[a-f0-9]{64}$/);
  const expectedPrompt = (await readFile(promptFile, 'utf8')).trimEnd();
  assert.match(expectedPrompt, /You are Hush/);
  const root = await realpath(await mkdtemp('/tmp/hush-release-'));
  let stopRemaining = async () => {};
  t.after(async () => {
    try { await stopRemaining(); }
    finally { await rm(root, { recursive: true, force: true }); }
  });
  const home = join(root, 'home'), tmp = join(root, 'tmp'), cwd = join(root, 'project');
  const bin = join(root, 'bin'), empty = join(root, 'empty'), old = join(root, 'old');
  const binary = join(bin, 'hush'), node = join(bin, 'node'), log = join(root, 'launches');
  await Promise.all([home, tmp, cwd, bin, empty, old].map(path => mkdir(path, { mode: 0o700 })));
  await copyFile(candidate, binary);
  await chmod(binary, 0o755);
  assert.equal(sha(await readFile(binary)), sha(await readFile(candidate)));
  await copyFile(process.execPath, node);
  await chmod(node, 0o755);
  assert.deepEqual((await readdir(bin)).sort(), ['hush', 'node']);
  const python = await invoke('/usr/bin/env', ['python3', '-c', 'import sys; print(sys.executable)']);
  assert.equal(python.code, 0, python.stderr);
  const walkthrough = (thread, action, ...args) => invoke(python.stdout.trim(),
    ['-c', pickerPTY, binary, thread, 'Loopback Codex reply', action, join(transcriptDir, `${action}.txt`), ...args], { cwd, env });
  if (tuiWalkthrough) await mkdir(transcriptDir, { recursive: true, mode: 0o700 });
  await writeFile(join(old, 'node'), '#!/bin/sh\nif [ "$1" = --version ]; then echo v22.18.9; else exit 99; fi\n', { mode: 0o700 });

  const account = 'release-fixture-account';
  const token = 'fixture.' + Buffer.from(JSON.stringify({ 'https://api.openai.com/auth': { chatgpt_account_id: account } })).toString('base64') + '.fixture';
  const seen = [], unexpected = [];
  const privateRequested = Promise.withResolvers(), privateClosed = Promise.withResolvers();
  let exchanges = 0;
  const provider = createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const body = Buffer.concat(chunks).toString();
    if (req.method === 'POST' && req.url === '/oauth/token') {
      const form = new URLSearchParams(body);
      if (form.get('grant_type') === 'authorization_code' && form.get('code') === 'fixture-code' && form.get('code_verifier')) {
        exchanges++;
        res.setHeader('Content-Type', 'application/json');
        res.end(JSON.stringify({ access_token: token, refresh_token: 'fixture-refresh', expires_in: 3600 }));
        return;
      }
    }
    if (req.method === 'POST' && req.url === '/fixture/responses' && req.headers.authorization === 'Bearer ' + token && req.headers['chatgpt-account-id'] === account) {
      try {
        const request = JSON.parse(body);
        if (JSON.stringify(request.input).includes('release-private-kill-only')) {
          res.once('close', privateClosed.resolve);
          res.writeHead(200, { 'Content-Type': 'text/event-stream' });
          res.flushHeaders();
          privateRequested.resolve(request);
          return;
        }
        seen.push(request);
        const batch = JSON.stringify(request.input).includes(batchPrompt) &&
          !request.input.some(item => item.call_id === 'call_release_batch');
        if (batch) replyBatch(res, requestTools(request).find(tool => tool.name === 'codemode'));
        else reply(res);
        return;
      } catch { /* report as unexpected */ }
    }
    unexpected.push([req.method, req.url]);
    res.writeHead(400).end();
  });
  provider.listen(0, '127.0.0.1');
  await once(provider, 'listening');
  t.after(() => { provider.closeAllConnections(); provider.close(); });
  const preload = join(root, 'fixture.mjs');
  await writeFile(preload, `
    import { appendFileSync, readFileSync, lstatSync } from 'node:fs';
    import { createHash } from 'node:crypto';
    import { dirname } from 'node:path';
    import { isMainThread } from 'node:worker_threads';
    if (isMainThread) {
      const entry = process.argv[1];
      appendFileSync(${JSON.stringify(log)}, JSON.stringify({ pid: process.pid, entry, args: process.argv.slice(2),
        version: process.version, mode: lstatSync(entry).mode & 0o777, dirMode: lstatSync(dirname(entry)).mode & 0o777,
        sha: createHash('sha256').update(readFileSync(entry)).digest('hex') }) + '\\n');
    }
    const fetchFixture = globalThis.fetch;
    const zlib = process.getBuiltinModule('node:zlib');
    if (zlib) zlib.zstdCompressSync = undefined;
    globalThis.WebSocket = undefined;
    globalThis.fetch = (input, init) => {
      const url = new URL(input instanceof Request ? input.url : input);
      const auth = url.origin === 'https://auth.openai.com';
      const codex = ['https://chatgpt.com', 'https://api.openai.com'].includes(url.origin) && url.pathname.endsWith('/responses');
      if (!auth && !codex) throw Error('Outside loopback fixture');
      const local = new URL('http://127.0.0.1:${provider.address().port}');
      local.pathname = auth ? url.pathname : '/fixture/responses';
      return fetchFixture(input instanceof Request ? new Request(local, input) : local, { ...init, redirect: 'error' });
    };
  `);
  const env = { HOME: home, TMPDIR: tmp, PATH: bin, LANG: 'C', TERM: 'xterm-256color', HUSH_NO_ANIM: '1',
    NODE_NO_WARNINGS: '1', NODE_OPTIONS: `--import=${preload}` };
  const run = (args, overrides = {}) => invoke(binary, args, { cwd, env: { ...env, ...overrides } });
  const good = async (args, pattern, overrides) => {
    const r = await run(args, overrides);
    assert.equal(r.code, 0, JSON.stringify(r));
    if (pattern) assert.match(r.stdout, pattern);
    return r.stdout;
  };
  const bad = async (args, pattern, overrides) => {
    const r = await run(args, overrides);
    assert.equal(r.code, 1, JSON.stringify(r));
    assert.match(r.stderr, pattern);
    assert.equal(r.stdout, '');
  };
  const profile = join(home, '.hush'), auth = join(profile, 'auth.json');
  const socket = join(profile, 'runtime', 'hush.sock');
  const launches = async () => (await readFile(log, 'utf8')).trimEnd().split('\n').map(s => JSON.parse(s));
  // Use the negotiated shutdown on this isolated socket. In a concurrent
  // start the last PID in the launch log can be a lock loser, not the writer.
  const stop = async () => {
    const result = await good(['stop', '--socket', socket, '--json']);
    assert.deepEqual(JSON.parse(result), { stopped: true });
    await absent(socket);
  };
  stopRemaining = async () => { await run(['stop', '--socket', socket]); };
  await bad(['status'], /Node\.js 22\.19\.0.*on PATH/, { PATH: empty });
  await bad(['status'], /found v22\.18\.9/, { PATH: old });
  await bad(['status', '--socket', join(root, 'absent.sock')], /socket|connect/i, { PATH: empty });
  await absent(log);
  assert.deepEqual(await readdir(home), []);
  await good(['auth', 'status'], /no Hush-managed credential/);
  // Exercise concurrent extraction and prove release selection ignores decoys.
  const decoys = [bin, cwd].map(dir => join(dir, 'hush-backend.mjs'));
  for (const path of decoys) await writeFile(path, 'throw Error("decoy");\n');
  await Promise.all(Array.from({ length: 3 }, () => good(['auth', 'status'], /no Hush-managed credential/)));
  for (const path of decoys) await rm(path);
  const override = join(root, 'trusted-override.mjs');
  await writeFile(override, 'console.log("explicit trusted override");\n');
  await good(['auth', 'status'], /^explicit trusted override\n$/, { NODE_OPTIONS: '', HUSH_BACKEND: override });
  await good(['--backend', override, 'auth', 'status'], /^explicit trusted override\n$/,
    { NODE_OPTIONS: '', HUSH_BACKEND: join(root, 'missing-override') });
  await absent(profile);
  const login = await invoke(python.stdout.trim(), ['-c', pty, binary], { cwd, env });
  assert.equal(login.code, 0, login.stdout + login.stderr);
  assert.match(login.stdout, /signed in with OAuth/);
  assert.equal(exchanges, 1);
  await good(['auth', 'status'], /OAuth configured/);

  await writeFile(join(profile, 'APPEND_SYSTEM.md'), 'release fixture personal supplement\n');
  await writeFile(join(cwd, 'AGENTS.md'), 'release fixture project supplement\n');
  await mkdir(join(cwd, '.hush'));
  await writeFile(join(cwd, '.hush', 'settings.json'), '{"codemode":true}', { mode: 0o600 });
  const profileBefore = (await readdir(profile)).sort();
  const authBefore = await readFile(auth);
  await good(['--no-session', '--print', 'release-private-only'], /^Loopback Codex reply\n$/);
  assert.deepEqual((await readdir(profile)).sort(), profileBefore);
  assert.deepEqual(await readFile(auth), authBefore);
  assert.deepEqual(await readdir(tmp), []);
  await absent(join(profile, 'runtime'));
  assert.match(JSON.stringify(seen[0]), /release-private-only/);
  assert.deepEqual(requestTools(seen[0]), []);

  await t.test('installed invocation-private host stops on client SIGKILL', async () => {
    // Do not substitute a --backend override for the released embedded bytes.
    const child = spawn(binary, ['--no-session', '--print', 'release-private-kill-only'], { cwd, env, stdio: 'ignore' });
    const exit = once(child, 'exit');
    const bounded = async (promise, label) => {
      const timer = new AbortController();
      try {
        return await Promise.race([promise, delay(7000, null, { signal: timer.signal }).then(() => assert.fail(label))]);
      } finally { timer.abort(); }
    };
    try {
      const request = await bounded(Promise.race([privateRequested.promise, exit.then(code => assert.fail(`early exit: ${code}`))]),
        'private CLI did not reach loopback provider');
      assert.deepEqual(requestTools(request), []);
    } finally { child.kill('SIGKILL'); await exit; }
    assert.deepEqual(await exit, [null, 'SIGKILL']);
    await bounded(privateClosed.promise, 'provider connection outlived private CLI');
    await absent(join(profile, 'runtime'));
    assert.deepEqual(await readFile(auth), authBefore);
    // A killed parent cannot unlink its directory. Only an inert socket inode
    // may remain briefly; neither the store nor the extracted bundle can remain.
    for (const name of await readdir(tmp)) {
      for (const entry of await readdir(join(tmp, name)).catch(e => e.code === 'ENOENT' ? [] : Promise.reject(e))) {
        assert.equal(entry, 'hush.sock');
        assert.ok(await lstat(join(tmp, name, entry)).then(s => s.isSocket(), e => e.code === 'ENOENT'));
      }
    }
  });

  const status = () => good(['status', '--json']).then(JSON.parse);
  const [first, ...arrivals] = await Promise.all(Array.from({ length: 4 }, status));
  for (const arrival of arrivals) assert.deepEqual(arrival, first);
  const lifecycle = await readFile(join(profile, 'runtime', 'hush.log'), 'utf8');
  assert.equal(lifecycle.split('\n').filter(row => row.endsWith(' listening')).length, 1);
  const samples = [];
  for (let i = 0; i < 20; i++) {
    const start = performance.now();
    const attached = JSON.parse(await good(['status', '--json', '--socket', socket], null, { PATH: empty }));
    samples.push(performance.now() - start);
    assert.equal(attached.epoch, first.epoch);
  }
  samples.sort((a, b) => a - b);
  const p95 = samples[18], targetMs = 100;
  const reportOnly = process.env.HUSH_PERF_MODE === 'report';
  t.diagnostic(JSON.stringify({ platform: process.platform, arch: process.arch, count: samples.length,
    boundary: 'installed process spawn through status stdout/exit, explicit warm socket, no Node on PATH',
    medianMs: +((samples[9] + samples[10]) / 2).toFixed(2), p95Ms: +p95.toFixed(2), targetMs, reportOnly }));
  if (reportOnly && p95 >= targetMs) {
    t.diagnostic(`performance target missed (report-only): warm attach p95 ${p95.toFixed(2)} ms, limit ${targetMs} ms`);
  } else {
    assert.ok(p95 < targetMs, `warm attach p95 ${p95.toFixed(2)} ms must be below ${targetMs} ms`);
  }
  const id = JSON.parse(await good(['thread', 'new', '--json'])).id;
  await good(['--print', '--thread', id, 'release-first-durable'], /^Loopback Codex reply\n$/);
  assert.equal(seen[1].model, 'gpt-6-sol');
  const directTools = ['read', 'bash', 'edit', 'write'];
  assert.deepEqual(requestTools(seen[1]).map(tool => tool.name), directTools);
  await good(['auth', 'logout'], /no Hush-managed credential/);
  if (tuiWalkthrough) {
    const relogin = await walkthrough(id, 'login');
    assert.equal(relogin.code, 0, relogin.stdout + relogin.stderr);
    const ptyAttach = relogin.stdout.match(/^pty_attach_ms=(\d+(?:\.\d+)?)$/m);
    assert.ok(ptyAttach, 'missing PTY measurement');
    t.diagnostic(JSON.stringify({ platform: process.platform, arch: process.arch,
      boundary: 'installed TUI process spawn through existing transcript text on PTY', attachMs: Number(ptyAttach[1]) }));
  } else {
    const relogin = await invoke(python.stdout.trim(), ['-c', pty, binary], { cwd, env });
    assert.equal(relogin.code, 0, relogin.stdout + relogin.stderr);
    assert.match(relogin.stdout, /signed in with OAuth/);
  }
  assert.equal(exchanges, 2);
  await good(['auth', 'status'], /OAuth configured/);
  const withCatalog = action => native(socket, async call => {
    const hello = await call('hello', { versions: [1], capabilities: { modelCatalog: [1], piEvents: [1] } });
    assert.deepEqual(hello.capabilities, { modelCatalog: 1 });
    return action(call);
  });
  const catalog = () => withCatalog(call => call('ext.modelCatalog.list', { threadId: id }));
  const report = await catalog();
  const alternative = report.models.find(row => row.available && row.provider === 'openai-codex' &&
    row.modelId !== report.current.modelId && report.models.filter(other => other.modelId.includes(row.modelId)).length === 1);
  assert.ok(alternative, 'need a second available native Codex model');
  const selected = alternative.modelId;
  const model = { provider: alternative.provider, modelId: selected };
  const threadState = async () => JSON.parse(await good(['status', '--thread', id, '--json']));
  const thinkingLevel = alternative.thinkingLevels.at(-1);
  assert.ok(thinkingLevel, 'available model has no selectable thinking level');
  if (tuiWalkthrough) {
    const picker = await walkthrough(id, 'model', selected);
    assert.equal(picker.code, 0, picker.stdout + picker.stderr);
  } else {
    await withCatalog(call => call('thread.configure', { threadId: id, change: { model, thinkingLevel } }));
  }
  const savedLevel = (await threadState()).agent.thinkingLevel;
  if (tuiWalkthrough) assert.ok(alternative.thinkingLevels.includes(savedLevel));
  else assert.equal(savedLevel, thinkingLevel);
  assert.deepEqual((await catalog()).current, model);
  const checkSelected = (request, marker) => {
    assert.equal(request.model, selected);
    const promptText = content => typeof content === 'string' ? content : (content ?? []).map(part => part.text).join('\n');
    const instructions = [promptText(request.instructions), ...(request.input ?? [])
      .filter(m => ['system', 'developer'].includes(m.role)).map(m => promptText(m.content))].join('\n');
    let at = 0;
    for (const text of [expectedPrompt, '## Active tools and working directory', 'release fixture personal supplement', 'release fixture project supplement']) {
      const found = instructions.indexOf(text, at);
      assert.ok(found >= at, 'missing or out of order rendered Hush prompt section');
      assert.equal(instructions.split(text).length, 2, 'duplicated rendered Hush prompt section');
      at = found + text.length;
    }
    assert.match(JSON.stringify(request.input), marker);
    assert.doesNotMatch(JSON.stringify(request.input), /release-private-only/);
  };
  await good(['--print', '--thread', id, 'release-after-configure'], /^Loopback Codex reply\n$/);
  checkSelected(seen[2], /release-after-configure/);
  await stop();
  const restarted = await status();
  assert.notEqual(restarted.epoch, first.epoch);
  assert.deepEqual((await catalog()).current, model);
  assert.equal((await threadState()).agent.thinkingLevel, savedLevel);
  await good(['--print', '--thread', id, 'release-after-restart'], /^Loopback Codex reply\n$/);
  checkSelected(seen[3], /release-after-restart/);
  assert.match(JSON.stringify(seen[3].input), /release-first-durable/);
  assert.deepEqual(requestTools(seen[3]).map(tool => tool.name), directTools);

  await good(['auth', 'logout'], /no Hush-managed credential/);
  const afterLogout = await readFile(auth, 'utf8').catch(e => {
    if (e.code !== 'ENOENT') throw e;
    return '{}';
  });
  assert.equal(JSON.parse(afterLogout || '{}')['openai-codex'], undefined);
  await bad(['--print', '--thread', id, 'release-after-logout'], /hush auth login/);
  assert.equal(seen.length, 4);
  await stop();

  await t.test('optional: installed Codemode enabled only on the host', async () => {
    await writeFile(join(profile, 'settings.json'), '{"codemode":true}', { mode: 0o600 });
    const signin = await invoke(python.stdout.trim(), ['-c', pty, binary], { cwd, env });
    assert.equal(signin.code, 0, signin.stdout + signin.stderr);
    assert.equal(exchanges, 3);
    await good(['--no-session', '--print', 'release-private-with-codemode-enabled'], /^Loopback Codex reply\n$/);
    assert.deepEqual(requestTools(seen[4]), []);
    await writeFile(join(cwd, 'first.txt'), 'KEY=north\n');
    await writeFile(join(cwd, 'second.txt'), 'KEY=south\n');
    await good(['--print', '--thread', id, batchPrompt], /^Loopback Codex reply\n$/);
    assert.deepEqual(requestTools(seen[5]).map(tool => tool.name), [...directTools, 'codemode']);
    const batchResult = seen[6].input.find(item => item.call_id === 'call_release_batch' && item.type.endsWith('_output'));
    assert.match(batchResult?.output ?? '', /Script completed[\s\S]*north, south/);
    assert.doesNotMatch(batchResult.output, /KEY=/);
    assert.equal(seen.length, 7);
    await good(['auth', 'logout'], /no Hush-managed credential/);
    await stop();
  });
  await t.test('installed image worker decodes and resizes an attachment', async () => {
    const signin = await invoke(python.stdout.trim(), ['-c', pty, binary], { cwd, env });
    assert.equal(signin.code, 0, signin.stdout + signin.stderr);
    const threadId = JSON.parse(await good(['thread', 'new', '--json'])).id;
    // A 4100×8 solid RGB PNG exceeds the host's 4096-pixel axis cap for
    // fitted output, but is within the accepted input bounds.
    const data = 'iVBORw0KGgoAAAANSUhEUgAAEAQAAAAICAIAAAD/pw2ZAAAAkUlEQVR42u3bQQkAMAzAwHqplmqZfwfTETg4Bfln9h4AAAAAAAAAAAAAABAyEgAAAAAAAAAAAAAAQIsZAAAAAAAAAAAAAAAAYswAAAAAAAAAAAAAAAAQYwYAAAAAAAAAAAAAAIAYMwAAAAAAAAAAAAAAAMSYAQAAAAAAAAAAAAAAIMYMAAAAAAAAAAAAAAAAMR+6l5/qSnJ7ugAAAABJRU5ErkJggg==';
    const caption = 'release-fitted-image';
    await native(socket, async call => {
      const hello = await call('hello', { versions: [1], capabilities: { images: [1] } });
      assert.deepEqual(hello.capabilities, { images: 1 });
      const { submissionId } = await call('ext.images.submit', {
        threadId, requestId: caption, whenBusy: 'reject', text: caption, images: [{ mimeType: 'image/png', data }],
      });
      const settled = await call('submission.wait', { threadId, submissionId });
      assert.equal(settled.status, 'done', JSON.stringify(settled));
    });
    const request = seen.at(-1);
    const content = request.input.find(m => m.role === 'user' && m.content.some(p => p.text === caption)).content;
    const fitted = content.find(p => p.type === 'input_image')?.image_url;
    assert.match(fitted, /^data:image\/(?:png|jpeg);base64,/);
    assert.notEqual(fitted, `data:image/png;base64,${data}`);
    await stop();
  });
  assert.deepEqual(unexpected, []);
  await absent(join(home, '.pi'));
  const records = await launches();
  assert.equal(new Set(records.map(r => r.entry)).size, records.length);
  for (const r of records) {
    assert.equal(r.version, 'v22.19.0');
    assert.equal(r.sha, digest, 'extracted bytes differ from lockfile-built artifact');
    assert.equal(r.mode, 0o400);
    assert.equal(r.dirMode, 0o700);
    await absent(dirname(r.entry));
  }
});
