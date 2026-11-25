# Release redistribution inventory

The standalone release archives carry the Hush license and `THIRD_PARTY_NOTICES/` from this
directory. Its index gives a version, source and the applicable original
license or attribution for each identified component. SHA-256 filenames
deduplicate identical text. The notices do not claim that every compiled
input was retained by the linker. See [WASM rebuilds](../wasm/README.md).

- Snapshot scope: Go 1.27.4 and the 28 linked modules across four targets,
  including platform-specific D-Bus and dynamic-loading modules.
  The npm lock and esbuild byte-contributor maps cover the main bundle and
  both workers. Separate entries account for generated/vendored code, data,
  the embedded WASM pack and other WASM payloads with their runtime notices.
  Node.js and build-only executables are not distributed.
- Pass through the exact copyright, permission, disclaimer and applicable
  notice texts for identified MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0,
  ISC, BlueOak-1.0.0, Unicode-3.0, Unicode-DFS-2016, and Apache-2.0 with LLVM
  exception components. Include Go patent grants and the ncurses notice.
  Choose MIT where applicable for optional Rust dual licenses (including
  memchr); alternate texts are retained for attribution where present.
  `wide` 0.7.28 is accounted for under its supplied Zlib text. Preserve
  file-specific libm notices under their stated terms. Keep the applicable
  Unlicense notice for fast-sha256 and the CC0 and file-level notice for the
  WASI-libc dlmalloc. None of these decisions admits an unidentified component.
- The release job extracts the same archive it will upload, checks the
  executable, notices, lock, and embedded payload inventory, and runs the
  installed-artifact tests. A changed scope needs an updated inventory and
  review, not just a successful build. The corpus under `texts/` contains
  upstream bytes; do not reformat it.
