# Standalone MVP candidate baseline

`standalone-rc-2026-11-23` is the initial test baseline for native store cutover. It is an unpublished, unsigned candidate, not a released predecessor. The reproducible clean-install path for the candidate uses the bundled backend, Hush-owned login and model catalog, and system Node.js 22.19.0. It does not require a Pi installation, profile, or `piEvents`.

| Artifact | SHA-256 |
| --- | --- |
| `dist/release/darwin-arm64/hush` | `0839338292d88f334d6b8f47b51060081abcc8f2431ca62a8a2525f678ef1aed` |
| `dist/release/linux-arm64/hush` | `eec512d5bb9755f97f927ffcc2d8da77d59b834e1c5d3d32255ca82a1f17c21f` |
| Embedded `hush-backend.mjs` (both artifacts) | `5dae1638e00fe45a5b5d12958cd3c8e1387d557cec3f833a6e68090360234f61` |

The build used Go 1.27.4, Node.js 22.19.0, the npm lockfile, and Git `618614082adead46a33609c62c20c792089b12eb` **plus working-tree inputs**. The source manifest has SHA-256 `45c56d9d2fdc2d70bf8d4bb9e94a58cb437a8e5cb7796b5c842a633df8403a8a`. A build of that commit alone is not this candidate. Original evidence paths are `dist/release/verification.md` and `dist/release/review-evidence/` (including `source.sha256`); those and the binaries are local build artifacts, not repository files.

## Verified and still open

- Both arm64 artifacts passed clean-install loopback checks at minimum Node. Coverage includes bundled prompt, native model selection, restart/resume, logout, and tool-free invocation-private operation. A separate loopback fixture covered OAuth refresh after restart on both candidates.
- An authorized live Codex smoke passed on the exact macOS arm64 artifact: browser OAuth, native print, same-thread restart/resume, logout, and rejection before a new admission. Linux live and forced live OAuth refresh were not tested.
- Neither amd64 variant was built or verified in this record. The separate MVP review attempt produced no independent report. Before publication, obtain independent review of the exact artifacts offered and verify every advertised target. Do not use the local coordinator checks as an independent verdict or claim a later build passed these checks.

## Use in upgrade tests

Use one of the exact candidates above to initialize an isolated standalone **format-1** store at the pinned runtime. Record the artifact digest, fixture inputs, store identity and generation, and observed native admissions. Test only features the source artifact could emit; approvals, product scheduling and file rewind are not part of this baseline. Verify the old executable's actual startup behavior against the proposed cutover fence.

Exercise missing task shapes with separately versioned synthetic seeds and record their digests, builders and task definitions. Do not label a seed as an artifact-produced store or a verification of an unshipped feature. If the exact source artifact is unavailable, fixture tests can still run, but the real-source cutover check remains unverified. Replace this baseline in the supported migration matrix only with an explicitly identified, tested successor.
