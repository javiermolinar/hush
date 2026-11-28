import { isAbsolute, posix } from "node:path";
import { defineExtension, defineTool } from "@earendil-works/pi-durable";
import { requireExecutionWorkspace } from "./execution-workspace.js";
import { checkFilters } from "./rewind-git.js";
import type { ToolPolicy } from "./tool-policy.js";
import { workspaceGitOutput, type WorkspaceTrust } from "./workspace-trust.js";

export const delegateGitName = "git";
export const gitCommands = ["status", "diff", "show", "log"] as const;
type Command = typeof gitCommands[number];

const outputBytes = 48 * 1024, errorBytes = 2048, defaultCommits = 100;
const text = (value: string) => value.isWellFormed() && !/[\p{Cc}\p{Zl}\p{Zp}]/u.test(value);
const anyText = /^[^]{1,256}$/u;
const count = /^\d{1,4}$/;
const date = /^[\w :.,+\-/]{1,64}$/;

// Options here only select or format output. Anything that writes a file,
// runs a program, reads outside the repository or changes Git's config
// (--output, --ext-diff, --textconv, --no-index, --show-signature, -c, -C)
// is absent, and revisions follow --end-of-options so none can act as one.
const diffFlags = ["--stat", "--shortstat", "--numstat", "--summary", "--name-only", "--name-status", "--raw", "-p", "--patch",
  "-s", "--no-patch", "-w", "--ignore-all-space", "-b", "--ignore-space-change", "--ignore-blank-lines", "-M", "--find-renames",
  "--no-renames", "-W", "--function-context", "-R", "--minimal", "--patience", "--histogram", "--word-diff"];
const diffValues: Record<string, RegExp> = { "-U": count, "--unified": count, "--diff-filter": /^[ACDMRTUXB*]{1,16}$/i,
  "--word-diff": /^(plain|porcelain|none)$/, "--stat": /^\d{1,4}(,\d{1,4}){0,2}$/ };
// %G placeholders verify signatures, which runs the configured GPG program,
// also with a "+", "-" or " " modifier (%+G?). A bare name could be a
// pretty.<name> alias from the repository's config, so only built-in names
// and literal formats are accepted.
const format = /^(oneline|short|medium|full|fuller|reference|email|raw|mboxrd|(?:t?format:|(?=[^]*%))(?![^]*%[-+ ]?G)[^]{1,256})$/u;
const commitFlags = ["--oneline", "--abbrev-commit", "--no-abbrev-commit", "--decorate", "--no-decorate"];
const commitValues: Record<string, RegExp> = { "--format": format, "--pretty": format,
  "--date": /^(relative|local|iso|iso8601|iso-strict|rfc|rfc2822|short|raw|unix|human|default)$/,
  "--decorate": /^(short|full|auto|no)$/ };
const flags: Record<Command, ReadonlySet<string>> = {
  // status -v shows a diff with textconv, and status has no --no-textconv.
  status: new Set(["-s", "--short", "-b", "--branch", "-sb", "-bs", "--long", "--porcelain", "-u", "--untracked-files",
    "--ahead-behind", "--no-ahead-behind", "--show-stash"]),
  diff: new Set([...diffFlags, "--cached", "--staged", "--merge-base"]),
  show: new Set([...diffFlags, ...commitFlags]),
  log: new Set([...diffFlags, ...commitFlags, "--graph", "--reverse", "--first-parent", "--no-merges", "--merges", "--all", "--follow",
    "--date-order", "--topo-order", "--left-right"]),
};
const values: Record<Command, Readonly<Record<string, RegExp>>> = {
  status: { "--porcelain": /^v[12]$/, "-u": /^(no|normal|all)$/, "--untracked-files": /^(no|normal|all)$/ },
  diff: diffValues,
  show: { ...diffValues, ...commitValues },
  log: { ...diffValues, ...commitValues, "-n": count, "--max-count": count, "--skip": count, "--since": date, "--after": date,
    "--until": date, "--before": date, "--author": anyText, "--committer": anyText, "--grep": anyText, "-S": anyText, "-G": anyText },
};
// Always applied: never color, external diff drivers, conversion filters for
// display, signature checks, or a submodule's own status.
const forced: Record<Command, readonly string[]> = {
  status: ["--ignore-submodules=dirty"],
  diff: ["--no-color", "--no-ext-diff", "--no-textconv", "--ignore-submodules=dirty"],
  show: ["--no-color", "--no-ext-diff", "--no-textconv", "--no-show-signature"],
  log: ["--no-color", "--no-ext-diff", "--no-textconv", "--no-show-signature"],
};

// Paths and revisions must stay inside the checkout. `git diff` with two
// arguments, one outside the repository, silently becomes `--no-index` and
// compares arbitrary files, even after --end-of-options.
const inside = (value: string) => {
  const path = posix.normalize(value);
  return !isAbsolute(value) && path !== ".." && !path.startsWith("../");
};

