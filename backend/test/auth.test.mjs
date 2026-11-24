import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { ModelRuntime } from "@earendil-works/pi-coding-agent";
import providers from "../../app/internal/authproviders/providers.json" with { type: "json" };

const exec = promisify(execFile);
const bundle = fileURLToPath(new URL("../../dist/hush-backend.mjs", import.meta.url));

test("the Hush picker offers the pinned Pi built-in OAuth providers", async () => {
  const runtime = await ModelRuntime.create({ credentials: {}, modelsPath: null, refreshOnCreate: false });
  assert.deepEqual(providers.map(p => p.id).sort(), runtime.getProviders().filter(p => p.auth.oauth).map(p => p.id).sort());
  assert.equal(providers[0].id, "openai-codex");
});

// Drive only a local, offline login. Secrets sent at the hidden prompt must
// never echo; on every exit the child must restore the terminal's flags.
const pty = String.raw`
import os, re, select, subprocess, sys, termios, time, urllib.parse
node, entry, provider, mode = sys.argv[1:]
master, slave = os.openpty()
initial = termios.tcgetattr(slave)
child = subprocess.Popen([node, entry, 'auth', 'login', provider], stdin=slave, stdout=slave, stderr=slave)
try:
    seen = b''
    sent = False
    method_chosen = False
    deadline = time.monotonic() + 6
    while time.monotonic() < deadline:
        if not select.select([master], [], [], .05)[0]:
            if child.poll() is not None:
                break
            continue
        chunk = os.read(master, 4096)
        seen += chunk
        sys.stdout.buffer.write(chunk)
        sys.stdout.buffer.flush()
        if provider == 'anthropic' and not method_chosen and b'[2] Copy code' in seen:
            os.write(master, b'2\n')
            method_chosen = True
        if not sent and b'(hidden)' in seen:
            assert not termios.tcgetattr(slave)[3] & termios.ECHO
            if mode == 'cancel':
                data = b'\x03'
            elif provider == 'openai':
                url = re.search(rb'https://auth.openai.com/api/accounts/authorize\?[^\r\n ]+', seen)[0]
                params = urllib.parse.parse_qs(urllib.parse.urlsplit(url.decode()).query)
                query = urllib.parse.urlencode({'state': params['state'][0], 'code': 'fixture-auth-code', 'client_id': 'fixture-client'})
                data = (params['redirect_uri'][0] + '?' + query + '\n').encode()
            else:
                data = b'fixture-auth-code\n'
            os.write(master, data)
            sent = True
    result = child.wait(timeout=1)
    assert b'fixture-auth-code' not in seen
    mask = termios.ECHO | termios.ICANON
    assert termios.tcgetattr(slave)[3] & mask == initial[3] & mask
    sys.exit(result)
finally:
    if child.poll() is None:
        child.kill()
        child.wait()
    os.close(master)
    os.close(slave)
`;

