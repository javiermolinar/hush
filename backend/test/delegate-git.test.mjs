import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { access, mkdir, mkdtemp, realpath, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test } from "node:test";
import { gitArguments } from "../../dist/backend/src/delegate-git.js";
import { allowsTool } from "../../dist/backend/src/tool-policy.js";
import { workspaceGitOutput } from "../../dist/backend/src/workspace-trust.js";

const prefix = ["--no-replace-objects", "--literal-pathspecs", "-c", "log.showSignature=false", "-c", "color.ui=never",
  "-c", "format.pretty=medium"];
const argv = (...args) => {
  const parsed = gitArguments(...args);
  assert.ok("argv" in parsed, JSON.stringify(parsed));
  return parsed.argv.slice(prefix.length);
};
const rejected = (command, args, pattern) => {
  const parsed = gitArguments(command, args);
  assert.ok("error" in parsed, `${command} ${JSON.stringify(args)} must be rejected`);
  assert.match(parsed.error, pattern);
};

test("delegate Git keeps revisions out of option position and scopes paths to pwd", () => {
  assert.deepEqual(gitArguments("status", []).argv.slice(0, prefix.length), prefix);
  assert.deepEqual(argv("diff", ["--stat", "main...HEAD", "--", "a.txt"], "pkg"),
    ["diff", "--no-color", "--no-ext-diff", "--no-textconv", "--ignore-submodules=dirty", "--stat", "--end-of-options", "main...HEAD", "--", "pkg/a.txt"]);
  assert.deepEqual(argv("status", ["-sb", "a.txt", "../b.txt"], "pkg/sub"),
    ["status", "--ignore-submodules=dirty", "-sb", "--", "pkg/sub/a.txt", "pkg/b.txt"]);
  assert.deepEqual(argv("show", ["HEAD~1:src/x.ts", "--format=%h %s"]),
    ["show", "--no-color", "--no-ext-diff", "--no-textconv", "--no-show-signature", "--format=%h %s", "--end-of-options", "HEAD~1:src/x.ts", "--"]);
  assert.deepEqual(argv("log", ["--oneline"]), ["log", "--no-color", "--no-ext-diff", "--no-textconv", "--no-show-signature",
    "--oneline", "--max-count=100", "--end-of-options", "--"]);
  assert.deepEqual(argv("log", ["-5", "--since", "2 weeks ago", "-n", "3", "-Sneedle"]).slice(5),
    ["--max-count=5", "--since=2 weeks ago", "-n3", "-Sneedle", "--end-of-options", "--"]);
  // After "--", everything is a path, even text that looks like an option.
  assert.deepEqual(argv("diff", ["--", "--output=x"]).slice(-2), ["--", "--output=x"]);
});

test("delegate Git keeps paths and revisions inside the checkout", () => {
  // `git diff --end-of-options /outside --` would silently become --no-index.
  for (const [command, args, pwd] of [["diff", ["/etc/hosts"]], ["diff", ["--", "/etc/hosts"]], ["diff", ["../../etc/hosts"]],
    ["diff", ["--", "../x"]], ["diff", ["--", "../../x"], "pkg"], ["status", ["/etc"]], ["log", ["--", ".."]], ["show", [".."]]])
    assert.match(gitArguments(command, args, pwd).error, /stay inside the checkout/, JSON.stringify(args));
  assert.deepEqual(argv("diff", ["--", "../x"], "pkg").slice(-1), ["x"]);
});

test("only a read-only subagent's direct calls can use delegate Git", () => {
  const workspace = { cwd: "/w", root: "/w", device: "1", inode: "2", birth: "3", grant: "g" };
  assert.equal(allowsTool({ kind: "read-only", workspace }, "git", false), true);
  assert.equal(allowsTool({ kind: "read-only", workspace, pwd: "pkg" }, "git", false), true);
  assert.equal(allowsTool({ kind: "read-only", workspace }, "git", true), false);
  // A path-restricted worker must not read the rest of the repository through Git.
  assert.equal(allowsTool({ kind: "worker", workspace, paths: ["a.txt"] }, "git", false), false);
  assert.equal(allowsTool({ kind: "subagent-write", workspace }, "git", false), false);
  assert.equal(allowsTool({ kind: "default", workspace }, "git", false), false);
});

