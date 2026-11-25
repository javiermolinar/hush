// Check the archive that will be uploaded against the reviewed byte inputs
// and attribution inventory. Missing or changed inputs fail verification.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { lstatSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';

const [install, os] = process.argv.slice(2);
assert.ok(install && ['linux', 'darwin'].includes(os));
const root = 'app/redistribution';
const notices = join(install, 'THIRD_PARTY_NOTICES');
const read = path => {
  assert.ok(lstatSync(path).isFile(), `expected regular file: ${path}`);
  return readFileSync(path);
};
const sha = bytes => createHash('sha256').update(bytes).digest('hex');
const table = bytes => bytes.toString('utf8').trimEnd().split('\n').slice(1).map(row => row.split('\t'));
assert.deepEqual(readdirSync(install).sort(), ['LICENSE', 'THIRD_PARTY_NOTICES', 'hush']);
assert.deepEqual(read(join(install, 'LICENSE')), read('LICENSE'));
assert.ok(lstatSync(notices).isDirectory());
assert.deepEqual(readdirSync(notices).sort(), ['INDEX.tsv', 'README.txt', 'texts']);
assert.ok(read(join(notices, 'README.txt')).length > 0);
assert.ok(lstatSync(join(notices, 'texts')).isDirectory());
const index = read(join(notices, 'INDEX.tsv'));
assert.deepEqual(index, read(join(root, 'THIRD_PARTY_NOTICES', 'INDEX.tsv')));
const rows = table(index);
const texts = new Set();
for (const row of rows) {
  assert.equal(row.length, 5, row.join('\t'));
  const m = /^texts\/([a-f0-9]{64})\.txt$/.exec(row[4]);
  assert.ok(m, `invalid notice reference: ${row[4]}`);
  if (!texts.has(m[1])) assert.equal(sha(read(join(notices, row[4]))), m[1], row[4]);
  texts.add(m[1]);
}
assert.deepEqual(readdirSync(join(notices, 'texts')).sort(), [...texts].sort().map(h => `${h}.txt`));
const indexed = new Set(rows.map(([name, version]) => `${name}@${version}`));
const lock = read('package-lock.json');
assert.equal(sha(lock), '8371854dd34377e20c81ca41830abb99260c5008af6b8310db063418dc8ad566', 'refresh attribution for a changed npm lock');
const pkgs = JSON.parse(lock).packages;
const audited = new Set();
for (const [name, version, terms, , path, integrity] of table(read(join(root, 'embedded-npm-lock.tsv')))) {
  assert.equal(pkgs[path]?.integrity, integrity, path);
  assert.equal(pkgs[path].version, version, path);
  assert.equal(pkgs[path].license, terms, path);
  assert.ok(indexed.has(`${name}@${version}`), `missing license: ${name}@${version}`);
  audited.add(path);
}
const input = JSON.parse(read('dist/hush-backend-inputs.json'));
const contributing = new Set(['node_modules/quickjs-wasi']); // embedded explicitly
for (const meta of Object.values(input)) {
  for (const output of Object.values(meta.outputs)) {
    for (const [path, { bytesInOutput }] of Object.entries(output.inputs)) {
      const m = /^(.*node_modules\/(?:@[^/]+\/)?[^/]+)\//.exec(path);
      if (bytesInOutput && m) contributing.add(m[1]);
    }
  }
}
assert.deepEqual([...contributing].sort(), [...audited].sort(), 'npm byte contributors changed');

const bundle = read('dist/hush-backend.mjs');
const bytes = read(join(install, 'hush'));
assert.ok(bytes.includes(bundle), 'archive executable does not contain measured backend');
const workerSources = Array.from(bundle.toString('utf8').matchAll(/data:text\/javascript;base64,([A-Za-z0-9+/=]{100,})/g),
  ([, url]) => Buffer.from(url, 'base64').toString('utf8'));
assert.equal(workerSources.length, 2);
const embeddedWasm = new Set();
for (const src of [bundle.toString('utf8'), ...workerSources]) {
  for (const [, payload] of src.matchAll(/\b(AGFzbQE[A-Za-z0-9+/=]{120,})/g)) {
    embeddedWasm.add(sha(Buffer.from(payload, 'base64')));
  }
}
assert.deepEqual([...embeddedWasm].sort(), table(read(join(root, 'wasm.tsv'))).map(row => row[1]).sort(), 'WASM bytes changed');

const go = execFileSync('go', ['version', '-m', join(install, 'hush')], { encoding: 'utf8' });
assert.match(go.split('\n')[0], /: go1\.27\.4$/);
const linked = [...go.matchAll(/^\s*dep\s+(\S+)\s+(v\S+)\s+(h1:\S+)$/gm)].map(([, ...fields]) => fields.join('\t'));
const expected = table(read(join(root, 'go-linked.tsv'))).filter(([name]) =>
  !(name === 'github.com/ebitengine/purego' && os === 'linux') &&
  !(name === 'github.com/godbus/dbus/v5' && os === 'darwin'));
for (const [name, version] of expected) assert.ok(indexed.has(`${name}@${version}`), `missing license: ${name}@${version}`);
assert.deepEqual(linked.sort(), expected.map(fields => fields.join('\t')).sort(), 'linked Go modules changed');
console.log(JSON.stringify({ noticeIndexSha256: sha(index), entries: rows.length, texts: texts.size,
  npmContributors: audited.size, wasmPayloads: embeddedWasm.size, goDependencies: linked.length }));
