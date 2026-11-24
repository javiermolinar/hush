# Rebuild the release WASM inputs

The packaged image worker uses a Hush build of Photon 0.3.3 with the matching
generated wasm-bindgen JavaScript. It does not embed the npm WASM, which has no
complete build inventory tied to its bytes. The two Milo 0.9.0 WASM variants and
their generated CommonJS wrappers in Undici 8.12.4 reproduce byte-for-byte, so
the package remains unchanged. Users do not need Rust. Unbundled development
can still use Pi Durable's npm image adapter.

On macOS arm64 with curl, Rustup and SHA-256 tools on PATH, run:

```sh
bash app/wasm/rebuild-photon.sh /tmp/photon-rebuild
cmp /tmp/photon-rebuild/photon_rs_bg.wasm backend/wasm/photon_rs_bg.wasm
cmp /tmp/photon-rebuild/web/photon_rs.js backend/wasm/photon_rs.js
bash app/wasm/rebuild-milo.sh /tmp/milo-rebuild
```

Use new output directories. The scripts verify downloaded source/tool archive
hashes, compiler commits and Cargo locks. They compile the `wasm32-unknown-unknown`
runtime from the matching Rust sources using locked dependencies. Photon uses the
source crate's default `enable_wasm` features, wasm-bindgen 0.2.95 (web target)
and Binaryen 123 with explicit reference-types and bulk-memory support. Milo
uses its release build flags, including separate SIMD and non-SIMD builds, and
Binaryen 123. The Milo script checks both final payloads and wrappers.

The builds emit Cargo artifact records for target and host inputs; Photon also
emits a pre-optimization linker map. [The inventory](../redistribution/rust-build-inputs.tsv)
lists compiled inputs as a conservative superset, not a claim that optimization
retained every crate. A host executable such as wasm-opt is not linked into the
application. Attribution for the emitted glue, bundled data and file-specific
source material accompanies the distributed payloads. Update the inventory and
notices and verify a new candidate if any embedded payload changes.
