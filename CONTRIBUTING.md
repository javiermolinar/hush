# Contributing to hush

Use [AGENTS.md](AGENTS.md) to find the guide for your change. From the checkout root, with
[Mise](https://mise.jdx.dev/), Python 3 and a C compiler:

```sh
mise install
mise run setup
mise run test
mise run test:race
```

[CI](.github/workflows/ci.yml) runs these checks on Linux amd64 and macOS arm64, including the TUI
geometry, golden frames and frame budgets. The Go race run uses `-short` to skip timing checks.
Linux also checks Go formatting, verifies the SDK schema
copies (`go generate ./sdk`), and verifies the embedded release binary at the minimum supported Node
version. The normal tests use local fixtures and don't need API keys. Live provider and real OS
keyring checks remain opt-in.

[Security](.github/workflows/security.yml) scans reachable Git history with Gitleaks, checks workflow
syntax and security with actionlint and zizmor, and checks the npm lockfile for high and critical
advisories and Go code for reachable vulnerabilities on Linux. It runs on pull requests, pushes to
`main`, merge queues, and weekly. To run the checks locally:

```sh
mise --env security install
mise --env security exec -- gitleaks git --redact=100 --verbose --no-banner --ignore-gitleaks-allow --log-opts=--all .
mise --env security exec -- actionlint
mise --env security exec -- zizmor --offline --strict-collection .github/
mise --env security exec -- npm audit --audit-level=high
(cd app && mise --env security exec -- govulncheck ./... ../sdk/... ../tui/...)
```

Treat new secret findings as real until verified. Rotate exposed credentials and clean up the affected
history before sharing it. Don't add broad path or rule exclusions for fixtures or plan metadata. Use
[private reporting](SECURITY.md) for suspected vulnerabilities.

## Visual changes

Inspect light and dark captures in Catppuccin Latte and Mocha. `TestReleaseFrames` writes sample
production-model captures when `HUSH_RELEASE_FRAMES` points to an existing directory. Capture the
changed flow separately if it isn't covered. Regenerate goldens with `go test ./tui -run Golden -update`
only for an intended change, and review the frame diffs.

## Measure host integration latency

After the development build, run:

```sh
node app/test/bench-host-integrations.mjs
go test -count=1 -v ./tui -run '^TestFrameBudget/(file-picker|skill-slash|skill-sheet|loaded-context)$'
```

The offline benchmark uses a temporary profile and a real Unix socket. It
reports p50, p95 and p99 latency for file scope and pages (including 3800
entries), skill discovery and admission (including a fresh-prompt preview of
64 skills), prompt metadata, and supervisor dispatch and attach. It does not register an OS service or use
real credentials. Set `HUSH_BENCH_SAMPLES` to change the sample count. Record
that count, the machine, filesystem and Node version when comparing results.
Run without other tests competing for CPU or disk. The Go cases measure
`Update` + `View` separately and enforce the 2.08 ms p99 budget.

## Measure the fast path

The performance contract has a case for each clause. They run in `mise run test`, which
enforces their targets on a quiet native host by default; hosted CI sets
`HUSH_PERF_MODE=report` and only reports them. A case that misses its target today logs
`performance target missed (known, until <step>)` instead of failing, naming the plan step that
fixes it. After the development build:

```sh
go test -count=1 -v ./sdk -run TestReplicaApplyBudget                # replica patch cost
go test ./sdk -run '^$' -bench ReplicaApply                          # ns/op, B/op, allocs/op
go test -count=1 -v ./tui -run '^TestFrameBudget/(replica-streaming|typing-(1|4|16)k|image-typing|linked-streaming)$'
HUSH_TEST_BACKEND="$PWD/dist/hush-backend.mjs" go test -count=1 -v ./tui -run TestThreadSwitchLatency
npm run server:bench:feed                                            # server Feed flush
HUSH_BENCH_SAMPLES=30 node app/test/cold-start.test.mjs              # spawn to compatible hello
```

Thread switching and cold start have no target yet and only report. Record the machine, OS, and
Go and Node versions with any numbers you compare.

For a 60-second idle TUI request count and an external worktree edit latency
measurement, run after the development build:

```sh
HUSH_TEST_BACKEND="$PWD/dist/hush-backend.mjs" HUSH_TEST_IDLE_60S=1 \
  go test -count=1 -v ./tui -run '^TestWorkspaceWatchesLive$'
```

The test uses a real Unix socket and a temporary Git repository. It counts all
requests written by the idle TUI after the watches are armed; the seed client
and trust CLI are measured separately. The shorter default run checks tool and
grant updates without waiting 60 seconds.

After the development build, run `node backend/test/bench-rewind.mjs` to preview and
restore a fixture with 100,000 tracked files while a second socket client streams.
The offline benchmark reports event-loop delay through the durable confirm-to-ready
path and enforces p99 ≤ 50 ms by default. Run it on a quiet local filesystem.

Measure cold and pooled metadata calls on a local Unix socket and at a
simulated 50 ms round trip:

```sh
go test -count=1 -v ./sdk -run '^TestMetadataPoolRoundTrips$'
```

## Setting up the GitHub repository

1. **Review what will be pushed.** Keep runtime data, session logs and provider credentials out of
   Git. After staging the intended files, scan staged changes too:

   ```sh
   mise --env security exec -- gitleaks git --staged --redact=100 --verbose --ignore-gitleaks-allow .
   ```

   `.gitignore` does not remove files from earlier commits. If your history contains local files
   that should stay private, start the new repository from a reviewed snapshot rather than pushing
   that history. The Go module paths assume `github.com/javiermolinar/hush`; if you choose a different
   slug, update the module declarations and imports together. Once the files you want are committed,
   create the repo, for example with GitHub CLI (replace `OWNER`):

   ```sh
   gh repo create OWNER/hush --private --source=. --remote=origin --push
   ```

2. **Set Actions permissions.** Keep the default token read-only and don't allow Actions to create
   or approve pull requests. Use GitHub-hosted runners for untrusted PRs. The workflows pin external
   actions to full commit SHAs.
3. **Protect `main`** after the checks have run on GitHub, if supported by your plan. Require PRs and
   the `Test (Linux)`, `Test (macOS)`, `Repository security`, and `Dependency audit` checks. Review
   workflow and scanner config changes, and block force pushes and deletion.
4. **Install the [Renovate GitHub App](https://github.com/apps/renovate)** for this repository only.
   [Its config](.github/renovate.json) schedules routine updates and lockfile maintenance weekly,
   without automerge. Approve majors and Pi version bumps through the Dependency Dashboard. For Pi,
   update the pinned wire fixtures and reference links in the same PR. For Go toolchain updates, review
   `mise.toml`, `go.work`, the module `go` directives and docs together. Keep the release Node pin on
   the minimum supported version until that runtime requirement changes.
5. **Enable GitHub security features** where available: the dependency graph, Dependabot alerts,
   secret scanning with push protection, and private vulnerability reporting (on a public repo).
   Leave Dependabot update PRs off to avoid duplicates with Renovate. To opt into
   [CodeQL](.github/workflows/codeql.yml), enable GitHub code scanning and set the Actions repository
   variable `ENABLE_CODEQL` to `true`. This requires a public repo or GitHub Code Security on a private
   repo. Use this workflow instead of default setup. CodeQL uploads only from `main` and isn't a
   required PR check.

## Release candidates

Run [Release candidate](.github/workflows/release-candidate.yml) manually from green `main`. It runs
`mise run release` natively for Linux and macOS on amd64 and arm64. Each successful job attaches
its tested install tarball, checksum, source and build identity, and evidence from Node.js 22.19.0.
The offline slices separate the default no-Pi install, native recovery, the bundled opt-in Pi profile,
and optional commands. The blocking installed test covers CLI OAuth, confirms model and thinking
selection over the native protocol after restart, and records warm attach from process start
through `status` exit (20 samples). The evidence includes light and dark production-model frames
(`evidence/frames/*.ansi`); view each in its matching Catppuccin Latte or Mocha palette. These
are model fixtures, not terminal-emulator screenshots.

After a successful matrix, run [Release PTY compatibility](.github/workflows/release-pty.yml)
with its numeric run ID. It checks TUI login and model picking against the verified Linux amd64
archive, measures attach through the first existing transcript text on a PTY, and uploads the
walkthrough log and transcripts.
This separate run reports terminal automation failures without changing the release matrix result;
CLI login, native model selection, resume, and recovery remain blocking on every target.

Shared GitHub runners report the 100 ms warm-attach target, the 2.08 ms TUI frame
budget, and the 2 ms intro frame budget without failing on timing alone. Look for
`performance target missed (report-only)` in the logs; `evidence/candidate.txt`
records whether thresholds were enforced. Correctness checks, including SIGKILL
recovery, still fail the job. For a controlled measurement, run `mise run release`
on a quiet native host with `HUSH_PERF_MODE` unset. Thresholds are enforced by default;
a green hosted job is not evidence that a missed performance target passed.

Artifacts expire after 14 days. No tag is created and nothing is signed or published. The checks
and captures don't replace visual review, authorized live-provider tests, or independent review of the
exact artifacts before publication; see the [standalone candidate baseline](docs/standalone-release-baseline.md)
and [operator runbook](docs/store-cutover.md). The tarballs carry Hush's license
and hash-indexed third-party notices. If linked Go modules, npm contributors or
WASM inputs change, refresh the [redistribution inventory](app/redistribution/README.md)
and review the new archives before publishing.