// Builds Git's argv from the model's command and arguments. Paths follow
// "--" and are relative to the delegate's working directory.
export function gitArguments(command: Command, args: readonly string[], pwd = ""): { argv: string[] } | { error: string } {
  const options: string[] = [], revisions: string[] = [], paths: string[] = [];
  let dashes = false;
  for (let i = 0; i < args.length; i++) {
    const arg = args[i]!;
    if (!text(arg) || !arg) return { error: "Git arguments must be nonempty single-line text." };
    if (dashes) { paths.push(arg); continue; }
    if (arg === "--") { dashes = true; continue; }
    if (!arg.startsWith("-") || arg === "-") { (command === "status" ? paths : revisions).push(arg); continue; }
    if (command === "log" && /^-\d{1,4}$/.test(arg)) { options.push(`--max-count=${arg.slice(1)}`); continue; }
    if (flags[command].has(arg)) { options.push(arg); continue; }
    const long = arg.startsWith("--"), split = long ? arg.indexOf("=") : -1;
    const name = long ? split < 0 ? arg : arg.slice(0, split) : arg.slice(0, 2);
    let value: string | undefined = long ? split < 0 ? undefined : arg.slice(split + 1) : arg.length > 2 ? arg.slice(2) : undefined;
    const rule = Object.hasOwn(values[command], name) ? values[command][name] : undefined;
    if (!rule) return { error: `git ${command} does not allow ${name.slice(0, 64)}. Allowed options select or format output only.` };
    if (value === undefined) {
      value = args[++i];
      if (value === undefined || !text(value)) return { error: `${name} needs a value.` };
    }
    if (!rule.test(value)) return { error: `Invalid value for ${name}.` };
    options.push(long ? `${name}=${value}` : `${name}${value}`);
  }
  if (command === "log" && !options.some(o => o.startsWith("--max-count=") || o.startsWith("-n"))) options.push(`--max-count=${defaultCommits}`);
  const scoped = paths.map(path => pwd ? posix.join(pwd, path) : path);
  if (!revisions.every(inside) || !paths.every(path => !isAbsolute(path) && inside(posix.join(pwd, path))))
    return { error: "Paths and revisions must be relative and stay inside the checkout." };
  // format.pretty from the repository's config could name a %G format.
  return { argv: ["--no-replace-objects", "--literal-pathspecs", "-c", "log.showSignature=false", "-c", "color.ui=never",
    "-c", "format.pretty=medium", command,
    ...forced[command], ...options, ...command === "status" ? [] : ["--end-of-options", ...revisions], "--", ...scoped] };
}

function display(bytes: Buffer, limit: number) {
  let end = Math.min(bytes.length, limit);
  while (end < bytes.length && end > 0 && (bytes[end]! & 0xc0) === 0x80) end--;
  return bytes.toString("utf8", 0, end).toWellFormed().replace(/\r\n?/g, "\n")
    .replace(/[\p{Cc}\p{Zl}\p{Zp}\u202a-\u202e\u2066-\u2069]/gu, c => c === "\n" || c === "\t" ? c : "\uFFFD");
}
const failure = (message: string) => ({ output: [{ type: "text" as const, text: message }], isError: true });

// Offered only to read-only subagents whose grant covers the repository root:
// history and objects aren't limited to a subdirectory. It runs through the
// same hardened runner as Git review, refuses repositories with clean or
// smudge filters, and checks the grant again before releasing any output.
export function delegateGit(trust: WorkspaceTrust, policy: ToolPolicy) {
  const tool = defineTool({ name: delegateGitName,
    description: "Run a read-only Git command in this checkout: status, diff, show or log. args holds options and revisions, then \"--\" and " +
      `paths relative to the working directory, inside the checkout. Only options that select or format output are allowed. log shows ${defaultCommits} ` +
      `commits unless -n is given. Output is cut at ${outputBytes / 1024} KiB; narrow it with paths, --stat or -n.`,
    callers: ["model"], replay: "safe", executionMode: "parallel", outputLimits: { maxBytes: outputBytes + 256, maxLines: outputBytes },
    parameters: { type: "object", properties: { command: { type: "string", enum: gitCommands },
      args: { type: "array", maxItems: 32, items: { type: "string", maxLength: 1024 } } },
      required: ["command"], additionalProperties: false } as const,
    async execute({ command, args = [] }: { command: Command; args?: string[] }, api, ctx) {
      if (!gitCommands.includes(command)) return failure("Use status, diff, show or log.");
      const target = await policy.readOnlyWorkspace(delegateGitName, api, ctx);
      if (!target) return failure("Git is only available to read-only subagents in a trusted checkout.");
      const parsed = gitArguments(command, args, target.pwd);
      if ("error" in parsed) return failure(parsed.error);
      const signal = ctx.abortSignal ?? new AbortController().signal, w = target.workspace;
      try {
        const out = await trust.withGit(w.root, signal, async identity => {
          if (identity.root.path !== identity.cwd.path) return "subdirectory" as const;
          await requireExecutionWorkspace(trust, w, signal);
          // Status and worktree diffs run clean filters to compare files.
          try { await checkFilters(w.root, signal); } catch { signal.throwIfAborted(); return "filters" as const; }
          return workspaceGitOutput(w.root, signal, parsed.argv, outputBytes);
        });
        if (out === "subdirectory") return failure("Git is only available when the repository root is trusted.");
        if (out === "filters") return failure("Git is unavailable in repositories with Git filters or conditional includes configured.");
        // A grant or directory replaced while Git ran does not authorize the output.
        await requireExecutionWorkspace(trust, w, signal);
        if (!out.ok) return failure(`git ${command} failed:\n${display(out.text, errorBytes).trim() || "no diagnostic"}`);
        const body = display(out.text, outputBytes);
        return { output: [{ type: "text" as const, text: out.truncated ? `${body}\n[Output cut at ${outputBytes / 1024} KiB]` : body || "(no output)" }] };
      } catch {
        signal.throwIfAborted();
        return failure("The checkout is no longer trusted or is not a Git working tree.");
      }
    },
  });
  return defineExtension({ name: "hush-delegate-git", tools: [tool] });
}
