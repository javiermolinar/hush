#!/usr/bin/env bash
# Reproduce the two Milo payloads already present in the locked Undici package.
set -euo pipefail
if [[ $# != 1 || $(uname -s)-$(uname -m) != Darwin-arm64 ]]; then
  echo 'usage on macOS arm64: bash app/wasm/rebuild-milo.sh NEW_OUTPUT_DIRECTORY' >&2
  exit 2
fi
umask 022
mkdir "$1"
cd "$1"
build_root=$(pwd -P)
export CARGO_HOME="$build_root/cargo"
export CARGO_TERM_COLOR=never
unset CARGO_ENCODED_RUSTFLAGS RUSTC_WRAPPER RUSTC_WORKSPACE_WRAPPER RUSTC RUSTFLAGS

toolchain=nightly-2026-07-29
commit=e6e2e971df2401ba74d904a239679ac6d234b345
binaryen=binaryen-version_123-arm64-macos
verify() { printf '%s  %s\n' "$2" "$1" | sha256sum -c -; }
curl -fsSL --retry 3 "https://github.com/WebAssembly/binaryen/releases/download/version_123/$binaryen.tar.gz" -o "$binaryen.tar.gz"
verify "$binaryen.tar.gz" 74428be348c1a09863e7b642a1fa948cabf8ec9561052233d8288e941951725b
tar -xzf "$binaryen.tar.gz"
curl -fsSL --retry 3 "https://codeload.github.com/ShogunPanda/milo/tar.gz/$commit" -o "milo-$commit.tar.gz"
verify "milo-$commit.tar.gz" 524c6108461b98b8e3d97d626c26ec5246deae9d7615d0aa5f484b93117eeee5
tar -xzf "milo-$commit.tar.gz"

rustup toolchain install "$toolchain" --profile minimal --target wasm32-unknown-unknown --component rust-src
rustc "+$toolchain" -Vv
[[ $(rustc "+$toolchain" -Vv | awk '/commit-hash:/{print $2}') = 26ae60a9eeb20b4935be49d7a931a650fa1d2923 ]]
sysroot=$(rustc "+$toolchain" --print sysroot)
verify "$sysroot/lib/rustlib/src/rust/library/Cargo.lock" 0cd31e4d5c49d7cfbfc479e5cb63001a60c8fdca365677e8ee92137a2b5aaeeb
src="$build_root/milo-$commit"
verify "$src/parser/Cargo.lock" 3a3eb8805d986bf1eb4b10fe30ac8fafc064eb6b6ef6471e4b4bafd69bf21ea1
mkdir -p "$src/dist/wasm/release/binary"
cd "$src/parser"
for variant in simd no-simd; do
  export RUSTFLAGS='-Z unstable-options -C panic=immediate-abort'
  opts=(--enable-bulk-memory-opt)
  if [[ "$variant" = simd ]]; then
    RUSTFLAGS="$RUSTFLAGS -C target-feature=+simd128"
    opts+=(--enable-simd)
  fi
  CARGO_TARGET_DIR="$src/parser/target" cargo "+$toolchain" build --locked --lib -Z unstable-options -Z build-std=std,panic_abort \
    --target wasm32-unknown-unknown --release --message-format=json > "$build_root/$variant-artifacts.jsonl"
  "$build_root/binaryen-version_123/bin/wasm-opt" -O3 "${opts[@]}" -o "../dist/wasm/release/binary/$variant.wasm" \
    target/wasm32-unknown-unknown/release/milo_parser.wasm
done
unset RUSTFLAGS
verify ../dist/wasm/release/binary/simd.wasm 2833147ba0f82efd36ac313fad192510a9a0e3342ecf17a139b383ea82afc03a
verify ../dist/wasm/release/binary/no-simd.wasm ac63077966ddb6f0037e8f9d63981d0234b2ae6358035d1a4a896d2a6cb9030b
cargo "+$toolchain" build --locked --release --manifest-path ../scripts/Cargo.toml --target-dir ../scripts/target --bin postbuild-wasm
../scripts/target/release/postbuild-wasm release
verify ../dist/wasm/release/package-cjs/src/simd/index.js 3bc5b7120aa8732d6a1fb2692714d549d4025aa27482ba3b47e7b80ff74f23cc
verify ../dist/wasm/release/package-cjs/src/no-simd/index.js ece090d1e2e962e0de7428bf542b900b373876e5cf1d7eda43e5cd08a92bf9b7
