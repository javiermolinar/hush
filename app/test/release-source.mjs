// Capture the inputs to a local or CI candidate, including uncommitted files
// in the build tree. Do not equate a dirty candidate with a build of HEAD.
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { lstatSync, readFileSync, readlinkSync } from 'node:fs';

const git = args => execFileSync('git', args, { encoding: 'utf8' });
const sha = data => createHash('sha256').update(data).digest('hex');
const inputs = git(['ls-files', '--cached', '--others', '--exclude-standard', '-z', '--',
  'app/', 'backend/', 'protocol/', 'sdk/', 'server/', 'tui/', 'go.work', 'go.work.sum',
  'package.json', 'package-lock.json', 'tsconfig.json', 'mise.toml', 'LICENSE', '.github/workflows/release-candidate.yml']);
const files = Array.from(new Set(inputs.split('\0').filter(Boolean))).sort().map(path => {
  try {
    const st = lstatSync(path);
    if (st.isSymbolicLink()) return { path, link: readlinkSync(path) };
    return { path, sha256: sha(readFileSync(path)) };
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    return { path, missing: true };
  }
});
console.log(JSON.stringify({ version: 1, git: git(['rev-parse', 'HEAD']).trim(), files }, null, 2));