test("delegate Git rejects options that write files, run programs or change Git's config", () => {
  for (const [command, args] of [
    ["diff", ["--output=/tmp/x"]], ["diff", ["--output", "/tmp/x"]], ["diff", ["--ext-diff"]], ["diff", ["--textconv"]],
    ["diff", ["--no-index", "/etc/passwd", "x"]], ["log", ["--show-signature"]], ["show", ["-c", "core.pager=sh"]],
    ["status", ["-C", "/"]], ["log", ["--exec=sh"]], ["diff", ["--git-dir=/tmp"]], ["log", ["--all-match", "--output=x"]],
    ["log", ["-o"]], ["diff", ["-M50%"]], ["status", ["--ignored"]], ["log", ["--constructor"]], ["status", ["-v"]],
    ["status", ["--verbose"]], ["diff", ["--out=x"]], ["log", ["--outp", "x"]],
  ]) rejected(command, args, /does not allow/);
  // A bare name could be a pretty.<name> alias from the repository's config.
  rejected("log", ["--format=myalias"], /Invalid value/);
  assert.ok("argv" in gitArguments("log", ["--format=fuller"]));
  assert.ok("argv" in gitArguments("log", ["--pretty=format:%h"]));
  rejected("log", ["--format=%G?"], /Invalid value/);
  // Git accepts a modifier between % and the placeholder.
  for (const modified of ["%+G?", "%-G?", "% GS", "%h %+GK"]) rejected("log", [`--format=${modified}`], /Invalid value/);
  rejected("log", ["--pretty", "%GS"], /Invalid value/);
  rejected("log", ["--max-count=1e9"], /Invalid value/);
  rejected("log", ["--date=format:%s"], /Invalid value/);
  rejected("log", ["--author"], /needs a value/);
  rejected("diff", ["a\nb"], /single-line/);
  rejected("diff", [""], /single-line/);
});

test("delegate Git does not run configured diff, textconv or signature programs", { timeout: 15_000 }, async t => {
  const dir = await realpath(await mkdtemp("/tmp/hush-delegate-git-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const repo = join(dir, "repo"), marker = join(dir, "ran");
  await mkdir(repo);
  const env = { HOME: dir, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" };
  const git = (...args) => execFileSync("git", args, { cwd: repo, env, stdio: "pipe" }).toString();
  git("init", "-q");
  git("config", "user.email", "fixture@example.test"); git("config", "user.name", "Fixture");
  await writeFile(join(repo, "a.txt"), "one\n");
  git("add", "."); git("commit", "-qm", "base");
  // A commit with a signature header, so log and show have one to verify.
  const tree = git("rev-parse", "HEAD^{tree}").trim(), parent = git("rev-parse", "HEAD").trim();
  const signed = execFileSync("git", ["hash-object", "-t", "commit", "-w", "--stdin"], { cwd: repo, env, input:
    `tree ${tree}\nparent ${parent}\nauthor a <a@b> 1 +0000\ncommitter a <a@b> 1 +0000\n` +
    "gpgsig -----BEGIN PGP SIGNATURE-----\n \n abc\n -----END PGP SIGNATURE-----\n\nsigned\n" }).toString().trim();
  git("update-ref", "HEAD", signed);
  await writeFile(join(repo, "a.txt"), "two\n");
  await writeFile(join(repo, ".gitattributes"), "*.txt diff=evil\n");
  git("config", "diff.external", `touch ${marker}`);
  git("config", "diff.evil.textconv", `touch ${marker}; cat`);
  git("config", "log.showSignature", "true");
  // Git runs gpg.program without a shell.
  const gpg = join(dir, "gpg.sh");
  await writeFile(gpg, `#!/bin/sh\ntouch ${marker}\nexit 1\n`, { mode: 0o755 });
  git("config", "gpg.program", gpg);
  git("config", "core.pager", `touch ${marker}`);
  // A repository default format can name a signature placeholder.
  git("config", "format.pretty", "format:%G? %s");
  const run = async (command, args = []) => {
    const out = await workspaceGitOutput(repo, t.signal, gitArguments(command, args).argv, 4096);
    return { ...out, text: out.text.toString() };
  };
  const diff = await run("diff");
  assert.equal(diff.ok, true);
  assert.match(diff.text, /^-one\n\+two$/m);
  assert.match((await run("log", ["--oneline"])).text, /signed/);
  assert.match((await run("log", ["-1"])).text, /^commit [0-9a-f]+\n/, "the forced default format, not format.pretty");
  assert.match((await run("show", ["--stat", "HEAD~1"])).text, /a\.txt/);
  assert.match((await run("status", ["--short"])).text, / M a\.txt/);
  // A file named "--" lets an implicit --no-index diff read outside files.
  await writeFile(join(repo, "--"), "x\n");
  assert.ok("error" in gitArguments("diff", [join(dir, "outside")]));
  await assert.rejects(access(marker), { code: "ENOENT" }, "no configured program ran");
  const bad = await run("show", ["no-such-revision"]);
  assert.equal(bad.ok, false);
  assert.match(bad.text, /no-such-revision/);
  await writeFile(join(repo, "big.txt"), "x".repeat(8192));
  const big = await run("diff", ["--", "big.txt"]);
  git("add", "big.txt");
  const cut = await run("diff", ["--cached", "--", "big.txt"]);
  assert.equal(big.text, "", "an untracked file has no diff");
  assert.equal(cut.ok, true);
  assert.equal(cut.truncated, true);
  assert.ok(cut.text.length <= 4096);
});
