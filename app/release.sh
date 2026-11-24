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
if [ "${1:-verify}" = verify ]; then
  # The fixture copies the exact candidate to a clean install.
  node app/test/release.test.mjs "$stage/hush" "$bundle_sha" backend/src/prompts/system.md
  npm run headless:check # Preserved opt-in Pi compatibility and privacy regressions.
fi
mv -f "$stage/hush" "$output/hush"
artifact_sha=$(sha256 "$output/hush")
printf '%s  %s\n' "$artifact_sha" "$output/hush" "$bundle_sha" 'embedded hush-backend.mjs'
