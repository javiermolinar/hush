#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
case "${1:-verify}" in
  build|verify) ;;
  *) echo 'usage: sh app/release.sh [build|verify]' >&2; exit 2 ;;
esac
os=$(go env GOHOSTOS)
arch=$(go env GOHOSTARCH)
case "$os/$arch" in
  darwin/arm64|darwin/amd64|linux/arm64|linux/amd64) ;;
  *) echo "unsupported release builder: $os/$arch" >&2; exit 1 ;;
esac
node -e 'const [major, minor] = process.versions.node.split(".").map(Number); if (major < 22 || (major === 22 && minor < 19)) { console.error("Building requires Node.js 22.19.0 or later"); process.exit(1); }'
if [ "${1:-verify}" = verify ] && [ "$(node --version)" != v22.19.0 ]; then
  echo 'Verification requires Node.js exactly 22.19.0 on PATH' >&2
  exit 1
fi
assets=app/internal/backendstart/release-assets
mkdir "$assets" # Refuse a concurrent build or stale generated input.
stage=
cleanup() {
  rm -f "$assets/hush-backend.mjs"
  rmdir "$assets"
  if [ -n "$stage" ]; then rm -rf "$stage"; fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
npm ci --ignore-scripts
npm run backend:bundle
cp dist/hush-backend.mjs "$assets/hush-backend.mjs"
output="dist/release/$os-$arch"
mkdir -p "$output"
stage=$(mktemp -d "$output/.build-XXXXXXXX")
CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -tags hush_release -o "$stage/hush" ./app/cmd/hush
sha256() {
  node --input-type=module -e 'import {createHash} from "node:crypto"; import {readFileSync} from "node:fs"; console.log(createHash("sha256").update(readFileSync(process.argv[1])).digest("hex"));' "$1"
}
bundle_sha=$(sha256 "$assets/hush-backend.mjs")
artifact_sha=$(sha256 "$stage/hush")
if [ "${1:-verify}" = verify ]; then
  evidence="$stage/evidence"
  archive="hush-$os-$arch.tar.gz"
  mkdir -p "$evidence/frames" "$stage/install"
  cp LICENSE "$stage/LICENSE"
  cp -R app/redistribution/THIRD_PARTY_NOTICES "$stage/THIRD_PARTY_NOTICES"
  COPYFILE_DISABLE=1 tar -czf "$stage/$archive" -C "$stage" hush LICENSE THIRD_PARTY_NOTICES
  printf '%s  %s\n' "$(sha256 "$stage/$archive")" "$archive" > "$stage/$archive.sha256"
  tar -xzf "$stage/$archive" -C "$stage/install"
  test "$(sha256 "$stage/install/hush")" = "$artifact_sha"
  node app/test/release-notices.mjs "$stage/install" "$os" > "$evidence/redistribution.json"
  perf_mode=enforced
  if [ "${HUSH_PERF_MODE:-}" = report ]; then perf_mode=report-only; fi
  {
    printf '%s\n' 'Release evidence v1' "target: $os-$arch" 'targets: linux-amd64 linux-arm64 darwin-amd64 darwin-arm64'
    printf '%s\n' "commit: $(git rev-parse HEAD)" "Go: $(go version)" "Node (minimum): $(node --version)"
    printf '%s\n' "binary sha256: $artifact_sha" "embedded bundle sha256: $bundle_sha" "lockfile sha256: $(sha256 package-lock.json)"
    printf '%s\n' "Performance thresholds: $perf_mode" 'Providers: synthetic loopback only, no live-provider or OS keyring verification'
  } > "$evidence/candidate.txt"
  node app/test/release-source.mjs > "$evidence/source.json"
  go version -m "$stage/hush" > "$evidence/go-buildinfo.txt"
  # Print full failed slices in CI without masking the exit status with tee.
  record() {
    name=$1; shift
    log="$evidence/$name.log"
    rc=0
    "$@" > "$log" 2>&1 || rc=$?
    printf '\n--- %s (exit %s) ---\n' "$name" "$rc"
    awk '{print}' "$log"
    return "$rc"
  }
  export HUSH_TEST_CLI="$PWD/$stage/install/hush" HUSH_TEST_BACKEND="$PWD/dist/hush-backend.mjs"
  test "$(sha256 "$HUSH_TEST_BACKEND")" = "$bundle_sha"
  record installed node app/test/release.test.mjs "$HUSH_TEST_CLI" "$bundle_sha" backend/src/prompts/system.md
  record typescript npm run build
  record native-protocol npm run protocol:check
  record native-recovery node --test --test-concurrency=1 server/test/core.test.mjs backend/test/backend.test.mjs
  optional='schedule CLI|checkpoint CLI|workspace trust'
  pi='^Pi |^raw native protocol|^persistent headless joins'
  record native-cli node --test --test-skip-pattern="$optional|$pi" app/test/headless.test.mjs
  # Run the bundled opt-in profile separately, with real producer fixtures and
  # bare-host rejection. A slice that matches no tests must not certify it.
  record pi-reference npm run profile:check
  record pi-backend node --test --test-concurrency=1 server/test/pi-events.test.mjs backend/test/pi-events.test.mjs
  record pi-cli node --test --test-name-pattern="$pi" app/test/headless.test.mjs
  grep -Eq '^# pass [1-9][0-9]*$' "$evidence/pi-cli.log"
  record optional-native-cli node --test --test-name-pattern="$optional" app/test/headless.test.mjs
  grep -Eq '^# pass [1-9][0-9]*$' "$evidence/optional-native-cli.log"
  export HUSH_RELEASE_FRAMES="$PWD/$evidence/frames"
  record go-regressions go test -count=1 -p=1 -v -skip 'TestKitty|TestImageFromDrop' ./sdk/... ./tui/... ./app/...
  for theme in latte mocha; do
    for frame in alt-up-before alt-up-after selection copied nested-escape-before nested-escape-after reconnecting; do
      test -s "$evidence/frames/$theme-$frame.ansi"
    done
  done
  node app/test/release-source.mjs > "$stage/source-after.json"
  cmp "$evidence/source.json" "$stage/source-after.json"
  mv -f "$stage/$archive" "$stage/$archive.sha256" "$output/"
  rm -rf "$output/evidence"
  mv "$evidence" "$output/evidence"
fi
mv -f "$stage/hush" "$output/hush"
printf '%s  %s\n' "$artifact_sha" "$output/hush" "$bundle_sha" 'embedded hush-backend.mjs'