const offline = String.raw`
import assert from 'node:assert/strict';
const reply = body => new Response(JSON.stringify(body), { status: 200 });
globalThis.fetch = async (input, opts) => {
  const url = new URL(input);
  if (url.href === 'https://auth.openai.com/api/accounts/oauth/token') {
    assert.equal(opts.body.get('code'), 'fixture-auth-code');
    assert.equal(opts.body.get('client_id'), 'fixture-client');
    return reply({ access_token: 'fixture-openai-access', refresh_token: 'fixture-openai-refresh',
      id_token: 'fixture-id-token', scope: 'chatgpt.tokens.use.direct', expires_in: 3600 });
  }
  if (url.href === 'https://platform.claude.com/v1/oauth/token') {
    const body = JSON.parse(opts.body);
    assert.equal(body.code, 'fixture-auth-code');
    assert.equal(body.redirect_uri, 'https://platform.claude.com/oauth/code/callback');
    return reply({ access_token: 'fixture-anthropic-access', refresh_token: 'fixture-anthropic-refresh', expires_in: 3600 });
  }
  if (url.href === 'https://openrouter.ai/api/v1/auth/keys') {
    assert.equal(JSON.parse(opts.body).code, 'fixture-auth-code');
    return reply({ key: 'fixture-openrouter-key' });
  }
  if (url.href === 'https://auth.x.ai/oauth2/device/code') {
    return reply({ device_code: 'fixture-private-device-code', user_code: 'TEST-USER',
      verification_uri: process.env.BAD_DEVICE_URL === '1' ? 'https://auth.x.ai.example.org/activate' : 'https://auth.x.ai/activate',
      expires_in: 60, interval: 0.01 });
  }
  if (url.href === 'https://auth.x.ai/oauth2/token') {
    assert.equal(process.env.BAD_DEVICE_URL, undefined, 'polling must stop on a rejected verification URL');
    assert.equal(opts.body.get('device_code'), 'fixture-private-device-code');
    return reply({ access_token: 'fixture-xai-access', refresh_token: 'fixture-xai-refresh', expires_in: 3600 });
  }
  throw new Error('Unexpected request in offline fixture');
};
`;

async function setup(t) {
  const home = await mkdtemp("/tmp/hush-oauth-");
  t.after(() => rm(home, { recursive: true, force: true }));
  const dir = join(home, ".hush"), preload = join(home, "offline.mjs");
  await mkdir(dir, { mode: 0o700 });
  await writeFile(preload, offline);
  const env = { HOME: home, PATH: process.env.PATH, NODE_NO_WARNINGS: "1", NODE_OPTIONS: `--import=${preload}`,
    PI_OAUTH_CALLBACK_HOST: "192.0.2.1" }; // Must be ignored even by ChatGPT's module-level binding.
  const run = async (cmd, args, overrides = {}) => {
    try { return { code: 0, ...await exec(cmd, args, { env: { ...env, ...overrides }, cwd: home, timeout: 10_000 }) }; }
    catch (err) {
      if (typeof err.code !== "number") throw err;
      return { code: err.code, stdout: err.stdout, stderr: err.stderr };
    }
  };
  return { home, dir, env,
    auth: (...args) => run(process.execPath, [bundle, "auth", ...args]),
    login: (id, mode = "login", overrides) => run("python3", ["-c", pty, process.execPath, bundle, id, mode], overrides),
  };
}

