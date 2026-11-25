#!/usr/bin/env bash
# Release engineering only; neither installation nor ordinary builds require Rust.
# Reproduce the audited WASM on the supported Apple Silicon build host.
set -euo pipefail
if [[ $# != 1 || $(uname -s)-$(uname -m) != Darwin-arm64 ]]; then
  echo 'usage on macOS arm64: bash app/wasm/rebuild-photon.sh NEW_OUTPUT_DIRECTORY' >&2
  exit 2
fi
umask 022
mkdir "$1"
cd "$1"
build_root=$(pwd -P)
export CARGO_HOME="$build_root/cargo"
export CARGO_TERM_COLOR=never
unset CARGO_ENCODED_RUSTFLAGS RUSTC_WRAPPER RUSTC_WORKSPACE_WRAPPER RUSTC RUSTFLAGS

toolchain=nightly-2025-01-04
crate=photon-rs-0.3.3
bindgen=wasm-bindgen-0.2.95-aarch64-apple-darwin
binaryen=binaryen-version_123-arm64-macos
verify() { printf '%s  %s\n' "$2" "$1" | sha256sum -c -; }
fetch() {
  curl -fsSL --retry 3 "$2" -o "$1"
  verify "$1" "$3"
  tar -xzf "$1"
}
fetch "$crate.crate" "https://static.crates.io/crates/photon-rs/$crate.crate" 06e9b6d764d6656aba14f5ae43fbf48ce98ff1b1a6beb4d2e801eb0729f88773
fetch "$bindgen.tar.gz" "https://github.com/wasm-bindgen/wasm-bindgen/releases/download/0.2.95/$bindgen.tar.gz" 7d924ae562494e7bbdd81ce673ebdcfd1b343d0b00850587035a85bbbaf0ab7c
fetch "$binaryen.tar.gz" "https://github.com/WebAssembly/binaryen/releases/download/version_123/$binaryen.tar.gz" 74428be348c1a09863e7b642a1fa948cabf8ec9561052233d8288e941951725b

rustup toolchain install "$toolchain" --profile minimal --target wasm32-unknown-unknown --component rust-src
rustc "+$toolchain" -Vv
[[ $(rustc "+$toolchain" -Vv | awk '/commit-hash:/{print $2}') = 3f43b1a636738f41c48df073c5bcb97a97bf8459 ]]
sysroot=$(rustc "+$toolchain" --print sysroot)
verify "$sysroot/lib/rustlib/src/rust/library/Cargo.lock" aa34aebc09586809bf72b16b62114d811baba843bb8296c10f07c90e4e5b8e31
verify "$crate/Cargo.lock" aa3e9c86b0380b8cd5cb768985f7e270aab01a5b35c24cbfa8533fa74343318f
cargo "+$toolchain" fetch --locked --manifest-path "$crate/Cargo.toml" --target wasm32-unknown-unknown
# build-std compiles only the std source and locked runtime inputs for this target.
# Cargo may fetch those locked crates here; app and std lock digests are rechecked.
RUSTFLAGS="--remap-path-prefix=$CARGO_HOME=/cargo --remap-path-prefix=$sysroot/lib/rustlib/src/rust=/rust-src --remap-path-prefix=$build_root=/build" \
  CARGO_TARGET_DIR="$build_root/target" cargo "+$toolchain" rustc --locked -Z build-std=std,panic_abort \
    --manifest-path "$crate/Cargo.toml" --lib --release --target wasm32-unknown-unknown \
    --message-format=json -- -C "link-arg=-Map=$build_root/photon-link.map" >photon-artifacts.jsonl
verify "$sysroot/lib/rustlib/src/rust/library/Cargo.lock" aa34aebc09586809bf72b16b62114d811baba843bb8296c10f07c90e4e5b8e31
verify "$crate/Cargo.lock" aa3e9c86b0380b8cd5cb768985f7e270aab01a5b35c24cbfa8533fa74343318f
"$bindgen/wasm-bindgen" target/wasm32-unknown-unknown/release/photon_rs.wasm --out-dir web --target web --out-name photon_rs
binaryen-version_123/bin/wasm-opt -O3 --enable-mutable-globals --enable-reference-types --enable-bulk-memory \
  web/photon_rs_bg.wasm -o photon_rs_bg.wasm
sha256sum photon_rs_bg.wasm web/photon_rs.js
