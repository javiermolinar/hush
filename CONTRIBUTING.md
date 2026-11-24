# Contributing to hush

Read [AGENTS.md](AGENTS.md) for project and TUI constraints. From the checkout root, with
[Mise](https://mise.jdx.dev/), Python 3 and a C compiler:

```sh
mise install
mise run setup
mise run test
mise run test:race
```

[CI](.github/workflows/ci.yml) runs these checks on Linux amd64 and macOS arm64, including the TUI
geometry, golden frames and frame budgets. Linux also checks Go formatting, verifies the SDK schema
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
`mise run release` natively for Linux and macOS on amd64 and arm64 and attaches a tarball and its
SHA-256 checksum to each successful job. Artifacts expire after 14 days. No tag is created and nothing
is signed or published. The checks don't replace live-provider tests or independent review of the exact
artifacts before publication; see the [standalone candidate baseline](docs/standalone-release-baseline.md).