test("browser OAuth keeps separate keyring entries and preserves Pi's OAuth metadata", { timeout: 20_000 }, async t => {
  const { home, dir, env, auth, login } = await setup(t);
  // A private pipe fixture; never opens the user's OS keyring.
  const helper = join(home, "helper.mjs");
  const items = join(home, "items");
  await mkdir(items, { mode: 0o700 });
  await writeFile(helper, `#!${process.execPath}
    import { readFileSync, writeFileSync, rmSync, existsSync } from 'node:fs';
    import { join } from 'node:path';
    const { operation, scope, provider = 'openai-codex', record } = JSON.parse(readFileSync(0, 'utf8'));
    if (!/^[a-f0-9]{64}$/.test(scope) || !/^[a-z-]+$/.test(provider)) process.exit(2);
    const file = join(${JSON.stringify(items)}, provider);
    if (operation === 'set') writeFileSync(file, JSON.stringify(record), { mode: 0o600 });
    else if (operation === 'delete') rmSync(file, { force: true });
    else if (operation === 'get' && existsSync(file)) process.stdout.write(readFileSync(file));
  `, { mode: 0o700 });
  env.HUSH_KEYRING_HELPER = helper;
  await writeFile(join(dir, "credential-store"), "keyring\n", { mode: 0o600 });
  const codex = JSON.stringify({ type: "oauth", access: "fixture-codex", refresh: "fixture-codex-refresh",
    expires: Date.now() + 3_600_000, accountId: "fixture-account" });
  await writeFile(join(items, "openai-codex"), codex, { mode: 0o600 });
  const chatgpt = await login("openai");
  assert.equal(chatgpt.code, 0, chatgpt.stdout + chatgpt.stderr);
  assert.match(chatgpt.stdout, /openai: signed in with OAuth/);
  assert.doesNotMatch(chatgpt.stdout, /fixture-(openai|auth-code|client)/);
  const saved = JSON.parse(await readFile(join(items, "openai"), "utf8"));
  assert.equal(saved.clientId, "fixture-client");
  assert.deepEqual(saved.scopes, ["chatgpt.tokens.use.direct"]);
  const id = JSON.parse(await readFile(join(dir, "openai-device.json"), "utf8")).deviceId;
  assert.ok(chatgpt.stdout.includes(encodeURIComponent(`urn:uuid:${id}`)));
  const cancelled = await login("openai", "cancel");
  assert.equal(cancelled.code, 130, cancelled.stdout + cancelled.stderr);
  assert.ok(cancelled.stdout.includes(encodeURIComponent(`urn:uuid:${id}`)));
  assert.deepEqual(JSON.parse(await readFile(join(items, "openai"), "utf8")), saved);

  const router = await login("openrouter");
  assert.equal(router.code, 0, router.stdout + router.stderr);
  assert.match(router.stdout, /openrouter: signed in with OAuth/);
  assert.equal(JSON.parse(await readFile(join(items, "openrouter"), "utf8")).refresh, "");
  for (const id of ["openai", "openrouter"]) {
    assert.deepEqual(await auth("status", id), { code: 0, stdout: `${id}: OAuth configured\n`, stderr: "" });
  }
  assert.deepEqual(await auth("logout", "openai"), { code: 0, stdout: "openai: no Hush-managed credential\n", stderr: "" });
  assert.deepEqual(await auth("status"), { code: 0, stdout: "openai-codex: OAuth configured\n", stderr: "" });
  assert.equal(await readFile(join(items, "openai-codex"), "utf8"), codex);
  await assert.rejects(stat(join(dir, "auth.json")), { code: "ENOENT" });
});

test("device OAuth prints only validated browser instructions and saves the selected provider", { timeout: 15_000 }, async t => {
  const { dir, auth, login } = await setup(t);
  const authFile = join(dir, "auth.json");
  const blocked = await login("xai", "login", { BAD_DEVICE_URL: "1" });
  assert.equal(blocked.code, 1, blocked.stdout + blocked.stderr);
  assert.doesNotMatch(blocked.stdout + blocked.stderr, /example\.org|TEST-USER|fixture-private-device-code/);
  const savedBefore = await readFile(authFile, "utf8");
  assert.doesNotMatch(savedBefore, /xai/);
  const device = await login("xai");
  assert.equal(device.code, 0, device.stdout + device.stderr);
  assert.match(device.stdout, /https:\/\/auth\.x\.ai\/activate/);
  assert.match(device.stdout, /Enter this one-time code in the browser: TEST-USER/);
  assert.doesNotMatch(device.stdout, /fixture-(private-device-code|xai-)/);
  assert.equal(JSON.parse(await readFile(authFile, "utf8")).xai.type, "oauth");
  assert.deepEqual(await auth("status", "xai"), { code: 0, stdout: "xai: OAuth configured\n", stderr: "" });

  const anthropic = await login("anthropic");
  assert.equal(anthropic.code, 0, anthropic.stdout + anthropic.stderr);
  assert.match(anthropic.stdout, /anthropic: signed in with OAuth/);
  const file = JSON.parse(await readFile(authFile, "utf8"));
  assert.equal(file.anthropic.type, "oauth");
  assert.deepEqual(await auth("logout", "xai"), { code: 0, stdout: "xai: no Hush-managed credential\n", stderr: "" });
  assert.deepEqual(JSON.parse(await readFile(authFile, "utf8")), { anthropic: file.anthropic });
});
